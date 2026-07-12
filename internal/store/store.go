// Package store は重複排除+zstd圧縮付きのコンテンツアドレスストレージエンジン。
//
// 保存の流れ:
//  1. 入力ストリームを FastCDC で可変長チャンク(平均1MiB)に分割
//  2. 各チャンクの SHA-256 を計算し、既存チャンクなら参照カウントだけ増やす(重複排除)
//  3. 新規チャンクは zstd で圧縮して保存。圧縮で縮まないデータ(画像・動画等)は
//     そのまま raw 保存して無駄なサイズ増加を防ぐ
//  4. ファイル本体はチャンクハッシュの列(マニフェスト)として bbolt に記録
//
// 削除時は参照カウントを減らし、どのファイルからも参照されなくなった
// チャンクだけを物理削除する。
package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	bolt "go.etcd.io/bbolt"

	"github.com/nomixio260-a11y/ashuku/internal/chunker"
)

// ErrNotFound は指定IDのファイルが存在しないことを示す。
var ErrNotFound = errors.New("file not found")

const (
	compressionZstd  = "zstd"
	compressionRaw   = "raw"
	compressionDelta = "zstd-delta"
)

// deltaDictID はデルタ圧縮フレームに付ける zstd 辞書ID(非ゼロなら何でもよい)。
const deltaDictID = 1

// DefaultMaxDeltaDepth はデルタチェーンの深さ上限のデフォルト。
// デルタチャンク自身も次の類似チャンクのベースになれる(= 直近世代との
// 差分が取れて、世代ドリフトによるデルタ肥大を防ぐ)。上限に達すると
// 次は plain 保存(再アンカー)になる。深いほど削減率は上がるが、
// 読み出し時に最大この段数のチェーン復元が必要になる。
// 実測(100世代バックアップ): 深さ8=31.6x, 32=82.7x(RESEARCH.md 参照)。
// キーフレーム方式(深さ1+効率閾値での再アンカー)も実測したが、
// ドリフト蓄積でデルタが肥大しチェーン方式に大きく劣った。
const DefaultMaxDeltaDepth = 16

// deltaAcceptRatio: デルタサイズが通常圧縮の何割未満なら採用するか。
const deltaAcceptRatio = 0.9

// Config はストアの動作設定。ゼロ値はデフォルト(balanced, デルタ有効, 1MiB)。
type Config struct {
	// Compression は zstd 圧縮レベル: "fast" | "balanced"(デフォルト) | "max"
	Compression string
	// DisableDelta は類似チャンクへのデルタ圧縮を無効化する。
	DisableDelta bool
	// AvgChunkSize は平均チャンクサイズ(バイト)。0 ならデフォルト(1MiB)。
	// 初回オープン時にストアへ永続化され、以降の指定は無視される
	// (途中で変えると既存データとの重複排除が効かなくなるため)。
	AvgChunkSize int
	// MaxDeltaDepth はデルタチェーンの深さ上限。0 ならデフォルト(16)。
	// 深いほど多世代バックアップの削減率が上がるが読み出しが遅くなる。
	MaxDeltaDepth int
}

func (c Config) encoderLevel() (zstd.EncoderLevel, error) {
	switch c.Compression {
	case "fast":
		return zstd.SpeedFastest, nil
	case "", "balanced":
		return zstd.SpeedBetterCompression, nil
	case "max":
		return zstd.SpeedBestCompression, nil
	default:
		return 0, fmt.Errorf("不明な圧縮レベル %q (fast | balanced | max)", c.Compression)
	}
}

// Store はストレージエンジン本体。メソッドは並行呼び出し安全
// (書き込みは bbolt の単一ライタで直列化される)。
type Store struct {
	dir       string
	db        *bolt.DB
	enc       *zstd.Encoder
	dec       *zstd.Decoder
	level     zstd.EncoderLevel
	delta     bool
	chunkSize int
	maxDepth  int
}

