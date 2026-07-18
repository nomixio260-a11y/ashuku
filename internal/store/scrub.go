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
	"bytes"
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
	// Completed はこのバッチでストア全体の1周が完了したか
	// (ローリングスクラブの進捗表示用)。
	Completed bool `json:"completed"`
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

// keyScrubCursor はローリングスクラブの進捗カーソル(最後に検証した
// チャンクハッシュ)。
var keyScrubCursor = []byte("scrub_cursor")

// Scrub は全チャンクの完全性を検証する。実行中も読み書きは可能
// (View スナップショットで走査するため、途中の変更は次回に回る)。
func (s *Store) Scrub() (*ScrubResult, error) {
	return s.ScrubSome(0)
}

// ScrubSome は最大 maxChunks 個(0 = 全チャンク)をカーソル位置から検証する
// ローリングスクラブ。進捗カーソルは永続化され、次回はその続きから検証する
// (末尾に達したら先頭へ戻る)。巨大ストアの定期スクラブを「毎回全走査で
// 数時間ディスクを飽和」から「毎回一定量ずつ、数日で一周」に変える。
//
// 検証読みはチャンク/リージョンキャッシュを見ず・入れない: キャッシュ経由
// ではディスクの bit rot を検出できず(以前の実装の検証漏れ)、また走査が
// ホットなキャッシュを洗い流すのも防ぐ。
func (s *Store) ScrubSome(maxChunks int) (*ScrubResult, error) {
	// カーソルの次から最大 maxChunks 個を収集する(検証は重いのでロック外で
	// 行う)。バケット末尾に達したらその時点で「1周完了」とし、次回は先頭から
	// 新しい周を始める(周回をバッチ内で跨がないことで進捗管理を単純にする)。
	var hashes []string
	completed := false // このバッチで1周が完了したか
	err := s.db.View(func(tx *bolt.Tx) error {
		cursor := tx.Bucket(bucketSettings).Get(keyScrubCursor)
		c := tx.Bucket(bucketChunks).Cursor()
		k, _ := c.First()
		if cursor != nil {
			k, _ = c.Seek(cursor)
			if k != nil && bytes.Equal(k, cursor) {
				k, _ = c.Next() // カーソル自身は前回検証済み
			}
		}
		for ; k != nil; k, _ = c.Next() {
			if maxChunks > 0 && len(hashes) >= maxChunks {
				return nil // 枠いっぱい(続きは次回)
			}
			hashes = append(hashes, string(k))
		}
		completed = true // 末尾まで走り切った
		return nil
	})
	if err != nil {
		return nil, err
	}

	res := &ScrubResult{}
	bad := make(map[string]bool)
	var last string
	for _, hash := range hashes {
		// readChunkVerify は伸長 + SHA-256 照合をキャッシュ迂回で行う。
		data, err := s.readChunkVerify(hash)
		if err != nil {
			// メタは存在するが読めない/検証失敗 → 破損 or 欠損
			if isMissing(err) {
				res.Missing = append(res.Missing, hash)
			} else {
				res.Corrupt = append(res.Corrupt, hash)
			}
			bad[hash] = true
			last = hash
			continue
		}
		res.ChunksChecked++
		res.BytesChecked += int64(len(data))
		last = hash
	}
	res.Completed = completed

	// 進捗カーソルを保存(1周完了ならリセットして次回は先頭から)
	err = s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketSettings)
		if completed {
			return b.Delete(keyScrubCursor)
		}
		if last == "" {
			return nil
		}
		return b.Put(keyScrubCursor, []byte(last))
	})
	if err != nil {
		return nil, err
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
