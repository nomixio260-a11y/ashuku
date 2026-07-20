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
