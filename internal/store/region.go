package store

// リージョン圧縮(ソリッド圧縮 / Data Domain の compression region 相当)。
//
// 各チャンクを独立に zstd 圧縮すると、チャンクをまたぐ冗長性(exact-dup
// dedup と類似デルタで拾えないもの)を失う。実測では、実データ(混合)で
// チャンク独立圧縮 5.45x に対しソリッド圧縮(全体1本)は 6.01x で +10%、
// 8チャンク束のリージョン圧縮でその 8割超(5.92x)が取れる。
//
// そこでオフラインパス(Optimize)で、ファイルのマニフェスト順に連続する
// 独立圧縮チャンクを regionChunks 個ずつまとめ、生バイトを連結して1本の
// zstd-19 で圧縮し直す。各チャンクのメタは (RegionID, RegionOff) を指す。
//
// 読み出し: リージョンを伸長(伸長済みをキャッシュ)し、[off:off+rawSize]
// をスライスして SHA-256 検証。ファイルの順次読み出しでは各リージョンは
// 1回だけ伸長される。
//
// クラッシュ安全性: リージョンファイルを先に書き、メンバーのメタを単一
// トランザクションで切り替え、旧表現を後で消す(pack/repack と同じ機構)。
// 「連結圧縮後サイズ < メンバーの現表現サイズ合計」の場合のみ実行するため、
// 物理容量は単調減少する。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// regionChunks は1リージョンにまとめるチャンク数。実データ(Goソース tar
// 64MiB)での実測: 独立圧縮比で 束8+19=9.97% / 束16+22=11.35% / 束32+22=
// 12.26% 改善。16 は 32 の利得の大半を取りつつ、コールドなランダム読みの
// 伸長コスト(最大 ~16MiB)を半分に抑えるバランス点(RESEARCH.md §4.15)。
// 順次読み出しはリージョンキャッシュが吸収する。
const regionChunks = 16

// regionMeta はリージョン1本の使用量。
type regionMeta struct {
	StoredSize  int64 `json:"stored"`  // 圧縮後サイズ(物理)
	MemberCount int   `json:"members"` // 総メンバーチャンク数
	LiveCount   int   `json:"live"`    // 参照が残っているメンバー数
}

func (s *Store) regionPath(id string) string {
	return filepath.Join(s.dir, "regions", id[:2], id)
}

func regionCacheKey(id string) string { return "R:" + id }

func getRegionMeta(tx *bolt.Tx, id string) (*regionMeta, error) {
	raw := tx.Bucket(bucketRegions).Get([]byte(id))
	if raw == nil {
		return nil, nil
	}
	var rm regionMeta
	if err := json.Unmarshal(raw, &rm); err != nil {
		return nil, err
	}
	return &rm, nil
}

// putRegionMeta はリージョンメタを保存し、維持カウンタを差分更新する。
func putRegionMeta(tx *bolt.Tx, id string, rm *regionMeta) error {
	b := tx.Bucket(bucketRegions)
	var old regionMeta
	hadOld := false
	if raw := b.Get([]byte(id)); raw != nil {
		if err := json.Unmarshal(raw, &old); err != nil {
			return err
		}
		hadOld = true
	}
	raw, err := json.Marshal(rm)
	if err != nil {
		return err
	}
	if err := b.Put([]byte(id), raw); err != nil {
		return err
	}
	return adjustCounters(tx, func(sc *storeCounters) {
		if !hadOld {
			sc.RegionCount++
			sc.RegionBytes += rm.StoredSize
			return
		}
		sc.RegionBytes += rm.StoredSize - old.StoredSize
	})
}

// deleteRegionMeta はリージョンメタを削除し、維持カウンタを減算する。
func deleteRegionMeta(tx *bolt.Tx, id string) error {
	b := tx.Bucket(bucketRegions)
	raw := b.Get([]byte(id))
	if raw == nil {
		return nil
	}
	var old regionMeta
	if err := json.Unmarshal(raw, &old); err != nil {
		return err
	}
	if err := b.Delete([]byte(id)); err != nil {
		return err
	}
	return adjustCounters(tx, func(sc *storeCounters) {
		sc.RegionCount--
		sc.RegionBytes -= old.StoredSize
	})
}

