package store

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"math"
	"math/rand"
	"os"
	"path/filepath"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestGIFPrecompEndToEnd(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	// Go 産 GIF(アニメ)を作る
	pal := make(color.Palette, 256)
	for i := 0; i < 256; i++ {
		pal[i] = color.RGBA{uint8(i), uint8(i * 3), uint8(i * 7), 255}
	}
	g := &gif.GIF{}
	for f := 0; f < 4; f++ {
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
	orig := buf.Bytes()
	m := putBytes(t, s, "anim.gif", orig)
	if m.Encoding != EncodingGIFV1 {
		t.Fatalf("GIF が分解されていない: encoding=%q", m.Encoding)
	}
	got := getBytes(t, s, m.ID)
	if !bytes.Equal(got, orig) {
		t.Fatal("GIF の読み戻しがビット一致しない")
	}
	st, _ := s.Stats()
	t.Logf("GIF: logical=%d physical=%d (%.1fx)", st.LogicalBytes, st.PhysicalBytes,
		float64(st.LogicalBytes)/float64(st.PhysicalBytes))
	if st.PhysicalBytes >= int64(len(orig)) {
		t.Fatalf("GIF が縮んでいない: %d -> %d", len(orig), st.PhysicalBytes)
	}
}

// makeAVIStore は MJPEG AVI をテスト用に組み立てる(precomp パッケージの
// makeAVI と同型)。frames は JPEG バイト列。
func makeAVIStore(frames [][]byte) []byte {
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

// makeVideoFrame は「前フレームと少しだけ違う」写真調 JPEG を作る
// (実際の MJPEG 動画のフレーム間相関を模す)。
func makeVideoFrame(t *testing.T, frame int) []byte {
	t.Helper()
	w, h := 320, 240
	img := image.NewYCbCr(image.Rect(0, 0, w, h), image.YCbCrSubsampleRatio420)
	rng := rand.New(rand.NewSource(77)) // 全フレーム共通の背景
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			base := 40 + (x/16)*8 + (y/16)*5 + rng.Intn(6)
			// 動く矩形(フレームごとに位置が少しずれる)
			if x > 40+frame*6 && x < 100+frame*6 && y > 60 && y < 140 {
				base += 80
			}
			img.Y[y*img.YStride+x] = uint8(base % 255)
		}
	}
	for i := range img.Cb {
		img.Cb[i] = 120
		img.Cr[i] = 132
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 85}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestMJPEGVideoEndToEnd(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	var frames [][]byte
	var frameTotal int
	for i := 0; i < 12; i++ {
		f := makeVideoFrame(t, i)
		frameTotal += len(f)
		frames = append(frames, f)
	}
	orig := makeAVIStore(frames)
	m := putBytes(t, s, "video.avi", orig)
	if m.Encoding != EncodingAVIV1 {
		t.Fatalf("AVI が分解されていない: encoding=%q", m.Encoding)
	}
	got := getBytes(t, s, m.ID)
	if !bytes.Equal(got, orig) {
		t.Fatal("AVI の読み戻しがビット一致しない")
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	got = getBytes(t, s, m.ID)
	if !bytes.Equal(got, orig) {
		t.Fatal("Optimize 後の読み戻しがビット一致しない")
	}
	st, _ := s.Stats()
	t.Logf("MJPEG動画: 元=%d 物理=%d (%.2fx = %.1f%%削減)", len(orig), st.PhysicalBytes,
		float64(len(orig))/float64(st.PhysicalBytes),
		100*(1-float64(st.PhysicalBytes)/float64(len(orig))))
	if st.PhysicalBytes >= int64(len(orig)) {
		t.Fatal("MJPEG 動画が縮んでいない")
	}
}

func TestProgressiveJPEGEndToEnd(t *testing.T) {
	t.Parallel()
	files, _ := filepath.Glob("../precomp/testdata/progjpeg/*.jpg")
	if len(files) == 0 {
		t.Skip("progjpeg testdata なし")
	}
	s := newTestStore(t)
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		m := putBytes(t, s, filepath.Base(fn), orig)
		if m.Encoding != EncodingJPEGV1 {
			t.Fatalf("%s: progressive JPEG が分解されていない: %q", fn, m.Encoding)
		}
		got := getBytes(t, s, m.ID)
		if !bytes.Equal(got, orig) {
			t.Fatalf("%s: 読み戻しがビット一致しない", fn)
		}
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	// Optimize 後も全ファイル一致
	list, _, _ := s.ListPage("", "", 0)
	for _, fm := range list {
		orig, _ := os.ReadFile("../precomp/testdata/progjpeg/" + fm.Name)
		got := getBytes(t, s, fm.ID)
		if !bytes.Equal(got, orig) {
			t.Fatalf("%s: Optimize 後にビット一致しない", fm.Name)
		}
	}
}

func TestWAVPrecompEndToEnd(t *testing.T) {
	t.Parallel()
	files, _ := filepath.Glob("../precomp/testdata/wav/*.wav")
	aiffs, _ := filepath.Glob("../precomp/testdata/aiff/*.aiff")
	files = append(files, aiffs...)
	if len(files) == 0 {
		t.Skip("wav testdata なし")
	}
	s := newTestStore(t)
	adopted := 0
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		m := putBytes(t, s, filepath.Base(fn), orig)
		if m.Encoding == EncodingWAVV1 || m.Encoding == EncodingAIFFV1 {
			adopted++
		}
		got := getBytes(t, s, m.ID)
		if !bytes.Equal(got, orig) {
			t.Fatalf("%s: 読み戻しがビット一致しない (encoding=%q)", fn, m.Encoding)
		}
	}
	if adopted == 0 {
		t.Fatal("WAV が1つも分解されなかった")
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	list, _, _ := s.ListPage("", "", 0)
	for _, fm := range list {
		orig, _ := readAudioTestdata(fm.Name)
		if !bytes.Equal(getBytes(t, s, fm.ID), orig) {
			t.Fatalf("%s: Optimize 後にビット一致しない", fm.Name)
		}
	}
}

func readAudioTestdata(name string) ([]byte, error) {
	for _, d := range []string{"../precomp/testdata/wav/", "../precomp/testdata/aiff/"} {
		if b, err := os.ReadFile(d + name); err == nil {
			return b, nil
		}
	}
	return nil, os.ErrNotExist
}

func TestBMPPrecompEndToEnd(t *testing.T) {
	t.Parallel()
	files, _ := filepath.Glob("../precomp/testdata/bmp/*.bmp")
	if len(files) == 0 {
		t.Skip("bmp testdata なし")
	}
	s := newTestStore(t)
	adopted := 0
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		m := putBytes(t, s, filepath.Base(fn), orig)
		if m.Encoding == EncodingBMPV1 {
			adopted++
		}
		if !bytes.Equal(getBytes(t, s, m.ID), orig) {
			t.Fatalf("%s: 読み戻しがビット一致しない", fn)
		}
	}
	if adopted == 0 {
		t.Fatal("BMP が1つも分解されなかった")
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
}

func TestTIFFPrecompEndToEnd(t *testing.T) {
	t.Parallel()
	files, _ := filepath.Glob("../precomp/testdata/tiff/*.tiff")
	if len(files) == 0 {
		t.Skip("tiff testdata なし")
	}
	s := newTestStore(t)
	adopted := 0
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		m := putBytes(t, s, filepath.Base(fn), orig)
		if m.Encoding == EncodingTIFFV1 {
			adopted++
		}
		if !bytes.Equal(getBytes(t, s, m.ID), orig) {
			t.Fatalf("%s: 読み戻しがビット一致しない", fn)
		}
	}
	if adopted == 0 {
		t.Fatal("非圧縮 TIFF が1つも分解されなかった")
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
}

// TestCSVPrecompEndToEnd は矩形 CSV を投入→列指向で分解採用→読み戻しビット
// 一致→Optimize 後もビット一致、を実ストアで確認する。物理サイズが行指向の
// 素通し保存より小さいことも確認する。
func TestCSVPrecompEndToEnd(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(11))
	var b bytes.Buffer
	b.WriteString("ts,user,action,latency_ms,bytes,ok,region\n")
	regions := []string{"us-east", "us-west", "eu-central", "ap-south"}
	acts := []string{"get", "put", "del", "list"}
	ts := 1700000000
	for i := 0; i < 20000; i++ {
		ts += rng.Intn(5)
		fmt.Fprintf(&b, "%d,user%d,%s,%d,%d,%d,%s\n",
			ts, rng.Intn(5000), acts[rng.Intn(4)], rng.Intn(2000),
			rng.Intn(1000000), rng.Intn(2), regions[rng.Intn(4)])
	}
	data := b.Bytes()

	s := newTestStore(t)
	m := putBytes(t, s, "access.csv", data)
	if m.Encoding != EncodingCSVV1 {
		t.Fatalf("CSV が列指向分解されなかった: encoding=%q", m.Encoding)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("読み戻しがビット一致しない")
	}

	// 列指向の物理サイズ < 素通し(precomp無効)で保存した物理サイズ。
	s2 := newTestStore(t)
	m2, err := s2.PutWithOptions("access.csv", bytes.NewReader(data), PutOptions{DisablePrecomp: true})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Encoding != "" {
		t.Fatalf("DisablePrecomp なのに分解された: %q", m2.Encoding)
	}
	st1, _ := s.Stats()
	st2, _ := s2.Stats()
	t.Logf("列指向 物理=%d / 行指向素通し 物理=%d (-%.1f%%)",
		st1.PhysicalBytes, st2.PhysicalBytes,
		float64(st2.PhysicalBytes-st1.PhysicalBytes)*100/float64(st2.PhysicalBytes))
	if st1.PhysicalBytes >= st2.PhysicalBytes {
		t.Fatalf("列指向が行指向より縮んでいない: %d >= %d", st1.PhysicalBytes, st2.PhysicalBytes)
	}

	// Optimize 後もビット一致・スクラブ緑。
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("Optimize 後の読み戻しが一致しない")
	}
	sr, err := s.Scrub()
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.Corrupt) != 0 || len(sr.Missing) != 0 {
		t.Fatalf("スクラブで破損検出: %+v", sr)
	}
}

