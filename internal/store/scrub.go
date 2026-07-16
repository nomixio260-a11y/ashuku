package store

// スクラブ(データ完全性検証)。
//
// ストレージは時間とともにサイレントな破損(bit rot・部分書き込み・
// ハードウェア障害)を起こしうる。ashuku は全チャンクを内容ハッシュで
// アドレスするため、保存データを伸長して SHA-256 を照合すれば破損を
// 確実に検出できる。Scrub は全チャンクを走査してこの検証を行い、
// 破損チャンクと、どのファイルが影響を受けるかを報告する。
//
// 定期実行(cron や -scrub-every)で bit rot を早期発見でき、バックアップ
// からの復旧判断に使える。読み出し経路も毎回ハッシュ検証するので
// 「壊れたデータを返す」ことはないが、Scrub は使われていないチャンクの
// 破損も先回りで見つけられる点が価値。

import (
	"encoding/json"
	"errors"
	"io/fs"
	"sort"

	bolt "go.etcd.io/bbolt"
)

// ScrubResult はスクラブの結果。
type ScrubResult struct {
	// ChunksChecked は検証したチャンク数。
	ChunksChecked int `json:"chunks_checked"`
	// BytesChecked は検証した論理バイト数(生データ換算)。
	BytesChecked int64 `json:"bytes_checked"`
	// Corrupt は破損(伸長失敗またはハッシュ不一致)したチャンクのハッシュ列。
	Corrupt []string `json:"corrupt,omitempty"`
	// Missing は保存ファイルが失われたチャンクのハッシュ列。
	Missing []string `json:"missing,omitempty"`
	// AffectedFiles は破損/欠損チャンクを参照するファイルの (id, name)。
	AffectedFiles []AffectedFile `json:"affected_files,omitempty"`
}

// AffectedFile は破損の影響を受けるファイル。
type AffectedFile struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Healthy は破損・欠損がなかったかを返す。
func (r *ScrubResult) Healthy() bool {
	return len(r.Corrupt) == 0 && len(r.Missing) == 0
}

// Scrub は全チャンクの完全性を検証する。実行中も読み書きは可能
// (View スナップショットで走査するため、途中の変更は次回に回る)。
func (s *Store) Scrub() (*ScrubResult, error) {
	// 走査対象のハッシュを収集(検証は重いのでロック外で行う)
	var hashes []string
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketChunks).ForEach(func(k, _ []byte) error {
			hashes = append(hashes, string(k))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}

	res := &ScrubResult{}
	bad := make(map[string]bool)
	for _, hash := range hashes {
		// readChunk は伸長 + SHA-256 照合を行うので、成功=完全性OK。
		data, err := s.readChunk(hash)
		if err != nil {
			// メタは存在するが読めない/検証失敗 → 破損 or 欠損
			if isMissing(err) {
				res.Missing = append(res.Missing, hash)
			} else {
				res.Corrupt = append(res.Corrupt, hash)
			}
			bad[hash] = true
			continue
		}
		res.ChunksChecked++
		res.BytesChecked += int64(len(data))
	}

	if len(bad) > 0 {
		res.AffectedFiles = s.filesReferencing(bad)
	}
	sort.Strings(res.Corrupt)
	sort.Strings(res.Missing)
	return res, nil
}

// filesReferencing は bad のいずれかのチャンクを参照するファイルを返す。
func (s *Store) filesReferencing(bad map[string]bool) []AffectedFile {
	var affected []AffectedFile
	s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketFiles).ForEach(func(_, v []byte) error {
			var m FileManifest
			if err := json.Unmarshal(v, &m); err != nil {
				return nil
			}
			for _, h := range m.Chunks {
				if bad[h] {
					affected = append(affected, AffectedFile{ID: m.ID, Name: m.Name})
					break
				}
			}
			return nil
		})
	})
	return affected
}

// isMissing は「保存ファイルが存在しない」系のエラーかを判定する。
func isMissing(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}