// readRegionRaw はリージョンを伸長した生バイト全体を返す。codec はメンバー
// メタの Compression(リージョン全体の圧縮形式。"" と zstd は zstd)。
// useCache が偽ならキャッシュを見ず・入れず、ディスク上のバイトを検証する
// (スクラブ用)。
func (s *Store) readRegionRaw(id, codec string, rawTotal int64, useCache bool) ([]byte, error) {
	if useCache {
		if data, ok := s.cache.get(regionCacheKey(id)); ok {
			return data, nil
		}
	}
	stored, err := os.ReadFile(s.regionPath(id))
	if err != nil {
		return nil, err
	}
	var raw []byte
	switch codec {
	case compressionBr:
		raw, err = brotliDecode(stored, rawTotal)
	case compressionZstdBCJ:
		raw, err = s.dec.DecodeAll(stored, make([]byte, 0, rawTotal))
		if err == nil {
			raw = bcjX86Decode(raw)
		}
	case compressionBrBCJ:
		raw, err = brotliDecode(stored, rawTotal)
		if err == nil {
			raw = bcjX86Decode(raw)
		}
	default:
		raw, err = s.dec.DecodeAll(stored, make([]byte, 0, rawTotal))
	}
	if err != nil {
		return nil, fmt.Errorf("リージョン伸長に失敗: %w", err)
	}
	if useCache {
		s.cache.put(regionCacheKey(id), raw)
	}
	return raw, nil
}

// readChunkFromRegion はリージョン内チャンクを取り出して検証する。
func (s *Store) readChunkFromRegion(hash string, meta *ChunkMeta, useCache bool) ([]byte, error) {
	// リージョンの生合計サイズは伸長時に確定するので、まず十分な見積りで伸長。
	raw, err := s.readRegionRaw(meta.RegionID, meta.Compression, meta.RegionOff+meta.RawSize, useCache)
	if err != nil {
		return nil, err
	}
	if meta.RegionOff+meta.RawSize > int64(len(raw)) {
		return nil, fmt.Errorf("リージョン範囲外です")
	}
	data := raw[meta.RegionOff : meta.RegionOff+meta.RawSize]
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("リージョン内チャンクが破損しています")
	}
	return data, nil
}

