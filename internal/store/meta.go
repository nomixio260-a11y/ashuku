package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/nomixio260-a11y/ashuku/internal/precomp"
)

var (
	bucketFiles  = []byte("files")
	bucketChunks = []byte("chunks")
	// bucketSketches は類似チャンク検索用の索引: 特徴値(8バイト) → チャンクハッシュ。
	// デルタ圧縮のベース候補(非デルタチャンク)だけが登録される。
	bucketSketches = []byte("sketches")
	// bucketSettings はストア作成時に確定する設定(チャンクサイズ等)。
	bucketSettings = []byte("settings")
	// bucketPacks はパックファイルごとの使用量(total/live バイト)。
	bucketPacks = []byte("packs")
	// bucketUsers は所有者ごとの論理使用量(バイト、int64 BigEndian)。
	bucketUsers = []byte("users")
	// bucketOwnerChunks は「所有者がマニフェスト経由で参照しているチャンク」の
	// 参照数索引。チャンク直接ダウンロードの読み出し権チェックに使う
	// (ハッシュを知っているだけでは他人のデータを取得できないようにする)。
	bucketOwnerChunks = []byte("ownerchunks")
	// bucketRegions はリージョンごとの使用量(圧縮後サイズ・メンバー数・生存数)。
	bucketRegions = []byte("regions")
	// bucketDirtyChunks / bucketDirtyFiles はインクリメンタル最適化の
	// 作業キュー: 前回の最適化以降に追加されたチャンク/ファイルだけを積む。
	// これにより毎時の Optimize が全メタ走査 O(ストア全体)ではなく
	// O(新着データ)で済む(全走査の完全パスは低頻度で別途実行する)。
	bucketDirtyChunks = []byte("dirtychunks")
	bucketDirtyFiles  = []byte("dirtyfiles")
	// bucketFileOwners は「所有者 → ファイル」の二次索引。
	// キーは ownerKey(owner) + 0x1F + 反転UnixNano(8B) + fileID で、
	// プレフィックス走査が自然に作成日時の降順(新しい順)になる。
	// 値は一覧表示用の小さなレコード(ID/名前/サイズ/作成日時)なので、
	// 一覧のために巨大なマニフェスト(チャンク列込み。1万チャンクで
	// ~660KB の JSON)を読む必要がない。
	// これにより List(owner) は O(その所有者のファイル数)・ソート不要・
	// カーソルページング可能になり、多数のユーザーが多数の小ファイルを
	// 持つ運用でも一覧取得が店全体の規模に影響されない。
	bucketFileOwners = []byte("fileowners")
)

var keyAvgChunkSize = []byte("avg_chunk_size")

// keyCounters は維持カウンタ(storeCounters)の settings キー。
// Stats() をチャンク全走査 O(N) から O(1) にするため、ファイル/チャンク/
// リージョンの各メタ書き込みヘルパが同一トランザクション内で差分更新する
// (クラッシュ整合)。全走査による照合・修復は fsck が担う。
var keyCounters = []byte("counters_v1")

// storeCounters は Stats の主要値の維持カウンタ。
type storeCounters struct {
	FileCount     int64
	LogicalBytes  int64
	ChunkedBytes  int64 // チャンク化視点の論理サイズ(precomp は展開データ)
	ChunkCount    int64
	DeltaChunks   int64
	UniqueBytes   int64
	ChunkPhysical int64 // チャンク表現の StoredSize 合計
	RegionCount   int64
	RegionBytes   int64 // リージョン表現の StoredSize 合計
}

func loadCounters(tx *bolt.Tx) (*storeCounters, bool) {
	raw := tx.Bucket(bucketSettings).Get(keyCounters)
	if len(raw) != 9*8 {
		return nil, false
	}
	var c storeCounters
	for i, p := range []*int64{&c.FileCount, &c.LogicalBytes, &c.ChunkedBytes,
		&c.ChunkCount, &c.DeltaChunks, &c.UniqueBytes, &c.ChunkPhysical,
		&c.RegionCount, &c.RegionBytes} {
		*p = int64(binary.BigEndian.Uint64(raw[i*8:]))
	}
	return &c, true
}

func saveCounters(tx *bolt.Tx, c *storeCounters) error {
	var raw [9 * 8]byte
	for i, v := range []int64{c.FileCount, c.LogicalBytes, c.ChunkedBytes,
		c.ChunkCount, c.DeltaChunks, c.UniqueBytes, c.ChunkPhysical,
		c.RegionCount, c.RegionBytes} {
		binary.BigEndian.PutUint64(raw[i*8:], uint64(v))
	}
	return tx.Bucket(bucketSettings).Put(keyCounters, raw[:])
}

