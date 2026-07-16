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
	"bytes"
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
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	bolt "go.etcd.io/bbolt"

	"github.com/nomixio260-a11y/ashuku/internal/chunker"
	"github.com/nomixio260-a11y/ashuku/internal/precomp"
	"github.com/nomixio260-a11y/ashuku/internal/zstdc"
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
// 実測(100世代バックアップ): 深さ8=31.6x, 16=39.0x, 32=82.7x(RESEARCH.md
// 参照)。深いチェーンの読み出しコストは chunkCache が吸収する。
// キーフレーム方式(深さ1+効率閾値での再アンカー)も実測したが、
// ドリフト蓄積でデルタが肥大しチェーン方式に大きく劣った。
const DefaultMaxDeltaDepth = 32

// DefaultCacheBytes は伸長済みチャンクキャッシュの容量デフォルト(128MiB)。
const DefaultCacheBytes = 128 << 20

// deltaAcceptRatio: デルタサイズが通常圧縮の何割未満なら採用するか。
const deltaAcceptRatio = 0.9

// Config はストアの動作設定。ゼロ値はデフォルト(auto, デルタ有効, 1MiB)。
type Config struct {
	// Compression は zstd 圧縮モード:
	//   "auto"(デフォルト) = チャンクごとに fast で探査し、十分縮む場合のみ
	//                        最高レベルで再圧縮(テキストは最高圧縮率、
	//                        圧縮不能データは高速のまま)
	//   "fast" | "balanced" | "max" = 固定レベル
	// アップロード単位で PutOptions により上書きできる。
	Compression string
	// DisableDelta は類似チャンクへのデルタ圧縮を無効化する。
	DisableDelta bool
	// AvgChunkSize は平均チャンクサイズ(バイト)。0 ならデフォルト(1MiB)。
	// 初回オープン時にストアへ永続化され、以降の指定は無視される
	// (途中で変えると既存データとの重複排除が効かなくなるため)。
	AvgChunkSize int
	// MaxDeltaDepth はデルタチェーンの深さ上限。0 ならデフォルト(32)。
	// 深いほど多世代バックアップの削減率が上がるが読み出しが遅くなる
	// (チェーン読み出しはキャッシュで大部分吸収される)。
	MaxDeltaDepth int
	// CacheBytes は伸長済みチャンクキャッシュの容量。0 ならデフォルト(128MiB)。
	CacheBytes int64
	// DisablePrecomp は gzip precompression(zlib産gzipを展開して保存)を
	// 無効化する。CGO 無効ビルドでは常に無効。
	DisablePrecomp bool
	// PrecompMaxPlain は precompression が扱う展開データの上限(バイト)。
	// 0 ならデフォルト(64MiB)。分解・再構成はメモリ上で行うため、
	// この値 × PrecompParallel がメモリ使用量の上限になる。
	// 超えるストリームは安全に素通し(通常のストリーミング保存)される。
	PrecompMaxPlain int64
	// PrecompParallel は precompression の同時実行数上限。0 ならデフォルト(2)。
	// 超過分は precompression をスキップして通常経路で保存される
	// (待たせない: 多人数同時アップロードでのメモリ爆発を防ぐ)。
	PrecompParallel int
}

// DefaultPrecompMaxPlain は precompression の展開上限デフォルト(64MiB)。
const DefaultPrecompMaxPlain = 64 << 20

// DefaultPrecompParallel は precompression の同時実行数デフォルト。
const DefaultPrecompParallel = 2

// ErrQuotaExceeded は所有者のクォータ超過を示す。
var ErrQuotaExceeded = errors.New("容量クォータを超過しています")

// ErrTooLarge はアップロードサイズ上限の超過を示す。
var ErrTooLarge = errors.New("アップロードサイズが上限を超えています")

// ValidCompression は圧縮モード名の妥当性を検査する(""はデフォルト=auto)。
func ValidCompression(mode string) bool {
	switch mode {
	case "", "auto", "fast", "balanced", "max":
		return true
	}
	return false
}