// buildRegions は全ファイルを対象に、ファイル順に連続する独立圧縮チャンクを
// リージョンにまとめる(フルパス)。
func (s *Store) buildRegions(res *OptimizeResult) error {
	var lists [][]string
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
			var m FileManifest
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			if len(m.Chunks) >= 2 {
				lists = append(lists, m.Chunks)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	return s.buildRegionsForChunkLists(lists, res)
}

// buildRegionsForChunkLists はチャンク列(ファイル順)の集合をリージョンに
// まとめる本体(フルパス・インクリメンタルパス共通)。
func (s *Store) buildRegionsForChunkLists(lists [][]string, res *OptimizeResult) error {
	placed := make(map[string]bool) // このパスで既にリージョン化したチャンク
	for _, chunks := range lists {
		var run []string
		flush := func() error {
			if len(run) >= 2 {
				if err := s.packRegion(run, res); err != nil {
					return err
				}
				for _, h := range run {
					placed[h] = true
				}
			}
			run = run[:0]
			return nil
		}
		for _, h := range chunks {
			ok, err := s.regionEligible(h, placed)
			if err != nil {
				return err
			}
			if !ok {
				if err := flush(); err != nil {
					return err
				}
				continue
			}
			// 同一 run 内の重複は1回だけ
			dup := false
			for _, e := range run {
				if e == h {
					dup = true
					break
				}
			}
			if dup {
				continue
			}
			run = append(run, h)
			if len(run) >= regionChunks {
				if err := flush(); err != nil {
					return err
				}
			}
		}
		if err := flush(); err != nil {
			return err
		}
	}
	return nil
}

// smallChunkMax はこのサイズ以下の独立チャンクを「小チャンク」とみなし、
// ファイルをまたいでソリッド圧縮の対象にする。平均チャンク(1MiB)未満の
// 小さなファイルは単一チャンクになり、buildRegions のファイル内グループ化から
// 漏れる。多数のユーザーが小さなファイル(設定・JSON・小さなログ・
// サムネイル等)を大量に投下すると、それぞれが独立圧縮され、ファイルを
// またぐ共通部分(共有辞書)を取りこぼす。ここで拾い直す。
const smallChunkMax = 512 << 10

// smallRegionMaxMembers は小チャンクリージョンの最大メンバー数。
const smallRegionMaxMembers = 64

// smallRegionRawMax は小チャンクリージョンの生バイト上限。1メンバーの
// 読み出しでこのサイズまで伸長しうる(読み出し増幅の上限)。テストから調整可能。
var smallRegionRawMax int64 = 4 << 20

// buildSmallChunkRegions は zstd 圧縮済みの小チャンクをファイル横断で集め、
// スーパーフィーチャで内容の近いものを隣接させてから、生バイトで
// smallRegionRawMax まで束ねて1本の zstd-19 で再圧縮する。
//
// packRegion の「連結圧縮後サイズ < メンバー現表現合計の場合のみ採用」
// 判定により、束ねても縮まない組み合わせは不採用となり物理容量は後退しない。
// 採用されなかった小チャンクは RegionTried を立て、次回以降の Optimize で
// 同じ束を無駄に再試行しない(新しく届いた小チャンクだけが対象になる)。
//
// 対象を zstd 圧縮済みに限るのは、raw(圧縮不能=乱数・メディア)チャンクを
// 束ねてもソリッド圧縮は効かず、リージョンファイルの書き捨てを増やすだけ
// だからである。
func (s *Store) buildSmallChunkRegions(res *OptimizeResult) error {
	var chunks []smallChunk
	err := s.db.View(func(tx *bolt.Tx) error {
		return forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			if sc, ok := smallChunkCandidate(hash, meta); ok {
				chunks = append(chunks, sc)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	return s.buildSmallChunkRegionsFrom(chunks, res)
}

type smallChunk struct {
	hash    string
	raw     int64
	feature uint64
}

// smallChunkCandidate はメタが小チャンクソリッド圧縮の対象かを判定する。
func smallChunkCandidate(hash string, meta *ChunkMeta) (smallChunk, bool) {
	if meta.RegionID == "" && !meta.RegionTried &&
		independentComp(meta.Compression) && meta.Compression != compressionRaw &&
		meta.RefCount > 0 &&
		meta.RawSize > 0 && meta.RawSize <= smallChunkMax {
		var f uint64
		if len(meta.Features) > 0 {
			f = meta.Features[0]
		}
		return smallChunk{hash: hash, raw: meta.RawSize, feature: f}, true
	}
	return smallChunk{}, false
}

// buildSmallChunkRegionsFrom は候補列を束ねてソリッド圧縮する本体
// (フルパス・インクリメンタルパス共通)。
func (s *Store) buildSmallChunkRegionsFrom(chunks []smallChunk, res *OptimizeResult) error {
	if len(chunks) < 2 {
		return nil
	}
	// 内容の近いチャンク(共有スーパーフィーチャ)を隣接させ、ソリッド圧縮の
	// 効きを最大化する。特徴が無い/一致しないチャンクはハッシュ順で安定化。
	sort.Slice(chunks, func(i, j int) bool {
		if chunks[i].feature != chunks[j].feature {
			return chunks[i].feature < chunks[j].feature
		}
		return chunks[i].hash < chunks[j].hash
	})

	var run []string
	var runBytes int64
	var attempted []string
	flush := func() error {
		if len(run) >= 2 {
			attempted = append(attempted, run...)
			if err := s.packRegion(run, res); err != nil {
				return err
			}
		}
		run = run[:0]
		runBytes = 0
		return nil
	}
	for _, c := range chunks {
		if len(run) >= smallRegionMaxMembers || (runBytes > 0 && runBytes+c.raw > smallRegionRawMax) {
			if err := flush(); err != nil {
				return err
			}
		}
		run = append(run, c.hash)
		runBytes += c.raw
	}
	if err := flush(); err != nil {
		return err
	}

	// 束ねても採用されなかった(=まだ独立 zstd 表現のままの)小チャンクに
	// RegionTried を立て、次回の Optimize で同じ束を再試行しない。
	if len(attempted) == 0 {
		return nil
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		for _, hash := range attempted {
			meta, err := getChunkMeta(tx, hash)
			if err != nil {
				return err
			}
			if meta == nil || meta.RegionID != "" ||
				!independentComp(meta.Compression) || meta.Compression == compressionRaw {
				continue // 採用された or 状況が変わった
			}
			if meta.RegionTried {
				continue
			}
			meta.RegionTried = true
			if err := putChunkMeta(tx, hash, meta); err != nil {
				return err
			}
		}
		return nil
	})
}

// regionEligible はチャンクがリージョン化の対象か(独立 zstd/raw 表現で、
// まだリージョン/デルタ化されておらず、このパスで未処理か)を返す。
func (s *Store) regionEligible(hash string, placed map[string]bool) (bool, error) {
	if placed[hash] {
		return false, nil
	}
	var ok bool
	err := s.db.View(func(tx *bolt.Tx) error {
		meta, err := getChunkMeta(tx, hash)
		if err != nil || meta == nil {
			return err
		}
		ok = meta.RegionID == "" && independentComp(meta.Compression) &&
			meta.RefCount > 0
		return nil
	})
	return ok, err
}

// packRegion は run のチャンク群を1本のリージョンに再圧縮し、
// 圧縮後サイズがメンバーの現表現合計より小さい場合のみ切り替える。
func (s *Store) packRegion(run []string, res *OptimizeResult) error {
	// メンバーの生データを連結(ロック外)
	type member struct {
		hash string
		off  int64
		raw  int64
	}
	var members []member
	var buf []byte
	for _, h := range run {
		data, err := s.readChunk(h)
		if err != nil {
			return nil // 並行削除など → このリージョンは諦める
		}
		members = append(members, member{hash: h, off: int64(len(buf)), raw: int64(len(data))})
		buf = append(buf, data...)
	}
	if len(members) < 2 {
		return nil
	}
	// オフラインパスなので最強コーデック群のベストオブ(libzstd-22 大窓 /
	// brotli-11 / BCJ 併用)。リージョンは複数チャンクの連結なので、大窓に
	// よりチャンクをまたぐ遠距離の反復も1本のフレーム内で拾える。
	compressed, regionComp, _ := s.offlineCompressBest(buf)

	regionID := newID()
	// メンバーの現表現サイズ合計を見積もる
	var oldTotal int64
	s.db.View(func(tx *bolt.Tx) error {
		for _, m := range members {
			meta, err := getChunkMeta(tx, m.hash)
			if err == nil && meta != nil {
				oldTotal += meta.StoredSize
			}
		}
		return nil
	})
	if int64(len(compressed)) >= oldTotal {
		return nil // 縮まないなら不採用
	}

	// リージョンファイルを先に書く(クラッシュ安全)
	if err := s.writeRegionFile(regionID, compressed); err != nil {
		return err
	}

	committed := false
	var oldPaths []string
	err := s.db.Update(func(tx *bolt.Tx) error {
		// 全メンバーがまだ独立表現のままか再確認(並行変更に備える)
		live := 0
		metas := make([]*ChunkMeta, len(members))
		for i, m := range members {
			meta, err := getChunkMeta(tx, m.hash)
			if err != nil {
				return err
			}
			if meta == nil || meta.RegionID != "" || !independentComp(meta.Compression) {
				return nil // 状況が変わった → このリージョンは中止
			}
			metas[i] = meta
			live++
		}
		rm := &regionMeta{StoredSize: int64(len(compressed)), MemberCount: len(members), LiveCount: live}
		if err := putRegionMeta(tx, regionID, rm); err != nil {
			return err
		}
		for i, m := range members {
			meta := metas[i]
			// 旧表現(ファイル/パック)の解放パスを回収
			path, err := s.releaseRep(tx, m.hash, meta)
			if err != nil {
				return err
			}
			if path != "" {
				oldPaths = append(oldPaths, path)
			}
			meta.Compression = regionComp // リージョン全体の圧縮形式(ソリッド)
			meta.RegionID = regionID
			meta.RegionOff = m.off
			meta.StoredSize = 0 // 容量はリージョン側に計上
			meta.Rep = ""
			meta.PackID = ""
			meta.PackOff = 0
			if err := putChunkMeta(tx, m.hash, meta); err != nil {
				return err
			}
		}
		res.RegionsBuilt++
		res.RegionChunks += len(members)
		committed = true
		return nil
	})
	if err != nil {
		return err
	}
	if committed {
		s.removeChunkFiles(oldPaths)
	} else {
		os.Remove(s.regionPath(regionID))
	}
	return nil
}

func (s *Store) writeRegionFile(id string, data []byte) error {
	return durableWrite(s.regionPath(id), data)
}

// releaseRegionMember はリージョンメンバー1つの参照が消えたときに
// LiveCount を減らし、0 になったリージョンファイルを削除対象として返す。
func (s *Store) releaseRegionMember(tx *bolt.Tx, regionID string) (string, error) {
	rm, err := getRegionMeta(tx, regionID)
	if err != nil || rm == nil {
		return "", err
	}
	rm.LiveCount--
	if rm.LiveCount > 0 {
		return "", putRegionMeta(tx, regionID, rm)
	}
	if err := deleteRegionMeta(tx, regionID); err != nil {
		return "", err
	}
	return s.regionPath(regionID), nil
}

// compactRegions は生存率の低いリージョン(死んだメンバーが多い)の
// 生き残りチャンクを独立表現へ戻し、リージョンを削除して空間を回収する。
func (s *Store) compactRegions(res *OptimizeResult) error {
	type target struct {
		id string
		rm regionMeta
	}
	var targets []target
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketRegions).ForEach(func(k, v []byte) error {
			var rm regionMeta
			if err := json.Unmarshal(v, &rm); err != nil {
				return err
			}
			// 生存率50%未満のリージョンは解体して詰め直す
			if rm.LiveCount*2 < rm.MemberCount {
				targets = append(targets, target{id: string(k), rm: rm})
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	for _, t := range targets {
		if err := s.dissolveRegion(t.id, res); err != nil {
			return err
		}
	}
	return nil
}

// dissolveRegion はリージョンの生き残りメンバーを独立 zstd 表現に戻し、
// リージョンを削除する(buildRegions が次パスで詰め直す)。
func (s *Store) dissolveRegion(regionID string, res *OptimizeResult) error {
	// 生き残りメンバーを集める
	var live []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			if meta.RegionID == regionID {
				live = append(live, hash)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	for _, hash := range live {
		data, err := s.readChunk(hash)
		if err != nil {
			continue
		}
		out := s.compressChunk(data, "balanced")
		comp := compressionZstd
		if len(out) >= len(data) {
			out, comp = data, compressionRaw
		}
		loc, err := s.writeRep(hash, "", out)
		if err != nil {
			return err
		}
		err = s.db.Update(func(tx *bolt.Tx) error {
			meta, err := getChunkMeta(tx, hash)
			if err != nil || meta == nil || meta.RegionID != regionID {
				return err
			}
			meta.Compression = comp
			meta.RegionID = ""
			meta.RegionOff = 0
			meta.StoredSize = int64(len(out))
			return applyRepLocation(tx, meta, "", loc, int64(len(out)))
		})
		if err != nil {
			return err
		}
	}
	// リージョンを削除
	var path string
	err = s.db.Update(func(tx *bolt.Tx) error {
		if err := deleteRegionMeta(tx, regionID); err != nil {
			return err
		}
		path = s.regionPath(regionID)
		return nil
	})
	if err != nil {
		return err
	}
	os.Remove(path)
	s.cache.remove(regionCacheKey(regionID))
	res.RegionsCompacted++
	return nil
}
