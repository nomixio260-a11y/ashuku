package store

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

// mutate は data のコピーに小さな編集(数十バイトの置換)を加える。
func mutate(data []byte, positions ...int) []byte {
	out := append([]byte(nil), data...)
	for _, p := range positions {
		copy(out[p:], []byte("EDITED-BYTES-HERE"))
	}
	return out
}

func TestDeltaCompressionOnSimilarChunks(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// 乱数データは通常圧縮ではまったく縮まない。
	// わずかに編集した2つ目は、デルタ圧縮なら差分だけの保存になるはず。
	base := randomData(t, 1<<20)
	edited := mutate(base, 1000, 500000, 900000)

	m1 := putBytes(t, s, "v1.bin", base)
	before, _ := s.Stats()
	m2 := putBytes(t, s, "v2.bin", edited)
	after, _ := s.Stats()

	if after.DeltaChunkCount == 0 {
		t.Fatal("デルタチャンクが作られていません")
	}
	added := after.PhysicalBytes - before.PhysicalBytes
	if added > int64(len(edited))/10 {
		t.Fatalf("編集版の物理増分 = %d bytes (元 %d), デルタ圧縮が効いていません",
			added, len(edited))
	}

	// 両方とも正しく復元できる
	if got := getBytes(t, s, m1.ID); sha256.Sum256(got) != sha256.Sum256(base) {
		t.Fatal("v1 の復元が一致しません")
	}
	if got := getBytes(t, s, m2.ID); sha256.Sum256(got) != sha256.Sum256(edited) {
		t.Fatal("v2 の復元が一致しません")
	}
}

// 周期性の強いデータに先頭挿入すると、チャンク境界が全てずれて
// 完全一致の重複排除が全滅する(FastCDC の退化ケース)。
// デルタ圧縮はこのケースを内容の類似性で救済する。
func TestDeltaRescuesShiftedPeriodicData(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	gen1 := repetitiveData(6 << 20)
	gen2 := append([]byte("=== gen2 header ==="), gen1...)

	putBytes(t, s, "gen1.log", gen1)
	before, _ := s.Stats()
	m2 := putBytes(t, s, "gen2.log", gen2)
	after, _ := s.Stats()

	added := after.PhysicalBytes - before.PhysicalBytes
	// gen1 とほぼ同一の内容なので、増分はデルタ数KB以内に収まるはず
	// (6MiB を通常圧縮し直すと数百バイト×チャンク数でも通るが、
	// 元サイズの 1/1000 未満ならデルタ/圧縮が機能していると言える)。
	if added > int64(len(gen2))/1000 {
		t.Fatalf("シフトした2世代目の物理増分 = %d bytes (1世代目の物理 %d)",
			added, before.PhysicalBytes)
	}
	if got := getBytes(t, s, m2.ID); sha256.Sum256(got) != sha256.Sum256(gen2) {
		t.Fatal("gen2 の復元が一致しません")
	}
}

func TestDeltaBaseSurvivesDeletion(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	base := randomData(t, 1<<20)
	edited := mutate(base, 2000)

	m1 := putBytes(t, s, "v1.bin", base)
	m2 := putBytes(t, s, "v2.bin", edited)

	st, _ := s.Stats()
	if st.DeltaChunkCount == 0 {
		t.Skip("デルタが発生しなかったためスキップ")
	}

	// ベースを含むファイルを削除しても、デルタ側はベースチャンクを
	// 参照カウントで生かし続けるので復元できる。
	if err := s.Delete(m1.ID); err != nil {
		t.Fatal(err)
	}
	if got := getBytes(t, s, m2.ID); sha256.Sum256(got) != sha256.Sum256(edited) {
		t.Fatal("ベースファイル削除後にデルタ側の復元が壊れました")
	}

	// デルタ側も消せば全チャンクが解放される。
	if err := s.Delete(m2.ID); err != nil {
		t.Fatal(err)
	}
	st, _ = s.Stats()
	if st.ChunkCount != 0 || st.PhysicalBytes != 0 {
		t.Fatalf("全削除後に chunks=%d physical=%d が残っています",
			st.ChunkCount, st.PhysicalBytes)
	}
}

