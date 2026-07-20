package precomp

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// compressibleBlob は base64 復号で縮む「構造のあるバイナリ」。
func compressibleBlob(n int, seed int64) []byte {
	r := rand.New(rand.NewSource(seed))
	words := [][]byte{[]byte("record"), []byte("value"), []byte("field"), []byte("data")}
	var b bytes.Buffer
	for b.Len() < n {
		b.Write(words[r.Intn(len(words))])
		b.WriteByte(byte(r.Intn(16)))
	}
	return b.Bytes()[:n]
}

func TestBase64RoundTrip(t *testing.T) {
	blob := compressibleBlob(3000, 1)
	std := base64.StdEncoding.EncodeToString(blob)
	stdNoPad := base64.RawStdEncoding.EncodeToString(blob)
	url := base64.RawURLEncoding.EncodeToString(compressibleBlob(3000, 2))
	cases := map[string][]byte{
		"json-std-pad": []byte(`{"name":"x","payload":"` + std + `","n":1}`),
		"raw-std":      []byte("prefix " + stdNoPad + " suffix texttexttext"),
		"url-safe":     []byte("token=" + url + "; path=/"),
		"multi": []byte(`{"a":"` + std + `","b":"` +
			base64.StdEncoding.EncodeToString(compressibleBlob(2000, 3)) + `"}`),
	}
	for name, data := range cases {
		u, ok := TryUnwrapBase64(data, 1<<20)
		if !ok {
			t.Logf("%s: 非採用(安全)", name)
			continue
		}
		rt, err := ReconstructBase64(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("%s: 復元エラー: %v", name, err)
		}
		if !bytes.Equal(rt, data) {
			t.Fatalf("%s: 往復不一致\n orig=%q\n  got=%q", name, data, rt)
		}
	}
}

// b64Wrap は base64 文字列を幅 w で折り、各行(末尾含む)を sep で終える。
func b64WrapTest(s string, w int, sep string) string {
	var b strings.Builder
	for i := 0; i < len(s); i += w {
		e := i + w
		if e > len(s) {
			e = len(s)
		}
		b.WriteString(s[i:e])
		b.WriteString(sep)
	}
	return b.String()
}

// TestBase64Wrapped は行折り返し(PEM/MIME)base64 が 1 セグメントに畳まれ、
// 各種セパレータ・パディング・末尾区切りで往復一致することを確認する。
func TestBase64Wrapped(t *testing.T) {
	blobText := func(n int, seed int64) []byte {
		r := rand.New(rand.NewSource(seed))
		var b bytes.Buffer
		for b.Len() < n {
			fmt.Fprintf(&b, "line %d user=alice status=ok region=us\n", r.Intn(100))
		}
		return b.Bytes()[:n]
	}
	enc := base64.StdEncoding.EncodeToString(blobText(6000, 1))
	cases := map[string]string{
		"pem-lf-trail":     "-----BEGIN X-----\n" + b64WrapTest(enc, 64, "\n") + "-----END X-----\n",
		"mime-crlf-trail":  "Content:\r\n" + b64WrapTest(enc, 76, "\r\n") + "--boundary--\r\n",
		"lf-no-trail":      "data=" + strings.TrimSuffix(b64WrapTest(enc, 64, "\n"), "\n") + " end",
		"width-60":         "x\n" + b64WrapTest(enc, 60, "\n") + "y\n",
	}
	for name, data := range cases {
		d := []byte(data)
		u, ok := TryUnwrapBase64(d, 1<<20)
		if !ok {
			t.Logf("%s: 非採用(安全)", name)
			continue
		}
		rt, err := ReconstructBase64(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, d) {
			t.Fatalf("%s: 折り返し往復不一致: err=%v", name, err)
		}
		// 折り返しは 1 セグメントに畳まれるはず。
		if len(u.Recipe.Segments) != 1 || u.Recipe.Segments[0].LineW == 0 {
			t.Errorf("%s: 折り返しが 1 セグメントに畳まれていない(segs=%d LineW=%d)",
				name, len(u.Recipe.Segments), u.Recipe.Segments[0].LineW)
		}
	}
}

func TestBase64Compresses(t *testing.T) {
	// 少数の大きめ・ユニークな圧縮可能バイナリを base64 した文書(証明書束・
	// ペイロードダンプ等)は、復号採用で明確に縮むはず。
	var b strings.Builder
	for i := 0; i < 8; i++ {
		enc := base64.StdEncoding.EncodeToString(compressibleBlob(8192, int64(i+100)))
		fmt.Fprintf(&b, "-----BEGIN BLOB %d-----\n%s\n-----END BLOB %d-----\n", i, enc, i)
	}
	data := []byte(b.String())
	u, ok := TryUnwrapBase64(data, 1<<20)
	if !ok {
		t.Fatal("base64-heavy JSON が採用されなかった")
	}
	rt, err := ReconstructBase64(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, data) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	orig := probeLen(data)
	dec := probeLen(u.Chunked)
	t.Logf("probe: base64 形=%d 復号形=%d (-%.1f%%) 領域=%d",
		orig, dec, float64(orig-dec)*100/float64(orig), len(u.Recipe.Segments))
	if dec >= orig {
		t.Fatal("base64 復号形が縮んでいない")
	}
}

func TestBase64Rejects(t *testing.T) {
	// 圧縮不能(乱数)を base64 した領域は復号しても縮まない → 不採用。
	r := rand.New(rand.NewSource(9))
	rnd := make([]byte, 4096)
	r.Read(rnd)
	incompJSON := []byte(`{"blob":"` + base64.StdEncoding.EncodeToString(rnd) + `"}`)
	// 通常テキスト(base64 領域が短い)。
	prose := []byte(strings.Repeat("The quick brown fox jumps over the lazy dog. ", 40))
	for name, data := range map[string][]byte{"incompressible": incompJSON, "prose": prose} {
		if u, ok := TryUnwrapBase64(data, 1<<20); ok {
			// 採用されても往復一致すれば安全。
			rt, err := ReconstructBase64(u.Recipe, u.Chunked)
			if err != nil || !bytes.Equal(rt, data) {
				t.Fatalf("%s: 採用されたが往復不一致", name)
			}
			t.Logf("%s: 採用されたが往復一致(安全)", name)
		}
	}
}

func FuzzTryUnwrapBase64(f *testing.F) {
	f.Add([]byte(`{"p":"` + base64.StdEncoding.EncodeToString([]byte("hello world hello world hello")) + `"}`))
	f.Add([]byte("AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJKKKKLLLLMMMMNNNNOOOO"))
	f.Add([]byte("not base64 at all, just spaces and words here to mutate around"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapBase64(data, 1<<20)
		if !ok {
			return
		}
		rt, err := ReconstructBase64(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, data) {
			t.Fatalf("採用した base64 がビット一致で戻らない (err=%v)", err)
		}
	})
}
