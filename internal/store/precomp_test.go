package store

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/jpeg"
	"math"
	"math/rand"
	"strings"
	"testing"

	"github.com/nomixio260-a11y/ashuku/internal/precomp"
)

// zlibGzip はテスト用に zlib 産 gzip を合成する(precomp パッケージの
// Reconstruct がシステム zlib そのものを使うことを利用)。
func zlibGzip(t *testing.T, plain []byte, level int) []byte {
	t.Helper()
	header := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3}
	orig, err := precomp.Reconstruct(header, level, plain)
	if err != nil {
		t.Fatal(err)
	}
	return orig
}

func textData(size int) []byte {
	line := "2026-07-15T00:00:00Z INFO precompression test line with some repetitive content\n"
	return bytes.Repeat([]byte(line), size/len(line)+1)[:size]
}

func TestPrecompGzipRoundTrip(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	orig := zlibGzip(t, textData(4<<20), 6)

	m := putBytes(t, s, "logs.gz", orig)
	if m.Encoding != EncodingGzipZlibV1 {
		t.Fatalf("encoding = %q, want %q", m.Encoding, EncodingGzipZlibV1)
	}
	if m.Size != int64(len(orig)) {
		t.Fatalf("size = %d, want %d(元の gzip サイズ)", m.Size, len(orig))
	}

	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(orig) {
		t.Fatal("gzip の復元がビット一致しません")
	}
}

// gzip 化されたログでも、展開データに対して圧縮・重複排除が効くことを確認。
// gzip のまま保存すると圧縮不能(raw)だが、precomp で展開されるため縮む。
func TestPrecompImprovesRatio(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	orig := zlibGzip(t, textData(8<<20), 6)
	putBytes(t, s, "logs.gz", orig)

	st, _ := s.Stats()
	// gzip 済み(≈2MB)をそのまま保存すると ratio ≈ 1。
	// 展開データ(8MB の高冗長テキスト)に対する zstd はさらに縮むので、
	// physical < gzip サイズとなるはず。
	if st.PhysicalBytes >= int64(len(orig)) {
		t.Fatalf("physical %d >= gzip %d: precomp の削減効果がありません",
			st.PhysicalBytes, len(orig))
	}
}

// 「同じデータの gzip を2世代」— gzip のままでは中身が丸ごと違うバイト列に
// なり dedup が効かないが、展開データ同士なら dedup/デルタが効く。
func TestPrecompEnablesCrossGzipDedup(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	plain1 := textData(8 << 20)
	plain2 := append(append([]byte("gen2 header line\n"), plain1...), []byte("tail change\n")...)

	putBytes(t, s, "gen1.gz", zlibGzip(t, plain1, 6))
	before, _ := s.Stats()
	m2 := putBytes(t, s, "gen2.gz", zlibGzip(t, plain2, 6))
	after, _ := s.Stats()

	added := after.PhysicalBytes - before.PhysicalBytes
	// gzip のまま保存していたら2世代目は丸ごと別バイト列(≈2MB)。
	// 展開データ越しなら差分のみ = 元データの 1/1000 未満に収まるはず。
	if added > int64(len(plain2))/1000 {
		t.Fatalf("2世代目の物理増分 %d bytes: gzip 越しの dedup が効いていません(1世代目 %d)",
			added, before.PhysicalBytes)
	}
	// 復元はビット一致
	got := getBytes(t, s, m2.ID)
	if sha256.Sum256(got) != sha256.Sum256(zlibGzip(t, plain2, 6)) {
		t.Fatal("gen2 の復元がビット一致しません")
	}
}

// zlib 産でない gzip(Go 標準)は素通しで通常保存され、正しく復元される。
func TestPrecompFallbackForNonZlibGzip(t *testing.T) {
	s := newTestStore(t)
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(textData(1 << 20))
	w.Close()
	orig := buf.Bytes()

	m := putBytes(t, s, "go.gz", orig)
	if m.Encoding != "" {
		t.Fatalf("Go産 gzip が precomp されました (encoding=%q)", m.Encoding)
	}
	got := getBytes(t, s, m.ID)
	if !bytes.Equal(got, orig) {
		t.Fatal("フォールバック保存の復元が一致しません")
	}
}