// adjustCounters はカウンタを差分更新する。未初期化(移行前)なら何もしない
// (Open 時の移行が全走査で初期化する)。
func adjustCounters(tx *bolt.Tx, fn func(*storeCounters)) error {
	c, ok := loadCounters(tx)
	if !ok {
		return nil
	}
	fn(c)
	return saveCounters(tx, c)
}

// scanCounters は全走査でカウンタを計算する(移行・fsck の照合用)。
func scanCounters(tx *bolt.Tx) (*storeCounters, error) {
	c := &storeCounters{}
	err := tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
		var m FileManifest
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		c.FileCount++
		c.LogicalBytes += m.Size
		if m.ChunkedSize > 0 {
			c.ChunkedBytes += m.ChunkedSize
		} else {
			c.ChunkedBytes += m.Size
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = tx.Bucket(bucketChunks).ForEach(func(_, v []byte) error {
		var cm ChunkMeta
		if err := unmarshalChunkMeta(v, &cm); err != nil {
			return err
		}
		c.ChunkCount++
		c.UniqueBytes += cm.RawSize
		c.ChunkPhysical += cm.StoredSize
		if cm.Compression == compressionDelta {
			c.DeltaChunks++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	err = tx.Bucket(bucketRegions).ForEach(func(_, v []byte) error {
		var rm regionMeta
		if err := json.Unmarshal(v, &rm); err != nil {
			return err
		}
		c.RegionCount++
		c.RegionBytes += rm.StoredSize
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// migrateCounters はカウンタ未初期化のストアで一度だけ全走査から初期化する。
func migrateCounters(tx *bolt.Tx) error {
	if _, ok := loadCounters(tx); ok {
		return nil
	}
	c, err := scanCounters(tx)
	if err != nil {
		return err
	}
	return saveCounters(tx, c)
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// keyFileOwnersMigrated は fileowners 索引のバックフィル完了フラグ。
// 索引導入前(または旧形式)のストアを開いたとき、一度だけ全ファイルから
// 索引を再構築する(以降は putFileManifest/deleteFileManifest が維持する)。
// v2: キーに反転タイムスタンプ、値に一覧レコードを持つ形式。
var keyFileOwnersMigrated = []byte("fileowners_migrated_v2")

// FileManifest は保存済みファイル1件のメタデータ。
type FileManifest struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
	// Owner はファイルの所有者ID(APIキーごとの分離)。"" は共有(認証なし運用)。
	Owner string `json:"owner,omitempty"`
	// Chunks は復元順に並んだチャンクハッシュ(hex)の列。
	Chunks []string `json:"chunks"`
	// Encoding は保存時に適用した可逆変換。"" = なし。
	// "gzip-zlib-v1" = zlib産gzipを展開して保存(チャンク列は展開データ)。
	Encoding string `json:"encoding,omitempty"`
	// PrecompHeader / PrecompLevel は gzip/zlib 再構成レシピ
	// (ヘッダ原文と zlib 圧縮レベル)。単一ストリーム用。
	PrecompHeader []byte `json:"precomp_header,omitempty"`
	PrecompLevel  int    `json:"precomp_level,omitempty"`
	// PrecompMembers はマルチメンバー gzip の再構成レシピ列。
	PrecompMembers []precomp.Member `json:"precomp_members,omitempty"`
	// PrecompPNG は PNG コンテナの再構成レシピ。
	PrecompPNG *precomp.PNGRecipe `json:"precomp_png,omitempty"`
	// PrecompContainer は ZIP / PDF コンテナの再構成レシピ
	// (チャンク列 = スケルトン+展開データ列)。
	PrecompContainer *precomp.ContainerRecipe `json:"precomp_container,omitempty"`
	// OrigSHA256 は元ストリームの SHA-256(復元時の最終検証用)。
	OrigSHA256 string `json:"orig_sha256,omitempty"`
	// ChunkedSize はチャンク化された内容のサイズ。precompression 適用時は
	// 展開データのサイズになり Size(元ストリーム)と異なる。0 なら Size と同じ。
	ChunkedSize int64 `json:"chunked_size,omitempty"`

	// precompPlain は Put 中に展開データを一時的に保持する(永続化しない)。
	precompPlain []byte
}

// precompression のエンコーディング名。
const (
	// EncodingGzipZlibV1 は zlib産 gzip(単一メンバー)。
	EncodingGzipZlibV1 = "gzip-zlib-v1"
	// EncodingZlibV1 は生 zlib ストリーム(gitオブジェクト・PDF FlateDecode等)。
	EncodingZlibV1 = "zlib-v1"
	// EncodingGzipMultiV1 はマルチメンバー gzip(連結gzip・ローテートログ等)。
	EncodingGzipMultiV1 = "gzip-multi-v1"
	// EncodingPNGV1 は PNG コンテナ(IDAT の zlib を展開して保存)。
	EncodingPNGV1 = "png-zlib-v1"
	// EncodingZipV1 は ZIP コンテナ(docx/xlsx/jar 等を含む)。zlib 産の
	// deflate メンバーを展開して保存し、スケルトンごとチャンク化する。
	EncodingZipV1 = "zip-deflate-v1"
	// EncodingPDFV1 は PDF コンテナ(FlateDecode の zlib を展開して保存)。
	EncodingPDFV1 = "pdf-zlib-v1"
)

// ChunkMeta はユニークチャンク1件のメタデータ。
type ChunkMeta struct {
	// Compression は "zstd"、"raw"、"zstd-delta" のいずれか。
	Compression string `json:"compression"`
	RawSize     int64  `json:"raw_size"`
	StoredSize  int64  `json:"stored_size"`
	RefCount    int64  `json:"ref_count"`
	// BaseHash は zstd-delta のとき、デルタの基準となるチャンクのハッシュ。
	BaseHash string `json:"base_hash,omitempty"`
	// Depth はデルタチェーンの深さ(0 = plain/raw、1 = plainへのデルタ、…)。
	// 深さは maxDeltaDepth で制限され、読み出しコストの上限を保証する。
	Depth int `json:"depth,omitempty"`
	// Rep は保存表現のID(ファイル名サフィックス)。chain repack でチャンクの
	// 保存表現を差し替えるとき、新旧の表現を別ファイルとして共存させ、
	// メタデータが常に実在するファイルを指すことを保証する(クラッシュ安全)。
	// 空文字は初期表現(サフィックスなし)。
	Rep string `json:"rep,omitempty"`
	// PackID / PackOff は表現がパックファイル内に格納されている場合の位置
	// (長さは StoredSize)。PackID が空ならファイル表現(hash+Rep 名)。
	PackID  string `json:"pack,omitempty"`
	PackOff int64  `json:"poff,omitempty"`
	// RegionID / RegionOff はリージョン(複数チャンクをまとめて1本の zstd で
	// ソリッド圧縮した保存単位)内の位置。RegionID が非空なら、このチャンクの
	// 生バイトはリージョンを伸長した [RegionOff : RegionOff+RawSize] にある。
	// StoredSize はリージョン内の按分ではなく 0(容量はリージョン側で計上)。
	RegionID  string `json:"reg,omitempty"`
	RegionOff int64  `json:"roff,omitempty"`
	// Staged はクライアント直接アップロードされ、まだどのマニフェストにも
	// コミットされていないチャンクの登録時刻(unix秒)。RefCount==0 のまま
	// TTL を過ぎると Optimize が掃除する。
	Staged int64 `json:"staged,omitempty"`
	// DeltaTried はデルタ圧縮の適用判定を済ませたことを示す
	// (オフラインデルタパスが同じチャンクを繰り返し評価しないため)。
	DeltaTried bool `json:"dt,omitempty"`
	// RegionTried は小チャンクのファイル横断ソリッド圧縮を試したが採用され
	// なかったことを示す(buildSmallChunkRegions が同じ「束ねても縮まない」
	// 小チャンクを Optimize のたびに再試行してディスクを空回りさせないため)。
	// リージョン化に成功したチャンクは RegionID が非空になるので別途区別できる。
	RegionTried bool `json:"rt,omitempty"`
	// Features は類似検索索引に登録した特徴値(削除時の索引掃除に使う)。
	Features []uint64 `json:"features,omitempty"`
}

func openMetaDB(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("メタデータDBを開けません: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketFiles, bucketChunks, bucketSketches, bucketSettings, bucketPacks, bucketUsers, bucketOwnerChunks, bucketRegions, bucketFileOwners, bucketDirtyChunks, bucketDirtyFiles} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func getFileManifest(tx *bolt.Tx, id string) (*FileManifest, error) {
	raw := tx.Bucket(bucketFiles).Get([]byte(id))
	if raw == nil {
		return nil, ErrNotFound
	}
	var m FileManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// manifestSizes はカウンタ計上用の (論理, チャンク化視点) サイズを返す。
func manifestSizes(m *FileManifest) (int64, int64) {
	chunked := m.Size
	if m.ChunkedSize > 0 {
		chunked = m.ChunkedSize
	}
	return m.Size, chunked
}

func putFileManifest(tx *bolt.Tx, m *FileManifest) error {
	b := tx.Bucket(bucketFiles)
	isNew := b.Get([]byte(m.ID)) == nil
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := b.Put([]byte(m.ID), raw); err != nil {
		return err
	}
	// 所有者→ファイルの二次索引を維持(値は一覧表示レコード)。
	rec, err := marshalFileIndexRecord(m)
	if err != nil {
		return err
	}
	if err := tx.Bucket(bucketFileOwners).Put(ownerFileKey(m.Owner, m.CreatedAt, m.ID), rec); err != nil {
		return err
	}
	if !isNew {
		return nil // 実運用でマニフェストの上書きはない(防御のみ)
	}
	// 新規ファイルをインクリメンタル最適化の対象に積む。
	if err := tx.Bucket(bucketDirtyFiles).Put([]byte(m.ID), nil); err != nil {
		return err
	}
	logical, chunked := manifestSizes(m)
	return adjustCounters(tx, func(sc *storeCounters) {
		sc.FileCount++
		sc.LogicalBytes += logical
		sc.ChunkedBytes += chunked
	})
}

// deleteFileManifest はマニフェストと所有者索引を同一トランザクションで消し、
// 維持カウンタを減算する。
func deleteFileManifest(tx *bolt.Tx, m *FileManifest) error {
	if err := tx.Bucket(bucketFileOwners).Delete(ownerFileKey(m.Owner, m.CreatedAt, m.ID)); err != nil {
		return err
	}
	if err := tx.Bucket(bucketFiles).Delete([]byte(m.ID)); err != nil {
		return err
	}
	logical, chunked := manifestSizes(m)
	return adjustCounters(tx, func(sc *storeCounters) {
		sc.FileCount--
		sc.LogicalBytes -= logical
		sc.ChunkedBytes -= chunked
	})
}

// fileIndexRecord は fileowners 索引の値(一覧表示に必要な最小限)。
// マニフェスト本体(チャンク列・precomp レシピ込み)を読まずに一覧を
// 返すためのもの。所有者はキー側にあるので持たない。
type fileIndexRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

func marshalFileIndexRecord(m *FileManifest) ([]byte, error) {
	return json.Marshal(fileIndexRecord{ID: m.ID, Name: m.Name, Size: m.Size, CreatedAt: m.CreatedAt})
}

// ownerFileKey は fileowners バケットのキー:
// 所有者キー + 0x1F + 反転UnixNano(8B BigEndian) + ファイルID。
// 区切りの 0x1F により、ある所有者IDが別の所有者IDの接頭辞であっても
// プレフィックス走査が混ざらない(例: "u" と "u2")。反転タイムスタンプに
// より昇順走査 = 作成日時の降順(新しい順)になり、ソート不要で
// カーソルページングできる。末尾の ID は同時刻の衝突を一意化する。
func ownerFileKey(owner string, created time.Time, id string) []byte {
	ok := ownerKey(owner)
	key := make([]byte, 0, len(ok)+1+8+len(id))
	key = append(key, ok...)
	key = append(key, 0x1F)
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], ^uint64(created.UnixNano()))
	key = append(key, ts[:]...)
	return append(key, id...)
}

// ownerFilePrefix は List(owner) のプレフィックス走査に使うキー接頭辞。
func ownerFilePrefix(owner string) []byte {
	ok := ownerKey(owner)
	return append(ok, 0x1F)
}

// migrateFileOwners は fileowners 索引が現行形式(v2)でなければ全ファイル
// から一度だけ再構築する(索引導入前・旧形式ストアの移行)。
func migrateFileOwners(tx *bolt.Tx) error {
	settings := tx.Bucket(bucketSettings)
	if settings.Get(keyFileOwnersMigrated) != nil {
		return nil
	}
	// 旧形式のエントリが残っていても混ざらないよう、作り直す。
	if err := tx.DeleteBucket(bucketFileOwners); err != nil && err != bolt.ErrBucketNotFound {
		return err
	}
	idx, err := tx.CreateBucket(bucketFileOwners)
	if err != nil {
		return err
	}
	err = tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
		var m FileManifest
		if err := json.Unmarshal(v, &m); err != nil {
			return err
		}
		rec, err := marshalFileIndexRecord(&m)
		if err != nil {
			return err
		}
		return idx.Put(ownerFileKey(m.Owner, m.CreatedAt, m.ID), rec)
	})
	if err != nil {
		return err
	}
	// 旧フラグは掃除する(あってもなくても動作は同じ)。
	settings.Delete([]byte("fileowners_migrated_v1"))
	return settings.Put(keyFileOwnersMigrated, []byte{1})
}

func getChunkMeta(tx *bolt.Tx, hash string) (*ChunkMeta, error) {
	raw := tx.Bucket(bucketChunks).Get([]byte(hash))
	if raw == nil {
		return nil, nil
	}
	var c ChunkMeta
	if err := unmarshalChunkMeta(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func unmarshalChunkMeta(raw []byte, c *ChunkMeta) error {
	return json.Unmarshal(raw, c)
}

// ownerKey は users バケットのキーを返す。bbolt は空キーを許さないため、
// 匿名所有者("")は NUL 1バイトの番兵キーに写像する(実際の API キーは
// 印字可能文字列なので衝突しない)。
func ownerKey(owner string) []byte {
	if owner == "" {
		return []byte{0}
	}
	return []byte(owner)
}

// decodeOwnerKey は ownerKey の逆変換(番兵キー {0} → "")。
func decodeOwnerKey(key []byte) string {
	if len(key) == 1 && key[0] == 0 {
		return ""
	}
	return string(key)
}

// setOwnerUsage は所有者の論理使用量を絶対値で設定する(fsck の修復用)。
func setOwnerUsage(tx *bolt.Tx, owner string, used int64) error {
	if used < 0 {
		used = 0
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(used))
	return tx.Bucket(bucketUsers).Put(ownerKey(owner), buf[:])
}

// ownerUsage は所有者の論理使用量を返す。
func ownerUsage(tx *bolt.Tx, owner string) (int64, error) {
	raw := tx.Bucket(bucketUsers).Get(ownerKey(owner))
	if raw == nil || len(raw) != 8 {
		return 0, nil
	}
	return int64(binary.BigEndian.Uint64(raw)), nil
}

// addOwnerUsage は所有者の論理使用量に delta を加算する(負も可)。
func addOwnerUsage(tx *bolt.Tx, owner string, delta int64) error {
	used, err := ownerUsage(tx, owner)
	if err != nil {
		return err
	}
	used += delta
	if used < 0 {
		used = 0
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(used))
	return tx.Bucket(bucketUsers).Put(ownerKey(owner), buf[:])
}

// ownerChunkKey は ownerchunks バケットのキー(所有者キー + 0x1F + ハッシュ)。
func ownerChunkKey(owner, hash string) []byte {
	ok := ownerKey(owner)
	key := make([]byte, 0, len(ok)+1+len(hash))
	key = append(key, ok...)
	key = append(key, 0x1F)
	return append(key, hash...)
}

// addOwnerChunkRefs は所有者のチャンク参照数を hashes の出現回数ぶん増減する。
func addOwnerChunkRefs(tx *bolt.Tx, owner string, hashes []string, delta int64) error {
	b := tx.Bucket(bucketOwnerChunks)
	counts := make(map[string]int64)
	for _, h := range hashes {
		counts[h] += delta
	}
	for h, d := range counts {
		key := ownerChunkKey(owner, h)
		var cur int64
		if raw := b.Get(key); len(raw) == 8 {
			cur = int64(binary.BigEndian.Uint64(raw))
		}
		cur += d
		if cur <= 0 {
			if err := b.Delete(key); err != nil {
				return err
			}
			continue
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(cur))
		if err := b.Put(key, buf[:]); err != nil {
			return err
		}
	}
	return nil
}

// ownerHasChunk は所有者がマニフェスト経由でチャンクを参照しているかを返す。
func ownerHasChunk(tx *bolt.Tx, owner, hash string) bool {
	return tx.Bucket(bucketOwnerChunks).Get(ownerChunkKey(owner, hash)) != nil
}

func putPackMeta(tx *bolt.Tx, packID string, pm *packMeta) error {
	raw, err := json.Marshal(pm)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketPacks).Put([]byte(packID), raw)
}

func unmarshalPackMeta(raw []byte, pm *packMeta) error {
	return json.Unmarshal(raw, pm)
}

// putChunkMeta はチャンクメタを保存し、維持カウンタを新旧の差分で更新する
// (全ミューテーションがここを通るため、カウンタ整合の一元点になる)。
func putChunkMeta(tx *bolt.Tx, hash string, c *ChunkMeta) error {
	b := tx.Bucket(bucketChunks)
	var old ChunkMeta
	hadOld := false
	if raw := b.Get([]byte(hash)); raw != nil {
		if err := unmarshalChunkMeta(raw, &old); err != nil {
			return err
		}
		hadOld = true
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if err := b.Put([]byte(hash), raw); err != nil {
		return err
	}
	return adjustCounters(tx, func(sc *storeCounters) {
		if !hadOld {
			sc.ChunkCount++
			sc.UniqueBytes += c.RawSize
			sc.ChunkPhysical += c.StoredSize
			sc.DeltaChunks += b2i(c.Compression == compressionDelta)
			return
		}
		sc.UniqueBytes += c.RawSize - old.RawSize
		sc.ChunkPhysical += c.StoredSize - old.StoredSize
		sc.DeltaChunks += b2i(c.Compression == compressionDelta) - b2i(old.Compression == compressionDelta)
	})
}

// deleteChunkMeta はチャンクメタを削除し、維持カウンタを減算する。
// 存在しなければ何もしない。
func deleteChunkMeta(tx *bolt.Tx, hash string) error {
	b := tx.Bucket(bucketChunks)
	raw := b.Get([]byte(hash))
	if raw == nil {
		return nil
	}
	var old ChunkMeta
	if err := unmarshalChunkMeta(raw, &old); err != nil {
		return err
	}
	if err := b.Delete([]byte(hash)); err != nil {
		return err
	}
	return adjustCounters(tx, func(sc *storeCounters) {
		sc.ChunkCount--
		sc.UniqueBytes -= old.RawSize
		sc.ChunkPhysical -= old.StoredSize
		sc.DeltaChunks -= b2i(old.Compression == compressionDelta)
	})
}

// resolveAvgChunkSize はストアの平均チャンクサイズを確定する。
// 初回はリクエスト値(0ならデフォルト)を保存し、以降は保存値を優先する。
// チャンクサイズが途中で変わると既存データとの重複排除が効かなくなるため、
// ストアの生涯で一貫させる必要がある。
func resolveAvgChunkSize(tx *bolt.Tx, requested int) (int, error) {
	b := tx.Bucket(bucketSettings)
	if raw := b.Get(keyAvgChunkSize); raw != nil && len(raw) == 8 {
		return int(binary.BigEndian.Uint64(raw)), nil
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(requested))
	if err := b.Put(keyAvgChunkSize, buf[:]); err != nil {
		return 0, err
	}
	return requested, nil
}

func featureKey(f uint64) []byte {
	var k [8]byte
	binary.BigEndian.PutUint64(k[:], f)
	return k[:]
}

// registerSketches は特徴値→ハッシュを索引に登録する。既存エントリは上書きする
// (最新のチャンクをベース候補にした方が、世代ドリフトでデルタが肥大しない)。
func registerSketches(tx *bolt.Tx, hash string, features []uint64) error {
	b := tx.Bucket(bucketSketches)
	for _, f := range features {
		if err := b.Put(featureKey(f), []byte(hash)); err != nil {
			return err
		}
	}
	return nil
}

// lookupSketches は特徴値に一致する既存チャンクのハッシュを重複なしで返す。
func lookupSketches(tx *bolt.Tx, features []uint64) []string {
	b := tx.Bucket(bucketSketches)
	var hashes []string
	for _, f := range features {
		hash := b.Get(featureKey(f))
		if hash == nil {
			continue
		}
		h := string(hash)
		dup := false
		for _, e := range hashes {
			if e == h {
				dup = true
				break
			}
		}
		if !dup {
			hashes = append(hashes, h)
		}
	}
	return hashes
}

// dropSketches は削除されるチャンクが登録した索引エントリを掃除する。
func dropSketches(tx *bolt.Tx, hash string, features []uint64) error {
	b := tx.Bucket(bucketSketches)
	for _, f := range features {
		key := featureKey(f)
		if string(b.Get(key)) != hash {
			continue // 別チャンクのエントリは残す
		}
		if err := b.Delete(key); err != nil {
			return err
		}
	}
	return nil
}
