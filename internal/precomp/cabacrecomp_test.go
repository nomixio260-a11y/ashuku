package precomp

import (
	"os"
	"testing"
)

// TestCabacRecompMeasure は全 I スライスを二次符号へ載せ替え、原 CABAC バイト数
// との差(圧縮率)を測る。モデルはスライス跨ぎに持続。
func TestCabacRecompMeasure(t *testing.T) {
	orig, err := os.ReadFile("testdata/h264/allintra_cabac.h264")
	if err != nil {
		t.Skip(err)
	}
	nals, _ := splitAnnexB(orig)
	spsMap := map[int]*h264SPS{}
	ppsMap := map[int]*h264PPS{}
	model := newCabacSecModel()
	enc := newRangeEncoder()
	var origBits, slices int
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
			cr := &h264Reader{b: rbsp, pos: bitpos}
			var states [1024]uint8
			cabacInitStates(&states, sl.sliceQP, true, 0)
			dec := newCabacDecoder(cr)
			sink := &cabacCaptureSink{d: dec, st: &states, enc: enc, m: model}
			mbState := newCabacMBState(sps.picWidthInMbs, sps.picHeightInMbs)
			total := sps.picWidthInMbs * sps.picHeightInMbs
			if !cabacISlice(sink, mbState, sl.firstMB, total, sl.sliceQP) {
				t.Fatalf("スライス %d 走査失敗", slices)
			}
			origBits += cr.pos - bitpos
			slices++
		}
	}
	enc.finish()
	origBytes := (origBits + 7) / 8
	secBytes := len(enc.out)
	if slices == 0 {
		t.Skip("I スライスなし")
	}
	delta := 100.0 * float64(origBytes-secBytes) / float64(origBytes)
	t.Logf("スライス数=%d 原CABAC=%dB 二次=%dB 削減=%.2f%%", slices, origBytes, secBytes, delta)
}
