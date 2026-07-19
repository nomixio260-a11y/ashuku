package precomp

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJPEGBenchExternal(t *testing.T) {
	dir := os.Getenv("JPEGBENCH_DIR")
	if dir == "" {
		t.Skip("JPEGBENCH_DIR 未設定")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	for _, fp := range files {
		orig, err := os.ReadFile(fp)
		if err != nil {
			continue
		}
		u, ok := TryUnwrapJPEG(orig, 0)
		if !ok {
			u, ok = TryUnwrapJPEGProgressive(orig, 0)
		}
		if !ok {
			t.Logf("%s: 不採用", filepath.Base(fp))
			continue
		}
		probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
		t.Logf("%s: %d → %d(-%.1f%%)", filepath.Base(fp), len(orig), len(probe),
			100*float64(len(orig)-len(probe))/float64(len(orig)))
	}
}
