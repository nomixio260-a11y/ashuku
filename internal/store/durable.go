package store

// 耐久書き込みユーティリティ。
//
// temp ファイル + rename は「アトミックな置換」は保証するが、それだけでは
// クラッシュ耐久性は不十分:
//   - temp ファイルの中身が rename 前にディスクへ書かれている保証がない
//   - rename 自体(ディレクトリエントリの変更)がディスクへ書かれている保証がない
// このため、メタデータ(bbolt は commit で fsync 済み)がチャンクを指すのに
// 実データが失われる、というデータ損失が起きうる。
//
// durableWrite は (1) temp に書いて fsync、(2) rename、(3) 親ディレクトリを
// fsync、の順で「メタが指す前にファイルが確実にディスク上にある」ことを保証する。

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// durableWrite は data を path にアトミックかつ耐久的に書き込む。
func durableWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	// dir が新規作成される(例: シャード chunks/<2hex>/ の最初のチャンク)場合、
	// 末尾の fsyncDir(dir) は dir 自身のエントリを親に永続化しない。親を fsync
	// しないと、クラッシュ後に dir ごと(=配下の rename 済みファイルごと)失われる。
	// 新規作成のときだけ親も耐久化する(既存シャードでは余計な fsync をしない)。
	newDir := false
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		newDir = true
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // rename 成功後は存在しないので無害

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// (1) 中身をディスクへ
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// (2) アトミックな置換
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	// (3) ディレクトリエントリ(rename)をディスクへ
	if err := fsyncDir(dir); err != nil {
		return err
	}
	// (4) dir を新規作成した場合、その作成を親ディレクトリで耐久化する。
	if newDir {
		return fsyncDir(filepath.Dir(dir))
	}
	return nil
}

// fsyncDir はディレクトリを開いて fsync する(rename の耐久化)。
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	// 一部のプラットフォーム/FSではディレクトリ fsync が EINVAL を返すが、
	// その場合は rename が即時耐久とみなせるので無視してよい。
	if err := d.Sync(); err != nil && !isDirSyncUnsupported(err) {
		return err
	}
	return nil
}

func isDirSyncUnsupported(err error) bool {
	// 一部の FS はディレクトリ fsync に EINVAL を返す(非対応)。
	// その場合データファイル側は既に fsync 済みなので無視してよい。
	return errors.Is(err, syscall.EINVAL)
}
