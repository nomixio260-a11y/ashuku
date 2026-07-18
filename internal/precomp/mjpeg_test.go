package precomp

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// makeAVI は最小構成の MJPEG AVI を組み立てる(hdrl は形だけ、movi に
// JPEG フレーム列)。RIFF 構造として正しく、当パーサ・実プレイヤの
// 双方が歩ける形。
func makeAVI(t *testing.T, frames [][]byte) []byte {
	t.Helper()
	chunk := func(id string, body []byte) []byte {
		out := make([]byte, 0, 8+len(body)+1)
		out = append(out, id...)
		var sz [4]byte
		binary.LittleEndian.PutUint32(sz[:], uint32(len(body)))
		out = append(out, sz[:]...)
		out = append(out, body...)
		if len(body)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}
	list := func(typ string, body []byte) []byte {
		return chunk("LIST", append([]byte(typ), body...))
	}
	hdrl := list("hdrl", chunk("avih", make([]byte, 56)))
	var movi []byte
	for _, f := range frames {
		movi = append(movi, chunk("00dc", f)...)
	}
	body := append([]byte("AVI "), hdrl...)
	body = append(body, list("movi", movi)...)
	return chunk("RIFF", body)
}

func TestAVIMJPEGRoundTrip(t *testing.T) {
	var frames [][]byte
	for i := 0; i < 4; i++ {
		frames = append(frames, makePhoto(t, 160, 120, 85, int64(100+i)))
	}
	orig := makeAVI(t, frames)
	u, ok := TryUnwrapAVI(orig, 0)
	if !ok {
		t.Fatal("AVI/MJPEG が分解されない")
	}
	nFrames := 0
	for _, seg := range u.Recipe.Segments {
		if seg.PayloadLen > 0 {
			nFrames++
		}
	}
	if nFrames != 4 {
		t.Fatalf("分解フレーム数 %d != 4", nFrames)
	}
	back, err := ReconstructAVI(u.Recipe, u.Chunked)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, orig) {
		t.Fatal("AVI がビット一致で戻らない")
	}
	probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
	t.Logf("MJPEG AVI: orig=%d chunked=%d probe=%d (%.1f%%)",
		len(orig), len(u.Chunked), len(probe), 100*float64(len(probe))/float64(len(orig)))
}

func TestAVIRejectsGarbage(t *testing.T) {
	cases := [][]byte{
		nil,
		[]byte("RIFF"),
		append([]byte("RIFF\xff\xff\xff\xffAVI "), bytes.Repeat([]byte{0xAB}, 100)...),
		makeAVI(t, [][]byte{bytes.Repeat([]byte{1, 2, 3}, 100)}), // JPEG でないフレーム
	}
	for i, c := range cases {
		if _, ok := TryUnwrapAVI(c, 0); ok {
			t.Fatalf("case %d: 不正入力が採用された", i)
		}
	}
}
