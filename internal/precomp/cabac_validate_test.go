package precomp

import (
	"os"
	"testing"
)

// TestCabacISliceOracle は実 x264 CABAC 全イントラ ストリームの各 I スライスを
// 走査し、**ちょうど end_of_slice(terminate=1)で終わり、CABAC データを
// 使い切る**ことを確認する。CABAC は状態依存なので、文脈導出に1つでも誤りが
// あれば以降のビンが総崩れになり terminate が整列しない=遅発性デシンクの
// 検出オラクル(CAVLC で使った手法の CABAC 版)。
func TestCabacISliceOracle(t *testing.T) {
	orig, err := os.ReadFile("testdata/h264/allintra_cabac.h264")
	if err != nil {
		t.Skip(err)
	}
	nals, ok := splitAnnexB(orig)
	if !ok {
		t.Fatal("split 失敗")
	}
	spsMap := map[int]*h264SPS{}
	ppsMap := map[int]*h264PPS{}
	slices := 0
	for _, nal := range nals {
		typ := int(nal.data[0] & 0x1F)
		refIDC := int(nal.data[0] >> 5)
		rbsp := unescapeRBSP(nal.data)
		switch typ {
		case 7:
			if s, ok := parseSPSCABAC(rbsp[1:]); ok {
				spsMap[spsIDOf(rbsp[1:])] = s
			}
		case 8:
			if p, ok := parsePPS(rbsp[1:]); ok {
				ppsMap[ppsIDOf(rbsp[1:])] = p
			}
		case 1, 5:
			payload := rbsp[1:]
			pre := &h264Reader{b: payload}
			pre.ue()
			st2, _ := pre.ue()
			ppsID, _ := pre.ue()
			pps := ppsMap[int(ppsID)]
			if pps == nil || !pps.entropyCodingMode {
				t.Fatalf("PPS 不在 or CAVLC(このテストは CABAC 前提)")
			}
			sps := spsMap[pps.spsID]
			if sps == nil {
				t.Fatal("SPS 不在")
			}
			if st2 != 2 && st2 != 7 {
				t.Skipf("I 以外のスライス(type=%d)。このテストは全イントラ用", st2)
			}
			r := &h264Reader{b: payload}
			sl, ok := parseSliceHeader(r, sps, pps, typ, refIDC)
			if !ok {
				t.Fatal("スライスヘッダ解析失敗")
			}
			// cabac_alignment_one_bit: バイト境界まで進める
			bitpos := 8 + sl.headerBits
			for bitpos%8 != 0 {
				bitpos++
			}
			cr := &h264Reader{b: rbsp, pos: bitpos}
			var states [1024]uint8
			cabacInitStates(&states, sl.sliceQP, true, 0)
			dec := newCabacDecoder(cr)
			sink := &cabacDecSink{d: dec, st: &states}
			mbState := newCabacMBState(sps.picWidthInMbs, sps.picHeightInMbs)
			total := sps.picWidthInMbs * sps.picHeightInMbs
			if !cabacISlice(sink, mbState, sl.firstMB, total, sl.sliceQP) {
				t.Fatalf("スライス %d: 走査失敗(デシンク or 非対応構文, bitpos=%d/%d)",
					slices, cr.pos, len(rbsp)*8)
			}
			// 使い切り: 残りは rbsp_trailing のバイト詰めのみ(数バイト以内)
			remain := len(rbsp)*8 - cr.pos
			if remain > 16 {
				t.Fatalf("スライス %d: CABAC データを使い切っていない(残 %d bit)", slices, remain)
			}
			t.Logf("スライス %d: OK(%d MB, 使い切り, 残 %d bit)", slices, total, remain)
			slices++
		}
	}
	if slices == 0 {
		t.Fatal("I スライスが1つも走査されなかった")
	}
}

// parseSPSCABAC は CABAC 用に parseSPS を呼ぶだけ(共通)。
func parseSPSCABAC(rbsp []byte) (*h264SPS, bool) { return parseSPS(rbsp) }