// deltaLevel はデルタ圧縮に使うレベル。max モードのみ最高レベル、
// それ以外は balanced(デルタは小さいので速度優先で十分)。
func deltaLevelFor(mode string) zstd.EncoderLevel {
	if mode == "max" {
		return zstd.SpeedBestCompression
	}
	return zstd.SpeedBetterCompression
}

// Store はストレージエンジン本体。メソッドは並行呼び出し安全
// (書き込みは bbolt の単一ライタで直列化される)。
type Store struct {
	dir         string
	db          *bolt.DB
	encFast     *zstd.Encoder
	encBalanced *zstd.Encoder
	encBest     *zstd.Encoder
	dec         *zstd.Decoder
	mode        string            // デフォルト圧縮モード(auto/fast/balanced/max)
	level       zstd.EncoderLevel // デルタ圧縮のレベル
	delta       bool
	chunkSize   int
	maxDepth    int
	cache       *chunkCache
	pw          *packWriter
	precomp     bool
	precompMax  int64         // precompression の展開上限
	precompSem  chan struct{} // precompression の同時実行制限
	optMu       sync.Mutex    // Optimize の同時実行を直列化
}

// Open は dataDir 配下にストアを開く(なければ作成)。
func Open(dataDir string, cfg Config) (*Store, error) {
	if !ValidCompression(cfg.Compression) {
		return nil, fmt.Errorf("不明な圧縮モード %q (auto | fast | balanced | max)", cfg.Compression)
	}
	mode := cfg.Compression
	if mode == "" {
		mode = "auto"
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
		if err != nil {
			return err
		}
		// 所有者→ファイル索引を(未構築なら)一度だけ全ファイルから再構築する。
		return migrateFileOwners(tx)
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	if chunkSize != requested {
		log.Printf("ashuku: 平均チャンクサイズは初回設定 %d bytes を使用します(指定 %d は無視)",
			chunkSize, requested)
	}
	newEnc := func(level zstd.EncoderLevel) (*zstd.Encoder, error) {
		return zstd.NewWriter(nil, zstd.WithEncoderLevel(level))
	}
	encFast, err := newEnc(zstd.SpeedFastest)
	if err != nil {
		db.Close()
		return nil, err
	}
	encBalanced, err := newEnc(zstd.SpeedBetterCompression)
	if err != nil {
		db.Close()
		return nil, err
	}
	encBest, err := newEnc(zstd.SpeedBestCompression)
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
	cacheBytes := cfg.CacheBytes
	if cacheBytes <= 0 {
		cacheBytes = DefaultCacheBytes
	}
	precompMax := cfg.PrecompMaxPlain
	if precompMax <= 0 {
		precompMax = DefaultPrecompMaxPlain
	}
	precompPar := cfg.PrecompParallel
	if precompPar <= 0 {
		precompPar = DefaultPrecompParallel
	}
	st := &Store{
		dir:         dataDir,
		db:          db,
		encFast:     encFast,
		encBalanced: encBalanced,
		encBest:     encBest,
		dec:         dec,
		mode:        mode,
		level:       deltaLevelFor(mode),
		delta:       !cfg.DisableDelta,
		chunkSize:   chunkSize,
		maxDepth:    maxDepth,
		cache:       newChunkCache(cacheBytes),
		pw:          newPackWriter(filepath.Join(dataDir, "packs")),
		precomp:     !cfg.DisablePrecomp && precomp.Supported(),
		precompMax:  precompMax,
		precompSem:  make(chan struct{}, precompPar),
	}
	// クラッシュで取り残された temp ファイル(.tmp-*)を掃除する。
	// これらは rename 前に落ちた書き込みの残骸で、参照されていない。
	st.sweepTempFiles()
	return st, nil
}

// sweepTempFiles は chunks/ と regions/ 配下の孤児 temp ファイルを削除する。
func (s *Store) sweepTempFiles() {
	for _, sub := range []string{"chunks", "regions"} {
		root := filepath.Join(s.dir, sub)
		filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if !d.IsDir() && strings.HasPrefix(d.Name(), ".tmp-") {
				os.Remove(path)
			}
			return nil
		})
	}
}

