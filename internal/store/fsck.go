package store

// メタデータ整合性チェック(fsck)。
//
// Scrub がデータ完全性(保存バイトの破損)を見るのに対し、Fsck はメタデータの
// 論理的整合性を見る。参照カウントの drift(実装バグや異常終了による)、
// どのマニフェストからも参照されない孤児チャンク(容量リーク)、マニフェストが
// 参照するのに存在しないチャンク(壊れたファイル)、所有者使用量の不一致などを
// 検出し、必要なら修復する。
//
// 参照カウントのモデル: チャンクの RefCount =
//   (全マニフェストのチャンク列でそのチャンクが現れる回数)
// + (そのチャンクをベースにしているデルタチャンクの数)
// この期待値を全マニフェスト・全チャンクから再計算し、保存値と照合する。

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// FsckResult はメタデータ整合性チェックの結果。
type FsckResult struct {
	// RefcountMismatches は保存 RefCount が期待値と食い違ったチャンク数。
	RefcountMismatches int `json:"refcount_mismatches"`
	// OrphanChunks はどのマニフェスト・デルタからも参照されないチャンク数
	// (期待 RefCount が 0。容量リーク)。
	OrphanChunks int `json:"orphan_chunks"`
	// DanglingRefs はマニフェスト/デルタ/リージョンが参照するのに存在しない
	// チャンク・リージョンの数(壊れた参照)。
	DanglingRefs int `json:"dangling_refs"`
	// BrokenFiles は存在しないチャンクを参照するファイルの (id, name)。
	BrokenFiles []AffectedFile `json:"broken_files,omitempty"`
	// UsageMismatches は所有者使用量が実際のファイル合計と食い違った所有者数。
	UsageMismatches int `json:"usage_mismatches"`
	// FileIndexMismatches は所有者→ファイル索引(fileowners)の不整合数
	// (存在しないファイルを指す孤児エントリ + 実在ファイルに対する欠損
	// エントリ + マニフェストと食い違う表示レコード)。
	FileIndexMismatches int `json:"file_index_mismatches"`
	// Repaired は修復が実行されたか。
	Repaired bool `json:"repaired"`
}

// Healthy は不整合がなかったかを返す。
func (r *FsckResult) Healthy() bool {
	return r.RefcountMismatches == 0 && r.OrphanChunks == 0 &&
		r.DanglingRefs == 0 && r.UsageMismatches == 0 &&
		r.FileIndexMismatches == 0
}