// TestJSONLPrecompEndToEnd は同一スキーマ JSONL を投入→骨格+列指向で分解採用→
// 読み戻しビット一致→行指向素通しより物理が小さい、を実ストアで確認する。
func TestJSONLPrecompEndToEnd(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(13))
	regions := []string{"us-east", "us-west", "eu-central", "ap-south"}
	acts := []string{"get", "put", "del", "list"}
	var b bytes.Buffer
	ts := 1700000000
	for i := 0; i < 20000; i++ {
		ts += rng.Intn(5)
		fmt.Fprintf(&b, `{"ts":%d,"user":"user%d","action":"%s","latency_ms":%d,"bytes":%d,"ok":%t,"region":"%s"}`+"\n",
			ts, rng.Intn(5000), acts[rng.Intn(4)], rng.Intn(2000),
			rng.Intn(1000000), rng.Intn(2) == 0, regions[rng.Intn(4)])
	}
	data := b.Bytes()

	s := newTestStore(t)
	m := putBytes(t, s, "events.jsonl", data)
	if m.Encoding != EncodingJSONLV1 {
		t.Fatalf("JSONL が列指向分解されなかった: encoding=%q", m.Encoding)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("読み戻しがビット一致しない")
	}

	s2 := newTestStore(t)
	m2, err := s2.PutWithOptions("events.jsonl", bytes.NewReader(data), PutOptions{DisablePrecomp: true})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Encoding != "" {
		t.Fatalf("DisablePrecomp なのに分解された: %q", m2.Encoding)
	}
	st1, _ := s.Stats()
	st2, _ := s2.Stats()
	t.Logf("列指向 物理=%d / 行指向素通し 物理=%d (-%.1f%%)",
		st1.PhysicalBytes, st2.PhysicalBytes,
		float64(st2.PhysicalBytes-st1.PhysicalBytes)*100/float64(st2.PhysicalBytes))
	if st1.PhysicalBytes >= st2.PhysicalBytes {
		t.Fatalf("列指向が行指向より縮んでいない: %d >= %d", st1.PhysicalBytes, st2.PhysicalBytes)
	}

	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("Optimize 後の読み戻しが一致しない")
	}
	sr, err := s.Scrub()
	if err != nil {
		t.Fatal(err)
	}
	if len(sr.Corrupt) != 0 || len(sr.Missing) != 0 {
		t.Fatalf("スクラブで破損検出: %+v", sr)
	}
}

