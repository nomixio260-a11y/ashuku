package precomp

import (
	"os"
	"testing"
)

// TestHEVCHeaderParse は SPS/PPS/スライスヘッダの解析が実ストリームで
// 成立する(整列後にヘッダが終わる)ことを確認する。
func TestHEVCHeaderParse(t *testing.T) {
	orig, err := os.ReadFile("/tmp/claude-0/-home-user-ashuku/e83cdb6f-0723-5a84-8f80-54086f33fd24/scratchpad/hevcassets/i_min.h265")
	if err != nil {
		t.Skip(err)
	}
	if !IsHEVC(orig) {
		t.Fatal("IsHEVC=false")
	}
	nals, ok := splitAnnexB(orig)
	if !ok {
		t.Fatal("split 失敗")
	}
	var sps *hevcSPS
	var pps *hevcPPS
	slices := 0
	for _, nal := range nals {
		typ := hevcNALType(nal.data)
		rbsp := unescapeRBSP(nal.data)
		switch {
		case typ == hevcNALSPS:
			s, ok := parseHEVCSPS(rbsp[2:])
			if !ok {
				t.Fatal("SPS 解析失敗")
			}
			sps = s
			t.Logf("SPS: %dx%d ctb=%d(%dx%d) minTb=%d maxTb=%d trDepthI=%d sao=%v pcm=%v amp=%v rps=%d",
				s.width, s.height, 1<<uint(s.log2CtbSize), s.ctbW, s.ctbH, s.log2MinTb, s.log2MaxTb,
				s.maxTrDepthIntra, s.sao, s.pcmEnabled, s.amp, len(s.numDeltaPocs))
		case typ == hevcNALPPS:
			p, ok := parseHEVCPPS(rbsp[2:])
			if !ok {
				t.Fatal("PPS 解析失敗")
			}
			pps = p
			t.Logf("PPS: initQP=%d wpp=%v cuQpDelta=%v tskip=%v signHide=%v",
				p.initQP, p.entropyCodingSync, p.cuQPDeltaEnabled, p.transformSkip, p.signDataHiding)
		case hevcIsSlice(typ):
			if sps == nil || pps == nil {
				t.Fatal("PS 不在でスライス")
			}
			r := &h264Reader{b: rbsp[2:]}
			sl, ok := parseHEVCSliceHeader(r, sps, pps, typ)
			if !ok {
				t.Fatalf("スライスヘッダ解析失敗(type=%d)", typ)
			}
			if (16+sl.headerBits)%8 != 0 {
				t.Fatalf("ヘッダが整列していない: %d", sl.headerBits)
			}
			t.Logf("slice: type=%d qp=%d sao=%v/%v entry=%d hdrBits=%d rbspLen=%d",
				sl.sliceType, sl.sliceQP, sl.saoLuma, sl.saoChroma, sl.numEntry, sl.headerBits, len(rbsp))
			slices++
		}
	}
	if slices == 0 {
		t.Fatal("スライスなし")
	}
}