// Open は dataDir 配下にストアを開く(なければ作成)。
func Open(dataDir string, cfg Config) (*Store, error) {
	level, err := cfg.encoderLevel()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "chunks"), 0o700); err != nil {
		return nil, err
	}
	db, err := openMetaDB(filepath.Join(dataDir, "meta.db"))
	if err != nil {
		return nil, err
	}
	requested := cfg.AvgChunkSize
	if requested <= 0 {
		requested = chunker.DefaultAverageSize
	}
	var chunkSize int
	err = db.Update(func(tx *bolt.Tx) error {
		chunkSize, err = resolveAvgChunkSize(tx, requested)
		return err
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	if chunkSize != requested {
		log.Printf("ashuku: 平均チャンクサイズは初回設定 %d bytes を使用します(指定 %d は無視)",
			chunkSize, requested)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
	if err != nil {
		db.Close()
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		db.Close()
		return nil, err
	}
	maxDepth := cfg.MaxDeltaDepth
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDeltaDepth
	}
	return &Store{
		dir:       dataDir,
		db:        db,
		enc:       enc,
		dec:       dec,
		level:     level,
		delta:     !cfg.DisableDelta,
		chunkSize: chunkSize,
		maxDepth:  maxDepth,
	}, nil
}

// Close はストアを閉じる。
func (s *Store) Close() error {
	s.enc.Close()
	s.dec.Close()
	return s.db.Close()
}

func (s *Store) chunkPath(hash string) string {
	return filepath.Join(s.dir, "chunks", hash[:2], hash)
}

// Put は r の内容を name として保存し、マニフェストを返す。
func (s *Store) Put(name string, r io.Reader) (*FileManifest, error) {
	ck, err := chunker.New(r, s.chunkSize)
	if err != nil {
		return nil, err
	}

	m := &FileManifest{
		ID:        newID(),
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}

	for {
		chunk, err := ck.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.rollbackChunks(m.Chunks)
			return nil, fmt.Errorf("チャンク分割に失敗: %w", err)
		}
		hash := sha256.Sum256(chunk.Data)
		hexHash := hex.EncodeToString(hash[:])
		if err := s.storeChunk(hexHash, chunk.Data); err != nil {
			s.rollbackChunks(m.Chunks)
			return nil, err
		}
		m.Chunks = append(m.Chunks, hexHash)
		m.Size += int64(len(chunk.Data))
	}

	err = s.db.Update(func(tx *bolt.Tx) error {
		return putFileManifest(tx, m)
	})
	if err != nil {
		s.rollbackChunks(m.Chunks)
		return nil, err
	}
	return m, nil
}

// storeChunk はチャンクを重複排除しつつ保存する。
//
//  1. 既存チャンク(完全一致)なら参照カウントを増やすだけ(重複排除)
//  2. 新規なら類似チャンクを索引から探し、見つかればそれをベースに
//     デルタ圧縮(zstd 辞書圧縮)を試す。十分縮めばデルタで保存
//  3. それ以外は zstd 圧縮(縮まなければ raw)で保存
func (s *Store) storeChunk(hash string, data []byte) error {
	// 高速パス: 完全一致の既存チャンクは圧縮せずに参照カウントだけ増やす。
	existed := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err != nil || meta == nil {
			return err
		}
		existed = true
		meta.RefCount++
		return putChunkMeta(tx, hash, meta)
	})
	if err != nil || existed {
		return err
	}

	// 圧縮・類似検索は CPU/IO コストが高いので bbolt の書き込みロック外で行う。
	compressed := s.enc.EncodeAll(data, make([]byte, 0, len(data)/2))

	var features []uint64
	var baseHash string
	var deltaData []byte
	if s.delta {
		features = computeFeatures(data)
		baseHash, deltaData = s.tryDelta(features, data, len(compressed))
	}

	return s.db.Update(func(tx *bolt.Tx) error {
		// 並行アップロードが同じチャンクを先に登録した可能性を再確認。
		meta, err := getChunkMeta(tx, hash)
		if err != nil {
			return err
		}
		if meta != nil {
			meta.RefCount++
			return putChunkMeta(tx, hash, meta)
		}

		// デルタ採用時はベースがまだ存在し深さに余裕があるか確認し、参照を増やす。
		depth := 0
		if baseHash != "" {
			baseMeta, err := getChunkMeta(tx, baseHash)
			if err != nil {
				return err
			}
			if baseMeta == nil || baseMeta.Depth >= s.maxDepth {
				baseHash = "" // ベース消失/深すぎ → 通常圧縮にフォールバック
			} else {
				depth = baseMeta.Depth + 1
				baseMeta.RefCount++
				if err := putChunkMeta(tx, baseHash, baseMeta); err != nil {
					return err
				}
			}
		}

		newMeta := &ChunkMeta{RawSize: int64(len(data)), RefCount: 1, Features: features}
		var stored []byte
		switch {
		case baseHash != "":
			newMeta.Compression = compressionDelta
			newMeta.BaseHash = baseHash
			newMeta.Depth = depth
			stored = deltaData
		case len(compressed) < len(data):
			newMeta.Compression = compressionZstd
			stored = compressed
		default:
			newMeta.Compression = compressionRaw
			stored = data
		}
		newMeta.StoredSize = int64(len(stored))

		if err := s.writeChunkFile(hash, stored); err != nil {
			return err
		}
		// 深さ上限に達したチャンクはベースにできないので索引を汚さない。
		if newMeta.Depth < s.maxDepth {
			if err := registerSketches(tx, hash, features); err != nil {
				return err
			}
		}
		return putChunkMeta(tx, hash, newMeta)
	})
}

