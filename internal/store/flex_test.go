package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"hash/crc32"
	"math/rand"
	"testing"

	"github.com/nomixio260-a11y/ashuku/internal/precomp"
)

// pseudoText は語彙を再利用する疑似自然文(zstd で3倍程度に縮む)。
func pseudoText(size int) []byte {
	rng := rand.New(rand.NewSource(21))
	words := []string{"chunk", "delta", "storage", "compression", "backup",
		"generation", "physical", "logical", "stream", "manifest", "engine"}
	var buf bytes.Buffer
	buf.Grow(size + 16)
	for buf.Len() < size {
		buf.WriteString(words[rng.Intn(len(words))])
		if rng.Intn(10) == 0 {
			buf.WriteString(".\n")
		} else {
			buf.WriteByte(' ')
		}
	}
	return buf.Bytes()[:size]
}

// auto モード: 圧縮可能データは max 並みに縮み、乱数は膨張しない。
func TestAutoCompressionMode(t *testing.T) {
	sAuto, err := Open(t.TempDir(), Config{Compression: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	defer sAuto.Close()
	sMax, err := Open(t.TempDir(), Config{Compression: "max"})
	if err != nil {
		t.Fatal(err)
	}
	defer sMax.Close()

	text := pseudoText(8 << 20)
	putBytes(t, sAuto, "text", text)
	putBytes(t, sMax, "text", text)
	stAuto, _ := sAuto.Stats()
	stMax, _ := sMax.Stats()
	// auto は max の物理サイズの 5% 以内に収まるはず(圧縮可能データは
	// 最高レベルで再圧縮されるため)
	if stAuto.PhysicalBytes > stMax.PhysicalBytes*105/100 {
		t.Fatalf("auto physical %d > max %d の105%%: 再圧縮が働いていません",
			stAuto.PhysicalBytes, stMax.PhysicalBytes)
	}

	noise := randomData(t, 4<<20)
	m := putBytes(t, sAuto, "noise", noise)
	if got := getBytes(t, sAuto, m.ID); !bytes.Equal(got, noise) {
		t.Fatal("乱数データの復元が一致しません")
	}
	st2, _ := sAuto.Stats()
	if st2.PhysicalBytes-stAuto.PhysicalBytes > int64(len(noise)) {
		t.Fatal("乱数データが膨張しました")
	}
}

// アップロード単位の圧縮モード上書き。
func TestPerUploadCompressionOverride(t *testing.T) {
	s := newTestStore(t)
	text := repetitiveData(4 << 20)

	mFast, err := s.PutWithOptions("fast", bytes.NewReader(text), PutOptions{Compression: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	if got := getBytes(t, s, mFast.ID); !bytes.Equal(got, text) {
		t.Fatal("fast 指定アップロードの復元が一致しません")
	}

	if _, err := s.PutWithOptions("bad", bytes.NewReader(text), PutOptions{Compression: "ultra"}); err == nil {
		t.Fatal("不正な圧縮モードが受理されました")
	}
}

// 生 zlib ストリーム(git loose object 等)の分解・復元。
func TestZlibStreamPrecomp(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	plain := textData(2 << 20)
	// zlib ストリームを合成(header 0x78 0x9c = level 6 相当の CMF/FLG)
	orig, err := precomp.ReconstructZlib([]byte{0x78, 0x9c}, 6, plain)
	if err != nil {
		t.Fatal(err)
	}

	m := putBytes(t, s, "obj.zlib", orig)
	if m.Encoding != EncodingZlibV1 {
		t.Fatalf("encoding = %q, want %q", m.Encoding, EncodingZlibV1)
	}
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(orig) {
		t.Fatal("zlib ストリームの復元がビット一致しません")
	}
	// 展開データに対して圧縮が効いている
	st, _ := s.Stats()
	if st.PhysicalBytes >= int64(len(orig)) {
		t.Fatalf("physical %d >= zlib %d: 削減効果なし", st.PhysicalBytes, len(orig))
	}
}

// マルチメンバー gzip(連結 gzip = ローテートログの cat)の分解・復元。
func TestMultiMemberGzipPrecomp(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	var orig []byte
	var plains [][]byte
	for i, level := range []int{6, 9, 6} {
		plain := append(textData(1<<20), byte('0'+i))
		plains = append(plains, plain)
		member := zlibGzip(t, plain, level)
		orig = append(orig, member...)
	}

	m := putBytes(t, s, "rotated.gz", orig)
	if m.Encoding != EncodingGzipMultiV1 {
		t.Fatalf("encoding = %q, want %q", m.Encoding, EncodingGzipMultiV1)
	}
	if len(m.PrecompMembers) != 3 {
		t.Fatalf("members = %d, want 3", len(m.PrecompMembers))
	}
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(orig) {
		t.Fatal("マルチメンバー gzip の復元がビット一致しません")
	}
	// 3メンバーの中身はほぼ同一 → dedup/デルタで大幅に縮む
	st, _ := s.Stats()
	if st.PhysicalBytes >= int64(len(plains[0])) {
		t.Fatalf("physical %d: メンバー間の重複が回収されていません", st.PhysicalBytes)
	}
}

// makePNG は zlib 産の IDAT を持つ合成 PNG を作る(構造は本物と同一)。
func makePNG(t *testing.T, plain []byte, level int, idatSplit int) []byte {
	t.Helper()
	stream, err := precomp.ReconstructZlib([]byte{0x78, 0x9c}, level, plain)
	if err != nil {
		t.Fatal(err)
	}
	chunk := func(typ string, data []byte) []byte {
		out := make([]byte, 0, len(data)+12)
		var b4 [4]byte
		binary.BigEndian.PutUint32(b4[:], uint32(len(data)))
		out = append(out, b4[:]...)
		out = append(out, typ...)
		out = append(out, data...)
		crc := crc32.NewIEEE()
		crc.Write([]byte(typ))
		crc.Write(data)
		binary.BigEndian.PutUint32(b4[:], crc.Sum32())
		return append(out, b4[:]...)
	}
	png := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, 512)
	binary.BigEndian.PutUint32(ihdr[4:], 512)
	ihdr[8] = 8
	png = append(png, chunk("IHDR", ihdr)...)
	for off := 0; off < len(stream); off += idatSplit {
		end := off + idatSplit
		if end > len(stream) {
			end = len(stream)
		}
		png = append(png, chunk("IDAT", stream[off:end])...)
	}
	return append(png, chunk("IEND", nil)...)
}

// PNG コンテナの分解・ビット一致復元と削減効果。
func TestPNGPrecompRoundTrip(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	// フィルタ済みスキャンライン相当(構造的で zstd がよく縮む)
	plain := textData(4 << 20)
	orig := makePNG(t, plain, 6, 65536)

	m := putBytes(t, s, "image.png", orig)
	if m.Encoding != EncodingPNGV1 {
		t.Fatalf("encoding = %q, want %q", m.Encoding, EncodingPNGV1)
	}
	got := getBytes(t, s, m.ID)
	if sha256.Sum256(got) != sha256.Sum256(orig) {
		t.Fatal("PNG の復元がビット一致しません")
	}
	// 展開データに zstd-19 が効くので物理は PNG より小さい
	st, _ := s.Stats()
	if st.PhysicalBytes >= int64(len(orig)) {
		t.Fatalf("physical %d >= png %d: 削減効果なし", st.PhysicalBytes, len(orig))
	}
}

// 類似 PNG 同士(一部編集)で PNG 越しの dedup/デルタが効く。
func TestPNGCrossFileDedup(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	s := newTestStore(t)
	plain1 := textData(4 << 20)
	plain2 := append([]byte(nil), plain1...)
	copy(plain2[2<<20:], []byte("EDITED-PIXELS"))

	putBytes(t, s, "a.png", makePNG(t, plain1, 6, 65536))
	before, _ := s.Stats()
	m2 := putBytes(t, s, "b.png", makePNG(t, plain2, 6, 65536))
	after, _ := s.Stats()

	added := after.PhysicalBytes - before.PhysicalBytes
	if added > int64(len(plain2))/10 {
		t.Fatalf("類似PNGの物理増分 %d bytes: PNG越しのdedup/デルタが効いていません", added)
	}
	got := getBytes(t, s, m2.ID)
	if sha256.Sum256(got) != sha256.Sum256(makePNG(t, plain2, 6, 65536)) {
		t.Fatal("b.png の復元がビット一致しません")
	}
}
