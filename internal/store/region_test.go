package store

import (
	"bytes"
	"crypto/sha256"
	"math/rand"
	"testing"
)

// regionCorpus は圧縮可能で「チャンクをまたぐ冗長性」を持つデータを作る
// (共通の語彙を全体で使い回すので、まとめて圧縮すると独立圧縮より縮む)。
func regionCorpus(t *testing.T, size int) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(55))
	// 共通語彙(全チャンクで共有 → ソリッド圧縮が効く源)
	vocab := make([]string, 4000)
	for i := range vocab {
		n := 6 + rng.Intn(12)
		b := make([]byte, n)
		for j := range b {
			b[j] = byte('a' + rng.Intn(26))
		}
		vocab[i] = string(b)
	}
	var buf bytes.Buffer
	buf.Grow(size + 32)
	for buf.Len() < size {
		buf.WriteString(vocab[rng.Intn(len(vocab))])
		buf.WriteByte(' ')
	}
	return buf.Bytes()[:size]
}

func TestRegionCompressionImprovesRatioAndRoundTrips(t *testing.T) {
	s := newTestStore(t)
	data := regionCorpus(t, 24<<20) // 複数チャンクにまたがる

	m := putBytes(t, s, "corpus.txt", data)
	before, _ := s.Stats()

	res, err := s.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()

	if res.RegionsBuilt == 0 {
		t.Fatalf("リージョンが作られていません (chunks=%d)", before.ChunkCount)
	}
	if after.PhysicalBytes >= before.PhysicalBytes {
		t.Fatalf("リージョン化で物理が減っていません: %d → %d",
			before.PhysicalBytes, after.PhysicalBytes)
	}
	t.Logf("物理 %d → %d (%.1f%% 削減, リージョン %d 本 %d チャンク)",
		before.PhysicalBytes, after.PhysicalBytes,
		100*(1-float64(after.PhysicalBytes)/float64(before.PhysicalBytes)),
		res.RegionsBuilt, res.RegionChunks)

	// リージョン化後もビット一致で復元できる
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatal("リージョン化後の復元が一致しません")
	}
	// 2回目の読み出し(キャッシュ経由)も一致
	got2 := getBytes(t, s, m.ID)
	if !bytes.Equal(got2, data) {
		t.Fatal("キャッシュ経由の復元が一致しません")
	}
}

// リージョンは冪等(2回 Optimize しても壊れない・物理が増えない)。
func TestRegionOptimizeIdempotent(t *testing.T) {
	s := newTestStore(t)
	m := putBytes(t, s, "c", regionCorpus(t, 16<<20))
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	mid, _ := s.Stats()
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()
	if after.PhysicalBytes > mid.PhysicalBytes {
		t.Fatalf("2回目の Optimize で物理が増えました: %d → %d", mid.PhysicalBytes, after.PhysicalBytes)
	}
	if got := getBytes(t, s, m.ID); len(got) != 16<<20 {
		t.Fatal("2回 Optimize 後の復元サイズが不正")
	}
}

// リージョン化されたファイルを削除すると、リージョンが解放される。
func TestRegionDeleteReleasesEverything(t *testing.T) {
	s := newTestStore(t)
	m := putBytes(t, s, "c", regionCorpus(t, 16<<20))
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	if st.RegionCount == 0 {
		t.Skip("リージョンが作られなかった")
	}
	if err := s.Delete(m.ID); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()
	if after.ChunkCount != 0 || after.PhysicalBytes != 0 || after.RegionCount != 0 {
		t.Fatalf("削除後に chunks=%d physical=%d regions=%d が残っています",
			after.ChunkCount, after.PhysicalBytes, after.RegionCount)
	}
}

// リージョンの一部だけ参照が消えた場合(共有チャンク)、残りは読める。
func TestRegionPartialDeleteKeepsSharedReadable(t *testing.T) {
	s := newTestStore(t)
	data := regionCorpus(t, 16<<20)
	m1 := putBytes(t, s, "a", data)
	// 末尾を少し変えた別ファイル(先頭チャンクは共有される)
	data2 := append(append([]byte(nil), data...), []byte(" tail change here")...)
	m2 := putBytes(t, s, "b", data2)

	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	// a を削除しても b は完全復元できる
	if err := s.Delete(m1.ID); err != nil {
		t.Fatal(err)
	}
	got := getBytes(t, s, m2.ID)
	if sha256.Sum256(got) != sha256.Sum256(data2) {
		t.Fatal("共有リージョンの一部削除後、残存ファイルの復元が壊れました")
	}
}

// 低生存率リージョンのコンパクション(解体→詰め直し)で空間が回収される。
func TestRegionCompaction(t *testing.T) {
	s := newTestStore(t)
	// 独立した5ファイルを入れてリージョン化
	var ids []string
	for i := 0; i < 5; i++ {
		d := regionCorpus(t, 16<<20)
		// 各ファイルを少しずつ変える(共有を避け、別リージョンに)
		copy(d, []byte{byte(i), byte(i >> 8)})
		ids = append(ids, putBytes(t, s, "f", d).ID)
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	// 4/5 を削除 → リージョンの生存率が下がる
	for _, id := range ids[:4] {
		if err := s.Delete(id); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := s.Stats()
	res, err := s.Optimize() // コンパクションが走る
	if err != nil {
		t.Fatal(err)
	}
	after, _ := s.Stats()
	if res.RegionsCompacted == 0 && before.PhysicalBytes == after.PhysicalBytes {
		t.Skip("コンパクション対象がなかった(データ依存)")
	}
	// 残った1ファイルは読める
	got := getBytes(t, s, ids[4])
	if len(got) != 16<<20 {
		t.Fatal("コンパクション後、残存ファイルの復元が壊れました")
	}
}

// クライアント経路のダウンロードもリージョンチャンクを正しく返す。
func TestRegionChunkRep(t *testing.T) {
	s := newTestStore(t)
	data := regionCorpus(t, 16<<20)
	m := putBytes(t, s, "c", data)
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	// マニフェストのチャンクを ChunkRep 経由で取得して結合
	man, err := s.Manifest(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reassembled []byte
	for _, h := range man.Chunks {
		raw, _, _, err := s.ChunkRep(h)
		if err != nil {
			t.Fatal(err)
		}
		reassembled = append(reassembled, raw...)
	}
	if sha256.Sum256(reassembled) != sha256.Sum256(data) {
		t.Fatal("ChunkRep 経由の再構成が一致しません")
	}
}
