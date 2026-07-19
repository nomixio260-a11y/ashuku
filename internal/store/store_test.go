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
	s, err := Open(t.TempDir(), Config{})
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
	return randomDataSeed(t, size, 42)
}

func randomDataSeed(t *testing.T, size int, seed int64) []byte {
	t.Helper()
	buf := make([]byte, size)
	rng := rand.New(rand.NewSource(seed))
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	s := newTestStore(t)
	data := randomData(t, 4<<20)
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
	t.Parallel()
	s := newTestStore(t)
	// 「バックアップ2世代」: 同一の大きなデータ + 末尾に少しの差分
	base := randomData(t, 6<<20)
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
	t.Parallel()
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
	t.Parallel()
	s := newTestStore(t)
	data := randomData(t, 4<<20)
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
	t.Parallel()
	s := newTestStore(t)
	if err := s.Delete("nonexistent"); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// 小ファイルはチャンク+マニフェストが単一トランザクションで確定される。
// クォータ超過時は何もコミットされない(チャンクの孤児が残らない)。
func TestSmallFileQuotaAtomic(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	data := randomData(t, 64<<10)
	_, err := s.PutWithOptions("over", bytes.NewReader(data),
		PutOptions{Owner: "u", Quota: 1024})
	if err != ErrQuotaExceeded {
		t.Fatalf("err = %v, want ErrQuotaExceeded", err)
	}
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.ChunkCount != 0 || st.PhysicalBytes != 0 || st.FileCount != 0 {
		t.Fatalf("クォータ超過後に chunks=%d physical=%d files=%d が残っています",
			st.ChunkCount, st.PhysicalBytes, st.FileCount)
	}
	res, err := s.Fsck(false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Healthy() {
		t.Fatalf("fsck が不健全: %+v", res)
	}
}

// 同一チャンクが1つの小ファイル内に複数回現れても正しく確定される
// (単一トランザクション内の自己重複排除)。
func TestSmallFileDupChunksWithinFile(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// 同じ 300KiB ブロックを2回繰り返す(min チャンク 256KiB 超なので
	// 同一境界で同一チャンクが出うる)
	block := randomData(t, 300<<10)
	data := append(append([]byte(nil), block...), block...)
	m := putBytes(t, s, "dup-in-file", data)
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("自己重複ファイルの復元が一致しません")
	}
	res, err := s.Fsck(false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Healthy() {
		t.Fatalf("fsck が不健全: %+v", res)
	}
	if err := s.Delete(m.ID); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	if st.ChunkCount != 0 || st.PhysicalBytes != 0 {
		t.Fatalf("削除後に chunks=%d physical=%d が残っています", st.ChunkCount, st.PhysicalBytes)
	}
}

// 小ファイル(バッファ内)と大ファイル(ストリーミング)の境界をまたいでも
// 双方が正しく往復する。
func TestBufferBoundaryFiles(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// バッファ上限 = chunkSize*4 = 4MiB(デフォルト)前後のサイズ群
	for _, size := range []int{1 << 10, 256 << 10, 1 << 20, 4 << 20, (4 << 20) + 1, 6 << 20, 9 << 20} {
		data := randomDataSeed(t, size, int64(size))
		m := putBytes(t, s, "bound", data)
		got := getBytes(t, s, m.ID)
		if sha256.Sum256(got) != sha256.Sum256(data) {
			t.Fatalf("size=%d の復元が一致しません", size)
		}
	}
	res, err := s.Fsck(false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Healthy() {
		t.Fatalf("fsck が不健全: %+v", res)
	}
}

func TestList(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	putBytes(t, s, "one", []byte("hello"))
	putBytes(t, s, "two", []byte("world"))

	files, err := s.List("")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(files))
	}
}

// MinFreeBytes を極端に大きくすると、取り込みと Optimize がディスク保護で
// 拒否される(バックグラウンド処理が満杯を招かない)。
func TestDiskGuardBlocksWrites(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir(), Config{MinFreeBytes: 1 << 60})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.Put("x", bytes.NewReader(randomData(t, 64<<10))); err != ErrDiskFull {
		t.Fatalf("Put err = %v, want ErrDiskFull", err)
	}
	if _, err := s.Optimize(); err != ErrDiskFull {
		t.Fatalf("Optimize err = %v, want ErrDiskFull", err)
	}
}

// 維持カウンタ(Stats O(1)化)が、あらゆる経路(取り込み・重複・削除・
// リージョン化・repack・パック回収)の後も全走査と一致し続ける。
func TestCountersStayConsistent(t *testing.T) {
	if testing.Short() {
		t.Skip("重い E2E テスト; -short ではスキップ(CI の全テストジョブで実行)")
	}
	t.Parallel()
	s := newTestStore(t)
	// 多様なワークロード
	m1 := putBytes(t, s, "text", repetitiveData(6<<20))
	putBytes(t, s, "rand", randomData(t, 3<<20))
	putBytes(t, s, "dup", repetitiveData(6<<20)) // 全チャンク重複
	var smalls []*FileManifest
	for i := 0; i < 20; i++ {
		smalls = append(smalls, putBytes(t, s, "small", randomDataSeed(t, 30<<10, int64(1000+i))))
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(m1.ID); err != nil {
		t.Fatal(err)
	}
	for _, m := range smalls[:10] {
		if err := s.Delete(m.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Optimize(); err != nil { // リージョン解体・パック回収も踏む
		t.Fatal(err)
	}
	res, err := s.Fsck(false)
	if err != nil {
		t.Fatal(err)
	}
	if res.CounterMismatches != 0 {
		t.Fatalf("維持カウンタが全走査と食い違っています: %d フィールド", res.CounterMismatches)
	}
	if !res.Healthy() {
		t.Fatalf("fsck が不健全: %+v", res)
	}
}
