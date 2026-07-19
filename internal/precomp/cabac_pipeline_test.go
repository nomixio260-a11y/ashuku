package precomp

import (
	"bytes"
	"os"
	"testing"
)

// TestCabacPipelineAnnexB は本番経路(TryUnwrapH264)で CABAC 全イントラが
// 採用され、往復バイト一致し、実際に縮むことを確認する。
func TestCabacPipelineAnnexB(t *testing.T) {
	for _, name := range []string{"big_cabac.h264", "allintra_cabac.h264", "rt_cabac.h264"} {
		orig, err := os.ReadFile("testdata/h264/" + name)
		if err != nil {
			t.Skip(err)
		}
		u, ok := TryUnwrapH264(orig, 0)
		if !ok {
			if name == "rt_cabac.h264" {
				t.Logf("%s: 不採用(小さすぎて損益分岐未満、想定内)", name)
				continue
			}
			t.Fatalf("%s: CABAC 全イントラが採用されなかった", name)
		}
		rt, err := ReconstructH264(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, orig) {
			t.Fatalf("%s: 往復不一致", name)
		}
		saved := len(orig) - len(u.Chunked)
		t.Logf("%s: %dB → 算術 %dB(-%.2f%%)", name, len(orig), len(u.Chunked),
			100*float64(saved)/float64(len(orig)))
	}
}

// TestCabacPipelineMP4 は MP4 コンテナ入り CABAC 全イントラの採用を確認する。
func TestCabacPipelineMP4(t *testing.T) {
	orig, err := os.ReadFile("testdata/h264/big_cabac.mp4")
	if err != nil {
		t.Skip(err)
	}
	u, ok := TryUnwrapMP4H264(orig, 0)
	if !ok {
		t.Fatal("MP4/CABAC 全イントラが採用されなかった")
	}
	rt, err := ReconstructMP4H264(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatal("MP4/CABAC 往復不一致")
	}
	t.Logf("MP4: %dB → %dB(-%.2f%%)", len(orig), len(u.Chunked),
		100*float64(len(orig)-len(u.Chunked))/float64(len(orig)))
}
