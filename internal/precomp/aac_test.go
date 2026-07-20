package precomp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestAACRoundTrip は ADTS AAC-LC の分解→復元バイト一致と圧縮効果を確認する。
// 採用可否はゲート依存(周期的な合成音は zstd 単体が勝つため不採用が正しい)
// なので、採用されたら復元一致と圧縮を検査し、採用状況をログする。
func TestAACRoundTrip(t *testing.T) {
	ents, err := os.ReadDir("testdata/aac")
	if err != nil {
		t.Skip(err)
	}
	adopted := 0
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".aac" {
			continue
		}
		orig, err := os.ReadFile(filepath.Join("testdata/aac", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if !IsAAC(orig) {
			t.Errorf("%s: IsAAC=false", e.Name())
			continue
		}
		uw, ok := TryUnwrapAAC(orig, 0)
		if !ok {
			t.Logf("%s: 不採用(%dB)", e.Name(), len(orig))
			continue
		}
		adopted++
		rt, err := ReconstructAAC(uw.Recipe, uw.Chunked)
		if err != nil || !bytes.Equal(rt, orig) {
			t.Errorf("%s: 往復不一致 err=%v", e.Name(), err)
			continue
		}
		zo := jpegProbeEncoder.EncodeAll(orig, nil)
		zc := jpegProbeEncoder.EncodeAll(uw.Chunked, nil)
		t.Logf("%s: %dB→%dB zstd比 %d→%d(%.2f%%)", e.Name(), len(orig), len(uw.Chunked),
			len(zo), len(zc), 100*(float64(len(zc))-float64(len(zo)))/float64(len(zo)))
	}
	if adopted == 0 {
		t.Error("採用された AAC 素材がない")
	}
}

// TestAACVerify は全 AAC 素材が正準ハフマン再符号化でバイト一致すること
// (パーサの正しさ)を確認する。採用可否に関わらず検証は成立すべき。
func TestAACVerify(t *testing.T) {
	ents, err := os.ReadDir("testdata/aac")
	if err != nil {
		t.Skip(err)
	}
	aacInitDec()
	for _, e := range ents {
		if filepath.Ext(e.Name()) != ".aac" {
			continue
		}
		orig, _ := os.ReadFile(filepath.Join("testdata/aac", e.Name()))
		frames, ok := parseADTS(orig)
		if !ok {
			t.Errorf("%s: parseADTS 失敗", e.Name())
			continue
		}
		models := newAACModels()
		bad := 0
		for _, f := range frames {
			body := orig[f.start+f.hdrLen : f.start+f.frameLen]
			vw := &h264Writer{}
			vs := &aacCaptureSink{r: &h264Reader{b: body}, cw: vw, m: models}
			if !(f.numRDB == 1 && aacWalkRDB(vs, f.sfi) && !vs.bad &&
				vs.r.pos == len(body)*8 && bytes.Equal(vw.b, body)) {
				bad++
			}
		}
		if bad > 0 {
			t.Errorf("%s: %d/%d フレームが正準再符号化不一致", e.Name(), bad, len(frames))
		} else {
			t.Logf("%s: %d フレーム全て正準一致", e.Name(), len(frames))
		}
	}
}
