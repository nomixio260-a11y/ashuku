package store

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

// driftedStore は小さい深さ上限で多世代を投入し、rebase により
// ドリフトした星形チェーンを持つストアを作る。
func driftedStore(t *testing.T, gens int) (*Store, []string, [][]byte) {
	t.Helper()
	s, err := Open(t.TempDir(), Config{MaxDeltaDepth: 3})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	cur := randomData(t, 1<<20)
	var ids []string
	var contents [][]byte
	for g := 0; g < gens; g++ {
		m, err := s.Put("gen", bytes.NewReader(cur))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, m.ID)
		contents = append(contents, cur)
		cur = mutate(cur, (g*7919)%(len(cur)-32), (g*104729)%(len(cur)-32))
	}
	return s, ids, contents
}

func TestOptimizeReducesPhysicalAndPreservesData(t *testing.T) {
	s, ids, contents := driftedStore(t, 30)

	before, _ := s.Stats()
	res, err := s.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()

	if res.ChunksRepacked == 0 {
		t.Fatal("repack されたチャンクがありません(星形が検出されていない)")
	}
	if after.PhysicalBytes >= before.PhysicalBytes {
		t.Fatalf("physical %d → %d: Optimize で容量が減っていません",
			before.PhysicalBytes, after.PhysicalBytes)
	}

	// 全世代がビット単位で復元できる(identity は不変)
	for i, id := range ids {
		got := getBytes(t, s, id)
		if sha256.Sum256(got) != sha256.Sum256(contents[i]) {
			t.Fatalf("Optimize 後、世代 %d の復元が一致しません", i)
		}
	}

	// 論理サイズは不変
	if after.LogicalBytes != before.LogicalBytes {
		t.Fatalf("logical %d → %d: Optimize が論理サイズを変えました",
			before.LogicalBytes, after.LogicalBytes)
	}
}

func TestOptimizeIsIdempotent(t *testing.T) {
	s, _, _ := driftedStore(t, 20)

	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	mid, _ := s.Stats()
	res2, err := s.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()

	// 2回目はほぼ何もすることがなく、容量が増えることはない
	if after.PhysicalBytes > mid.PhysicalBytes {
		t.Fatalf("2回目の Optimize で physical が増えました: %d → %d",
			mid.PhysicalBytes, after.PhysicalBytes)
	}
	if res2.BytesAfter > res2.BytesBefore {
		t.Fatalf("repack が容量を増やしました: %d → %d", res2.BytesBefore, res2.BytesAfter)
	}
}

func TestOptimizeThenDeleteReleasesEverything(t *testing.T) {
	s, ids, _ := driftedStore(t, 20)

	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Stats()
	if st.ChunkCount != 0 || st.PhysicalBytes != 0 {
		t.Fatalf("全削除後に chunks=%d physical=%d が残っています(repack 後の参照カウント不整合)",
			st.ChunkCount, st.PhysicalBytes)
	}
}

func TestOptimizeOnEmptyStore(t *testing.T) {
	s := newTestStore(t)
	res, err := s.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	if res.StarsScanned != 0 || res.ChunksRepacked != 0 {
		t.Fatalf("空ストアで repack が発生: %+v", res)
	}
}