// TestLogPrecompEndToEnd は nginx combined ログを投入→骨格+列指向で分解採用→
// 読み戻しビット一致→行指向素通しより物理が小さい、を実ストアで確認する。
func TestLogPrecompEndToEnd(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(21))
	methods := []string{"GET", "GET", "POST", "HEAD"}
	paths := []string{"/", "/index.html", "/api/v1/users", "/static/app.js", "/favicon.ico"}
	codes := []int{200, 200, 304, 404, 500}
	uas := []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0", "curl/7.68.0", "Mozilla/5.0 (iPhone; CPU iPhone OS 14_0)"}
	var b bytes.Buffer
	sec := 0
	for i := 0; i < 20000; i++ {
		sec += rng.Intn(3)
		fmt.Fprintf(&b, "10.%d.%d.%d - - [%02d/Jul/2026:%02d:%02d:%02d +0000] \"%s %s HTTP/1.1\" %d %d \"-\" \"%s\"\n",
			rng.Intn(256), rng.Intn(256), rng.Intn(256), 1+rng.Intn(28),
			(sec/3600)%24, (sec/60)%60, sec%60, methods[rng.Intn(len(methods))], paths[rng.Intn(len(paths))],
			codes[rng.Intn(len(codes))], rng.Intn(50000), uas[rng.Intn(len(uas))])
	}
	data := b.Bytes()

	s := newTestStore(t)
	m := putBytes(t, s, "access.log", data)
	if m.Encoding != EncodingLogV1 {
		t.Fatalf("ログが列指向分解されなかった: encoding=%q", m.Encoding)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("読み戻しがビット一致しない")
	}

	s2 := newTestStore(t)
	m2, err := s2.PutWithOptions("access.log", bytes.NewReader(data), PutOptions{DisablePrecomp: true})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Encoding != "" {
		t.Fatalf("DisablePrecomp なのに分解された: %q", m2.Encoding)
	}
	st1, _ := s.Stats()
	st2, _ := s2.Stats()
	t.Logf("列指向 物理=%d / 行指向素通し 物理=%d (-%.1f%%)",
		st1.PhysicalBytes, st2.PhysicalBytes,
		float64(st2.PhysicalBytes-st1.PhysicalBytes)*100/float64(st2.PhysicalBytes))
	if st1.PhysicalBytes >= st2.PhysicalBytes {
		t.Fatalf("列指向が行指向より縮んでいない: %d >= %d", st1.PhysicalBytes, st2.PhysicalBytes)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("読み戻し再確認が一致しない")
	}
}

