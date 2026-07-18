package precomp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestTIFFRoundTripTestdata(t *testing.T) {
	files, _ := filepath.Glob("testdata/tiff/*.tiff")
	if len(files) == 0 {
		t.Skip("tiff testdata なし")
	}
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		if !IsTIFF(orig) {
			t.Fatalf("%s: TIFF と判定されない", fn)
		}
		u, ok := TryUnwrapTIFF(orig, 0)
		if !ok {
			t.Logf("%s: 不採用(圧縮 TIFF 等)", filepath.Base(fn))
			continue
		}
		back, err := ReconstructTIFF(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(back, orig) {
			t.Fatalf("%s: ビット一致しない err=%v", fn, err)
		}
		probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
		t.Logf("%s: orig=%d filtered+zstd≈%d (%.1f%%)", filepath.Base(fn), len(orig), len(probe),
			100*float64(len(probe)-len(orig))/float64(len(orig)))
	}
}
