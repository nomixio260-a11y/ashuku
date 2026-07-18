//go:build cgo

package precomp

// precomp パーサのファジング。
//
// gzip / zlib / ZIP / PDF の分解は攻撃者が制御する入力を扱うため、
// どんな入力でもパニック・メモリ暴走せず、受理した場合は必ずビット一致で
// 再構成できる(可逆性の不変量)ことをファザーで検証する。
// CI では各ターゲットを短時間ずつ実行し、シードは通常の go test でも
// 毎回検証される。

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
)

// fuzzMax はファジング時の展開上限(メモリ暴走防止。実運用より小さめ)。
const fuzzMax = 4 << 20

func fuzzSeedGzip(f *testing.F) {
	plain := []byte("seed data for fuzzing! seed data for fuzzing! seed data")
	if stream, err := deflateExact(plain, 6); err == nil {
		var gz bytes.Buffer
		gz.Write([]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3})
		gz.Write(stream)
		var tr [8]byte
		binary.LittleEndian.PutUint32(tr[:], crc32.ChecksumIEEE(plain))
		binary.LittleEndian.PutUint32(tr[4:], uint32(len(plain)))
		gz.Write(tr[:])
		f.Add(gz.Bytes())
	}
	f.Add([]byte{0x1f, 0x8b, 8, 0})
	f.Add([]byte("not gzip at all, just some text to mutate around freely"))
}

func FuzzTryUnwrapGzip(f *testing.F) {
	fuzzSeedGzip(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrap(data, fuzzMax)
		if !ok {
			return
		}
		// 受理したなら必ずビット一致で再構成できる(可逆性の不変量)
		rebuilt, err := Reconstruct(u.Header, u.Level, u.Plain)
		if err != nil || !bytes.Equal(rebuilt, data) {
			t.Fatalf("受理した gzip の再構成が一致しません (err=%v)", err)
		}
	})
}

func FuzzTryUnwrapZlib(f *testing.F) {
	plain := []byte("zlib seed data zlib seed data zlib seed data zlib seed!")
	if s, err := ReconstructZlib([]byte{0x78, 0x9c}, 6, plain); err == nil {
		f.Add(s)
	}
	f.Add([]byte{0x78, 0x9c, 0x00})
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapZlib(data, fuzzMax)
		if !ok {
			return
		}
		rebuilt, err := ReconstructZlib(u.Header, u.Level, u.Plain)
		if err != nil || !bytes.Equal(rebuilt, data) {
			t.Fatalf("受理した zlib の再構成が一致しません (err=%v)", err)
		}
	})
}

func FuzzTryUnwrapZip(f *testing.F) {
	// 正常な最小 ZIP をシードに
	plain := []byte("zip member content zip member content zip member content")
	if stream, err := deflateExact(plain, 6); err == nil {
		var out bytes.Buffer
		w16 := func(v uint16) { binary.Write(&out, binary.LittleEndian, v) }
		w32 := func(v uint32) { binary.Write(&out, binary.LittleEndian, v) }
		out.WriteString("PK\x03\x04")
		w16(20)
		w16(0)
		w16(8)
		w16(0)
		w16(0x21)
		w32(crc32.ChecksumIEEE(plain))
		w32(uint32(len(stream)))
		w32(uint32(len(plain)))
		w16(1)
		w16(0)
		out.WriteString("a")
		out.Write(stream)
		cd := out.Len()
		out.WriteString("PK\x01\x02")
		w16(20)
		w16(20)
		w16(0)
		w16(8)
		w16(0)
		w16(0x21)
		w32(crc32.ChecksumIEEE(plain))
		w32(uint32(len(stream)))
		w32(uint32(len(plain)))
		w16(1)
		w16(0)
		w16(0)
		w16(0)
		w16(0)
		w32(0)
		w32(0)
		out.WriteString("a")
		sz := out.Len() - cd
		out.WriteString("PK\x05\x06")
		w16(0)
		w16(0)
		w16(1)
		w16(1)
		w32(uint32(sz))
		w32(uint32(cd))
		w16(0)
		f.Add(out.Bytes())
	}
	f.Add([]byte("PK\x03\x04 truncated"))
	f.Fuzz(func(t *testing.T, data []byte) {
		chunked, recipe, ok := TryUnwrapZip(data, fuzzMax)
		if !ok {
			return
		}
		rebuilt, err := ReconstructContainer(recipe, chunked)
		if err != nil || !bytes.Equal(rebuilt, data) {
			t.Fatalf("受理した ZIP の再構成が一致しません (err=%v)", err)
		}
	})
}

