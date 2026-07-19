package precomp

import (
	"math/rand"
	"testing"
)

// TestCabacIdentity は自前 CABAC エンコーダ→デコーダの恒等性を、ランダムな
// ビン列・文脈列・バイパス・終端の混合で確認する(規格手続きの自己整合)。
func TestCabacIdentity(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 200; trial++ {
		var encStates, decStates [1024]uint8
		qp := rng.Intn(52)
		idc := rng.Intn(3)
		isI := rng.Intn(2) == 0
		cabacInitStates(&encStates, qp, isI, idc)
		cabacInitStates(&decStates, qp, isI, idc)

		type ev struct {
			kind int // 0=decision, 1=bypass, 2=terminate(0)
			ctx  int
			bin  int
		}
		n := 50 + rng.Intn(3000)
		evs := make([]ev, 0, n+1)
		for i := 0; i < n; i++ {
			k := rng.Intn(10)
			switch {
			case k < 7:
				evs = append(evs, ev{0, rng.Intn(1024), rng.Intn(2)})
			case k < 9:
				evs = append(evs, ev{1, 0, rng.Intn(2)})
			default:
				evs = append(evs, ev{2, 0, 0})
			}
		}
		evs = append(evs, ev{2, 0, 1}) // 最後に terminate=1

		w := &h264Writer{}
		enc := newCabacEncoder(w)
		for _, e := range evs {
			switch e.kind {
			case 0:
				enc.encodeDecision(&encStates[e.ctx], e.bin)
			case 1:
				enc.encodeBypass(e.bin)
			case 2:
				enc.encodeTerminate(e.bin)
			}
		}
		for w.nbit%8 != 0 {
			w.u1(0)
		}

		r := &h264Reader{b: w.b}
		dec := newCabacDecoder(r)
		for i, e := range evs {
			var got int
			switch e.kind {
			case 0:
				got = dec.decodeDecision(&decStates[e.ctx])
			case 1:
				got = dec.decodeBypass()
			case 2:
				got = dec.decodeTerminate()
				if e.bin == 0 && got != 0 {
					t.Fatalf("trial %d ev %d: terminate=1 が早すぎる", trial, i)
				}
			}
			if e.kind != 2 && got != e.bin {
				t.Fatalf("trial %d ev %d: bin 不一致 got=%d want=%d", trial, i, got, e.bin)
			}
		}
	}
}