// compressChunk はモードに応じてチャンクを圧縮する。
//
// auto は fast で探査し、十分縮む(1.11倍以上)データだけ最高レベルで
// 再圧縮する2段階方式(ZFS の zstd early-abort と同型。あちらは
// 圧縮不能データで4.5倍のスループット改善・容量コスト0.3%未満を実証)。
// 「fast では縮まないが高レベルなら縮む」病理ケースの偽陰性は、判定コスト
// との釣り合いから ZFS 同様に許容する(RESEARCH.md 参照)。
// 小チャンクは判定コスト比率が高く高レベルの利得も小さいため、
// ZFS(128KB未満は対象外)に倣い探査せず balanced 一発で圧縮する。
func (s *Store) compressChunk(data []byte, mode string) []byte {
	out := make([]byte, 0, len(data)/2)
	switch mode {
	case "fast":
		return s.encFast.EncodeAll(data, out)
	case "balanced":
		return s.encBalanced.EncodeAll(data, out)
	case "max":
		return s.bestCompress(data)
	default: // auto
		if len(data) < 128<<10 {
			return s.encBalanced.EncodeAll(data, out)
		}
		quick := s.encFast.EncodeAll(data, out)
		if len(quick)*10 >= len(data)*9 {
			return quick // ほぼ縮まない → 重い再圧縮は無駄
		}
		best := s.bestCompress(data)
		if len(best) < len(quick) {
			return best
		}
		return quick
	}
}

// bestCompress は使える中で最強のエンコーダで圧縮する。
// 本家 libzstd(level 19)が使えるビルドではそれを使い(純Go最高レベル
// = 本家 level 11 相当より、テキスト系で 8〜10% 小さい)、
// 出力は標準 zstd フレームなので復号側は変わらない。
func (s *Store) bestCompress(data []byte) []byte {
	if zstdc.Available() {
		if out, err := zstdc.Compress(data); err == nil {
			return out
		}
	}
	return s.encBest.EncodeAll(data, make([]byte, 0, len(data)/2))
}

// Close はストアを閉じる。
func (s *Store) Close() error {
	s.pw.close()
	s.encFast.Close()
	s.encBalanced.Close()
	s.encBest.Close()
	s.dec.Close()
	return s.db.Close()
}

// chunkPath は保存表現ファイルのパス。rep は表現ID(空=初期表現)。
func (s *Store) chunkPath(hash, rep string) string {
	name := hash
	if rep != "" {
		name = hash + "-" + rep
	}
	return filepath.Join(s.dir, "chunks", hash[:2], name)
}

// PutOptions はアップロード単位の動作指定。
type PutOptions struct {
	// Compression は圧縮モードの上書き("" ならストアのデフォルト)。
	// "auto" | "fast" | "balanced" | "max"
	Compression string
	// Owner はファイルの所有者ID(APIキーごとの分離に使う)。"" は共有(認証なし)。
	Owner string
	// Quota は所有者の論理容量上限(バイト)。0 なら無制限。
	// 使用量+このアップロードが上限を超えるとコミット時に
	// ErrQuotaExceeded で失敗する(チャンクはロールバックされる)。
	Quota int64
	// MaxBytes は1アップロードのサイズ上限。0 なら無制限。
	// 超えると ErrTooLarge で失敗する。
	MaxBytes int64
	// DisablePrecomp はこのアップロードで precompression を行わない
	// (サーバーCPU最小化モード用)。
	DisablePrecomp bool
}

// Put は r の内容を name として保存し、マニフェストを返す。
func (s *Store) Put(name string, r io.Reader) (*FileManifest, error) {
	return s.PutWithOptions(name, r, PutOptions{})
}