// TestRecursivePrecompGzipColumnar は .log.gz / .csv.gz を投入→ gzip 展開
// →内側で列指向化(再帰 precompression)→読み戻しビット一致→内側なし
// (gzip 展開のみ)より物理が小さい、を実ストアで確認する。
func TestRecursivePrecompGzipColumnar(t *testing.T) {
	t.Parallel()
	if !precomp.Supported() {
		t.Skip("CGO 無効(zlib 産 gzip を作れない)")
	}
	// nginx combined ログを gzip したデータ。
	rng := rand.New(rand.NewSource(41))
	methods := []string{"GET", "GET", "POST", "HEAD"}
	paths := []string{"/", "/index.html", "/api/v1/users", "/static/app.js", "/favicon.ico"}
	codes := []int{200, 200, 304, 404, 500}
	uas := []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0", "curl/7.68.0"}
	var logbuf bytes.Buffer
	sec := 0
	for i := 0; i < 15000; i++ {
		sec += rng.Intn(3)
		fmt.Fprintf(&logbuf, "10.%d.%d.%d - - [%02d/Jul/2026:%02d:%02d:%02d +0000] \"%s %s HTTP/1.1\" %d %d \"-\" \"%s\"\n",
			rng.Intn(256), rng.Intn(256), rng.Intn(256), 1+rng.Intn(28),
			(sec/3600)%24, (sec/60)%60, sec%60, methods[rng.Intn(len(methods))], paths[rng.Intn(len(paths))],
			codes[rng.Intn(len(codes))], rng.Intn(50000), uas[rng.Intn(len(uas))])
	}
	data := zlibGzip(t, logbuf.Bytes(), 6) // zlib 産 gzip(precomp が展開できる)

	s := newTestStore(t)
	m := putBytes(t, s, "access.log.gz", data)
	if m.Encoding != EncodingGzipZlibV1 {
		t.Fatalf("gzip 展開されなかった: encoding=%q", m.Encoding)
	}
	if m.InnerEncoding != EncodingLogV1 {
		t.Fatalf("内側でログ列指向化されなかった: inner=%q", m.InnerEncoding)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("読み戻しがビット一致しない(gzip+内側列指向の合成)")
	}
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("Optimize 後の読み戻しが一致しない")
	}
	st1, _ := s.Stats()
	t.Logf("gzip+内側列指向 物理=%d(元 gzip=%dB, 展開=%dB)", st1.PhysicalBytes, len(data), logbuf.Len())
}

