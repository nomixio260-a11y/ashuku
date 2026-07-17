package store

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func putOwned(t *testing.T, s *Store, owner, name string, data []byte) *FileManifest {
	t.Helper()
	m, err := s.PutWithOptions(name, bytes.NewReader(data), PutOptions{Owner: owner})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// List は所有者ごとに分離され、他の所有者のファイル数に影響されない。
// 所有者IDが別の所有者IDの接頭辞でも(0x1F 区切りで)混ざらない。
func TestListOwnerIsolationAndPrefix(t *testing.T) {
	s := newTestStore(t)
	putOwned(t, s, "u", "a", []byte("alpha"))
	putOwned(t, s, "u", "b", []byte("bravo"))
	putOwned(t, s, "u2", "c", []byte("charlie")) // "u" は "u2" の接頭辞
	putOwned(t, s, "", "d", []byte("shared"))    // 匿名所有者

	cases := map[string]int{"u": 2, "u2": 1, "": 1, "nobody": 0}
	for owner, want := range cases {
		files, err := s.List(owner)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != want {
			t.Fatalf("List(%q) = %d 件, want %d", owner, len(files), want)
		}
		for _, f := range files {
			if f.Owner != owner {
				t.Fatalf("List(%q) に所有者 %q のファイルが混入", owner, f.Owner)
			}
		}
	}
}

// 削除すると所有者索引も維持され、一覧から消える。
func TestListIndexMaintainedOnDelete(t *testing.T) {
	s := newTestStore(t)
	m1 := putOwned(t, s, "u", "a", []byte("one"))
	putOwned(t, s, "u", "b", []byte("two"))

	if err := s.Delete(m1.ID); err != nil {
		t.Fatal(err)
	}
	files, err := s.List("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Name != "b" {
		t.Fatalf("削除後の List = %d 件 (%v), want 1 件 [b]", len(files), names(files))
	}
	// 索引に孤児が残っていない(fsck が健全)
	assertFsckHealthy(t, s)
}

// 索引導入前に作られたストア(索引エントリなし)を開くと、バックフィルで
// 復旧して List が正しく動く。
func TestFileOwnerIndexBackfillOnOpen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	putOwned(t, s, "u", "a", []byte("one"))
	putOwned(t, s, "u", "b", []byte("two"))

	// 「古いストア」を再現: 索引エントリと移行フラグを消す。
	err = s.db.Update(func(tx *bolt.Tx) error {
		idx := tx.Bucket(bucketFileOwners)
		var keys [][]byte
		idx.ForEach(func(k, _ []byte) error {
			keys = append(keys, append([]byte(nil), k...))
			return nil
		})
		for _, k := range keys {
			if err := idx.Delete(k); err != nil {
				return err
			}
		}
		return tx.Bucket(bucketSettings).Delete(keyFileOwnersMigrated)
	})
	if err != nil {
		t.Fatal(err)
	}
	// 索引が空なので今 List すると 0 件(移行前の壊れた状態)
	if files, _ := s.List("u"); len(files) != 0 {
		t.Fatalf("索引除去後の List = %d 件, want 0 (前提確認)", len(files))
	}
	s.Close()

	// 再オープンでバックフィルが走る
	s2, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	files, err := s2.List("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("バックフィル後の List = %d 件, want 2", len(files))
	}
}

