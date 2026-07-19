package store

import (
	"os"
	"path/filepath"
	"testing"
)

// 健全なストアはスクラブで破損なしと報告する。
func TestScrubHealthy(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	putBytes(t, s, "a", randomData(t, 3<<20))
	putBytes(t, s, "b", repetitiveData(2<<20))

	res, err := s.Scrub()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Healthy() {
		t.Fatalf("健全なストアで破損 %d / 欠損 %d", len(res.Corrupt), len(res.Missing))
	}
	if res.ChunksChecked == 0 {
		t.Fatal("チャンクが検証されていません")
	}
}

// チャンクファイルを1バイト書き換えるとスクラブが破損を検出する。
func TestScrubDetectsCorruption(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// 乱数は圧縮されず単独チャンクファイルになる(パック格納だと壊しにくい)
	m := putBytes(t, s, "victim", randomData(t, 3<<20))

	// 保存ファイルを1つ壊す(先頭チャンクの表現ファイル)
	man, _ := s.Manifest(m.ID)
	target := ""
	filepath.WalkDir(filepath.Join(s.dir, "chunks"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && target == "" {
			target = p
		}
		return nil
	})
	if target == "" {
		t.Skip("チャンクファイルが見つからない(パック/リージョン格納)")
	}
	data, _ := os.ReadFile(target)
	if len(data) > 0 {
		data[len(data)/2] ^= 0xff // ビット反転
		os.WriteFile(target, data, 0o600)
	}
	// キャッシュを無効化して再読込させる
	for _, h := range man.Chunks {
		s.cache.remove(h)
	}

	res, err := s.Scrub()
	if err != nil {
		t.Fatal(err)
	}
	if res.Healthy() {
		t.Fatal("破損が検出されませんでした")
	}
	if len(res.AffectedFiles) == 0 {
		t.Fatal("影響ファイルが報告されていません")
	}
}

// チャンクファイルを削除するとスクラブが欠損を検出する。
func TestScrubDetectsMissing(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	m := putBytes(t, s, "victim", randomData(t, 3<<20))
	man, _ := s.Manifest(m.ID)

	target := ""
	filepath.WalkDir(filepath.Join(s.dir, "chunks"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && target == "" {
			target = p
		}
		return nil
	})
	if target == "" {
		t.Skip("チャンクファイルなし")
	}
	os.Remove(target)
	for _, h := range man.Chunks {
		s.cache.remove(h)
	}

	res, err := s.Scrub()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Missing) == 0 {
		t.Fatal("欠損が検出されませんでした")
	}
}

// _ = boltTx placeholder 除去済み

// 孤児 temp ファイルは起動時に掃除される。
func TestSweepTempFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	putBytes(t, s, "a", randomData(t, 2<<20))
	s.Close()

	// クラッシュで残った temp ファイルを模擬
	tmpPath := filepath.Join(dir, "chunks", "aa")
	os.MkdirAll(tmpPath, 0o700)
	orphan := filepath.Join(tmpPath, ".tmp-orphan123")
	os.WriteFile(orphan, []byte("garbage"), 0o600)

	// 再オープンで掃除される
	s2, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("孤児 temp ファイルが掃除されていません")
	}
}

// ローリングスクラブはカーソルを進めながら全チャンクを漏れなく一周する。
func TestScrubRolling(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	putBytes(t, s, "a", randomData(t, 5<<20)) // 複数チャンク
	st, _ := s.Stats()
	total := st.ChunkCount

	seen := 0
	rounds := 0
	for {
		res, err := s.ScrubSome(2)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Healthy() {
			t.Fatalf("健全なストアで破損検出: %+v", res)
		}
		seen += res.ChunksChecked
		rounds++
		if res.Completed {
			break
		}
		if rounds > total+5 {
			t.Fatal("ローリングスクラブが終わりません")
		}
	}
	if seen != total {
		t.Fatalf("一周の検証数 = %d, want %d", seen, total)
	}
	// 次の一周も最初から正しく始まる
	res, err := s.ScrubSome(0)
	if err != nil {
		t.Fatal(err)
	}
	if res.ChunksChecked != total || !res.Completed {
		t.Fatalf("2周目の全量スクラブ = %d/%v, want %d/true", res.ChunksChecked, res.Completed, total)
	}
}

// スクラブはキャッシュを迂回してディスクの破損を検出する(以前は読み出しで
// キャッシュに載ったチャンクの bit rot を見逃していた)。
func TestScrubDetectsCorruptionBehindCache(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	m := putBytes(t, s, "hot", randomData(t, 2<<20))
	// 読み出してキャッシュに載せる
	if got := getBytes(t, s, m.ID); len(got) != 2<<20 {
		t.Fatal("読み出し失敗")
	}
	// ディスク上の表現を全て破壊する(キャッシュには正しいデータが残る)
	corruptAllChunkFiles(t, s)
	res, err := s.Scrub()
	if err != nil {
		t.Fatal(err)
	}
	if res.Healthy() {
		t.Fatal("キャッシュ済みチャンクのディスク破損を見逃しました")
	}
}

// corruptAllChunkFiles は chunks/ 配下の全表現ファイルのビットを反転させる。
func corruptAllChunkFiles(t *testing.T, s *Store) {
	t.Helper()
	n := 0
	filepath.WalkDir(filepath.Join(s.dir, "chunks"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || len(data) == 0 {
			return nil
		}
		data[len(data)/2] ^= 0xff
		os.WriteFile(p, data, 0o600)
		n++
		return nil
	})
	if n == 0 {
		t.Skip("チャンクファイルが見つからない(パック/リージョン格納)")
	}
}
