package store

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// famVocabFile は family ごとに異なる語彙(共有語彙は無い)から作る小ファイル。
// 同じ family のファイルは語彙を共有し、異なる family は共有しない。
func famVocabFile(fam int, seed int64, size int) []byte {
	// family ごとに決定的な語彙(family 間で重ならないよう接頭辞を変える)。
	vrng := rand.New(rand.NewSource(int64(fam) * 7919))
	vocab := make([]string, 200)
	for i := range vocab {
		n := 4 + vrng.Intn(8)
		b := make([]byte, n)
		for j := range b {
			b[j] = byte('a' + vrng.Intn(26))
		}
		vocab[i] = fmt.Sprintf("f%d_%s", fam, string(b))
	}
	r := rand.New(rand.NewSource(seed))
	var buf bytes.Buffer
	buf.Grow(size + 16)
	for buf.Len() < size {
		buf.WriteString(vocab[r.Intn(len(vocab))])
		buf.WriteByte(' ')
	}
	return buf.Bytes()[:size]
}

// TestMinHashClustersBySchema は min-hash が「共有語彙(同一 family)」を
// 高確率で近接させ、異 family は近接させないことを確認する(スーパー特徴が
// 捉えられなかったクラスタリング信号)。
func TestMinHashClustersBySchema(t *testing.T) {
	const fams = 6
	const perFam = 8
	const size = 32 << 10
	sigs := make([][]uint64, 0, fams*perFam)
	fam := make([]int, 0, fams*perFam)
	for f := 0; f < fams; f++ {
		for m := 0; m < perFam; m++ {
			data := famVocabFile(f, int64(f*1000+m), size)
			sig := computeMinHash(data)
			if len(sig) != mhK {
				t.Fatalf("min-hash 署名長が %d(期待 %d)", len(sig), mhK)
			}
			sigs = append(sigs, sig)
			fam = append(fam, f)
		}
	}
	// 署名の要素一致数の平均を同 family / 異 family で比較。
	shared := func(a, b []uint64) int {
		n := 0
		for k := 0; k < mhK; k++ {
			if a[k] == b[k] {
				n++
			}
		}
		return n
	}
	var sameSum, diffSum float64
	var sameN, diffN int
	for i := range sigs {
		for j := i + 1; j < len(sigs); j++ {
			s := float64(shared(sigs[i], sigs[j]))
			if fam[i] == fam[j] {
				sameSum += s
				sameN++
			} else {
				diffSum += s
				diffN++
			}
		}
	}
	sameAvg := sameSum / float64(sameN)
	diffAvg := diffSum / float64(diffN)
	t.Logf("min-hash 平均共有要素数: 同 family=%.3f  異 family=%.3f", sameAvg, diffAvg)
	// 同 family は署名を有意に共有し、異 family はほぼ共有しないこと。
	if sameAvg < 1.0 {
		t.Fatalf("同 family の min-hash 共有が弱すぎる: %.3f", sameAvg)
	}
	if sameAvg <= diffAvg*3 {
		t.Fatalf("min-hash が family を弁別できていない: 同=%.3f 異=%.3f", sameAvg, diffAvg)
	}
}

// TestMinHashRegionRoundTrip は異種 family の小ファイル群を投入→Optimize
// (min-hash クラスタリングで小チャンクをソリッド圧縮)→全ファイルがビット
// 一致で復元できることを確認する(クラスタリング変更が壊さないこと)。
func TestMinHashRegionRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("重い E2E; -short でスキップ")
	}
	t.Parallel()
	s := newTestStore(t)
	const fams = 8
	const perFam = 20
	const size = 40 << 10
	type item struct {
		id   string
		data []byte
	}
	items := make([]item, 0, fams*perFam)
	// family をインターリーブして投入(実運用の多ユーザー投下を模す)。
	for m := 0; m < perFam; m++ {
		for f := 0; f < fams; f++ {
			data := famVocabFile(f, int64(f*10000+m), size)
			id := putBytes(t, s, "small", data).ID
			items = append(items, item{id, data})
		}
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	for i, it := range items {
		if got := getBytes(t, s, it.id); !bytes.Equal(got, it.data) {
			t.Fatalf("min-hash クラスタ後のファイル %d 復元不一致", i)
		}
	}
	sr, err := s.Scrub()
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.Corrupt) != 0 || len(sr.Missing) != 0 {
		t.Fatalf("スクラブ破損検出: %+v", sr)
	}
}