// TestBase64PrecompEndToEnd は base64 で符号化した圧縮可能バイナリの束
// (証明書束・ペイロードダンプ様)を投入→復号分解採用→読み戻しビット一致→
// base64 素通しより物理が小さい、を実ストアで確認する。
func TestBase64PrecompEndToEnd(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewSource(31))
	words := [][]byte{[]byte("record"), []byte("value"), []byte("field"), []byte("entry")}
	blob := func(n int) []byte {
		var b bytes.Buffer
		for b.Len() < n {
			b.Write(words[rng.Intn(len(words))])
			b.WriteByte(byte(rng.Intn(16)))
		}
		return b.Bytes()[:n]
	}
	var buf bytes.Buffer
	for i := 0; i < 12; i++ {
		enc := base64.StdEncoding.EncodeToString(blob(8192))
		fmt.Fprintf(&buf, "-----BEGIN BLOB %d-----\n%s\n-----END BLOB %d-----\n", i, enc, i)
	}
	data := buf.Bytes()

	s := newTestStore(t)
	m := putBytes(t, s, "bundle.b64", data)
	if m.Encoding != EncodingBase64V1 {
		t.Fatalf("base64 が復号分解されなかった: encoding=%q", m.Encoding)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("読み戻しがビット一致しない")
	}

	s2 := newTestStore(t)
	m2, err := s2.PutWithOptions("bundle.b64", bytes.NewReader(data), PutOptions{DisablePrecomp: true})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Encoding != "" {
		t.Fatalf("DisablePrecomp なのに分解された: %q", m2.Encoding)
	}
	st1, _ := s.Stats()
	st2, _ := s2.Stats()
	t.Logf("復号形 物理=%d / base64 素通し 物理=%d (-%.1f%%)",
		st1.PhysicalBytes, st2.PhysicalBytes,
		float64(st2.PhysicalBytes-st1.PhysicalBytes)*100/float64(st2.PhysicalBytes))
	if st1.PhysicalBytes >= st2.PhysicalBytes {
		t.Fatalf("base64 復号で物理が減っていません: %d >= %d", st1.PhysicalBytes, st2.PhysicalBytes)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), data) {
		t.Fatal("読み戻し再確認が一致しない")
	}
}

// TestH264PrecompEndToEnd は CAVLC の H.264 を投入→文脈算術で分解採用→
// 読み戻しビット一致→物理縮小、を実ストアで確認する。
func TestH264PrecompEndToEnd(t *testing.T) {
	t.Parallel()
	files, _ := filepath.Glob("../precomp/testdata/h264/v_*.h264")
	if len(files) == 0 {
		t.Skip("h264 testdata なし")
	}
	s := newTestStore(t)
	adopted := 0
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		m := putBytes(t, s, filepath.Base(fn), orig)
		if m.Encoding == EncodingH264V1 {
			adopted++
		}
		if !bytes.Equal(getBytes(t, s, m.ID), orig) {
			t.Fatalf("%s: 読み戻しがビット一致しない", fn)
		}
	}
	if adopted == 0 {
		t.Fatal("CAVLC H.264 が1つも分解されなかった")
	}
	st, _ := s.Stats()
	t.Logf("採用 %d/%d 物理=%d", adopted, len(files), st.PhysicalBytes)
	if _, err := s.Optimize(); err != nil {
		t.Fatal(err)
	}
	for _, fn := range files {
		orig, _ := os.ReadFile(fn)
		var id string
		l, _ := s.List("")
		for _, f := range l {
			if f.Name == filepath.Base(fn) {
				id = f.ID
			}
		}
		if !bytes.Equal(getBytes(t, s, id), orig) {
			t.Fatalf("%s: Optimize 後の読み戻しが一致しない", fn)
		}
	}
}

// TestMP4H264PrecompEndToEnd は MP4(CAVLC 動画+AAC 音声)の実ストア往復。
func TestMP4H264PrecompEndToEnd(t *testing.T) {
	t.Parallel()
	orig, err := os.ReadFile("../precomp/testdata/h264/v_av.mp4")
	if err != nil {
		t.Skip("mp4 testdata なし")
	}
	s := newTestStore(t)
	m := putBytes(t, s, "v_av.mp4", orig)
	if m.Encoding != EncodingMP4V1 {
		t.Fatalf("MP4 CAVLC が分解されなかった: encoding=%q", m.Encoding)
	}
	if !bytes.Equal(getBytes(t, s, m.ID), orig) {
		t.Fatal("読み戻しがビット一致しない")
	}
	st, _ := s.Stats()
	t.Logf("mp4 %d -> 物理 %d (-%.1f%%)", len(orig), st.PhysicalBytes,
		100*float64(int64(len(orig))-st.PhysicalBytes)/float64(len(orig)))
}
