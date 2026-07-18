package precomp

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"testing"
)

func makeGoGIF(t *testing.T, frames int) []byte {
	t.Helper()
	pal := make(color.Palette, 256)
	for i := 0; i < 256; i++ {
		pal[i] = color.RGBA{uint8(i), uint8(i * 3), uint8(i * 7), 255}
	}
	g := &gif.GIF{}
	for f := 0; f < frames; f++ {
		img := image.NewPaletted(image.Rect(0, 0, 160, 120), pal)
		for y := 0; y < 120; y++ {
			for x := 0; x < 160; x++ {
				img.SetColorIndex(x, y, uint8((x/8*13+y/8*5+f*11+(x*y)%3)%256))
			}
		}
		g.Image = append(g.Image, img)
		g.Delay = append(g.Delay, 10)
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestGIFUnwrapRoundTrip(t *testing.T) {
	for _, frames := range []int{1, 5} {
		orig := makeGoGIF(t, frames)
		u, ok := TryUnwrapGIF(orig, 0)
		if !ok {
			t.Fatalf("frames=%d: 分解されない", frames)
		}
		back, err := ReconstructGIF(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("frames=%d: 再構成失敗: %v", frames, err)
		}
		if !bytes.Equal(back, orig) {
			t.Fatalf("frames=%d: ビット一致しない", frames)
		}
		// 生ピクセルが probe で元より縮むこと(採用ゲートの妥当性)
		probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
		t.Logf("frames=%d: orig=%d chunked=%d probe=%d (%.1f%%)",
			frames, len(orig), len(u.Chunked), len(probe),
			100*float64(len(probe))/float64(len(orig)))
	}
}

func TestGIFUnwrapRejectsGarbage(t *testing.T) {
	// GIF ヘッダだけの断片・壊れた入力で panic せず不採用になること
	cases := [][]byte{
		[]byte("GIF89a"),
		append([]byte("GIF89a"), bytes.Repeat([]byte{0xFF}, 64)...),
		nil,
		[]byte("notgif contents here padding padding padding"),
	}
	for i, c := range cases {
		if _, ok := TryUnwrapGIF(c, 0); ok {
			t.Fatalf("case %d: ゴミ入力が採用された", i)
		}
	}
}
