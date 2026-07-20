package precomp

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHEVCWalkAssets は scratchpad の HEVC 素材群を検証走査する
// (遅延デシンク・オラクル: スライス末尾の消費バイト位置一致)。
func TestHEVCWalkAssets(t *testing.T) {
	dir := "/tmp/claude-0/-home-user-ashuku/e83cdb6f-0723-5a84-8f80-54086f33fd24/scratchpad/hevcassets"
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Skip(err)
	}
	found := 0
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".h265" {
			continue
		}
		found++
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		res, nalIdx, ok := hevcVerifyStream(data)
		if !ok {
			t.Errorf("%s: 検証失敗(NAL %d, %d スライス走査済)", e.Name(), nalIdx, res.slices)
			continue
		}
		t.Logf("%s: OK(%d スライス)", e.Name(), res.slices)
	}
	if found == 0 {
		t.Skip("素材なし")
	}
}
