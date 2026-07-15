package store

import (
	"bytes"
	"crypto/sha256"
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
