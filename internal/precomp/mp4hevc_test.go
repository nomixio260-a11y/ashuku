package precomp

import (
	"bytes"
	"os"
	"testing"
)

// TestMP4HEVCRoundTrip は hvc1 MP4 の分解→復元のバイト一致を確認する。
func TestMP4HEVCRoundTrip(t *testing.T) {
	for _, name := range []string{"testdata/hevc/cam_hvc1.mp4"} {
		orig, err := os.ReadFile(name)
		if err != nil {
			t.Skip(err)
		}
		uw, ok := TryUnwrapMP4HEVC(orig, 0)
		if !ok {
			t.Errorf("%s: 不採用", name)
			continue
		}
		rt, err := ReconstructMP4HEVC(uw.Recipe, uw.Chunked)
		if err != nil || !bytes.Equal(rt, orig) {
			t.Errorf("%s: 復元不一致", name)
			continue
		}
		zo := jpegProbeEncoder.EncodeAll(orig, nil)
		zc := jpegProbeEncoder.EncodeAll(uw.Chunked, nil)
		t.Logf("%s: %dB zstd比 %d→%d(%.2f%%)", name, len(orig), len(zo), len(zc),
			100*(float64(len(zc))-float64(len(zo)))/float64(len(zo)))
	}
}
