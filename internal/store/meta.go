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
)

var keyAvgChunkSize = []byte("avg_chunk_size")

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
	// Staged はクライアント直接アップロードされ、まだどのマニフェストにも
	// コミットされていないチャンクの登録時刻(unix秒)。RefCount==0 のまま
	// TTL を過ぎると Optimize が掃除する。
	Staged int64 `json:"staged,omitempty"`
	// DeltaTried はデルタ圧縮の適用判定を済ませたことを示す
	// (オフラインデルタパスが同じチャンクを繰り返し評価しないため)。
	DeltaTried bool `json:"dt,omitempty"`
	// Features は類似検索索引に登録した特徴値(削除時の索引掃除に使う)。
	Features []uint64 `json:"features,omitempty"`
}

func openMetaDB(path string) (*bolt.DB, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("メタデータDBを開けません: %w", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketFiles, bucketChunks, bucketSketches, bucketSettings, bucketPacks, bucketUsers, bucketOwnerChunks} {
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

func putFileManifest(tx *bolt.Tx, m *FileManifest) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketFiles).Put([]byte(m.ID), raw)
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

func putChunkMeta(tx *bolt.Tx, hash string, c *ChunkMeta) error {
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketChunks).Put([]byte(hash), raw)
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
