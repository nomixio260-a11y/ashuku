package store

import "syscall"

// FreeBytes はデータディレクトリのあるファイルシステムの空きバイト数を返す
// (非特権プロセスが使える空き = Bavail ベース)。
func (s *Store) FreeBytes() (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &st); err != nil {
		return 0, err
	}
	return int64(st.Bavail) * int64(st.Bsize), nil
}