// PutWithOptions はアップロード単位のオプション付きで保存する。
func (s *Store) PutWithOptions(name string, r io.Reader, opts PutOptions) (*FileManifest, error) {
	if !ValidCompression(opts.Compression) {
		return nil, fmt.Errorf("不明な圧縮モード %q", opts.Compression)
	}
	mode := opts.Compression
	if mode == "" {
		mode = s.mode
	}
	m := &FileManifest{
		ID:        newID(),
		Name:      name,
		CreatedAt: time.Now().UTC(),
	}

	m.Owner = opts.Owner

	// precompression: zlib産の deflate 系ストリーム(gzip 単一/マルチメンバー・
	// 生 zlib)なら「展開データ+レシピ」に分解し、展開データを dedup/デルタ/
	// zstd の対象にする(ビット一致検証済みの場合のみ)。
	// 該当しないストリーム・上限超過・同時実行枠の超過は通常経路へ素通し
	// (多人数同時アップロードでのメモリ爆発を防ぐ)。
	if s.precomp && !opts.DisablePrecomp {
		head := make([]byte, 3)
		n, _ := io.ReadFull(r, head)
		rest := io.MultiReader(bytes.NewReader(head[:n]), r)
		if n == 3 && (precomp.IsGzip(head) || precomp.IsZlib(head) || precomp.IsPNG(head)) && s.acquirePrecomp() {
			buf, overflow, err := readUpTo(rest, int(s.precompMax))
			if err != nil {
				s.releasePrecomp()
				return nil, err
			}
			if opts.MaxBytes > 0 && int64(len(buf)) > opts.MaxBytes {
				s.releasePrecomp()
				return nil, ErrTooLarge
			}
			if overflow == nil && s.tryPrecomp(m, buf) {
				// サイズ上限は元ストリーム(len(buf))に適用済み。展開データの
				// putStream には上限をかけない(展開は正当に大きくなりうる)。
				err := s.putStream(m, bytes.NewReader(m.precompPlain), mode, 0)
				s.releasePrecomp()
				if err != nil {
					return nil, err
				}
				// GET が返すのは元のストリームなので Size は元サイズに合わせ、
				// チャンク化視点のサイズ(展開データ)は別に記録する。
				m.ChunkedSize = m.Size
				m.Size = int64(len(buf))
				m.precompPlain = nil
				return m, s.commitManifest(m, opts.Quota)
			}
			s.releasePrecomp()
			if overflow == nil {
				rest = bytes.NewReader(buf)
			} else {
				rest = io.MultiReader(bytes.NewReader(buf), overflow)
			}
		}
		r = rest
	}

	if err := s.putStream(m, r, mode, opts.MaxBytes); err != nil {
		return nil, err
	}
	return m, s.commitManifest(m, opts.Quota)
}

