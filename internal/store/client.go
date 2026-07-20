package store

// クライアント支援プロトコル(restic/kopia 型)のストア側実装。
//
// チャンク分割・圧縮・展開をクライアントが行い、サーバーは
// 「検証して保存する」だけになる:
//
//  1. HasChunks: クライアントが持つチャンクハッシュ列のうち、サーバーに
//     無いものを返す(重複排除の交渉。既存分は転送も保存も不要になる)
//  2. PutChunkVerified: 無かったチャンクだけ、クライアントが圧縮した形で
//     受け取る。サーバーの仕事は伸長+SHA-256検証のみ(圧縮の10〜50倍速い)。
//     検証なしで受けると悪意あるクライアントが「ハッシュHの偽データ」で
//     全ユーザー共有の重複排除チャンクを汚染できるため、検証は省略できない
//  3. CommitClientManifest: チャンク列をファイルとして確定(参照カウント
//     加算・クォータ適用を単一トランザクションで)
//
// コミット前のチャンクは Staged 状態(RefCount 0)で保持され、TTL を過ぎた
// 孤児は Optimize が掃除する。デルタ圧縮は取り込み経路では行わず、
// バックグラウンドのオフラインデルタパス(Optimize)が後から適用する
// (取り込みCPUを最小化しつつ圧縮率は維持する)。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

// StagedTTL は未コミットチャンクを保持する期間。クライアントが
// アップロード途中で失踪した場合、この期間の後に掃除される。
const StagedTTL = 24 * time.Hour

// HasChunks は hashes のうちストアに存在しないものを返す。
// 存在した staged チャンクは TTL を延長する(「missing でないのでスキップ」
// した直後に GC で消えてコミットが失敗するレースの防止。kopia の
// SessionExpirationAge / S3 multipart TTL と同じ考え方)。
func (s *Store) HasChunks(hashes []string) ([]string, error) {
	var missing []string
	var stagedHits []string
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, h := range hashes {
			if !validChunkHash(h) {
				return fmt.Errorf("不正なチャンクハッシュ: %q", h)
			}
			meta, err := getChunkMeta(tx, h)
			if err != nil {
				return err
			}
			if meta == nil {
				missing = append(missing, h)
			} else if meta.Staged > 0 {
				stagedHits = append(stagedHits, h)
			}
		}
		return nil
	})
	if err != nil || len(stagedHits) == 0 {
		return missing, err
	}
	now := time.Now().Unix()
	err = s.batchUpdate(func(tx *bolt.Tx) error {
		for _, h := range stagedHits {
			meta, err := getChunkMeta(tx, h)
			if err != nil || meta == nil || meta.Staged == 0 {
				continue
			}
			meta.Staged = now
			if err := putChunkMeta(tx, h, meta); err != nil {
				return err
			}
		}
		return nil
	})
	return missing, err
}

// OwnerHasChunk は所有者がマニフェスト経由でチャンクを参照しているかを返す
// (チャンク直接ダウンロードの読み出し権チェック用。ハッシュを知っている
// だけでは他人のデータを取得できない)。
func (s *Store) OwnerHasChunk(owner, hash string) (bool, error) {
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		ok = ownerHasChunk(tx, owner, hash)
		return nil
	})
	return ok, err
}

func validChunkHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// PutChunkVerified はクライアントが圧縮済みのチャンクを検証して保存する。
// compression は "zstd" または "none"。伸長結果の SHA-256 が hash と
// 一致しない場合は拒否する(共有重複排除の汚染防止)。
// 保存されたチャンクは Staged 状態になり、CommitClientManifest で確定する。
func (s *Store) PutChunkVerified(hash string, stored []byte, compression string, rawSize int64) error {
	if !validChunkHash(hash) {
		return fmt.Errorf("不正なチャンクハッシュ")
	}
	maxRaw := int64(s.chunkSize) * 4
	if rawSize <= 0 || rawSize > maxRaw {
		return fmt.Errorf("チャンクの展開サイズが範囲外です (1〜%d bytes)", maxRaw)
	}
	if int64(len(stored)) > rawSize+4096 {
		return fmt.Errorf("圧縮データが展開サイズより大きすぎます(圧縮せず none で送ってください)")
	}

	// 検証: 伸長(zstd 伸長は圧縮の数十倍速い)して内容ハッシュを確認する。
	var raw []byte
	switch compression {
	case compressionRaw, "none":
		compression = compressionRaw
		raw = stored
	case compressionZstd:
		var err error
		raw, err = s.dec.DecodeAll(stored, make([]byte, 0, rawSize))
		if err != nil {
			return fmt.Errorf("zstd 伸長に失敗: %w", err)
		}
	default:
		return fmt.Errorf("不明な圧縮形式 %q (zstd | none)", compression)
	}
	if int64(len(raw)) != rawSize {
		return fmt.Errorf("展開サイズが申告(%d)と一致しません(実際 %d)", rawSize, len(raw))
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != hash {
		return fmt.Errorf("チャンク内容がハッシュと一致しません")
	}

	// 類似検索用の特徴はここで計算しておく(1パスのローリングハッシュで安価)。
	// デルタ圧縮そのものはオフラインパスに任せる。
	features := computeFeatures(raw)
	var minhash []uint64
	if rawSize <= smallChunkMax { // 小チャンクのみクラスタリング用 min-hash
		minhash = computeMinHash(raw)
	}

	return s.batchUpdate(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err != nil {
			return err
		}
		if meta != nil {
			return nil // 既存(検証済み)。何もしない
		}
		loc, err := s.writeRep(hash, "", stored)
		if err != nil {
			return err
		}
		newMeta := &ChunkMeta{
			Compression: compression,
			RawSize:     rawSize,
			StoredSize:  int64(len(stored)),
			RefCount:    0,
			Staged:      time.Now().Unix(),
			Features:    features,
			MinHash:     minhash,
		}
		if err := applyRepLocation(tx, newMeta, "", loc, int64(len(stored))); err != nil {
			return err
		}
		if err := registerSketches(tx, hash, features); err != nil {
			return err
		}
		// 新チャンクをインクリメンタル最適化(オフラインデルタ・リージョン化)
		// の対象に積む。
		if err := tx.Bucket(bucketDirtyChunks).Put([]byte(hash), nil); err != nil {
			return err
		}
		return putChunkMeta(tx, hash, newMeta)
	})
}

// CommitClientManifest はクライアントが列挙したチャンク列をファイルとして
// 確定する。全チャンクの存在検証・参照カウント加算・クォータ適用を
// 単一トランザクションで行う(失敗時は何も変わらない)。
// 存在しないチャンクがあれば missing に列挙して返す。
func (s *Store) CommitClientManifest(name, owner string, hashes []string, quota, maxBytes int64) (*FileManifest, []string, error) {
	m := &FileManifest{
		ID:        newID(),
		Name:      name,
		Owner:     owner,
		CreatedAt: time.Now().UTC(),
		Chunks:    hashes,
	}
	var missing []string
	err := s.batchUpdate(func(tx *bolt.Tx) error {
		missing = missing[:0] // Batch 再実行に備え毎回リセット
		total := int64(0)
		metas := make([]*ChunkMeta, len(hashes))
		for i, h := range hashes {
			if !validChunkHash(h) {
				return fmt.Errorf("不正なチャンクハッシュ: %q", h)
			}
			meta, err := getChunkMeta(tx, h)
			if err != nil {
				return err
			}
			if meta == nil {
				missing = append(missing, h)
				continue
			}
			metas[i] = meta
			total += meta.RawSize
		}
		if len(missing) > 0 {
			return ErrChunksMissing
		}
		if maxBytes > 0 && total > maxBytes {
			return ErrTooLarge
		}
		used, err := ownerUsage(tx, owner)
		if err != nil {
			return err
		}
		if quota > 0 && used+total > quota {
			return ErrQuotaExceeded
		}
		for i, h := range hashes {
			meta := metas[i]
			meta.RefCount++
			meta.Staged = 0
			if err := putChunkMeta(tx, h, meta); err != nil {
				return err
			}
		}
		m.Size = total
		if err := addOwnerUsage(tx, owner, total); err != nil {
			return err
		}
		if err := addOwnerChunkRefs(tx, owner, hashes, 1); err != nil {
			return err
		}
		return putFileManifest(tx, m)
	})
	if err != nil {
		return nil, missing, err
	}
	return m, nil, nil
}

