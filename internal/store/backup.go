package store

// メタデータのバックアップ。
//
// チャンク実データはコンテンツアドレスのファイル群として冗長に分散するが、
// メタデータ(bbolt の meta.db)は単一ファイルで、これが壊れると全チャンクが
// 無事でも全マニフェスト・索引・参照カウントを失い、事実上の全損になる。
// これが唯一の単一障害点(SPOF)。
//
// bbolt は読み取りトランザクション中に一貫したスナップショットを別ファイルへ
// 書き出せる(MVCC のため書き込みと並行しても整合性が保たれる)。これを使い、
// 稼働中でも安全にメタデータのホットバックアップを取る。バックアップは
// 別ディレクトリ(理想的には別ボリューム/オフサイト)にコピーしておけば、
// meta.db 破損時にそこから復旧できる。

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

// BackupMeta はメタデータの一貫したスナップショットを path に書き出す
// (temp+rename で原子的)。稼働中に呼んでも安全。
func (s *Store) BackupMeta(path string) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return 0, err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.db.View(func(tx *bolt.Tx) error {
		var werr error
		n, werr = tx.WriteTo(f)
		return werr
	})
	if err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return 0, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	if err := fsyncDir(filepath.Dir(path)); err != nil {
		return 0, err
	}
	return n, nil
}

// BackupMetaRotating は data/meta-backups/ にタイムスタンプ付きスナップショットを
// 作り、最新 keep 個だけ残す(古いものは削除)。at は現在時刻。
func (s *Store) BackupMetaRotating(at time.Time, keep int) (string, int64, error) {
	dir := filepath.Join(s.dir, "meta-backups")
	name := fmt.Sprintf("meta-%s.db", at.UTC().Format("20060102-150405"))
	path := filepath.Join(dir, name)
	n, err := s.BackupMeta(path)
	if err != nil {
		return "", 0, err
	}
	// ローテーション: meta-*.db を名前(=時刻)順に並べ、古いものを削除
	if keep > 0 {
		entries, _ := os.ReadDir(dir)
		var backups []string
		for _, e := range entries {
			nm := e.Name()
			if !e.IsDir() && len(nm) > 5 && nm[:5] == "meta-" && filepath.Ext(nm) == ".db" {
				backups = append(backups, nm)
			}
		}
		sort.Strings(backups) // タイムスタンプ名なので辞書順=時刻順
		for len(backups) > keep {
			os.Remove(filepath.Join(dir, backups[0]))
			backups = backups[1:]
		}
	}
	return path, n, nil
}
