package store

import (
	"bytes"
	"crypto/sha256"
	"io"
	"math/rand"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// repetitiveData はログのような高冗長データを作る。
func repetitiveData(size int) []byte {
	line := []byte("2026-07-12T00:00:00Z INFO request handled path=/api/v1/files status=200 duration=12ms\n")
	buf := make([]byte, 0, size)
	for len(buf) < size {
		buf = append(buf, line...)
	}
	return buf[:size]
}

func randomData(t *testing.T, size int) []byte {
	t.Helper()
	buf := make([]byte, size)
	rng := rand.New(rand.NewSource(42))
	if _, err := rng.Read(buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

func putBytes(t *testing.T, s *Store, name string, data []byte) *FileManifest {
	t.Helper()
	m, err := s.Put(name, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func getBytes(t *testing.T, s *Store, id string) []byte {
	t.Helper()
	_, r, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newTestStore(t)
	// チャンク境界をまたぐサイズ(数MiB)で往復の完全一致を確認
	data := randomData(t, 5<<20)

	m := putBytes(t, s, "blob.bin", data)
	if m.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", m.Size, len(data))
	}

	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("復元データが元データと一致しません")
	}
}

func TestEmptyFile(t *testing.T) {
	s := newTestStore(t)
	m := putBytes(t, s, "empty", nil)
	if m.Size != 0 {
		t.Fatalf("size = %d, want 0", m.Size)
	}
	if got := getBytes(t, s, m.ID); len(got) != 0 {
		t.Fatalf("復元サイズ = %d, want 0", len(got))
	}
}

func TestCompressionOnRepetitiveData(t *testing.T) {
	s := newTestStore(t)
	putBytes(t, s, "app.log", repetitiveData(8<<20))

	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.TotalRatio < 10 {
		t.Fatalf("高冗長データの総削減倍率 = %.1f, 10倍以上を期待", st.TotalRatio)
	}
}

func TestIncompressibleDataStoredRaw(t *testing.T) {
	s := newTestStore(t)
	data := randomData(t, 4 << 20)
	putBytes(t, s, "noise.bin", data)

	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	// raw フォールバックにより物理サイズは論理サイズを超えない
	if st.PhysicalBytes > st.LogicalBytes {
		t.Fatalf("physical %d > logical %d: raw フォールバックが働いていません",
			st.PhysicalBytes, st.LogicalBytes)
	}
}

func TestDeduplicationAcrossFiles(t *testing.T) {
	s := newTestStore(t)
	// 「バックアップ2世代」: 同一の大きなデータ + 末尾に少しの差分
	base := randomData(t, 6 << 20)
	gen1 := base
	gen2 := append(append([]byte{}, base...), []byte(strings.Repeat("diff", 100))...)

	putBytes(t, s, "backup-gen1", gen1)
	before, _ := s.Stats()
	putBytes(t, s, "backup-gen2", gen2)
	after, _ := s.Stats()

	added := after.PhysicalBytes - before.PhysicalBytes
	// 2世代目の物理増分は共有チャンク以外(末尾チャンク程度)に収まる
	if added > int64(len(gen2))/2 {
		t.Fatalf("2世代目の物理増分 = %d bytes, 重複排除が効いていません", added)
	}
	if after.DedupRatio < 1.5 {
		t.Fatalf("dedup_ratio = %.2f, 1.5以上を期待", after.DedupRatio)
	}
}

func TestDeleteReleasesSpace(t *testing.T) {
	s := newTestStore(t)
	m1 := putBytes(t, s, "a", randomData(t, 3<<20))
	m2 := putBytes(t, s, "b", repetitiveData(3<<20))

	if err := s.Delete(m1.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(m1.ID); err != ErrNotFound {
		t.Fatalf("削除済みファイルの Get = %v, want ErrNotFound", err)
	}
	// m2 は影響を受けない
	if got := getBytes(t, s, m2.ID); len(got) != 3<<20 {
		t.Fatal("残存ファイルが壊れました")
	}

	if err := s.Delete(m2.ID); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	if st.PhysicalBytes != 0 || st.ChunkCount != 0 {
		t.Fatalf("全削除後も physical=%d chunks=%d が残っています",
			st.PhysicalBytes, st.ChunkCount)
	}
}

func TestDeleteSharedChunksKeepsOtherFile(t *testing.T) {
	s := newTestStore(t)
	data := randomData(t, 4 << 20)
	m1 := putBytes(t, s, "copy1", data)
	m2 := putBytes(t, s, "copy2", data) // 完全に同一 → 全チャンク共有

	if err := s.Delete(m1.ID); err != nil {
		t.Fatal(err)
	}
	// 共有チャンクは m2 が参照しているので残っているはず
	got := getBytes(t, s, m2.ID)
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("共有チャンク削除により残存ファイルが破損しました")
	}
}

func TestDeleteNotFound(t *testing.T) {
	s := newTestStore(t)
	if err := s.Delete("nonexistent"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestList(t *testing.T) {
	s := newTestStore(t)
	putBytes(t, s, "one", []byte("hello"))
	putBytes(t, s, "two", []byte("world"))

	files, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
}