// ErrChunksMissing はマニフェストが参照するチャンクが未アップロードで
// あることを示す(missing 一覧つきで返る)。
var ErrChunksMissing = errStr("マニフェストが参照するチャンクが存在しません")

type errStr string

func (e errStr) Error() string { return string(e) }

// ChunkRep はチャンクの保存表現を返す(クライアント側伸長用)。
// 通常は保存されたままのバイト列(zstd または raw)を無変換で返すので
// サーバーの CPU コストはほぼゼロ。デルタ表現のチャンクだけは展開して
// raw で返す。戻り値の compression は "zstd" | "none"。
func (s *Store) ChunkRep(hash string) (data []byte, compression string, rawSize int64, err error) {
	var meta *ChunkMeta
	err = s.db.View(func(tx *bolt.Tx) error {
		var err error
		meta, err = getChunkMeta(tx, hash)
		return err
	})
	if err != nil {
		return nil, "", 0, err
	}
	if meta == nil {
		return nil, "", 0, ErrNotFound
	}
	// リージョン内チャンクはソリッド表現なので単独では返せない。
	// 展開して raw で返す(クライアントは再圧縮せずそのまま検証する)。
	if meta.RegionID != "" {
		raw, err := s.readChunk(hash)
		if err != nil {
			return nil, "", 0, err
		}
		return raw, "none", meta.RawSize, nil
	}
	switch meta.Compression {
	case compressionZstd, compressionRaw:
		var stored []byte
		if meta.PackID != "" {
			stored, err = s.readFromPack(meta.PackID, meta.PackOff, meta.StoredSize)
		} else {
			stored, err = os.ReadFile(s.chunkPath(hash, meta.Rep))
		}
		if err != nil {
			// repack との競合はチャンク再読込(readChunk)にフォールバック
			raw, rerr := s.readChunk(hash)
			if rerr != nil {
				return nil, "", 0, rerr
			}
			return raw, "none", meta.RawSize, nil
		}
		kind := "none"
		if meta.Compression == compressionZstd {
			kind = "zstd"
		}
		return stored, kind, meta.RawSize, nil
	default: // delta 等は展開して返す
		raw, err := s.readChunk(hash)
		if err != nil {
			return nil, "", 0, err
		}
		return raw, "none", meta.RawSize, nil
	}
}

// AvgChunkSize はストアのチャンク分割パラメータ(クライアントが同じ
// パラメータで分割しないと重複排除が効かない)。
func (s *Store) AvgChunkSize() int { return s.chunkSize }

// sweepStagedChunks は TTL を過ぎた未コミットチャンクを掃除する
// (Optimize から呼ばれる)。
func (s *Store) sweepStagedChunks(res *OptimizeResult) error {
	cutoff := time.Now().Add(-StagedTTL).Unix()
	var stale []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			if meta.RefCount == 0 && meta.Staged > 0 && meta.Staged < cutoff {
				stale = append(stale, hash)
			}
			return nil
		})
	})
	if err != nil || len(stale) == 0 {
		return err
	}
	var orphans []string
	err = s.db.Update(func(tx *bolt.Tx) error {
		for _, hash := range stale {
			meta, err := getChunkMeta(tx, hash)
			if err != nil {
				return err
			}
			if meta == nil || meta.RefCount != 0 {
				continue // 掃除の合間にコミットされた
			}
			if err := deleteChunkMeta(tx, hash); err != nil {
				return err
			}
			if err := dropSketches(tx, hash, meta.Features); err != nil {
				return err
			}
			path, err := s.releaseRep(tx, hash, meta)
			if err != nil {
				return err
			}
			if path != "" {
				orphans = append(orphans, path)
			}
			res.StagedSwept++
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.removeChunkFiles(orphans)
	return nil
}
