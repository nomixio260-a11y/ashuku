package precomp

import (
	"bytes"
	"os"
	"testing"
)

// TestCabacPipelinePB は P/B スライス入り CABAC の本番経路(採用+往復一致)。
func TestCabacPipelinePB(t *testing.T) {
	for _, name := range []string{"p_main.h264", "b_high.h264"} {
		orig, err := os.ReadFile("testdata/h264/" + name)
		if err != nil {
			t.Skip(err)
		}
		u, ok := TryUnwrapH264(orig, 0)
		if !ok {
			t.Fatalf("%s: 採用されなかった", name)
		}
		rt, err := ReconstructH264(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, orig) {
			t.Fatalf("%s: 往復不一致", name)
		}
		t.Logf("%s: %dB → %dB(-%.2f%%)", name, len(orig), len(u.Chunked),
			100*float64(len(orig)-len(u.Chunked))/float64(len(orig)))
	}
}

// TestCabacPipelinePhoneMP4 はスマホ様 MP4(High, B, 8x8)の本番経路。
func TestCabacPipelinePhoneMP4(t *testing.T) {
	orig, err := os.ReadFile("testdata/h264/phone_like.mp4")
	if err != nil {
		t.Skip(err)
	}
	u, ok := TryUnwrapMP4H264(orig, 0)
	if !ok {
		t.Fatal("phone_like.mp4: 採用されなかった")
	}
	rt, err := ReconstructMP4H264(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatal("phone_like.mp4: 往復不一致")
	}
	t.Logf("phone_like.mp4: %dB → %dB(-%.2f%%)", len(orig), len(u.Chunked),
		100*float64(len(orig)-len(u.Chunked))/float64(len(orig)))
}
