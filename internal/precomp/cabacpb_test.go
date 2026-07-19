package precomp

import (
	"os"
	"path/filepath"
	"testing"
)

// cabacOracleFile は1ファイルの全スライスを走査し、各スライスが end_of_slice に
// ビット整列することを確認する(遅発性デシンク・オラクル)。
func cabacOracleFile(t *testing.T, path string) {
	t.Helper()
	orig, err := os.ReadFile(path)
	if err != nil {
		t.Skip(err)
	}
	nals, ok := splitAnnexB(orig)
	if !ok {
		t.Fatal("split 失敗")
	}
	spsMap := map[int]*h264SPS{}
	ppsMap := map[int]*h264PPS{}
	var mbState *cabacMBState
	slices := 0
	counts := map[int]int{}
	for _, nal := range nals {
		typ := int(nal.data[0] & 0x1F)
		refIDC := int(nal.data[0] >> 5)
		rbsp := unescapeRBSP(nal.data)
		switch typ {
		case 7:
			if s, ok := parseSPS(rbsp[1:]); ok {
				spsMap[spsIDOf(rbsp[1:])] = s
			} else {
				t.Fatal("SPS 解析失敗")
			}
		case 8:
			if p, ok := parsePPS(rbsp[1:]); ok {
				ppsMap[ppsIDOf(rbsp[1:])] = p
			} else {
				t.Fatal("PPS 解析失敗")
			}
		case 1, 5:
			payload := rbsp[1:]
			pre := &h264Reader{b: payload}
			pre.ue()
			pre.ue()
			ppsID, _ := pre.ue()
			pps := ppsMap[int(ppsID)]
			if pps == nil || !pps.entropyCodingMode {
				t.Fatal("PPS 不在 or CAVLC")
			}
			sps := spsMap[pps.spsID]
			r := &h264Reader{b: payload}
			sl, ok := parseSliceHeader(r, sps, pps, typ, refIDC)
			if !ok {
				t.Fatalf("スライス %d: ヘッダ解析失敗", slices)
			}
			counts[sl.sliceType%5]++
			bitpos := 8 + sl.headerBits
			for bitpos%8 != 0 {
				bitpos++
			}
			sc := cabacSliceCtxOf(sl, sps, pps)
			cr := &h264Reader{b: rbsp, pos: bitpos}
			var states [1024]uint8
			cabacInitStates(&states, sl.sliceQP, sc.isI(), sc.initIDC)
			sink := &cabacDecSink{d: newCabacDecoder(cr), st: &states}
			if mbState == nil || mbState.mbW != sps.picWidthInMbs || mbState.mbH != sps.picHeightInMbs {
				mbState = newCabacMBState(sps.picWidthInMbs, sps.picHeightInMbs)
			}
			total := sps.picWidthInMbs * sps.picHeightInMbs
			if !cabacSlice(sink, mbState, sc, sl.firstMB, total, sl.sliceQP) {
				t.Fatalf("スライス %d(type=%d qp=%d): 走査失敗 bitpos=%d/%d",
					slices, sl.sliceType, sl.sliceQP, cr.pos, len(rbsp)*8)
			}
			remain := len(rbsp)*8 - cr.pos
			if remain > 16 {
				t.Fatalf("スライス %d(type=%d): 使い切っていない(残 %d bit)", slices, sl.sliceType, remain)
			}
			slices++
		}
	}
	t.Logf("%s: %d スライス OK(I=%d P=%d B=%d)", filepath.Base(path), slices, counts[2], counts[0], counts[1])
}

const pbStreamDir = "/tmp/claude-0/-home-user-ashuku/e83cdb6f-0723-5a84-8f80-54086f33fd24/scratchpad/pbstreams"

func TestCabacPSliceOracle(t *testing.T) {
	cabacOracleFile(t, filepath.Join(pbStreamDir, "p_main.h264"))
}

func TestCabacPWpredOracle(t *testing.T) {
	cabacOracleFile(t, filepath.Join(pbStreamDir, "p_wpred.h264"))
}

func TestCabacP8x8Oracle(t *testing.T) {
	cabacOracleFile(t, filepath.Join(pbStreamDir, "p_high8x8.h264"))
}

func TestCabacBOracle(t *testing.T) {
	cabacOracleFile(t, filepath.Join(pbStreamDir, "b_high.h264"))
}

func TestCabacScreenRecOracle(t *testing.T) {
	cabacOracleFile(t, filepath.Join(pbStreamDir, "screenrec.h264"))
}