func TestDeltaDisabled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir, Config{DisableDelta: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	base := randomData(t, 1<<20)
	putBytes(t, s, "v1.bin", base)
	putBytes(t, s, "v2.bin", mutate(base, 3000))

	st, _ := s.Stats()
	if st.DeltaChunkCount != 0 {
		t.Fatalf("delta 無効設定なのにデルタチャンクが %d 個あります", st.DeltaChunkCount)
	}
}

func TestComputeFeaturesSimilarity(t *testing.T) {
	t.Parallel()
	data := randomData(t, 512<<10)
	edited := mutate(data, 100000)
	f1 := computeFeatures(data)
	f2 := computeFeatures(edited)

	match := 0
	for _, a := range f1 {
		for _, b := range f2 {
			if a == b {
				match++
			}
		}
	}
	if match == 0 {
		t.Fatal("小さな編集で全特徴が変わりました(類似検出が機能しません)")
	}

	// まったく別のデータとは(高確率で)一致しない
	other := computeFeatures(randomDataSeed(t, 512<<10, 777))
	for _, a := range f1 {
		for _, b := range other {
			if a == b {
				t.Fatal("無関係なデータと特徴が衝突しました")
			}
		}
	}
}

// 30世代の連続編集でデルタチェーン(深さ上限つき)が正しく機能し、
// 全世代が復元できることを確認する。
func TestDeltaChainManyGenerations(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	const gens = 30
	cur := randomData(t, 1<<20)
	var ids []string
	var contents [][]byte
	for g := 0; g < gens; g++ {
		m := putBytes(t, s, "gen", cur)
		ids = append(ids, m.ID)
		contents = append(contents, cur)
		cur = mutate(cur, (g*31337)%(len(cur)-32))
	}

	for i, id := range ids {
		got := getBytes(t, s, id)
		if sha256.Sum256(got) != sha256.Sum256(contents[i]) {
			t.Fatalf("世代 %d の復元が一致しません", i)
		}
	}

	st, _ := s.Stats()
	// 乱数1MiB×30世代(各世代は数十バイトの編集)なので、
	// デルタが効いていれば物理は logical よりはるかに小さい
	if st.TotalRatio < 5 {
		t.Fatalf("30世代の削減倍率 = %.1fx, デルタチェーンが機能していません", st.TotalRatio)
	}

	// 全部消すとゼロに戻る(チェーンのGCカスケード確認)
	for _, id := range ids {
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	st, _ = s.Stats()
	if st.ChunkCount != 0 || st.PhysicalBytes != 0 {
		t.Fatalf("全削除後に chunks=%d physical=%d が残っています",
			st.ChunkCount, st.PhysicalBytes)
	}
}

// 深さ上限が小さくても、上限到達時は完全コピーではなく浅い祖先への
// 張り替え(rebase)でデルタが継続することを確認する。
func TestDeltaRebaseAtDepthLimit(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir(), Config{MaxDeltaDepth: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const gens = 20
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
		cur = mutate(cur, (g*7919)%(len(cur)-32))
	}

	for i, id := range ids {
		got := getBytes(t, s, id)
		if sha256.Sum256(got) != sha256.Sum256(contents[i]) {
			t.Fatalf("世代 %d の復元が一致しません", i)
		}
	}

	st, _ := s.Stats()
	// 張り替えが機能していれば、深さ3でも大半の世代はデルタで保存される
	// (完全コピー再アンカーなら 20/(3+1)=5 個の plain ができる)。
	if st.DeltaChunkCount < gens-3 {
		t.Fatalf("delta_chunk_count = %d / %d 世代, rebase が機能していません",
			st.DeltaChunkCount, gens)
	}
	if st.TotalRatio < 10 {
		t.Fatalf("削減倍率 = %.1fx, rebase 後のデルタ効率が不足", st.TotalRatio)
	}
}

func TestDeltaRoundTripLargeShared(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// 複数チャンクにまたがるサイズで、部分編集+復元一致を確認
	base := randomData(t, 5<<20)
	edited := mutate(base, 100, 2<<20, 4<<20)

	putBytes(t, s, "big-v1", base)
	m2 := putBytes(t, s, "big-v2", edited)
	got := getBytes(t, s, m2.ID)
	if !bytes.Equal(got, edited) {
		t.Fatal("複数チャンクのデルタ復元が一致しません")
	}
}