// Fsck はメタデータの整合性を検証する。repair が真なら、参照カウントの
// 修正・孤児チャンクの解放・所有者使用量の再計算を行う(壊れた参照=
// データ欠損は報告のみ。復旧にはバックアップが必要)。
//
// 全メタを1つの読み取り(repair 時は書き込み)トランザクションで処理する
// ため、実行中は一貫したスナップショットを見る。
func (s *Store) Fsck(repair bool) (*FsckResult, error) {
	res := &FsckResult{Repaired: repair}
	work := s.db.View
	if repair {
		work = s.db.Update
	}
	var orphanPaths []string
	err := work(func(tx *bolt.Tx) error {
		// 1) 期待参照カウントを再計算
		expected := map[string]int64{}     // チャンク → 期待 RefCount
		manifestRefs := map[string]int64{} // チャンク → マニフェスト参照数
		ownerSize := map[string]int64{}    // 所有者 → 論理サイズ合計
		expectedIdx := map[string][]byte{} // 期待する fileowners キー → レコード
		var brokenFiles []AffectedFile
		chunks := tx.Bucket(bucketChunks)

		err := tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
			var m FileManifest
			if err := json.Unmarshal(v, &m); err != nil {
				return err
			}
			ownerSize[m.Owner] += m.Size
			rec, err := marshalFileIndexRecord(&m)
			if err != nil {
				return err
			}
			expectedIdx[string(ownerFileKey(m.Owner, m.CreatedAt, m.ID))] = rec
			broken := false
			for _, h := range m.Chunks {
				manifestRefs[h]++
				expected[h]++
				if chunks.Get([]byte(h)) == nil {
					res.DanglingRefs++
					broken = true
				}
			}
			if broken {
				brokenFiles = append(brokenFiles, AffectedFile{ID: m.ID, Name: m.Name})
			}
			return nil
		})
		if err != nil {
			return err
		}
		res.BrokenFiles = brokenFiles

		// デルタのベース参照とリージョン参照の健全性
		regions := tx.Bucket(bucketRegions)
		err = forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			if meta.Compression == compressionDelta && meta.BaseHash != "" {
				expected[meta.BaseHash]++
				if chunks.Get([]byte(meta.BaseHash)) == nil {
					res.DanglingRefs++
				}
			}
			if meta.RegionID != "" && regions.Get([]byte(meta.RegionID)) == nil {
				res.DanglingRefs++
			}
			return nil
		})
		if err != nil {
			return err
		}

		// 2) 保存 RefCount と期待値を照合(必要なら修正)
		var fixes []refFix
		err = forEachChunkMeta(tx, func(hash string, meta *ChunkMeta) error {
			exp := expected[hash]
			if meta.RefCount != exp {
				// staged チャンク(未コミット、期待0)は正常なので除外
				if meta.Staged > 0 && manifestRefs[hash] == 0 && exp == 0 {
					return nil
				}
				if exp == 0 {
					res.OrphanChunks++
				} else {
					res.RefcountMismatches++
				}
				if repair {
					fixes = append(fixes, refFix{hash: hash, want: exp})
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		// 3) 所有者使用量の照合
		var usageFixes []usageFix
		err = tx.Bucket(bucketUsers).ForEach(func(k, v []byte) error {
			owner := decodeOwnerKey(k)
			var stored int64
			if len(v) == 8 {
				stored = int64(binary.BigEndian.Uint64(v))
			}
			if stored != ownerSize[owner] {
				res.UsageMismatches++
				if repair {
					usageFixes = append(usageFixes, usageFix{owner: owner, want: ownerSize[owner]})
				}
			}
			return nil
		})
		if err != nil {
			return err
		}

		// 3.5) 所有者→ファイル索引(fileowners)の照合。
		// 孤児(実在しないファイルを指す)・欠損(実在ファイルに無い)・
		// 内容不一致(表示レコードがマニフェストと食い違う)を検出する。
		var idxOrphans [][]byte // 削除するエントリ
		var idxFixes [][2][]byte
		seenIdx := map[string]bool{}
		err = tx.Bucket(bucketFileOwners).ForEach(func(k, v []byte) error {
			ks := string(k)
			seenIdx[ks] = true
			want, ok := expectedIdx[ks]
			switch {
			case !ok:
				res.FileIndexMismatches++
				if repair {
					idxOrphans = append(idxOrphans, append([]byte(nil), k...))
				}
			case !bytes.Equal(v, want):
				res.FileIndexMismatches++
				if repair {
					idxFixes = append(idxFixes, [2][]byte{append([]byte(nil), k...), want})
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for ks, want := range expectedIdx {
			if !seenIdx[ks] {
				res.FileIndexMismatches++
				if repair {
					idxFixes = append(idxFixes, [2][]byte{[]byte(ks), want})
				}
			}
		}

		if !repair {
			return nil
		}

		// 4) 修復を適用
		for _, f := range fixes {
			meta, err := getChunkMeta(tx, f.hash)
			if err != nil || meta == nil {
				continue
			}
			if f.want == 0 {
				// 孤児: 参照ゼロ → メタと保存表現を解放する
				path, err := s.releaseRep(tx, f.hash, meta)
				if err != nil {
					return err
				}
				if path != "" {
					orphanPaths = append(orphanPaths, path)
				}
				if err := chunks.Delete([]byte(f.hash)); err != nil {
					return err
				}
				if err := dropSketches(tx, f.hash, meta.Features); err != nil {
					return err
				}
				continue
			}
			meta.RefCount = f.want
			if err := putChunkMeta(tx, f.hash, meta); err != nil {
				return err
			}
		}
		for _, f := range usageFixes {
			if err := setOwnerUsage(tx, f.owner, f.want); err != nil {
				return err
			}
		}
		idx := tx.Bucket(bucketFileOwners)
		for _, k := range idxOrphans {
			if err := idx.Delete(k); err != nil {
				return err
			}
		}
		for _, kv := range idxFixes {
			if err := idx.Put(kv[0], kv[1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.removeChunkFiles(orphanPaths)
	sort.Slice(res.BrokenFiles, func(i, j int) bool {
		return res.BrokenFiles[i].ID < res.BrokenFiles[j].ID
	})
	return res, nil
}

type refFix struct {
	hash string
	want int64
}

type usageFix struct {
	owner string
	want  int64
}