// tryDelta は類似チャンクをベースとしたデルタ圧縮を試す。
// 通常圧縮より十分小さくなる場合のみ (baseHash, delta) を返す。
func (s *Store) tryDelta(features []uint64, data []byte, plainSize int) (string, []byte) {
	var baseHash string
	s.db.View(func(tx *bolt.Tx) error {
		if h := lookupSketch(tx, features); h != "" {
			meta, err := getChunkMeta(tx, h)
			if err == nil && meta != nil && meta.Depth < s.maxDepth {
				baseHash = h
			}
		}
		return nil
	})
	if baseHash == "" {
		return "", nil
	}
	base, err := s.readChunk(baseHash)
	if err != nil {
		return "", nil // ベースを読めなければ諦めて通常圧縮
	}
	delta, err := s.deltaCompress(base, data)
	if err != nil {
		return "", nil
	}
	// 効果が deltaAcceptRatio 未満なら不採用(→ plain 保存 = 新キーフレーム)。
	if float64(len(delta)) >= float64(plainSize)*deltaAcceptRatio {
		return "", nil
	}
	return baseHash, delta
}

// deltaCompress は base を zstd 辞書として data を圧縮する。
// base と共通する部分はほぼゼロコストになり、差分だけが残る。
func (s *Store) deltaCompress(base, data []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil,
		zstd.WithEncoderLevel(s.level),
		zstd.WithEncoderDictRaw(deltaDictID, base),
		zstd.WithWindowSize(deltaWindowSize(len(base)+len(data))),
	)
	if err != nil {
		return nil, err
	}
	defer enc.Close()
	return enc.EncodeAll(data, make([]byte, 0, 4096)), nil
}

// deltaWindowSize はベース+データ全体への後方参照が届く窓サイズ
// (2の冪、1MiB〜128MiB)を返す。
func deltaWindowSize(total int) int {
	w := 1 << 20
	for w < total && w < 128<<20 {
		w <<= 1
	}
	return w
}

// writeChunkFile は temp ファイル + rename でアトミックに書き込む。
func (s *Store) writeChunkFile(hash string, data []byte) error {
	path := s.chunkPath(hash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// rollbackChunks はアップロード失敗時に、加算済みの参照カウントを戻す。
func (s *Store) rollbackChunks(hashes []string) {
	if len(hashes) == 0 {
		return
	}
	orphans, err := s.releaseChunks(hashes)
	if err != nil {
		return // ロールバック失敗はチャンク孤児化のみで整合性は壊れない
	}
	s.removeChunkFiles(orphans)
}

// Get は指定IDのファイルのマニフェストと復元ストリームを返す。
func (s *Store) Get(id string) (*FileManifest, io.ReadCloser, error) {
	var m *FileManifest
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		m, err = getFileManifest(tx, id)
		return err
	})
	if err != nil {
		return nil, nil, err
	}

	pr, pw := io.Pipe()
	go func() {
		for _, hash := range m.Chunks {
			data, err := s.readChunk(hash)
			if err != nil {
				pw.CloseWithError(fmt.Errorf("チャンク %s の読み出しに失敗: %w", hash[:12], err))
				return
			}
			if _, err := pw.Write(data); err != nil {
				return // 読み手が閉じた
			}
		}
		pw.Close()
	}()
	return m, pr, nil
}

// readChunk はチャンクを読み出して伸長し、ハッシュを検証して返す。
func (s *Store) readChunk(hash string) ([]byte, error) {
	var meta *ChunkMeta
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		meta, err = getChunkMeta(tx, hash)
		return err
	})
	if err != nil {
		return nil, err
	}
	if meta == nil {
		return nil, fmt.Errorf("チャンクメタデータがありません")
	}

	stored, err := os.ReadFile(s.chunkPath(hash))
	if err != nil {
		return nil, err
	}
	data := stored
	switch meta.Compression {
	case compressionZstd:
		data, err = s.dec.DecodeAll(stored, make([]byte, 0, meta.RawSize))
		if err != nil {
			return nil, fmt.Errorf("伸長に失敗: %w", err)
		}
	case compressionDelta:
		// ベースチェーンをたどる。深さは maxDepth で制限されている。
		base, err := s.readChunk(meta.BaseHash)
		if err != nil {
			return nil, fmt.Errorf("ベースチャンクの読み出しに失敗: %w", err)
		}
		data, err = decodeDelta(base, stored, meta.RawSize)
		if err != nil {
			return nil, fmt.Errorf("デルタ伸長に失敗: %w", err)
		}
	}

	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("チャンクが破損しています")
	}
	return data, nil
}

