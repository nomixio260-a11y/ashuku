package precomp

import (
	"math/rand"
	"testing"
)

func TestRangeCoderRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 50; trial++ {
		n := rng.Intn(5000) + 1
		bits := make([]int, n)
		modelBit := make([]int, n) // どのモデルを使うか(文脈を模擬)
		for i := range bits {
			bits[i] = rng.Intn(2)
			modelBit[i] = rng.Intn(8)
		}
		eq := make([]int, n)
		for i := range eq {
			eq[i] = rng.Intn(2)
		}
		enc := newRangeEncoder()
		em := newModels(8)
		for i := 0; i < n; i++ {
			enc.encodeBit(&em[modelBit[i]], bits[i])
			enc.encodeBitEq(eq[i])
		}
		out := enc.finish()
		dec := newRangeDecoder(out)
		dm := newModels(8)
		for i := 0; i < n; i++ {
			if b := dec.decodeBit(&dm[modelBit[i]]); b != bits[i] {
				t.Fatalf("trial%d bit%d: %d!=%d", trial, i, b, bits[i])
			}
			if b := dec.decodeBitEq(); b != eq[i] {
				t.Fatalf("trial%d eq%d", trial, i)
			}
		}
	}
}

// 偏った分布は1ビット未満/シンボルに圧縮できる(算術符号の要点)。
func TestRangeCoderCompressesSkewed(t *testing.T) {
	enc := newRangeEncoder()
	m := newModels(1)
	n := 100000
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < n; i++ {
		b := 0
		if rng.Intn(100) < 5 {
			b = 1
		}
		enc.encodeBit(&m[0], b)
	}
	out := enc.finish()
	t.Logf("10万ビット(p=0.05)→ %d バイト (%.3f bit/sym)", len(out), 8*float64(len(out))/float64(n))
	if len(out) > n/8/2 {
		t.Fatalf("圧縮が効いていない: %d bytes", len(out))
	}
}
