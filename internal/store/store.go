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
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	// MinFreeBytes はディスク空きがこの値を下回ったら書き込みを拒否する
	// 予約(0 = ガードなし)。取り込み中(64チャンクごと)と Optimize の
	// 各フェーズ前に検査し、バックグラウンド処理自身がディスクを満杯に
	// することを防ぐ。API 層の事前拒否と同じ値を渡すこと。
	MinFreeBytes int64
}

// DefaultPrecompMaxPlain は precompression の展開上限デフォルト(64MiB)。
const DefaultPrecompMaxPlain = 64 << 20

// DefaultPrecompParallel は precompression の同時実行数デフォルト。
const DefaultPrecompParallel = 2

// ErrQuotaExceeded は所有者のクォータ超過を示す。
var ErrQuotaExceeded = errors.New("容量クォータを超過しています")

// ErrTooLarge はアップロードサイズ上限の超過を示す。
var ErrTooLarge = errors.New("アップロードサイズが上限を超えています")

// ErrDiskFull はディスク予約(MinFreeBytes)を下回ったことを示す。
var ErrDiskFull = errors.New("ディスクの空き容量が不足しています")

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
	precomp     bool          // zlib 系(gzip/zlib/png/zip/pdf、要 cgo)
	precompJPEG bool          // JPEG(純Go、cgo 不要)
	precompMax  int64         // precompression の展開上限
	precompSem  chan struct{} // precompression の同時実行制限
	cpuSem      chan struct{} // 取り込み圧縮の並列度(全アップロード共有)
	optMu       sync.Mutex    // Optimize の同時実行を直列化
	minFree     int64         // ディスク予約(0 = ガードなし)
	// writeWaiters は進行中の書き込みトランザクション要求数(batchUpdate の
	// 適応判定に使う: 並行書き込みがあるときだけグループコミットに切り替える)。
	writeWaiters atomic.Int64
	// txSolo / txBatched はグループコミット合流率のメトリクス用。
	txSolo    atomic.Int64
	txBatched atomic.Int64
}

// checkDiskSpace はディスク予約を検査する(取得失敗時は書き込みを止めない)。
func (s *Store) checkDiskSpace() error {
	if s.minFree <= 0 {
		return nil
	}
	free, err := s.FreeBytes()
	if err != nil || free >= s.minFree {
		return nil
	}
	return ErrDiskFull
}

// batchDelay はグループコミットの合流待ち時間の上限。並行書き込みが
// この窓に到着すると1回の fsync に合流する。単独の書き込みは batchUpdate の
// 適応判定で通常の Update(遅延なし)を使うため、この遅延を払わない。
const batchDelay = 2 * time.Millisecond

// batchUpdate は書き込みトランザクションを実行する。並行する書き込みが
// ある場合は bbolt の Batch でグループコミット(複数のコミットが1回の
// fsync に合流し、多ユーザー同時アップロードのスループットが桁で上がる)、
// 単独の場合は通常の Update(追加遅延なし)。
//
// 契約: fn はリトライされうる(Batch は同居した別の fn が失敗すると各 fn を
// 単独で再実行する)。fn 内で外の変数に書く場合は fn の先頭で毎回
// リセットし、途中経過を持ち越さないこと。
func (s *Store) batchUpdate(fn func(*bolt.Tx) error) error {
	n := s.writeWaiters.Add(1)
	defer s.writeWaiters.Add(-1)
	if n > 1 {
		s.txBatched.Add(1)
		return s.db.Batch(fn)
	}
	s.txSolo.Add(1)
	return s.db.Update(fn)
}

// RuntimeStats は可観測性用の内部カウンタ(キャッシュヒット率・
// グループコミット合流率)。
type RuntimeStats struct {
	CacheHits   int64 `json:"cache_hits"`
	CacheMisses int64 `json:"cache_misses"`
	TxSolo      int64 `json:"tx_solo"`
	TxBatched   int64 `json:"tx_batched"`
}

