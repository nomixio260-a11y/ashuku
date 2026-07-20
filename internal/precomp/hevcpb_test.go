package precomp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestHEVCInterVerify は P/B スライスを含む素材の走査整合を確認する
// (遅延デシンク・オラクル)。
func TestHEVCInterVerify(t *testing.T) {
	for _, name := range []string{"pb_small.h265", "phone_pb.mp4", "phone_pb.ts"} {
		p := filepath.Join("testdata/hevc", name)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if filepath.Ext(name) == ".h265" {
			res, nal, ok := hevcVerifyStream(data)
			if !ok {
				t.Errorf("%s: 走査失敗(NAL %d)", name, nal)
				continue
			}
			t.Logf("%s: OK(%d スライス)", name, res.slices)
		}
	}
}

// TestHEVCInterRoundTrip は P/B を含む素材の分解→復元バイト一致と圧縮効果。
func TestHEVCInterRoundTrip(t *testing.T) {
	type tc struct {
		name   string
		unwrap func([]byte) ([]byte, []byte, bool) // orig → (chunked, rt, ok)
	}
	cases := []tc{
		{"pb_small.h265", func(o []byte) ([]byte, []byte, bool) {
			u, ok := TryUnwrapHEVC(o, 0)
			if !ok {
				return nil, nil, false
			}
			rt, err := ReconstructHEVC(u.Recipe, u.Chunked)
			return u.Chunked, rt, err == nil
		}},
		{"phone_pb.mp4", func(o []byte) ([]byte, []byte, bool) {
			u, ok := TryUnwrapMP4HEVC(o, 0)
			if !ok {
				return nil, nil, false
			}
			rt, err := ReconstructMP4HEVC(u.Recipe, u.Chunked)
			return u.Chunked, rt, err == nil
		}},
		{"phone_pb.ts", func(o []byte) ([]byte, []byte, bool) {
			u, ok := TryUnwrapTS(o, 0)
			if !ok || u.Recipe.HEVC == nil {
				return nil, nil, false
			}
			rt, err := ReconstructTS(u.Recipe, u.Chunked)
			return u.Chunked, rt, err == nil
		}},
	}
	for _, c := range cases {
		orig, err := os.ReadFile(filepath.Join("testdata/hevc", c.name))
		if err != nil {
			continue
		}
		chunked, rt, ok := c.unwrap(orig)
		if !ok {
			t.Errorf("%s: 不採用", c.name)
			continue
		}
		if !bytes.Equal(rt, orig) {
			t.Errorf("%s: 往復不一致", c.name)
			continue
		}
		zo := jpegProbeEncoder.EncodeAll(orig, nil)
		zc := jpegProbeEncoder.EncodeAll(chunked, nil)
		t.Logf("%s: %dB→%dB zstd比 %d→%d(%.2f%%)", c.name, len(orig), len(chunked),
			len(zo), len(zc), 100*(float64(len(zc))-float64(len(zo)))/float64(len(zo)))
	}
}
