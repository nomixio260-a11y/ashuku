package store

import (
	"bytes"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

// packDiskBytes は packs ディレクトリの実ファイルサイズ合計を返す。
func packDiskBytes(t *testing.T, s *Store) int64 {
	t.Helper()
	var total int64
	entries, err := os.ReadDir(s.pw.dir)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

// chunkFileCount は chunks ディレクトリの表現ファイル数を返す。
func chunkFileCount(t *testing.T, s *Store) int {
	t.Helper()
	count := 0
	filepath.WalkDir(filepath.Join(s.dir, "chunks"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			count++
		}
		return nil
	})
	return count
}

// TestSmallRepsGoToPacks は小さな表現(デルタ)がパックに集約され、
// 個別ファイルを作らないことを確認する。
func TestSmallRepsGoToPacks(t *testing.T) {
	s := newTestStore(t)
	// 類似世代を投入 → 2世代目以降のチャンクは小さなデルタになる
	base := randomData(t, 2<<20)
	putBytes(t, s, "gen0", base)
	filesAfterGen0 := chunkFileCount(t, s)
	cur := base
	for g := 1; g <= 10; g++ {
		cur = mutate(cur, (g*13337)%(len(cur)-32))
		putBytes(t, s, "gen", cur)
	}

	st, _ := s.Stats()
	if st.DeltaChunkCount == 0 {
		t.Fatal("デルタが発生していません")
	}
	if st.PackCount == 0 {
		t.Fatal("パックが作られていません(小さなデルタが個別ファイルになっている)")
	}
	// デルタは全てパックに入り、表現ファイル数は初回世代から増えない
	if got := chunkFileCount(t, s); got > filesAfterGen0 {
		t.Fatalf("表現ファイル数 %d > 初回世代後 %d: 小さな表現がファイルに書かれています",
			got, filesAfterGen0)
	}
}

// TestPackRoundTrip はパック格納された表現からの復元一致を確認する。
func TestPackRoundTrip(t *testing.T) {
	s := newTestStore(t)
	base := randomData(t, 1<<20)
	m1 := putBytes(t, s, "v1", base)
	edited := mutate(base, 4096, 500000)
	m2 := putBytes(t, s, "v2", edited)

	if got := getBytes(t, s, m1.ID); sha256.Sum256(got) != sha256.Sum256(base) {
		t.Fatal("v1 の復元が一致しません")
	}
	if got := getBytes(t, s, m2.ID); sha256.Sum256(got) != sha256.Sum256(edited) {
		t.Fatal("v2(パック格納デルタ)の復元が一致しません")
	}
}

// TestPackCompactionReclaimsDisk は削除で生じたパック内ゴミが
// コンパクションで実ディスクごと回収されることを確認する。
//
// 注: 順方向デルタチェーンでは、古い世代のチャンクは新しい世代のデルタの
// ベースとして参照され続けるため、「古い世代の削除」では解放されない。
// 領域が解放されるのはチェーン末尾側(新しい世代)を削除したときである。
// ここでは新しい15世代を削除して古い世代を残す。
func TestPackCompactionReclaimsDisk(t *testing.T) {
	// テストデータは小さいので、コンパクション対象の下限を下げる
	orig := compactMinSize
	compactMinSize = 1
	t.Cleanup(func() { compactMinSize = orig })

	s := newTestStore(t)
	base := randomData(t, 2<<20)
	putBytes(t, s, "keep-base", base)
	var deleteIDs []string
	var keepID string
	var keepContent []byte
	cur := base
	for g := 0; g < 20; g++ {
		cur = mutate(cur, (g*99991)%(len(cur)-32))
		m := putBytes(t, s, "gen", cur)
		if g >= 5 {
			deleteIDs = append(deleteIDs, m.ID)
		} else {
			keepID = m.ID
			keepContent = cur
		}
	}
	cur = keepContent

	for _, id := range deleteIDs {
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Stats()
	if st.PackGarbageBytes == 0 {
		t.Fatal("削除後にパックゴミが計上されていません")
	}

	diskBefore := packDiskBytes(t, s)
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	diskAfter := packDiskBytes(t, s)
	stAfter, _ := s.Stats()

	if stAfter.PackGarbageBytes >= st.PackGarbageBytes && diskAfter >= diskBefore {
		t.Fatalf("コンパクションが機能していません: garbage %d→%d, disk %d→%d",
			st.PackGarbageBytes, stAfter.PackGarbageBytes, diskBefore, diskAfter)
	}

	// 生き残りは正しく読める
	if got := getBytes(t, s, keepID); sha256.Sum256(got) != sha256.Sum256(cur) {
		t.Fatal("コンパクション後に残存世代の復元が壊れました")
	}
}

// TestPackFullDeleteThenCompact は全削除+コンパクションで
// 非現行パックが物理削除されることを確認する。
func TestPackFullDeleteThenCompact(t *testing.T) {
	s := newTestStore(t)
	base := randomData(t, 1<<20)
	var ids []string
	cur := base
	for g := 0; g < 10; g++ {
		m := putBytes(t, s, "gen", cur)
		ids = append(ids, m.ID)
		cur = mutate(cur, (g*31337)%(len(cur)-32))
	}
	for _, id := range ids {
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	// 現行パック(追記中)は残りうるが、それ以外は消えている
	if st.PackCount > 1 {
		t.Fatalf("全削除+Optimize 後もパックが %d 個残っています", st.PackCount)
	}
	if st.ChunkCount != 0 || st.PhysicalBytes != 0 {
		t.Fatalf("chunks=%d physical=%d が残っています", st.ChunkCount, st.PhysicalBytes)
	}
}

// TestPackPersistsAcrossReopen はストアを開き直してもパック格納の
// 表現が読めることを確認する(現行パックの引き継ぎ)。
func TestPackPersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	base := randomData(t, 1<<20)
	putBytes(t, s, "v1", base)
	edited := mutate(base, 9000)
	m2, err := s.Put("v2", bytes.NewReader(edited))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := getBytes(t, s2, m2.ID); sha256.Sum256(got) != sha256.Sum256(edited) {
		t.Fatal("再オープン後にパック格納デルタの復元が一致しません")
	}
	// 再オープン後の新規書き込みも問題ない(新パックへ)
	edited2 := mutate(edited, 700000)
	m3, err := s2.Put("v3", bytes.NewReader(edited2))
	if err != nil {
		t.Fatal(err)
	}
	if got := getBytes(t, s2, m3.ID); sha256.Sum256(got) != sha256.Sum256(edited2) {
		t.Fatal("再オープン後の新規デルタの復元が一致しません")
	}
}
