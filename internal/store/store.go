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
	compressionZstd = "zstd"
	compressionRaw  = "raw"
)

// Store はストレージエンジン本体。メソッドは並行呼び出し安全
// (書き込みは bbolt の単一ライタで直列化される)。
type Store struct {
	dir string
	db  *bolt.DB
	enc *zstd.Encoder
	dec *zstd.Decoder
}

// Open は dataDir 配下にストアを開く(なければ作成)。
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "chunks"), 0o700); err != nil {
		return nil, err
	}
	db, err := openMetaDB(filepath.Join(dataDir, "meta.db"))
	if err != nil {
		return nil, err
	}
	// 容量優先のためデフォルトより高い圧縮レベルを使う。
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBetterCompression))
	if err != nil {
		db.Close()
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{dir: dataDir, db: db, enc: enc, dec: dec}, nil
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
	ck, err := chunker.New(r)
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

// storeChunk はチャンクを重複排除しつつ保存する。既存なら参照カウントを
// 増やすだけ。新規なら zstd 圧縮(縮まなければ raw)でファイルに書く。
func (s *Store) storeChunk(hash string, data []byte) error {
	// 圧縮は CPU コストが高いので bbolt の書き込みロック外で先にやっておき、
	// 既存チャンクだった場合は捨てる(重複時の無駄より lock 保持時間短縮を優先)。
	compressed := s.enc.EncodeAll(data, make([]byte, 0, len(data)/2))

	return s.db.Update(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err != nil {
			return err
		}
		if meta != nil {
			meta.RefCount++
			return putChunkMeta(tx, hash, meta)
		}

		stored := compressed
		compression := compressionZstd
		if len(compressed) >= len(data) {
			stored = data
			compression = compressionRaw
		}
		if err := s.writeChunkFile(hash, stored); err != nil {
			return err
		}
		return putChunkMeta(tx, hash, &ChunkMeta{
			Compression: compression,
			RawSize:     int64(len(data)),
			StoredSize:  int64(len(stored)),
			RefCount:    1,
		})
	})
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
	if meta.Compression == compressionZstd {
		data, err = s.dec.DecodeAll(stored, make([]byte, 0, meta.RawSize))
		if err != nil {
			return nil, fmt.Errorf("伸長に失敗: %w", err)
		}
	}

	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("チャンクが破損しています")
	}
	return data, nil
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
// (=物理削除してよい)ハッシュ列を返す。
func (s *Store) releaseChunks(hashes []string) ([]string, error) {
	var orphans []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		orphans = orphans[:0]
		for _, hash := range hashes {
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
	FileCount  int   `json:"file_count"`
	ChunkCount int   `json:"chunk_count"`
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
