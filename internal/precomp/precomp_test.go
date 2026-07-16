package precomp

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"hash/crc32"
	"math/rand"
	"strings"
	"testing"
)

// zlibGzip は「zlib 産の gzip」をテスト用に合成する(自前の deflateExact が
// システム zlib そのものなので、これが zlib 産の定義と一致する)。
func zlibGzip(t *testing.T, plain []byte, level int, name string) []byte {
	t.Helper()
	stream, err := deflateExact(plain, level)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	flags := byte(0)
	if name != "" {
		flags |= flagName
	}
	buf.Write([]byte{0x1f, 0x8b, 8, flags, 0x12, 0x34, 0x56, 0x78, 0, 3})
	if name != "" {
		buf.WriteString(name)
		buf.WriteByte(0)
	}
	buf.Write(stream)
	var trailer [8]byte
	binary.LittleEndian.PutUint32(trailer[:], crc32.ChecksumIEEE(plain))
	binary.LittleEndian.PutUint32(trailer[4:], uint32(len(plain)))
	buf.Write(trailer[:])
	return buf.Bytes()
}

func testPlain(size int) []byte {
	rng := rand.New(rand.NewSource(5))
	words := []string{"storage", "compress", "delta", "chunk", "backup", "zlib", "stream"}
	var b strings.Builder
	for b.Len() < size {
		b.WriteString(words[rng.Intn(len(words))])
		b.WriteByte(' ')
	}
	return []byte(b.String()[:size])
}

func TestUnwrapAndReconstructRoundTrip(t *testing.T) {
	if !Supported() {
		t.Skip("CGO 無効")
	}
	plain := testPlain(1 << 20)
	for _, level := range []int{1, 6, 9} {
		for _, name := range []string{"", "app.log"} {
			orig := zlibGzip(t, plain, level, name)
			u, ok := TryUnwrap(orig, 0)
			if !ok {
				t.Fatalf("level=%d name=%q: 分解に失敗", level, name)
			}
			if u.Level != level {
				t.Fatalf("level = %d, want %d", u.Level, level)
			}
			if !bytes.Equal(u.Plain, plain) {
				t.Fatal("展開データが一致しません")
			}
			rec, err := Reconstruct(u.Header, u.Level, u.Plain)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(rec, orig) {
				t.Fatalf("level=%d name=%q: 再構成がビット一致しません", level, name)
			}
		}
	}
}

// Go 標準の gzip.Writer 産のストリームは zlib と一致しないので、
// 正しく「分解不可」と判定されることを確認する(素通し保存に落ちる)。
func TestGoGzipFallsBack(t *testing.T) {
	if !Supported() {
		t.Skip("CGO 無効")
	}
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(testPlain(256 << 10))
	w.Close()
	if _, ok := TryUnwrap(buf.Bytes(), 0); ok {
		t.Fatal("Go 産 gzip が zlib 産と誤判定されました")
	}
}

func TestCorruptTrailerRejected(t *testing.T) {
	if !Supported() {
		t.Skip("CGO 無効")
	}
	orig := zlibGzip(t, testPlain(64<<10), 6, "")
	orig[len(orig)-2] ^= 0xff // ISIZE を壊す
	if _, ok := TryUnwrap(orig, 0); ok {
		t.Fatal("トレーラ破損が検出されていません")
	}
}

func TestNonGzipRejected(t *testing.T) {
	if _, ok := TryUnwrap([]byte("not a gzip stream at all........"), 0); ok {
		t.Fatal("非 gzip が受理されました")
	}
}
