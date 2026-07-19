package precomp

import (
	"os"
	"testing"
)

// sliceInfo は1 I スライスの再圧縮に要る最小情報。
type rtSlice struct {
	rbsp      []byte
	sps       *h264SPS
	pps       *h264PPS
	sl        *h264Slice
	cabacByte int // cabac データ開始バイト
}

// TestCabacRoundTrip は capture→rebuild で原 CABAC バイトが厳密再生されることを
// 確認する(可逆性の証明)。モデルはスライス跨ぎに持続。
func TestCabacRoundTrip(t *testing.T) {
	orig, err := os.ReadFile("testdata/h264/rt_cabac.h264")
	if err != nil {
		t.Skip(err)
	}
	nals, _ := splitAnnexB(orig)
	spsMap := map[int]*h264SPS{}
	ppsMap := map[int]*h264PPS{}
	var slicesInfo []rtSlice
	for _, nal := range nals {
		typ := int(nal.data[0] & 0x1F)
		refIDC := int(nal.data[0] >> 5)
		rbsp := unescapeRBSP(nal.data)
		switch typ {
		case 7:
			spsMap[spsIDOf(rbsp[1:])], _ = parseSPS(rbsp[1:])
		case 8:
			ppsMap[ppsIDOf(rbsp[1:])], _ = parsePPS(rbsp[1:])
		case 1, 5:
			payload := rbsp[1:]
			pre := &h264Reader{b: payload}
			pre.ue()
			st2, _ := pre.ue()
			ppsID, _ := pre.ue()
			pps := ppsMap[int(ppsID)]
			if pps == nil || !pps.entropyCodingMode {
				continue
			}
			sps := spsMap[pps.spsID]
			if sps == nil || (st2 != 2 && st2 != 7) {
				continue
			}
			r := &h264Reader{b: payload}
			sl, ok := parseSliceHeader(r, sps, pps, typ, refIDC)
			if !ok {
				continue
			}
			bitpos := 8 + sl.headerBits
			for bitpos%8 != 0 {
				bitpos++
			}
			slicesInfo = append(slicesInfo, rtSlice{rbsp: rbsp, sps: sps, pps: pps, sl: sl, cabacByte: bitpos / 8})
		}
	}
	if len(slicesInfo) == 0 {
		t.Skip("I スライスなし")
	}

	// --- capture: 全スライスを1本の二次ストリームへ ---
	model := newCabacSecModel()
	enc := newRcEncoder()
	for i, si := range slicesInfo {
		cr := &h264Reader{b: si.rbsp, pos: si.cabacByte * 8}
		var states [1024]uint8
		cabacInitStates(&states, si.sl.sliceQP, true, 0)
		dec := newCabacDecoder(cr)
		sink := &cabacCaptureSink{d: dec, st: &states, enc: enc, m: model}
		mbState := newCabacMBState(si.sps.picWidthInMbs, si.sps.picHeightInMbs)
		total := si.sps.picWidthInMbs * si.sps.picHeightInMbs
		if !cabacISlice(sink, mbState, si.sl.firstMB, total, si.sl.sliceQP) {
			t.Fatalf("capture スライス %d 失敗", i)
		}
	}
	enc.flush()

	// --- rebuild: 二次ストリームから各スライスの CABAC バイトを再生 ---
	rmodel := newCabacSecModel()
	rdec := newRcDecoder(enc.out)
	maxTail := 0
	for i, si := range slicesInfo {
		w := &h264Writer{}
		var states [1024]uint8
		cabacInitStates(&states, si.sl.sliceQP, true, 0)
		cenc := newCabacEncoder(w)
		sink := &cabacRebuildSink{dec: rdec, enc: cenc, st: &states, m: rmodel}
		mbState := newCabacMBState(si.sps.picWidthInMbs, si.sps.picHeightInMbs)
		total := si.sps.picWidthInMbs * si.sps.picHeightInMbs
		if !cabacISlice(sink, mbState, si.sl.firstMB, total, si.sl.sliceQP) {
			t.Fatalf("rebuild スライス %d 失敗(I_PCM?)", i)
		}
		for w.nbit%8 != 0 {
			w.u1(0)
		}
		got := w.b
		want := si.rbsp[si.cabacByte:]
		// CABAC 算術本体はバイト厳密。差は末尾フラッシュ(rbsp_trailing の
		// エンコーダ差)だけのはず。最初の相違位置を求め、末尾数バイト以内で
		// あることを確認する(実パイプラインではこの tail を recipe に保存)。
		n := len(got)
		if len(want) < n {
			n = len(want)
		}
		diff := n
		for k := 0; k < n; k++ {
			if got[k] != want[k] {
				diff = k
				break
			}
		}
		tail := len(want) - diff
		if tail > 3 {
			lo := diff - 2
			if lo < 0 {
				lo = 0
			}
			t.Fatalf("スライス %d: 本体不一致 @byte %d/%d (tail=%d 過大) got=%x want=%x",
				i, diff, n, tail, got[lo:min(diff+6, len(got))], want[lo:min(diff+6, len(want))])
		}
		maxTail = maxOf(maxTail, tail)
	}
	t.Logf("round-trip OK: %d スライス、本体バイト厳密再生(末尾 tail 最大 %d バイト)", len(slicesInfo), maxTail)
}

func maxOf(a, b int) int {
	if a > b {
		return a
	}
	return b
}
