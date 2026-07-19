package precomp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// 追加の外部アセットは MP3BENCH_DIR で指定(CI では testdata のみ)。
const mp3AssetDir = "testdata/mp3"

// TestMP3RoundTripAssets は実 MP3(CBR/VBR/mono/LSF)の採用+往復一致。
func TestMP3RoundTripAssets(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(mp3AssetDir, "*.mp3"))
	if len(files) == 0 {
		t.Skip("mp3 アセットなし")
	}
	adopted := 0
	for _, fp := range files {
		orig, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		u, ok := TryUnwrapMP3(orig, 0)
		base := filepath.Base(fp)
		if !ok {
			t.Logf("%s: 不採用", base)
			continue
		}
		rt, err := ReconstructMP3(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, orig) {
			t.Fatalf("%s: 往復不一致", base)
		}
		adopted++
		probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
		rawZ := jpegProbeEncoder.EncodeAll(orig, nil)
		t.Logf("%s: %dB(素zstd %dB)→ %dB(素zstd比 -%.2f%%, mode=%d)", base, len(orig), len(rawZ), len(probe),
			100*float64(len(rawZ)-len(probe))/float64(len(rawZ)), u.Recipe.Mode)
	}
	if adopted == 0 {
		t.Fatal("1つも採用されなかった")
	}
}
