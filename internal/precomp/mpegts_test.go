package precomp

import (
	"bytes"
	"os"
	"testing"
)

// TestTSPipeline は TS 内 H.264 の採用+往復バイト一致を確認する。
func TestTSPipeline(t *testing.T) {
	for _, name := range []string{"ts_video.ts", "ts_av.ts"} {
		orig, err := os.ReadFile("testdata/h264/" + name)
		if err != nil {
			t.Skip(err)
		}
		u, ok := TryUnwrapTS(orig, 0)
		if !ok {
			t.Fatalf("%s: 採用されなかった", name)
		}
		rt, err := ReconstructTS(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, orig) {
			t.Fatalf("%s: 往復不一致", name)
		}
		t.Logf("%s: %dB → %dB(-%.2f%%)", name, len(orig), len(u.Chunked),
			100*float64(len(orig)-len(u.Chunked))/float64(len(orig)))
	}
}

func TestTSRejectsGarbage(t *testing.T) {
	junk := bytes.Repeat([]byte{0x47, 0x00, 0x11, 0x22}, 200)
	if _, ok := TryUnwrapTS(junk, 0); ok {
		t.Fatal("ゴミが採用された")
	}
}

// TestFragMP4Pipeline は fragmented MP4 の採用+往復一致を確認する。
func TestFragMP4Pipeline(t *testing.T) {
	orig, err := os.ReadFile("testdata/h264/frag.mp4")
	if err != nil {
		t.Skip(err)
	}
	u, ok := TryUnwrapMP4H264(orig, 0)
	if !ok {
		t.Fatal("frag.mp4: 採用されなかった")
	}
	rt, err := ReconstructMP4H264(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatal("frag.mp4: 往復不一致")
	}
	t.Logf("frag.mp4: %dB → %dB(-%.2f%%)", len(orig), len(u.Chunked),
		100*float64(len(orig)-len(u.Chunked))/float64(len(orig)))
}
