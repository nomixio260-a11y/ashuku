package precomp

import (
	"bytes"
	"encoding/binary"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// makeWAV は 16-bit PCM の最小 WAV を組み立てる。
func makeWAV(nch, bps, rate int, samples []byte) []byte {
	var b bytes.Buffer
	dataLen := len(samples)
	byteRate := rate * nch * bps
	b.WriteString("RIFF")
	binary.Write(&b, binary.LittleEndian, uint32(36+dataLen))
	b.WriteString("WAVE")
	b.WriteString("fmt ")
	binary.Write(&b, binary.LittleEndian, uint32(16))
	binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&b, binary.LittleEndian, uint16(nch))
	binary.Write(&b, binary.LittleEndian, uint32(rate))
	binary.Write(&b, binary.LittleEndian, uint32(byteRate))
	binary.Write(&b, binary.LittleEndian, uint16(nch*bps))
	binary.Write(&b, binary.LittleEndian, uint16(bps*8))
	b.WriteString("data")
	binary.Write(&b, binary.LittleEndian, uint32(dataLen))
	b.Write(samples)
	if dataLen%2 == 1 {
		b.WriteByte(0)
	}
	return b.Bytes()
}

func TestWAVRoundTripSynthetic(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	// 相関のある 16-bit ステレオ(ランダムウォーク)= 予測が効く
	n := 8000
	buf := make([]byte, 0, n*4)
	var l, r int16
	for i := 0; i < n; i++ {
		l += int16(rng.Intn(200) - 100)
		r += int16(rng.Intn(200) - 100)
		var s [4]byte
		binary.LittleEndian.PutUint16(s[0:], uint16(l))
		binary.LittleEndian.PutUint16(s[2:], uint16(r))
		buf = append(buf, s[:]...)
	}
	orig := makeWAV(2, 2, 44100, buf)
	u, ok := TryUnwrapWAV(orig, 0)
	if !ok {
		t.Fatal("相関のある WAV が分解されない")
	}
	back, err := ReconstructWAV(u.Recipe, u.Chunked)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, orig) {
		t.Fatal("WAV がビット一致で戻らない")
	}
}

func TestWAVRoundTripTestdata(t *testing.T) {
	files, _ := filepath.Glob("testdata/wav/*.wav")
	if len(files) == 0 {
		t.Skip("wav testdata なし")
	}
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		u, ok := TryUnwrapWAV(orig, 0)
		if !ok {
			t.Logf("%s: 不採用(圧縮不能音声か)", fn)
			continue
		}
		back, err := ReconstructWAV(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		if !bytes.Equal(back, orig) {
			t.Fatalf("%s: ビット一致しない", fn)
		}
		probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
		t.Logf("%s: orig=%d resid+zstd≈%d (%.1f%%)", filepath.Base(fn), len(orig),
			len(probe)+len(u.Recipe.Suffix),
			100*float64(len(probe)+len(u.Recipe.Suffix)-len(orig))/float64(len(orig)))
	}
}

func TestWAVRejectsNonPCM(t *testing.T) {
	// float WAV(fmt=3)は対象外、雑音は不採用
	cases := [][]byte{
		nil,
		[]byte("RIFF\x00\x00\x00\x00WAVE"),
		makeWAV(1, 2, 8000, []byte{1, 2, 3, 4}), // 小さすぎ
	}
	for i, c := range cases {
		if _, ok := TryUnwrapWAV(c, 0); ok {
			t.Fatalf("case %d: 不正入力が採用された", i)
		}
	}
}

func TestAIFFRoundTripTestdata(t *testing.T) {
	files, _ := filepath.Glob("testdata/aiff/*.aiff")
	if len(files) == 0 {
		t.Skip("aiff testdata なし")
	}
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		if !IsAIFF(orig) {
			t.Fatalf("%s: AIFF と判定されない", fn)
		}
		u, ok := TryUnwrapAIFF(orig, 0)
		if !ok {
			t.Logf("%s: 不採用", fn)
			continue
		}
		back, err := ReconstructWAV(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(back, orig) {
			t.Fatalf("%s: ビット一致しない err=%v", fn, err)
		}
		if !u.Recipe.BigEndian {
			t.Fatal("AIFF なのに BigEndian=false")
		}
		probe := jpegProbeEncoder.EncodeAll(u.Chunked, nil)
		t.Logf("%s: orig=%d resid+zstd≈%d (%.1f%%)", filepath.Base(fn), len(orig),
			len(probe)+len(u.Recipe.Suffix),
			100*float64(len(probe)+len(u.Recipe.Suffix)-len(orig))/float64(len(orig)))
	}
}