// acquirePrecomp は precompression の実行枠を取る(待たずに false を返す)。
func (s *Store) acquirePrecomp() bool {
	select {
	case s.precompSem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Store) releasePrecomp() { <-s.precompSem }

// tryPrecomp は buf を gzip(単一/マルチ)または生 zlib として分解を試み、
// 成功したらマニフェストにレシピを記録して真を返す。
// 展開データは m.precompPlain に一時保持される。
func (s *Store) tryPrecomp(m *FileManifest, buf []byte) bool {
	sum := sha256.Sum256(buf)
	switch {
	case precomp.IsGzip(buf):
		// まず単一メンバーとして試し(既存フォーマット互換)、
		// だめならマルチメンバーとして試す。
		if u, ok := precomp.TryUnwrap(buf, s.precompMax); ok {
			m.Encoding = EncodingGzipZlibV1
			m.PrecompHeader = u.Header
			m.PrecompLevel = u.Level
			m.precompPlain = u.Plain
		} else if plain, members, ok := precomp.TryUnwrapGzipMulti(buf, s.precompMax); ok {
			m.Encoding = EncodingGzipMultiV1
			m.PrecompMembers = members
			m.precompPlain = plain
		} else {
			return false
		}
	case precomp.IsPNG(buf):
		u, ok := precomp.TryUnwrapPNG(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingPNGV1
		m.PrecompPNG = u.Recipe
		m.PrecompLevel = u.Level
		m.precompPlain = u.Plain
	case precomp.IsZlib(buf):
		u, ok := precomp.TryUnwrapZlib(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingZlibV1
		m.PrecompHeader = u.Header
		m.PrecompLevel = u.Level
		m.precompPlain = u.Plain
	default:
		return false
	}
	m.OrigSHA256 = hex.EncodeToString(sum[:])
	return true
}

// readUpTo は最大 max バイトまで読む。入力がそれ以下で終われば
// (全データ, nil) を、超えれば (先頭 max バイト, 残りのリーダ) を返す。
func readUpTo(r io.Reader, max int) ([]byte, io.Reader, error) {
	buf, err := io.ReadAll(io.LimitReader(r, int64(max)))
	if err != nil {
		return nil, nil, err
	}
	if len(buf) < max {
		return buf, nil, nil
	}
	// 続きがあるか1バイト先読み
	var probe [1]byte
	n, err := r.Read(probe[:])
	if n == 0 && (err == io.EOF || err == nil) {
		return buf, nil, nil
	}
	if err != nil && err != io.EOF {
		return nil, nil, err
	}
	return buf, io.MultiReader(bytes.NewReader(probe[:n]), r), nil
}

// putStream は r をチャンク化して保存し、m にチャンク列とサイズを記録する。
// maxBytes > 0 のとき、超過した時点で中断してロールバックする。
func (s *Store) putStream(m *FileManifest, r io.Reader, mode string, maxBytes int64) error {
	ck, err := chunker.New(r, s.chunkSize)
	if err != nil {
		return err
	}
	for {
		chunk, err := ck.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.rollbackChunks(m.Chunks)
			return fmt.Errorf("チャンク分割に失敗: %w", err)
		}
		if maxBytes > 0 && m.Size+int64(len(chunk.Data)) > maxBytes {
			s.rollbackChunks(m.Chunks)
			return ErrTooLarge
		}
		hash := sha256.Sum256(chunk.Data)
		hexHash := hex.EncodeToString(hash[:])
		if err := s.storeChunk(hexHash, chunk.Data, mode); err != nil {
			s.rollbackChunks(m.Chunks)
			return err
		}
		m.Chunks = append(m.Chunks, hexHash)
		m.Size += int64(len(chunk.Data))
	}
	return nil
}

// commitManifest はマニフェストを保存し、所有者の使用量を加算する。
// quota > 0 で使用量が上限を超える場合はコミットせず ErrQuotaExceeded を
// 返す(チェックと加算は同一トランザクションなので並行アップロードでも
// 突き抜けない)。失敗時はチャンク参照を戻す。
func (s *Store) commitManifest(m *FileManifest, quota int64) error {
	err := s.db.Update(func(tx *bolt.Tx) error {
		used, err := ownerUsage(tx, m.Owner)
		if err != nil {
			return err
		}
		if quota > 0 && used+m.Size > quota {
			return ErrQuotaExceeded
		}
		if err := addOwnerUsage(tx, m.Owner, m.Size); err != nil {
			return err
		}
		if err := addOwnerChunkRefs(tx, m.Owner, m.Chunks, 1); err != nil {
			return err
		}
		return putFileManifest(tx, m)
	})
	if err != nil {
		s.rollbackChunks(m.Chunks)
		return err
	}
	return nil
}

// OwnerUsage は所有者の論理使用量(バイト)を返す。
func (s *Store) OwnerUsage(owner string) (int64, error) {
	var used int64
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		used, err = ownerUsage(tx, owner)
		return err
	})
	return used, err
}

