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

// TestZombieRescueAfterRetentionDelete は「古い世代を削除しても、新しい
// 世代のデルタがベース参照で古いチャンクを生かし続ける」保持期限削除の
// 問題が、Optimize のゾンビ救出で解決されることを確認する。
func TestZombieRescueAfterRetentionDelete(t *testing.T) {
	s := newTestStore(t)

	// 10世代のチェーンを作り、最新の1世代だけ残して古い9世代を削除
	cur := randomData(t, 1<<20)
	var ids []string
	var last []byte
	for g := 0; g < 10; g++ {
		m := putBytes(t, s, "gen", cur)
		ids = append(ids, m.ID)
		last = cur
		cur = mutate(cur, (g*7919)%(len(cur)-32))
	}
	for _, id := range ids[:9] {
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}

	// 削除後もチェーンのゾンビが残っている(順方向チェーンの性質)
	before, _ := s.Stats()
	if before.ChunkCount <= 3 {
		t.Skipf("ゾンビが発生しませんでした (chunks=%d)", before.ChunkCount)
	}

	res, err := s.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()

	if res.ZombiesFreed == 0 {
		t.Fatalf("ゾンビが解放されていません (before chunks=%d after=%d)",
			before.ChunkCount, after.ChunkCount)
	}
	if after.PhysicalBytes >= before.PhysicalBytes {
		t.Fatalf("救出後に physical が減っていません: %d → %d",
			before.PhysicalBytes, after.PhysicalBytes)
	}
	// 残した最新世代は正しく復元できる
	got := getBytes(t, s, ids[9])
	if sha256.Sum256(got) != sha256.Sum256(last) {
		t.Fatal("ゾンビ救出後に残存世代の復元が壊れました")
	}
	// 最終的に残るのは「最新世代のチャンク+アンカー」程度まで縮む
	if after.ChunkCount > 4 {
		t.Fatalf("救出後もチャンクが %d 個残っています(チェーン収縮が不完全)", after.ChunkCount)
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

// インクリメンタル最適化は新着データだけを対象に、フルパスと同等の
// リージョン化・小チャンクソリッド圧縮を行う(作業キュー駆動)。
func TestOptimizeIncremental(t *testing.T) {
	s := newTestStore(t)
	data := regionCorpus(t, 20<<20)
	m := putBytes(t, s, "big", data)
	for i := 0; i < 30; i++ {
		putBytes(t, s, "small", smallVocabFile(int64(i), 40<<10))
	}
	before, _ := s.Stats()

	res, err := s.OptimizeIncremental()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()
	if res.RegionsBuilt == 0 {
		t.Fatal("インクリメンタルパスがリージョンを作っていません")
	}
	if after.PhysicalBytes >= before.PhysicalBytes {
		t.Fatalf("物理が減っていません: %d → %d", before.PhysicalBytes, after.PhysicalBytes)
	}
	// 往復
	got := getBytes(t, s, m.ID)
	if len(got) != len(data) {
		t.Fatal("復元サイズ不一致")
	}
	// 2回目は作業キューが空なので何もしない(冪等・軽量)
	res2, err := s.OptimizeIncremental()
	if err != nil {
		t.Fatal(err)
	}
	if res2.RegionsBuilt != 0 || res2.DeltaUpgraded != 0 {
		t.Fatalf("空キューでの2回目に作業が発生: %+v", res2)
	}
	// カウンタ整合
	fres, err := s.Fsck(false)
	if err != nil {
		t.Fatal(err)
	}
	if !fres.Healthy() {
		t.Fatalf("fsck が不健全: %+v", fres)
	}
}

// フルパスは作業キューを空にする(インクリメンタルとの引き継ぎ)。
func TestOptimizeFullClearsQueues(t *testing.T) {
	s := newTestStore(t)
	putBytes(t, s, "a", repetitiveData(3<<20))
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	res, err := s.OptimizeIncremental()
	if err != nil {
		t.Fatal(err)
	}
	if res.RegionsBuilt != 0 {
		t.Fatalf("フルパス後のインクリメンタルに残作業: %+v", res)
	}
}