// Runtime は現在の内部カウンタを返す。
func (s *Store) Runtime() RuntimeStats {
	return RuntimeStats{
		CacheHits:   s.cache.hits.Load(),
		CacheMisses: s.cache.misses.Load(),
		TxSolo:      s.txSolo.Load(),
		TxBatched:   s.txBatched.Load(),
	}
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
	// グループコミットの合流窓(batchUpdate 参照)。デフォルトの 10ms は
	// 並行書き込みのレイテンシに直結するので短く抑える。
	db.MaxBatchDelay = batchDelay
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
		if err := migrateFileOwners(tx); err != nil {
			return err
		}
		// 維持カウンタを(未初期化なら)一度だけ全走査から初期化する。
		return migrateCounters(tx)
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
		precompJPEG: !cfg.DisablePrecomp, // JPEG は純Goなので常に可能
		precompMax:  precompMax,
		precompSem:  make(chan struct{}, precompPar),
		cpuSem:      make(chan struct{}, max(1, runtime.GOMAXPROCS(0))),
		minFree:     cfg.MinFreeBytes,
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

// bestCompress は取り込み経路の最強エンコーダで圧縮する。
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

// maxCompress はオフライン経路(リージョン圧縮・背景再圧縮)用の最強圧縮:
// level 22(ultra)+ 入力サイズに合わせた大窓 + long-distance matching。
// 取り込み経路の bestCompress(19)よりさらに数%小さいが数倍遅いため、
// ユーザーを待たせない背景処理だけで使う(実測は RESEARCH.md §4.15)。
func (s *Store) maxCompress(data []byte) []byte {
	if zstdc.Available() {
		if out, err := zstdc.CompressMax(data); err == nil {
			return out
		}
	}
	return s.bestCompress(data)
}

// Close はストアを閉じる。
func (s *Store) Close() error {
	perr := s.pw.close()
	s.encFast.Close()
	s.encBalanced.Close()
	s.encBest.Close()
	s.dec.Close()
	if err := s.db.Close(); err != nil {
		return err
	}
	return perr
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
	// JPEG は純Go(cgo不要)なので precomp.Supported()=false でも扱える。
	// zlib 系(gzip/zlib/png/zip/pdf)は cgo が要る。
	if !opts.DisablePrecomp && (s.precomp || s.precompJPEG) {
		head := make([]byte, 12) // AVI(RIFF)判定に12バイト必要
		n, _ := io.ReadFull(r, head)
		head = head[:n]
		rest := io.MultiReader(bytes.NewReader(head), r)
		zlibFmt := s.precomp && (precomp.IsGzip(head) || precomp.IsZlib(head) ||
			precomp.IsPNG(head) || precomp.IsZip(head) || precomp.IsPDF(head))
		jpegFmt := s.precompJPEG && (precomp.IsJPEG(head) || precomp.IsGIF(head) ||
			precomp.IsAVI(head) || precomp.IsWAV(head) || precomp.IsAIFF(head) || precomp.IsBMP(head))
		if (zlibFmt || jpegFmt) && s.acquirePrecomp() {
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
				pending, err := s.putStream(m, bytes.NewReader(m.precompPlain), mode, 0)
				s.releasePrecomp()
				if err != nil {
					return nil, err
				}
				// GET が返すのは元のストリームなので Size は元サイズに合わせ、
				// チャンク化視点のサイズ(展開データ)は別に記録する。
				m.ChunkedSize = m.Size
				m.Size = int64(len(buf))
				m.precompPlain = nil
				return m, s.commitManifest(m, opts.Quota, pending)
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

	pending, err := s.putStream(m, r, mode, opts.MaxBytes)
	if err != nil {
		return nil, err
	}
	return m, s.commitManifest(m, opts.Quota, pending)
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
	case precomp.IsZip(buf):
		chunked, recipe, ok := precomp.TryUnwrapZip(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingZipV1
		m.PrecompContainer = recipe
		m.precompPlain = chunked
	case precomp.IsPDF(buf):
		chunked, recipe, ok := precomp.TryUnwrapPDF(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingPDFV1
		m.PrecompContainer = recipe
		m.precompPlain = chunked
	case precomp.IsZlib(buf):
		u, ok := precomp.TryUnwrapZlib(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingZlibV1
		m.PrecompHeader = u.Header
		m.PrecompLevel = u.Level
		m.precompPlain = u.Plain
	case precomp.IsJPEG(buf):
		u, ok := precomp.TryUnwrapJPEG(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingJPEGV1
		m.PrecompJPEG = u.Recipe
		m.precompPlain = u.Chunked
	case precomp.IsGIF(buf):
		u, ok := precomp.TryUnwrapGIF(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingGIFV1
		m.PrecompGIF = u.Recipe
		m.precompPlain = u.Chunked
	case precomp.IsAVI(buf):
		u, ok := precomp.TryUnwrapAVI(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingAVIV1
		m.PrecompAVI = u.Recipe
		m.precompPlain = u.Chunked
	case precomp.IsWAV(buf):
		u, ok := precomp.TryUnwrapWAV(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingWAVV1
		m.PrecompWAV = u.Recipe
		m.precompPlain = u.Chunked
	case precomp.IsAIFF(buf):
		u, ok := precomp.TryUnwrapAIFF(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingAIFFV1
		m.PrecompWAV = u.Recipe
		m.precompPlain = u.Chunked
	case precomp.IsBMP(buf):
		u, ok := precomp.TryUnwrapBMP(buf, s.precompMax)
		if !ok {
			return false
		}
		m.Encoding = EncodingBMPV1
		m.PrecompBMP = u.Recipe
		m.precompPlain = u.Chunked
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
//
// 小さなファイル(バッファ上限 = 最大チャンクサイズまで)はチャンクを
// 確定せず、準備(圧縮)だけして pending として返す。呼び出し側が
// commitManifest でマニフェストと同一トランザクションにまとめて確定する
// (小ファイル1個 = 書き込みTx 1回。多数の小ファイル投下で fsync 数が半減
// する上、クォータ超過時もチャンクが一切コミットされない完全な原子性になる)。
// 大きなファイルは従来どおりストリーミングで逐次確定する(メモリ一定)。
// pending が nil のときはチャンクは確定済み(ストリーミング経路)。
// chunkFuture は非同期で準備中(圧縮・類似検索)のチャンク。
type chunkFuture struct {
	pc   *preparedChunk
	err  error
	done chan struct{}
}

// prepareAsync はチャンクの準備(CPU の重い部分)をバックグラウンドで行う。
// 並列度はストア全体で cpuSem(コア数)に制限され、適用順は呼び出し側が
// future の待ち合わせ順で保存する。
func (s *Store) prepareAsync(hash string, data []byte, mode string) *chunkFuture {
	f := &chunkFuture{done: make(chan struct{})}
	go func() {
		defer close(f.done)
		s.cpuSem <- struct{}{}
		defer func() { <-s.cpuSem }()
		f.pc, f.err = s.prepareChunk(hash, data, mode)
	}()
	return f
}

func (f *chunkFuture) wait() (*preparedChunk, error) {
	<-f.done
	return f.pc, f.err
}

func (s *Store) putStream(m *FileManifest, r io.Reader, mode string, maxBytes int64) ([]*preparedChunk, error) {
	ck, err := chunker.New(r, s.chunkSize)
	if err != nil {
		return nil, err
	}
	bufLimit := int64(s.chunkSize) * 4 // 最大チャンクサイズ = 必ず1チャンクは収まる
	// 圧縮・類似検索はチャンクごとに独立なので、ウィンドウ付きの並列
	// パイプラインで行う(確定はストリーム順のまま)。ウィンドウは
	// コア数までに制限し、保持メモリ(チャンク生データ+圧縮出力)を
	// アップロードあたり最大 ~window×2×チャンクサイズに抑える。
	window := max(2, min(8, runtime.GOMAXPROCS(0)))
	pending := []*chunkFuture{} // 小ファイル: グループコミット候補
	var buffered int64
	inflight := []*chunkFuture{} // 大ファイル: 準備中チャンクの順序付き列
	var committed []string       // 逐次確定済みのチャンク(エラー時のロールバック対象)
	fail := func(err error) ([]*preparedChunk, error) {
		// 進行中の準備はバックグラウンドで完了して破棄される(リークなし)
		s.rollbackChunks(committed)
		return nil, err
	}
	// applyHead は inflight の先頭を待って確定する。
	applyHead := func() error {
		head := inflight[0]
		inflight = inflight[1:]
		pc, err := head.wait()
		if err != nil {
			return err
		}
		if err := s.applyPrepared(pc); err != nil {
			return err
		}
		committed = append(committed, pc.hash)
		return nil
	}
	// flush は pending を諦めて逐次確定パイプラインに切り替える
	// (大きなファイルと判明)。
	flush := func() error {
		inflight = append(inflight, pending...)
		pending = nil
		return nil
	}
	for i := 0; ; i++ {
		chunk, err := ck.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("チャンク分割に失敗: %w", err))
		}
		if maxBytes > 0 && m.Size+int64(len(chunk.Data)) > maxBytes {
			return fail(ErrTooLarge)
		}
		// 巨大アップロードが取り込み途中でディスクを埋め切らないよう、
		// 定期的(64チャンク≒64MiBごと)に残量を検査する。API 層の
		// 開始時チェックだけでは長いストリームの途中枯渇を防げない。
		if i%64 == 0 {
			if err := s.checkDiskSpace(); err != nil {
				return fail(err)
			}
		}
		hash := sha256.Sum256(chunk.Data)
		hexHash := hex.EncodeToString(hash[:])
		// チャンカーのバッファは再利用されるためコピーして保持する。
		data := append([]byte(nil), chunk.Data...)
		f := s.prepareAsync(hexHash, data, mode)
		switch {
		case pending != nil && buffered+int64(len(data)) <= bufLimit:
			pending = append(pending, f)
			buffered += int64(len(data))
		default:
			if pending != nil {
				if err := flush(); err != nil {
					return fail(err)
				}
			}
			inflight = append(inflight, f)
			for len(inflight) >= window {
				if err := applyHead(); err != nil {
					return fail(err)
				}
			}
		}
		m.Chunks = append(m.Chunks, hexHash)
		m.Size += int64(len(chunk.Data))
	}
	// 大ファイル経路: 残りの準備済みチャンクを順に確定
	for len(inflight) > 0 {
		if err := applyHead(); err != nil {
			return fail(err)
		}
	}
	// 小ファイル経路: 準備完了を待って呼び出し側のグループコミットへ渡す
	out := make([]*preparedChunk, 0, len(pending))
	for _, f := range pending {
		pc, err := f.wait()
		if err != nil {
			return fail(err)
		}
		out = append(out, pc)
	}
	if pending == nil {
		return nil, nil
	}
	return out, nil
}

// applyPrepared は準備済みチャンク1つを単独トランザクションで確定する
// (storeChunk の後半と同じ。vanished レースはロック外で圧縮し直す)。
func (s *Store) applyPrepared(pc *preparedChunk) error {
	for attempt := 0; ; attempt++ {
		var cleanup []string
		err := s.batchUpdate(func(tx *bolt.Tx) error {
			cleanup = cleanup[:0]
			return s.applyChunk(tx, pc, &cleanup)
		})
		if err == nil {
			return nil
		}
		s.removeChunkFiles(cleanup)
		if errors.Is(err, errChunkVanished) && attempt < 2 {
			s.fillPrepared(pc)
			continue
		}
		return err
	}
}

// commitManifest はマニフェストを保存し、所有者の使用量を加算する。
// quota > 0 で使用量が上限を超える場合はコミットせず ErrQuotaExceeded を
// 返す(チェックと加算は同一トランザクションなので並行アップロードでも
// 突き抜けない)。
//
// pending(小ファイルの準備済みチャンク)がある場合はチャンク確定も同じ
// トランザクションで行う: 成功すれば全部、失敗すれば何も残らない。
// pending が nil(ストリーミング経路)で失敗したときは確定済みチャンクの
// 参照を戻す。
func (s *Store) commitManifest(m *FileManifest, quota int64, pending []*preparedChunk) error {
	for attempt := 0; ; attempt++ {
		var cleanup []string
		err := s.batchUpdate(func(tx *bolt.Tx) error {
			cleanup = cleanup[:0] // Batch 再実行に備えリセット
			// クォータは先に判定する(超過時にチャンクの書き込みすら始めない)。
			used, err := ownerUsage(tx, m.Owner)
			if err != nil {
				return err
			}
			if quota > 0 && used+m.Size > quota {
				return ErrQuotaExceeded
			}
			for _, pc := range pending {
				if err := s.applyChunk(tx, pc, &cleanup); err != nil {
					return err
				}
			}
			if err := addOwnerUsage(tx, m.Owner, m.Size); err != nil {
				return err
			}
			if err := addOwnerChunkRefs(tx, m.Owner, m.Chunks, 1); err != nil {
				return err
			}
			return putFileManifest(tx, m)
		})
		if err == nil {
			return nil
		}
		s.removeChunkFiles(cleanup)
		if errors.Is(err, errChunkVanished) && attempt < 2 {
			// 既存見込みのチャンクが確定前に消えた(稀)。ロック外で圧縮し直す。
			for _, pc := range pending {
				if pc.compressed == nil {
					s.fillPrepared(pc)
				}
			}
			continue
		}
		if pending == nil {
			s.rollbackChunks(m.Chunks) // ストリーミング経路: 確定済み分を戻す
		}
		return err
	}
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

// preparedChunk は取り込み待ちチャンク。重い処理(圧縮・類似検索)は
// prepare 段階でトランザクション外に済ませ、確定(applyChunk)は
// メタ操作とディスク書き込みだけにする。小さなファイルでは複数チャンクと
// マニフェストを1つのトランザクションでまとめて確定できる。
type preparedChunk struct {
	hash string
	mode string
	data []byte
	// compressed が nil の場合は「既存チャンクの見込み」(prepare 時に存在を
	// 確認済みで、圧縮を省略した)。applyChunk 時に消えていたら
	// errChunkVanished を返し、呼び出し側がロック外で再 prepare する。
	compressed []byte
	features   []uint64
	baseHash   string
	deltaData  []byte
}

// errChunkVanished は「既存の見込みだったチャンクが確定時に消えていた」
// ことを示す(稀なレース。ロック外で圧縮し直して再試行する)。
var errChunkVanished = errors.New("chunk vanished between prepare and apply")

// prepareChunk はチャンク取り込みの重い前半(存在確認・圧縮・類似検索)を
// 行う。既存チャンクなら圧縮を省略する(重複排除の高速パス)。
func (s *Store) prepareChunk(hash string, data []byte, mode string) (*preparedChunk, error) {
	exists := false
	err := s.db.View(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		exists = meta != nil
		return err
	})
	if err != nil {
		return nil, err
	}
	pc := &preparedChunk{hash: hash, mode: mode, data: data}
	if !exists {
		s.fillPrepared(pc)
	}
	return pc, nil
}

// fillPrepared は圧縮・類似検索(CPU/IO の重い部分)を行う。
// bbolt の書き込みロック外で呼ぶこと。
func (s *Store) fillPrepared(pc *preparedChunk) {
	pc.compressed = s.compressChunk(pc.data, pc.mode)
	if s.delta {
		pc.features = computeFeatures(pc.data)
		pc.baseHash, pc.deltaData = s.tryDelta(pc.features, pc.data, len(pc.compressed))
	}
}

// applyChunk は準備済みチャンクをトランザクション内で確定する。
//
//  1. 既存チャンク(完全一致)なら参照カウントを増やすだけ(重複排除)
//  2. デルタ候補があればベースの生存・深さを再確認してデルタで保存
//  3. それ以外は zstd 圧縮(縮まなければ raw)で保存
//
// 新しく書いたファイル表現のパスは cleanup に積む(トランザクションが
// 失敗したときに呼び出し側が消す。パック追記の取り残しは無害なゴミで、
// コンパクションが回収する)。
func (s *Store) applyChunk(tx *bolt.Tx, pc *preparedChunk, cleanup *[]string) error {
	// 並行アップロードが同じチャンクを先に登録した可能性を再確認。
	meta, err := getChunkMeta(tx, pc.hash)
	if err != nil {
		return err
	}
	if meta != nil {
		meta.RefCount++
		return putChunkMeta(tx, pc.hash, meta)
	}
	if pc.compressed == nil {
		// 既存の見込みが外れた(prepare と apply の間に削除された)。
		// 圧縮は書き込みロック内でやりたくないので、外でやり直させる。
		return errChunkVanished
	}

	// デルタ採用時はベースがまだ存在し深さに余裕があるか確認し、参照を増やす。
	baseHash := pc.baseHash
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
	newMeta := &ChunkMeta{RawSize: int64(len(pc.data)), RefCount: 1, Features: pc.features, DeltaTried: true}
	var stored []byte
	switch {
	case baseHash != "":
		newMeta.Compression = compressionDelta
		newMeta.BaseHash = baseHash
		newMeta.Depth = depth
		stored = pc.deltaData
	case len(pc.compressed) < len(pc.data):
		newMeta.Compression = compressionZstd
		stored = pc.compressed
	default:
		newMeta.Compression = compressionRaw
		stored = pc.data
	}
	newMeta.StoredSize = int64(len(stored))

	loc, err := s.writeRep(pc.hash, "", stored)
	if err != nil {
		return err
	}
	if loc.packID == "" {
		*cleanup = append(*cleanup, s.chunkPath(pc.hash, ""))
	}
	if err := applyRepLocation(tx, newMeta, "", loc, int64(len(stored))); err != nil {
		return err
	}
	// 深さ上限に達したチャンクはベースにできないので索引を汚さない。
	if newMeta.Depth < s.maxDepth {
		if err := registerSketches(tx, pc.hash, pc.features); err != nil {
			return err
		}
	}
	// 新チャンクをインクリメンタル最適化(リージョン化等)の対象に積む。
	if err := tx.Bucket(bucketDirtyChunks).Put([]byte(pc.hash), nil); err != nil {
		return err
	}
	return putChunkMeta(tx, pc.hash, newMeta)
}

// storeChunk はチャンク1つを重複排除しつつ即時確定する(大きなファイルの
// ストリーミング経路)。小さなファイルはマニフェストと同一トランザクションで
// まとめて確定される(commitManifest 参照)。
func (s *Store) storeChunk(hash string, data []byte, mode string) error {
	pc, err := s.prepareChunk(hash, data, mode)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		var cleanup []string
		err := s.batchUpdate(func(tx *bolt.Tx) error {
			cleanup = cleanup[:0] // Batch 再実行に備えリセット
			return s.applyChunk(tx, pc, &cleanup)
		})
		if err == nil {
			return nil
		}
		s.removeChunkFiles(cleanup)
		if errors.Is(err, errChunkVanished) && attempt < 2 {
			s.fillPrepared(pc) // ロック外で圧縮し直して再試行
			continue
		}
		return err
	}
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
	// 再構成はメモリ上で行うため、同時実行を precompSem で制限する
	// (多数の並行ダウンロードでのメモリ爆発防止。枠が空くまで待つ)。
	// 枠は返却ストリームのクローズまで保持し、組み立て済みバッファの
	// 同時滞留数も制限する。
	if m.Encoding != "" {
		s.precompSem <- struct{}{}
		release := func() { <-s.precompSem }
		orig, err := s.reconstructPrecomp(m)
		if err != nil {
			release()
			return nil, nil, err
		}
		return m, &releaseReadCloser{Reader: bytes.NewReader(orig), release: release}, nil
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

// releaseReadCloser はクローズ時にコールバックを一度だけ呼ぶ ReadCloser。
type releaseReadCloser struct {
	io.Reader
	release func()
	once    sync.Once
}

func (r *releaseReadCloser) Close() error {
	r.once.Do(r.release)
	return nil
}

// reconstructPrecomp は precompression されたファイルの元ストリームを
// レシピから再構成し、SHA-256 で検証して返す。
func (s *Store) reconstructPrecomp(m *FileManifest) ([]byte, error) {
	var plain bytes.Buffer
	plain.Grow(int(m.ChunkedSize))
	for _, hash := range m.Chunks {
		data, err := s.readChunk(hash)
		if err != nil {
			return nil, fmt.Errorf("チャンク %s の読み出しに失敗: %w", hash[:12], err)
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
	case EncodingZipV1, EncodingPDFV1:
		orig, err = precomp.ReconstructContainer(m.PrecompContainer, plain.Bytes())
	case EncodingJPEGV1:
		orig, err = precomp.ReconstructJPEG(m.PrecompJPEG, plain.Bytes())
	case EncodingGIFV1:
		orig, err = precomp.ReconstructGIF(m.PrecompGIF, plain.Bytes())
	case EncodingAVIV1:
		orig, err = precomp.ReconstructAVI(m.PrecompAVI, plain.Bytes())
	case EncodingWAVV1, EncodingAIFFV1:
		orig, err = precomp.ReconstructWAV(m.PrecompWAV, plain.Bytes())
	case EncodingBMPV1:
		orig, err = precomp.ReconstructBMP(m.PrecompBMP, plain.Bytes())
	default:
		err = fmt.Errorf("未知のエンコーディング %q", m.Encoding)
	}
	if err != nil {
		return nil, fmt.Errorf("precompression の再構成に失敗: %w", err)
	}
	sum := sha256.Sum256(orig)
	if hex.EncodeToString(sum[:]) != m.OrigSHA256 {
		return nil, fmt.Errorf("再構成の検証に失敗しました(保存時と異なる zlib 実装の可能性)")
	}
	return orig, nil
}

// readChunk はチャンクを読み出して伸長し、ハッシュを検証して返す。
// 返り値のスライスはキャッシュと共有されるため変更してはならない。
// chain repack との競合(メタ読み後に旧表現ファイルが消える)は
// 一度だけのリトライで回復する。
func (s *Store) readChunk(hash string) ([]byte, error) {
	data, err := s.readChunkOnce(hash, true)
	if err != nil {
		data, err = s.readChunkOnce(hash, true)
	}
	return data, err
}

// readChunkVerify はスクラブ用の読み出し: キャッシュを見ず・入れず、
// 必ずディスク上のバイトを伸長・検証する(キャッシュ経由ではディスクの
// bit rot を検出できず、さらに全チャンク走査がキャッシュを洗い流すため)。
func (s *Store) readChunkVerify(hash string) ([]byte, error) {
	data, err := s.readChunkOnce(hash, false)
	if err != nil {
		data, err = s.readChunkOnce(hash, false)
	}
	return data, err
}

func (s *Store) readChunkOnce(hash string, useCache bool) ([]byte, error) {
	if useCache {
		if data, ok := s.cache.get(hash); ok {
			return data, nil
		}
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
		data, err := s.readChunkFromRegion(hash, meta, useCache)
		if err != nil {
			return nil, err
		}
		if useCache {
			s.cache.put(hash, data)
		}
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
	case compressionBr:
		data, err = brotliDecode(stored, meta.RawSize)
		if err != nil {
			return nil, fmt.Errorf("brotli 伸長に失敗: %w", err)
		}
	case compressionZstdBCJ:
		data, err = s.dec.DecodeAll(stored, make([]byte, 0, meta.RawSize))
		if err != nil {
			return nil, fmt.Errorf("伸長に失敗: %w", err)
		}
		data = bcjX86Decode(data)
	case compressionBrBCJ:
		data, err = brotliDecode(stored, meta.RawSize)
		if err != nil {
			return nil, fmt.Errorf("brotli 伸長に失敗: %w", err)
		}
		data = bcjX86Decode(data)
	case compressionDelta:
		// ベースチェーンをたどる。深さは maxDepth で制限されている。
		// ベースはキャッシュ利用可(このチャンク自体の保存バイトは
		// ディスクから読んでおり、ベースの完全性はベース自身のスクラブで
		// 独立に検証される)。
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
	if useCache {
		s.cache.put(hash, data)
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
// 所有者の使用量も減算する。マニフェスト削除と参照解放は単一トランザク
// ションで行う(以前は2トランザクションで、間にクラッシュすると参照
// カウントのリークが fsck まで残った)。
func (s *Store) Delete(id string) error {
	var orphans []string
	err := s.batchUpdate(func(tx *bolt.Tx) error {
		m, err := getFileManifest(tx, id)
		if err != nil {
			return err
		}
		if err := addOwnerUsage(tx, m.Owner, -m.Size); err != nil {
			return err
		}
		if err := addOwnerChunkRefs(tx, m.Owner, m.Chunks, -1); err != nil {
			return err
		}
		if err := deleteFileManifest(tx, m); err != nil {
			return err
		}
		orphans, err = s.releaseChunksTx(tx, m.Chunks) // Batch 再実行時は再代入
		return err
	})
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
	err := s.batchUpdate(func(tx *bolt.Tx) error {
		var err error
		orphans, err = s.releaseChunksTx(tx, hashes) // 再実行時は再代入される
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
		if err := deleteChunkMeta(tx, hash); err != nil {
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
	files, _, err := s.ListPage(owner, "", 0)
	return files, err
}

// ListPage は owner のファイル一覧を作成日時の降順でページ単位に返す。
// after は前ページの next カーソル("" で先頭から)、limit は最大件数
// (0 で無制限)。まだ続きがある場合は次ページ用の不透明カーソルを返す。
//
// 所有者索引(作成日時降順キー+表示レコード内蔵)のプレフィックス走査
// なので、O(このページの件数)で済む: 店全体はもちろん、その所有者の
// 全ファイルすら走査せず、巨大マニフェスト(チャンク列)も読まない。
func (s *Store) ListPage(owner, after string, limit int) ([]*FileManifest, string, error) {
	var files []*FileManifest
	var next string
	err := s.db.View(func(tx *bolt.Tx) error {
		prefix := ownerFilePrefix(owner)
		c := tx.Bucket(bucketFileOwners).Cursor()
		k, v := c.Seek(prefix)
		if after != "" {
			// カーソルはキーの接頭辞以降(反転TS+ID)の hex 表現。
			suffix, err := hex.DecodeString(after)
			if err != nil {
				return fmt.Errorf("カーソルが不正です")
			}
			k, v = c.Seek(append(append([]byte(nil), prefix...), suffix...))
			if k != nil && bytes.HasPrefix(k, prefix) && bytes.Equal(k[len(prefix):], suffix) {
				k, v = c.Next() // カーソル自身は前ページで返済み
			}
		}
		var lastSuffix []byte
		for ; k != nil && bytes.HasPrefix(k, prefix); k, v = c.Next() {
			if limit > 0 && len(files) >= limit {
				// まだ続きがある → 最後に返した要素をカーソルにする
				next = hex.EncodeToString(lastSuffix)
				return nil
			}
			var rec fileIndexRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return err
			}
			files = append(files, &FileManifest{
				ID:        rec.ID,
				Name:      rec.Name,
				Size:      rec.Size,
				CreatedAt: rec.CreatedAt,
				Owner:     owner,
			})
			lastSuffix = append(lastSuffix[:0], k[len(prefix):]...)
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return files, next, nil
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

// Stats は現在の容量統計を返す。主要値は各メタ書き込みが同一トランザク
// ションで維持するカウンタから O(1) で読む(以前はチャンク全走査 O(N) で、
// 大規模ストアでは1回が秒単位だった)。パック使用量だけはパックバケットを
// 走査する(パック数はチャンク数の数万分の1で安価)。
// カウンタの照合・修復は fsck が全走査で行う。
func (s *Store) Stats() (*Stats, error) {
	st := &Stats{}
	err := s.db.View(func(tx *bolt.Tx) error {
		c, ok := loadCounters(tx)
		if !ok {
			// 未初期化(通常は Open の移行で入るため起きない)
			var err error
			c, err = scanCounters(tx)
			if err != nil {
				return err
			}
		}
		st.FileCount = int(c.FileCount)
		st.ChunkCount = int(c.ChunkCount)
		st.DeltaChunkCount = int(c.DeltaChunks)
		st.LogicalBytes = c.LogicalBytes
		st.chunkedLogical = c.ChunkedBytes
		st.UniqueBytes = c.UniqueBytes
		st.PhysicalBytes = c.ChunkPhysical + c.RegionBytes
		st.RegionCount = int(c.RegionCount)
		return tx.Bucket(bucketPacks).ForEach(func(_, v []byte) error {
			var pm packMeta
			if err := unmarshalPackMeta(v, &pm); err != nil {
				return err
			}
			st.PackCount++
			st.PackGarbageBytes += pm.TotalBytes - pm.LiveBytes
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
