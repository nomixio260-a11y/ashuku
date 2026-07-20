package precomp

import (
	"bytes"
	"os"
	"testing"
)

// TestM4ARoundTrip は MP4/M4A 入り AAC の分解→復元バイト一致と圧縮を確認する。
func TestM4ARoundTrip(t *testing.T) {
	orig, err := os.ReadFile("testdata/aac/pink.m4a")
	if err != nil {
		t.Skip(err)
	}
	tr, ok := parseMP4AudioTrack(orig)
	if !ok {
		t.Fatal("parseMP4AudioTrack 失敗")
	}
	t.Logf("sfi=%d aot=%d samples=%d", tr.sfi, tr.objType, len(tr.offsets))
	uw, ok := TryUnwrapM4A(orig, 0)
	if !ok {
		t.Fatal("不採用")
	}
	rt, err := ReconstructM4A(uw.Recipe, uw.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("往復不一致 err=%v", err)
	}
	zo := jpegProbeEncoder.EncodeAll(orig, nil)
	zc := jpegProbeEncoder.EncodeAll(uw.Chunked, nil)
	t.Logf("pink.m4a: %dB→%dB zstd比 %d→%d(%.2f%%)", len(orig), len(uw.Chunked),
		len(zo), len(zc), 100*(float64(len(zc))-float64(len(zo)))/float64(len(zo)))
}
