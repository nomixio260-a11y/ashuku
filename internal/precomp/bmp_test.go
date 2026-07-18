package precomp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestBMPRoundTripTestdata(t *testing.T) {
	files, _ := filepath.Glob("testdata/bmp/*.bmp")
	if len(files) == 0 {
		t.Skip("bmp testdata なし")
	}
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		if !IsBMP(orig) {
			t.Fatalf("%s: BMP と判定されない", fn)
		}
		u, ok := TryUnwrapBMP(orig, 0)
		if !ok {
			t.Logf("%s: 不採用", fn)
			continue
		}
		back, err := ReconstructBMP(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(back, orig) {
			t.Fatalf("%s: ビット一致しない err=%v", fn, err)
		}
		probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
		t.Logf("%s: orig=%d filtered+zstd≈%d (%.1f%%)", filepath.Base(fn), len(orig),
			len(probe)+len(u.Recipe.Suffix),
			100*float64(len(probe)+len(u.Recipe.Suffix)-len(orig))/float64(len(orig)))
	}
}

func TestBMPRejectsCompressed(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte("BM"),
		append([]byte("BM"), make([]byte, 100)...), // ダミー
	}
	for i, c := range cases {
		if _, ok := TryUnwrapBMP(c, 0); ok {
			t.Fatalf("case %d: 不正入力が採用された", i)
		}
	}
}