// fsck は索引の孤児エントリ・欠損エントリを検出し、repair で修復する。
func TestFsckRepairsFileOwnerIndex(t *testing.T) {
	s := newTestStore(t)
	m := putOwned(t, s, "u", "a", []byte("one"))

	// 索引を故意に壊す: 実在ファイルのエントリを消し、存在しないファイルの
	// 孤児エントリを1つ追加する。
	err := s.db.Update(func(tx *bolt.Tx) error {
		idx := tx.Bucket(bucketFileOwners)
		if err := idx.Delete(ownerFileKey("u", m.CreatedAt, m.ID)); err != nil { // 欠損を作る
			return err
		}
		return idx.Put(ownerFileKey("u", time.Now(), "ghost-id"), nil) // 孤児を作る
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Fsck(false)
	if err != nil {
		t.Fatal(err)
	}
	if res.FileIndexMismatches != 2 {
		t.Fatalf("FileIndexMismatches = %d, want 2 (孤児1 + 欠損1)", res.FileIndexMismatches)
	}
	if res.Healthy() {
		t.Fatal("壊れた索引で Healthy=true になっています")
	}

	// 修復
	if _, err := s.Fsck(true); err != nil {
		t.Fatal(err)
	}
	assertFsckHealthy(t, s)
	// 修復後、List が正しく実在ファイルを返し、幽霊は返さない
	files, err := s.List("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].ID != m.ID {
		t.Fatalf("修復後の List = %v, want [%s]", names(files), m.ID)
	}
}

// List は作成日時の降順(新しい順)で返る。索引キーの反転タイムスタンプに
// よる順序であり、ソート処理は無い。
func TestListNewestFirst(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 5; i++ {
		putOwned(t, s, "u", fmt.Sprintf("f%d", i), []byte{byte(i)})
		time.Sleep(2 * time.Millisecond) // CreatedAt を確実に単調増加させる
	}
	files, err := s.List("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 {
		t.Fatalf("len = %d, want 5", len(files))
	}
	for i := 0; i < len(files)-1; i++ {
		if files[i].CreatedAt.Before(files[i+1].CreatedAt) {
			t.Fatalf("順序が新しい順ではありません: %v", names(files))
		}
	}
	if files[0].Name != "f4" || files[4].Name != "f0" {
		t.Fatalf("順序が不正: %v", names(files))
	}
}

// ListPage はカーソルで全件を漏れなく重複なく辿れる。
func TestListPagePagination(t *testing.T) {
	s := newTestStore(t)
	const n = 7
	for i := 0; i < n; i++ {
		putOwned(t, s, "u", fmt.Sprintf("f%d", i), []byte{byte(i)})
		time.Sleep(2 * time.Millisecond)
	}

	seen := map[string]bool{}
	cursor := ""
	pages := 0
	for {
		files, next, err := s.ListPage("u", cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, f := range files {
			if seen[f.ID] {
				t.Fatalf("ページ間でファイル %s (%s) が重複しました", f.ID, f.Name)
			}
			seen[f.ID] = true
		}
		if next == "" {
			break
		}
		if len(files) != 3 {
			t.Fatalf("途中ページの件数 = %d, want 3", len(files))
		}
		cursor = next
	}
	if len(seen) != n {
		t.Fatalf("ページング合計 = %d 件, want %d", len(seen), n)
	}
	if pages != 3 { // 3+3+1
		t.Fatalf("ページ数 = %d, want 3", pages)
	}

	// 不正カーソルはエラー
	if _, _, err := s.ListPage("u", "zz-not-hex", 3); err == nil {
		t.Fatal("不正カーソルがエラーになりません")
	}
}

// 一覧は索引レコードだけで組み立てられ、名前・サイズが正しい
// (巨大マニフェストを読まない実装になっても内容が欠けないことの確認)。
func TestListRecordFields(t *testing.T) {
	s := newTestStore(t)
	data := []byte("hello world, this is content")
	putOwned(t, s, "u", "record.txt", data)

	files, err := s.List("u")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("len = %d, want 1", len(files))
	}
	f := files[0]
	if f.Name != "record.txt" || f.Size != int64(len(data)) || f.Owner != "u" ||
		f.ID == "" || f.CreatedAt.IsZero() {
		t.Fatalf("一覧レコードのフィールドが不正: %+v", f)
	}
}

func names(files []*FileManifest) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Name
	}
	return out
}

func assertFsckHealthy(t *testing.T, s *Store) {
	t.Helper()
	res, err := s.Fsck(false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Healthy() {
		t.Fatalf("fsck が不健全: %+v", res)
	}
}