// decodeDelta は base を辞書としてデルタフレームを伸長する。
func decodeDelta(base, delta []byte, rawSize int64) ([]byte, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderDictRaw(deltaDictID, base))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	return dec.DecodeAll(delta, make([]byte, 0, rawSize))
}

// Delete はファイルを削除し、参照されなくなったチャンクを物理削除する。
func (s *Store) Delete(id string) error {
	var m *FileManifest
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		m, err = getFileManifest(tx, id)
		if err != nil {
			return err
		}
		return tx.Bucket(bucketFiles).Delete([]byte(id))
	})
	if err != nil {
		return err
	}

	orphans, err := s.releaseChunks(m.Chunks)
	if err != nil {
		return err
	}
	s.removeChunkFiles(orphans)
	return nil
}

// releaseChunks は各ハッシュの参照カウントを1減らし、0になった
// (=物理削除してよい)ハッシュ列を返す。デルタチャンクが消える場合は
// そのベースへの参照もカスケードして解放する。
func (s *Store) releaseChunks(hashes []string) ([]string, error) {
	var orphans []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		orphans = orphans[:0]
		queue := append([]string(nil), hashes...)
		for len(queue) > 0 {
			hash := queue[0]
			queue = queue[1:]
			meta, err := getChunkMeta(tx, hash)
			if err != nil {
				return err
			}
			if meta == nil {
				continue
			}
			meta.RefCount--
			if meta.RefCount > 0 {
				if err := putChunkMeta(tx, hash, meta); err != nil {
					return err
				}
				continue
			}
			if err := tx.Bucket(bucketChunks).Delete([]byte(hash)); err != nil {
				return err
			}
			if err := dropSketches(tx, hash, meta.Features); err != nil {
				return err
			}
			if meta.BaseHash != "" {
				queue = append(queue, meta.BaseHash)
			}
			orphans = append(orphans, hash)
		}
		return nil
	})
	return orphans, err
}

func (s *Store) removeChunkFiles(hashes []string) {
	for _, hash := range hashes {
		os.Remove(s.chunkPath(hash))
	}
}

// List は保存済みファイル一覧を作成日時の降順で返す。
func (s *Store) List() ([]*FileManifest, error) {
	var files []*FileManifest
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
			var m FileManifest
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			m.Chunks = nil // 一覧にはチャンク列は不要
			files = append(files, &m)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].CreatedAt.After(files[j].CreatedAt)
	})
	return files, nil
}

// Stats はストア全体の容量統計。
type Stats struct {
	FileCount  int `json:"file_count"`
	ChunkCount int `json:"chunk_count"`
	// DeltaChunkCount は類似チャンクへのデルタとして保存されたチャンク数。
	DeltaChunkCount int `json:"delta_chunk_count"`
	// LogicalBytes はアップロードされたデータの合計(見かけのサイズ)。
	LogicalBytes int64 `json:"logical_bytes"`
	// UniqueBytes は重複排除後のユニークデータ量(圧縮前)。
	UniqueBytes int64 `json:"unique_bytes"`
	// PhysicalBytes は実際にディスクを消費している量。
	PhysicalBytes int64 `json:"physical_bytes"`
	// DedupRatio = Logical / Unique(重複排除による削減倍率)
	DedupRatio float64 `json:"dedup_ratio"`
	// CompressionRatio = Unique / Physical(圧縮による削減倍率)
	CompressionRatio float64 `json:"compression_ratio"`
	// TotalRatio = Logical / Physical(総合削減倍率)
	TotalRatio float64 `json:"total_ratio"`
	// SavedBytes = Logical - Physical(節約できた容量)
	SavedBytes int64 `json:"saved_bytes"`
}

// Stats は現在の容量統計を集計して返す。
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{}
	err := s.db.View(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
			var m FileManifest
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			st.FileCount++
			st.LogicalBytes += m.Size
			return nil
		}); err != nil {
			return err
		}
		return tx.Bucket(bucketChunks).ForEach(func(_, v []byte) error {
			var c ChunkMeta
			if err := json.Unmarshal(v, &c); err != nil {
				return err
			}
			st.ChunkCount++
			if c.Compression == compressionDelta {
				st.DeltaChunkCount++
			}
			st.UniqueBytes += c.RawSize
			st.PhysicalBytes += c.StoredSize
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	st.DedupRatio = ratio(st.LogicalBytes, st.UniqueBytes)
	st.CompressionRatio = ratio(st.UniqueBytes, st.PhysicalBytes)
	st.TotalRatio = ratio(st.LogicalBytes, st.PhysicalBytes)
	st.SavedBytes = st.LogicalBytes - st.PhysicalBytes
	return st, nil
}

func ratio(a, b int64) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand の失敗は続行不能
	}
	return strings.ToLower(hex.EncodeToString(b[:]))
}
