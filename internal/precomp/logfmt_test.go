package precomp

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// genLogfmt は Go サービス風の logfmt(key=value)ログを生成する。ts が単調増加の
// エポック秒、level が enum、trace_id が hex、msg が引用文字列、latency が数値。
func genLogfmt(rows int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	levels := []string{"info", "info", "warn", "error", "debug"}
	msgs := []string{"request completed", "cache miss", "db query", "user logged in", "retry scheduled"}
	comps := []string{"auth", "api", "db", "cache", "worker"}
	var b bytes.Buffer
	ts := int64(1_700_000_000)
	for i := 0; i < rows; i++ {
		ts += int64(rng.Intn(5))
		tid := fmt.Sprintf("%08x%08x", rng.Uint32(), rng.Uint32())
		fmt.Fprintf(&b, "ts=%d level=%s component=%s trace_id=%s msg=%q latency=%dms\n",
			ts, levels[rng.Intn(len(levels))], comps[rng.Intn(len(comps))], tid,
			msgs[rng.Intn(len(msgs))], rng.Intn(2000))
	}
	return b.Bytes()
}

func TestLogfmtRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"go-service":     genLogfmt(500, 1),
		"no-trailing-nl": bytes.TrimRight(genLogfmt(300, 2), "\n"),
		"mixed-quoting": bytes.Repeat([]byte(`level=info msg="has spaces" code=200`+"\n"+
			`level=warn msg=nospaces code=404`+"\n"), 10),
		"quoted-escapes": bytes.Repeat([]byte(`ts=1 level=info msg="weird \"nested\" quote" n=5`+"\n"), 20),
		"empty-quote":    bytes.Repeat([]byte(`ts=1 level=info msg="" tag=""`+"\n"), 20),
	}
	for name, data := range cases {
		u, ok := TryUnwrapLogfmt(data, 1<<20)
		if !ok {
			t.Logf("%s: 非採用(安全)", name)
			continue
		}
		rt, err := ReconstructLogfmt(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("%s: 復元エラー: %v", name, err)
		}
		if !bytes.Equal(rt, data) {
			t.Fatalf("%s: 往復不一致\n orig=%q\n  got=%q", name, data, rt)
		}
	}
}

func TestLogfmtCompresses(t *testing.T) {
	data := genLogfmt(6000, 7)
	u, ok := TryUnwrapLogfmt(data, 1<<20)
	if !ok {
		t.Fatal("logfmt ログが採用されなかった")
	}
	rt, err := ReconstructLogfmt(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, data) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	if u.Recipe.Cols != 6 {
		t.Errorf("logfmt は 6 フィールド期待、実際 %d", u.Recipe.Cols)
	}
	orig := probeLen(data)
	col := probeLen(u.Chunked)
	t.Logf("probe: 行指向=%d 列指向=%d (-%.1f%%)", orig, col, float64(orig-col)*100/float64(orig))
	if col >= orig {
		t.Fatal("logfmt 列指向が縮んでいない")
	}
}

func TestLogfmtRejects(t *testing.T) {
	reject := map[string][]byte{
		"prose":     bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog.\n"), 20),
		"json":      bytes.Repeat([]byte(`{"a":1,"b":2,"c":3}`+"\n"), 20),
		"bare-only": bytes.Repeat([]byte("just some words with no equals sign here\n"), 20),
		"ragged": []byte("a=1 b=2 c=3\nd=4 e=5\nf=6 g=7 h=8\ni=9 j=1\nk=2 l=3\nm=4 n=5\n" +
			"o=6 p=7\nq=8 r=9\n"),
	}
	for name, data := range reject {
		if u, ok := TryUnwrapLogfmt(data, 1<<20); ok {
			rt, err := ReconstructLogfmt(u.Recipe, u.Chunked)
			if err != nil || !bytes.Equal(rt, data) {
				t.Fatalf("%s: 採用されたが往復不一致", name)
			}
			t.Logf("%s: 採用されたが往復一致(安全)", name)
		}
	}
}