func FuzzTryUnwrapPDF(f *testing.F) {
	plain := []byte("pdf stream content pdf stream content pdf stream content")
	if s, err := ReconstructZlib([]byte{0x78, 0x9c}, 6, plain); err == nil {
		var pdf bytes.Buffer
		pdf.WriteString("%PDF-1.4\n1 0 obj\n<</Filter /FlateDecode>>\nstream\n")
		pdf.Write(s)
		pdf.WriteString("\nendstream\nendobj\n%%EOF\n")
		f.Add(pdf.Bytes())
	}
	f.Add([]byte("%PDF-1.4\nstream\nnot zlib\nendstream"))
	f.Fuzz(func(t *testing.T, data []byte) {
		chunked, recipe, ok := TryUnwrapPDF(data, fuzzMax)
		if !ok {
			return
		}
		rebuilt, err := ReconstructContainer(recipe, chunked)
		if err != nil || !bytes.Equal(rebuilt, data) {
			t.Fatalf("受理した PDF の再構成が一致しません (err=%v)", err)
		}
	})
}

// ReconstructContainer は攻撃者が保存済みレシピを直接制御はできないが、
// 防御的に不正レシピでもパニックしないことを確認する。
func FuzzReconstructContainer(f *testing.F) {
	f.Add(int64(10), int64(0), int64(5), 6, []byte("0123456789abcdef"))
	f.Fuzz(func(t *testing.T, skelLen, pos, plainLen int64, level int, chunked []byte) {
		recipe := &ContainerRecipe{SkelLen: skelLen, Segments: []ContainerSegment{
			{SkelPos: pos, PlainLen: plainLen, Level: level & 0xff},
		}}
		ReconstructContainer(recipe, chunked) // パニックしなければよい
	})
}

// JPEG パーサ/トランスコーダのファジング(受理した入力は必ずビット一致で
// 再構成できる、かつ壊れた入力でパニックしない)。
func FuzzTryUnwrapJPEG(f *testing.F) {
	// 正常な baseline JPEG をシードに
	img := image.NewYCbCr(image.Rect(0, 0, 64, 64), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = byte(i * 7)
	}
	for i := range img.Cb {
		img.Cb[i] = 128
		img.Cr[i] = 128
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 80})
	f.Add(buf.Bytes())
	f.Add([]byte("\xFF\xD8\xFF garbage not really a jpeg but has the magic bytes"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapJPEG(data, 8<<20)
		if !ok {
			return
		}
		got, err := ReconstructJPEG(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("受理した JPEG の再構成が一致しません (err=%v)", err)
		}
	})
}

// FuzzTryUnwrapGIF は攻撃者制御の GIF での分解が panic せず、採用された
// 入力は必ずビット一致で再構成できることを検証する。
func FuzzTryUnwrapGIF(f *testing.F) {
	pal := make(color.Palette, 4)
	for i := range pal {
		pal[i] = color.RGBA{uint8(i * 80), uint8(i * 60), uint8(i * 40), 255}
	}
	img := image.NewPaletted(image.Rect(0, 0, 32, 32), pal)
	for i := range img.Pix {
		img.Pix[i] = uint8(i % 4)
	}
	var buf bytes.Buffer
	gif.Encode(&buf, img, nil)
	f.Add(buf.Bytes())
	f.Add([]byte("GIF89a garbage not a real gif but has the magic"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapGIF(data, 8<<20)
		if !ok {
			return
		}
		back, err := ReconstructGIF(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("採用した GIF の再構成に失敗: %v", err)
		}
		if !bytes.Equal(back, data) {
			t.Fatal("採用した GIF がビット一致で戻らない")
		}
	})
}