// precomp 無効設定では gzip もそのまま保存される。
func TestPrecompDisabled(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s, err := Open(t.TempDir(), Config{DisablePrecomp: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	orig := zlibGzip(t, textData(1<<20), 6)
	m := putBytes(t, s, "x.gz", orig)
	if m.Encoding != "" {
		t.Fatalf("無効設定なのに precomp されました (encoding=%q)", m.Encoding)
	}
	if got := getBytes(t, s, m.ID); !bytes.Equal(got, orig) {
		t.Fatal("復元が一致しません")
	}
}

// precomp されたファイルの削除で、展開データのチャンクが GC されることを確認。
func TestPrecompDeleteReleasesChunks(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	m := putBytes(t, s, "x.gz", zlibGzip(t, textData(4<<20), 9))
	if err := s.Delete(m.ID); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Stats()
	if st.ChunkCount != 0 || st.PhysicalBytes != 0 {
		t.Fatalf("削除後に chunks=%d physical=%d が残っています", st.ChunkCount, st.PhysicalBytes)
	}
}

// gzip マジックで始まるが gzip でない(切り詰められた)入力も安全に保存される。
func TestPrecompTruncatedGzipFallsBack(t *testing.T) {
	s := newTestStore(t)
	junk := append([]byte{0x1f, 0x8b, 8}, []byte(strings.Repeat("x", 100))...)
	m := putBytes(t, s, "junk.bin", junk)
	if m.Encoding != "" {
		t.Fatal("壊れた gzip が precomp されました")
	}
	if got := getBytes(t, s, m.ID); !bytes.Equal(got, junk) {
		t.Fatal("復元が一致しません")
	}
}

// ZIP コンテナ(zlib 産メンバー)が分解され、ビット一致で往復し、
// 「外側から zstd」より縮むことを確認する。
func TestPrecompZipRoundTrip(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)

	// 最小の手組み ZIP(1 deflate メンバー、zlib 産ストリーム)
	plain := textData(3 << 20)
	full, err := precomp.ReconstructZlib([]byte{0x78, 0x9c}, 6, plain)
	if err != nil {
		t.Fatal(err)
	}
	stream := full[2 : len(full)-4] // 生 deflate 部分
	orig := buildMinimalZip(t, "doc.xml", plain, stream)

	m := putBytes(t, s, "archive.zip", orig)
	if m.Encoding != EncodingZipV1 {
		t.Fatalf("encoding = %q, want %q", m.Encoding, EncodingZipV1)
	}
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(orig) {
		t.Fatal("ZIP の復元がビット一致しません")
	}
	st, _ := s.Stats()
	// deflate 済み ZIP は外側からは縮まない(≈1.0x)が、分解により
	// 展開データが zstd-19 で再圧縮されて物理が元より小さくなる
	if st.PhysicalBytes >= int64(len(orig)) {
		t.Fatalf("physical %d >= zip %d: 分解による削減が効いていません",
			st.PhysicalBytes, len(orig))
	}
	t.Logf("zip %d bytes → physical %d bytes (%.2fx)",
		len(orig), st.PhysicalBytes, float64(len(orig))/float64(st.PhysicalBytes))
}

// PDF(FlateDecode)が分解され、ビット一致で往復する。
func TestPrecompPDFRoundTrip(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	content := textData(2 << 20)
	stream, err := precomp.ReconstructZlib([]byte{0x78, 0x9c}, 6, content)
	if err != nil {
		t.Fatal(err)
	}
	var pdf bytes.Buffer
	fmt.Fprintf(&pdf, "%%PDF-1.4\n1 0 obj\n<</Length %d /Filter /FlateDecode>>\nstream\n", len(stream))
	pdf.Write(stream)
	pdf.WriteString("\nendstream\nendobj\ntrailer\n%%EOF\n")
	orig := append([]byte(nil), pdf.Bytes()...)

	m := putBytes(t, s, "doc.pdf", orig)
	if m.Encoding != EncodingPDFV1 {
		t.Fatalf("encoding = %q, want %q", m.Encoding, EncodingPDFV1)
	}
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(orig) {
		t.Fatal("PDF の復元がビット一致しません")
	}
	st, _ := s.Stats()
	if st.PhysicalBytes >= int64(len(orig)) {
		t.Fatalf("physical %d >= pdf %d: 分解による削減が効いていません",
			st.PhysicalBytes, len(orig))
	}
}

// buildMinimalZip はテスト用の最小 ZIP(deflate メンバー1つ)を手組みする。
func buildMinimalZip(t *testing.T, name string, plain, stream []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w16 := func(v uint16) { binary.Write(&out, binary.LittleEndian, v) }
	w32 := func(v uint32) { binary.Write(&out, binary.LittleEndian, v) }
	out.WriteString("PK\x03\x04")
	w16(20)
	w16(0)
	w16(8) // deflate
	w16(0)
	w16(0x21)
	w32(crc32.ChecksumIEEE(plain))
	w32(uint32(len(stream)))
	w32(uint32(len(plain)))
	w16(uint16(len(name)))
	w16(0)
	out.WriteString(name)
	out.Write(stream)
	cdStart := out.Len()
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
	w16(uint16(len(name)))
	w16(0)
	w16(0)
	w16(0)
	w16(0)
	w32(0)
	w32(0) // local header offset
	out.WriteString(name)
	cdSize := out.Len() - cdStart
	out.WriteString("PK\x05\x06")
	w16(0)
	w16(0)
	w16(1)
	w16(1)
	w32(uint32(cdSize))
	w32(uint32(cdStart))
	w16(0)
	return out.Bytes()
}

// makeJPEG は写真風の合成画像を baseline JPEG で符号化して返す。
func makeJPEG(t *testing.T, seed int64, quality int) []byte {
	t.Helper()
	const w, h = 640, 480
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	rng := rand.New(rand.NewSource(seed))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 128 + 80*math.Sin(float64(x)/40) + 50*math.Cos(float64(y)/55) + float64(rng.Intn(14)-7)
			img.Y[img.YOffset(x, y)] = clampU8(v)
		}
	}
	for i := range img.Cb {
		img.Cb[i] = clampU8(128 + 30*math.Sin(float64(i)/300))
		img.Cr[i] = clampU8(128 + 28*math.Cos(float64(i)/260))
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func clampU8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

// JPEG は分解して保存され、ビット一致で復元でき、物理が元より小さくなる。
func TestPrecompJPEGRoundTripAndGain(t *testing.T) {
	s := newTestStore(t)
	jpg := makeJPEG(t, 1, 85)
	m := putBytes(t, s, "photo.jpg", jpg)
	if m.Encoding != EncodingJPEGV1 {
		t.Fatalf("encoding = %q, want %q(分解されていない)", m.Encoding, EncodingJPEGV1)
	}
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(jpg) {
		t.Fatal("JPEG の復元がビット一致しません")
	}
	st, _ := s.Stats()
	if st.PhysicalBytes >= int64(len(jpg)) {
		t.Fatalf("physical %d >= jpeg %d: 分解で縮んでいません", st.PhysicalBytes, len(jpg))
	}
	t.Logf("JPEG %d → 物理 %d (%.1f%% 削減)", len(jpg), st.PhysicalBytes,
		100*(1-float64(st.PhysicalBytes)/float64(len(jpg))))
}

// 同一 JPEG を2枚アップロードすると、係数平面が一致してチャンク重複排除が
// 効き、2枚目の物理増分がほぼゼロになる(生 JPEG バイトでも exact dedup は
// 効くが、分解形は near-dup にも効くための基盤)。
func TestPrecompJPEGDedup(t *testing.T) {
	s := newTestStore(t)
	jpg := makeJPEG(t, 2, 88)
	putBytes(t, s, "a.jpg", jpg)
	before, _ := s.Stats()
	putBytes(t, s, "b.jpg", jpg)
	after, _ := s.Stats()
	added := after.PhysicalBytes - before.PhysicalBytes
	if added > int64(len(jpg))/10 {
		t.Fatalf("2枚目の物理増分 = %d(重複排除が効いていない)", added)
	}
}
