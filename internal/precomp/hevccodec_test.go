package precomp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestHEVCRoundTrip は testdata の HEVC 素材の分解→復元のバイト一致を確認する。
// 採用可否はゲート依存(小さい素材はレシピ費用で不採用が正しい)なので、
// 採用された場合のみ復元一致を検査し、採用状況をログする。
func TestHEVCRoundTrip(t *testing.T) {
	ents, err := os.ReadDir("testdata/hevc")
	if err != nil {
		t.Skip(err)
	}
	adopted := 0
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".h265" {
			continue
		}
		orig, err := os.ReadFile(filepath.Join("testdata/hevc", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		uw, ok := TryUnwrapHEVC(orig, 0)
		if !ok {
			t.Logf("%s: 不採用(%dB)", e.Name(), len(orig))
			continue
		}
		adopted++
		rt, err := ReconstructHEVC(uw.Recipe, uw.Chunked)
		if err != nil || !bytes.Equal(rt, orig) {
			t.Errorf("%s: 復元不一致", e.Name())
			continue
		}
		zo := jpegProbeEncoder.EncodeAll(orig, nil)
		zc := jpegProbeEncoder.EncodeAll(uw.Chunked, nil)
		t.Logf("%s: %dB chunked=%dB zstd比 %d→%d(%.2f%%)",
			e.Name(), len(orig), len(uw.Chunked), len(zo), len(zc),
			100*(float64(len(zc))-float64(len(zo)))/float64(len(zo)))
	}
	if adopted == 0 {
		t.Error("採用された素材がない")
	}
}

// TestHEVCVerifyTestdata は全 testdata 素材の走査整合(遅延デシンク・
// オラクル)を確認する。採用可否に関係なく走査自体は成立すべき。
func TestHEVCVerifyTestdata(t *testing.T) {
	ents, err := os.ReadDir("testdata/hevc")
	if err != nil {
		t.Skip(err)
	}
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".h265" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("testdata/hevc", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		res, nalIdx, ok := hevcVerifyStream(data)
		if !ok {
			t.Errorf("%s: 検証失敗(NAL %d)", e.Name(), nalIdx)
			continue
		}
		t.Logf("%s: OK(%d スライス)", e.Name(), res.slices)
	}
}