// storeChunk はチャンクを重複排除しつつ保存する。
//
//  1. 既存チャンク(完全一致)なら参照カウントを増やすだけ(重複排除)
//  2. 新規なら類似チャンクを索引から探し、見つかればそれをベースに
//     デルタ圧縮(zstd 辞書圧縮)を試す。十分縮めばデルタで保存
//  3. それ以外は zstd 圧縮(縮まなければ raw)で保存
func (s *Store) storeChunk(hash string, data []byte, mode string) error {
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
	compressed := s.compressChunk(data, mode)

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

		// サーバー経路の取り込みはこの場でデルタ判定済みなので、
		// オフラインデルタパスの対象から外す。
		newMeta := &ChunkMeta{RawSize: int64(len(data)), RefCount: 1, Features: features, DeltaTried: true}
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

		loc, err := s.writeRep(hash, "", stored)
		if err != nil {
			return err
		}
		if err := applyRepLocation(tx, newMeta, "", loc, int64(len(stored))); err != nil {
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

// tryDelta は類似チャンクをベースとしたデルタ圧縮を試す。特徴ごとの候補
// (最大 numFeatures 個)をすべて評価し、最小のデルタを採用する(best-of-N)。
// 通常圧縮より十分小さくなる場合のみ (baseHash, delta) を返す。
func (s *Store) tryDelta(features []uint64, data []byte, plainSize int) (string, []byte) {
	var candidates []string
	s.db.View(func(tx *bolt.Tx) error {
		seen := map[string]bool{}
		for _, h := range lookupSketches(tx, features) {
			// 深さ上限に達した候補は、チェーンの浅い祖先へ張り替える
			// (git の rebase 流。完全コピーの再アンカーより物理コストが
			// はるかに小さく、長期世代保持の削減率頭打ちを防ぐ)。
			for i := 0; h != "" && i <= s.maxDepth+1; i++ {
				meta, err := getChunkMeta(tx, h)
				if err != nil || meta == nil {
					h = ""
					break
				}
				if meta.Depth < s.maxDepth {
					break
				}
				h = meta.BaseHash
			}
			if h != "" && !seen[h] {
				seen[h] = true
				candidates = append(candidates, h)
			}
		}
		return nil
	})

	var bestHash string
	var bestDelta []byte
	for _, h := range candidates {
		base, err := s.readChunk(h)
		if err != nil {
			continue // ベースを読めない候補はスキップ
		}
		delta, err := s.deltaCompress(base, data)
		if err != nil {
			continue
		}
		if bestDelta == nil || len(delta) < len(bestDelta) {
			bestHash, bestDelta = h, delta
			// 十分小さいデルタが得られたら残り候補の評価は省略。
			if len(bestDelta) < plainSize/5 {
				break
			}
		}
	}
	if bestDelta == nil {
		return "", nil
	}
	// 効果が deltaAcceptRatio 未満なら不採用(→ plain 保存 = 新キーフレーム)。
	if float64(len(bestDelta)) >= float64(plainSize)*deltaAcceptRatio {
		return "", nil
	}
	return bestHash, bestDelta
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

// writeChunkFile はチャンク表現をアトミックかつ耐久的に書き込む
// (メタデータが指す前にファイルが確実にディスク上にあることを保証する)。
func (s *Store) writeChunkFile(hash, rep string, data []byte) error {
	return durableWrite(s.chunkPath(hash, rep), data)
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

	// precompression されたファイルはチャンク列(展開データ)からレシピで
	// 元のストリームをビット単位に再構成し、SHA-256 で検証してから返す。
	if m.Encoding != "" {
		var plain bytes.Buffer
		plain.Grow(int(m.ChunkedSize))
		for _, hash := range m.Chunks {
			data, err := s.readChunk(hash)
			if err != nil {
				return nil, nil, fmt.Errorf("チャンク %s の読み出しに失敗: %w", hash[:12], err)
			}
			plain.Write(data)
		}
		var orig []byte
		var err error
		switch m.Encoding {
		case EncodingGzipZlibV1:
			orig, err = precomp.Reconstruct(m.PrecompHeader, m.PrecompLevel, plain.Bytes())
		case EncodingZlibV1:
			orig, err = precomp.ReconstructZlib(m.PrecompHeader, m.PrecompLevel, plain.Bytes())
		case EncodingGzipMultiV1:
			orig, err = precomp.ReconstructGzipMulti(m.PrecompMembers, plain.Bytes())
		case EncodingPNGV1:
			orig, err = precomp.ReconstructPNG(m.PrecompPNG, m.PrecompLevel, plain.Bytes())
		default:
			err = fmt.Errorf("未知のエンコーディング %q", m.Encoding)
		}
		if err != nil {
			return nil, nil, fmt.Errorf("precompression の再構成に失敗: %w", err)
		}
		sum := sha256.Sum256(orig)
		if hex.EncodeToString(sum[:]) != m.OrigSHA256 {
			return nil, nil, fmt.Errorf("再構成の検証に失敗しました(保存時と異なる zlib 実装の可能性)")
		}
		return m, io.NopCloser(bytes.NewReader(orig)), nil
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
// 返り値のスライスはキャッシュと共有されるため変更してはならない。
// chain repack との競合(メタ読み後に旧表現ファイルが消える)は
// 一度だけのリトライで回復する。
func (s *Store) readChunk(hash string) ([]byte, error) {
	data, err := s.readChunkOnce(hash)
	if err != nil {
		data, err = s.readChunkOnce(hash)
	}
	return data, err
}

func (s *Store) readChunkOnce(hash string) ([]byte, error) {
	if data, ok := s.cache.get(hash); ok {
		return data, nil
	}
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

	// リージョン(ソリッド圧縮)内のチャンクは専用パスで取り出す。
	if meta.RegionID != "" {
		data, err := s.readChunkFromRegion(hash, meta)
		if err != nil {
			return nil, err
		}
		s.cache.put(hash, data)
		return data, nil
	}

	var stored []byte
	if meta.PackID != "" {
		stored, err = s.readFromPack(meta.PackID, meta.PackOff, meta.StoredSize)
	} else {
		stored, err = os.ReadFile(s.chunkPath(hash, meta.Rep))
	}
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
	s.cache.put(hash, data)
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

// Manifest は指定IDのマニフェストを返す(チャンク列は含む。読み出しは
// しないため所有者チェック等の軽い用途向け)。
func (s *Store) Manifest(id string) (*FileManifest, error) {
	var m *FileManifest
	err := s.db.View(func(tx *bolt.Tx) error {
		var err error
		m, err = getFileManifest(tx, id)
		return err
	})
	return m, err
}

// Delete はファイルを削除し、参照されなくなったチャンクを物理削除する。
// 所有者の使用量も減算する。
func (s *Store) Delete(id string) error {
	var m *FileManifest
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		m, err = getFileManifest(tx, id)
		if err != nil {
			return err
		}
		if err := addOwnerUsage(tx, m.Owner, -m.Size); err != nil {
			return err
		}
		if err := addOwnerChunkRefs(tx, m.Owner, m.Chunks, -1); err != nil {
			return err
		}
		return deleteFileManifest(tx, m)
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
// (=物理削除してよい)チャンクのファイルパス列を返す。
func (s *Store) releaseChunks(hashes []string) ([]string, error) {
	var orphans []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		var err error
		orphans, err = s.releaseChunksTx(tx, hashes)
		return err
	})
	return orphans, err
}

// releaseChunksTx は releaseChunks の本体(トランザクション内版)。
// デルタチャンクが消える場合はそのベースへの参照もカスケードして解放する。
func (s *Store) releaseChunksTx(tx *bolt.Tx, hashes []string) ([]string, error) {
	var orphans []string
	queue := append([]string(nil), hashes...)
	for len(queue) > 0 {
		hash := queue[0]
		queue = queue[1:]
		meta, err := getChunkMeta(tx, hash)
		if err != nil {
			return orphans, err
		}
		if meta == nil {
			continue
		}
		meta.RefCount--
		if meta.RefCount > 0 {
			if err := putChunkMeta(tx, hash, meta); err != nil {
				return orphans, err
			}
			continue
		}
		if err := tx.Bucket(bucketChunks).Delete([]byte(hash)); err != nil {
			return orphans, err
		}
		if err := dropSketches(tx, hash, meta.Features); err != nil {
			return orphans, err
		}
		if meta.BaseHash != "" {
			queue = append(queue, meta.BaseHash)
		}
		path, err := s.releaseRep(tx, hash, meta)
		if err != nil {
			return orphans, err
		}
		if path != "" {
			orphans = append(orphans, path)
		}
	}
	return orphans, nil
}

func (s *Store) removeChunkFiles(paths []string) {
	for _, path := range paths {
		os.Remove(path)
	}
}

// List は owner が所有するファイル一覧を作成日時の降順で返す
// (認証なし運用では owner は常に "" で全件が対象になる)。
func (s *Store) List(owner string) ([]*FileManifest, error) {
	var files []*FileManifest
	err := s.db.View(func(tx *bolt.Tx) error {
		// 所有者索引をプレフィックス走査し、その所有者のファイルだけを引く
		// (店全体を走査しないので、他ユーザーのファイル数に影響されない)。
		prefix := ownerFilePrefix(owner)
		fb := tx.Bucket(bucketFiles)
		c := tx.Bucket(bucketFileOwners).Cursor()
		for k, _ := c.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = c.Next() {
			id := k[len(prefix):]
			raw := fb.Get(id)
			if raw == nil {
				continue // 索引に対応するファイルが無い(fsck が掃除する)
			}
			var m FileManifest
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			m.Chunks = nil // 一覧にはチャンク列は不要
			files = append(files, &m)
		}
		return nil
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
	// PackCount はパックファイル数。小さな表現(デルタ等)はパックに
	// 集約され、ファイルシステムのブロック浪費を防ぐ。
	PackCount int `json:"pack_count"`
	// PackGarbageBytes はパック内の解放済み領域(次のコンパクションで回収)。
	PackGarbageBytes int64 `json:"pack_garbage_bytes"`
	// RegionCount はリージョン(ソリッド圧縮)ファイル数。
	RegionCount int `json:"region_count"`
	// chunkedLogical はチャンク化視点の論理サイズ合計(precompression 適用
	// ファイルは展開データのサイズ)。DedupRatio の分子に使う内部値。
	chunkedLogical int64
}

// Stats は現在の容量統計を集計して返す。
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{}
	err := s.db.View(func(tx *bolt.Tx) error {
		var chunkedLogical int64
		if err := tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
			var m FileManifest
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			st.FileCount++
			st.LogicalBytes += m.Size
			if m.ChunkedSize > 0 {
				chunkedLogical += m.ChunkedSize
			} else {
				chunkedLogical += m.Size
			}
			return nil
		}); err != nil {
			return err
		}
		st.chunkedLogical = chunkedLogical
		if err := tx.Bucket(bucketChunks).ForEach(func(_, v []byte) error {
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
		}); err != nil {
			return err
		}
		if err := tx.Bucket(bucketPacks).ForEach(func(_, v []byte) error {
			var pm packMeta
			if err := unmarshalPackMeta(v, &pm); err != nil {
				return err
			}
			st.PackCount++
			st.PackGarbageBytes += pm.TotalBytes - pm.LiveBytes
			return nil
		}); err != nil {
			return err
		}
		// リージョンチャンクは StoredSize=0 なので、圧縮後サイズはここで計上する。
		return tx.Bucket(bucketRegions).ForEach(func(_, v []byte) error {
			var rm regionMeta
			if err := json.Unmarshal(v, &rm); err != nil {
				return err
			}
			st.RegionCount++
			st.PhysicalBytes += rm.StoredSize
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	st.DedupRatio = ratio(st.chunkedLogical, st.UniqueBytes)
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