// FuzzTryUnwrapAVI は攻撃者制御の AVI での分解が panic せず、採用された
// 入力は必ずビット一致で再構成できることを検証する。
func FuzzTryUnwrapAVI(f *testing.F) {
	frame := makePhotoFuzz(64, 64)
	chunk := func(id string, body []byte) []byte {
		out := append([]byte(nil), id...)
		var sz [4]byte
		binary.LittleEndian.PutUint32(sz[:], uint32(len(body)))
		out = append(out, sz[:]...)
		out = append(out, body...)
		if len(body)%2 == 1 {
			out = append(out, 0)
		}
		return out
	}
	movi := append([]byte("movi"), chunk("00dc", frame)...)
	body := append([]byte("AVI "), chunk("LIST", movi)...)
	f.Add(chunk("RIFF", body))
	f.Add([]byte("RIFF\x10\x00\x00\x00AVI LIST garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapAVI(data, 8<<20)
		if !ok {
			return
		}
		back, err := ReconstructAVI(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("採用した AVI の再構成に失敗: %v", err)
		}
		if !bytes.Equal(back, data) {
			t.Fatal("採用した AVI がビット一致で戻らない")
		}
	})
}

// makePhotoFuzz はシード用の小さな baseline JPEG を作る。
func makePhotoFuzz(w, h int) []byte {
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	for i := range img.Y {
		img.Y[i] = byte(i * 13)
	}
	for i := range img.Cb {
		img.Cb[i] = 128
		img.Cr[i] = 128
	}
	var buf bytes.Buffer
	jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85})
	return buf.Bytes()
}

// FuzzTryUnwrapProgressive は攻撃者制御の progressive JPEG での分解が
// panic せず、採用された入力は必ずビット一致で戻ることを検証する。
func FuzzTryUnwrapProgressive(f *testing.F) {
	files, _ := filepath.Glob("testdata/progjpeg/*.jpg")
	for _, fn := range files {
		if b, err := os.ReadFile(fn); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("\xFF\xD8\xFF\xC2 progressive-ish garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapJPEG(data, 8<<20)
		if !ok {
			return
		}
		back, err := ReconstructJPEG(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("採用した JPEG の再構成に失敗: %v", err)
		}
		if !bytes.Equal(back, data) {
			t.Fatal("採用した JPEG がビット一致で戻らない")
		}
	})
}

// FuzzTryUnwrapWAV は攻撃者制御の WAV での分解が panic せず、採用された
// 入力は必ずビット一致で戻ることを検証する。
func FuzzTryUnwrapWAV(f *testing.F) {
	files, _ := filepath.Glob("testdata/wav/*.wav")
	for _, fn := range files {
		if b, err := os.ReadFile(fn); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("RIFF\x24\x00\x00\x00WAVEfmt garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapWAV(data, 8<<20)
		if !ok {
			return
		}
		back, err := ReconstructWAV(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("採用した WAV の再構成に失敗: %v", err)
		}
		if !bytes.Equal(back, data) {
			t.Fatal("採用した WAV がビット一致で戻らない")
		}
	})
}

// FuzzTryUnwrapAIFF は攻撃者制御の AIFF での分解が panic せず、採用入力が
// ビット一致で戻ることを検証する。
func FuzzTryUnwrapAIFF(f *testing.F) {
	files, _ := filepath.Glob("testdata/aiff/*.aiff")
	for _, fn := range files {
		if b, err := os.ReadFile(fn); err == nil {
			f.Add(b)
		}
	}
	f.Add([]byte("FORM\x00\x00\x00\x10AIFFCOMM garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapAIFF(data, 8<<20)
		if !ok {
			return
		}
		back, err := ReconstructWAV(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("採用した AIFF の再構成に失敗: %v", err)
		}
		if !bytes.Equal(back, data) {
			t.Fatal("採用した AIFF がビット一致で戻らない")
		}
	})
}
