package precomp

import (
	"bytes"
	"os"
	"testing"
)

// TestMP4H264RoundTrip は MP4(音声トラック込み)の CAVLC 動画が採用され、
// ファイル全体がバイト一致で復元されることを確認する。
func TestMP4H264RoundTrip(t *testing.T) {
	orig, err := os.ReadFile("testdata/h264/v_av.mp4")
	if err != nil {
		t.Skip(err)
	}
	u, ok := TryUnwrapMP4H264(orig, 1<<30)
	if !ok {
		t.Fatal("MP4 CAVLC が採用されなかった")
	}
	rt, err := ReconstructMP4H264(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	t.Logf("%d -> %d (-%.1f%%)", len(orig), len(u.Chunked),
		100*float64(len(orig)-len(u.Chunked))/float64(len(orig)))
}

// TestMP4H264RejectsCABAC は CABAC の MP4 を素通しする。
// CABAC の P スライス入り MP4 も §4.37 で採用対象になった(往復一致を確認)。
func TestMP4H264CABACPSlices(t *testing.T) {
	orig, err := os.ReadFile("testdata/h264/v_cabac.mp4")
	if err != nil {
		t.Skip(err)
	}
	u, ok := TryUnwrapMP4H264(orig, 1<<30)
	if !ok {
		t.Fatal("P スライス入り CABAC MP4 が採用されなかった")
	}
	rt, err := ReconstructMP4H264(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatal("往復不一致")
	}
	t.Logf("v_cabac.mp4: %dB → %dB(-%.2f%%)", len(orig), len(u.Chunked),
		100*float64(len(orig)-len(u.Chunked))/float64(len(orig)))
}

func FuzzTryUnwrapMP4H264(f *testing.F) {
	if b, err := os.ReadFile("testdata/h264/v_av.mp4"); err == nil {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapMP4H264(data, 1<<22)
		if !ok {
			return
		}
		rt, err := ReconstructMP4H264(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, data) {
			t.Fatalf("採用されたが往復不一致: err=%v", err)
		}
	})
}
