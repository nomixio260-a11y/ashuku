package precomp

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

func genJSONL(rows int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	regions := []string{"us-east", "us-west", "eu-central", "ap-south"}
	acts := []string{"get", "put", "del", "list"}
	var b bytes.Buffer
	ts := 1700000000
	for i := 0; i < rows; i++ {
		ts += rng.Intn(5)
		fmt.Fprintf(&b, `{"ts":%d,"user":"user%d","action":"%s","latency_ms":%d,"bytes":%d,"ok":%t,"region":"%s"}`+"\n",
			ts, rng.Intn(5000), acts[rng.Intn(4)], rng.Intn(2000),
			rng.Intn(1000000), rng.Intn(2) == 0, regions[rng.Intn(4)])
	}
	return b.Bytes()
}

func TestJSONLRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"typical":         genJSONL(500, 1),
		"no-trailing-nl":  bytes.TrimRight(genJSONL(300, 2), "\n"),
		"spaces":          bytes.Repeat([]byte(`{"a": 1, "b": "x", "c": true}`+"\n"), 20),
		"escapes":         bytes.Repeat([]byte(`{"msg":"line \"quoted\" and \\ slash","n":5}`+"\n"), 20),
		"nulls-bools":     bytes.Repeat([]byte(`{"a":null,"b":false,"c":123}`+"\n"), 20),
		"unicode":         bytes.Repeat([]byte(`{"name":"日本語テスト","id":42}`+"\n"), 20),
		"negative-floats": bytes.Repeat([]byte(`{"x":-1.5e3,"y":-42,"z":0}`+"\n"), 20),
	}
	for name, data := range cases {
		u, ok := TryUnwrapJSONL(data, 1<<20)
		if !ok {
			t.Logf("%s: 非採用(縮まないかスキーマ不一致)— 素通しは安全", name)
			continue
		}
		rt, err := ReconstructJSONL(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("%s: 復元エラー: %v", name, err)
		}
		if !bytes.Equal(rt, data) {
			t.Fatalf("%s: 往復不一致\n orig=%q\n  got=%q", name, data, rt)
		}
	}
}

func TestJSONLCompresses(t *testing.T) {
	data := genJSONL(6000, 7)
	u, ok := TryUnwrapJSONL(data, 1<<20)
	if !ok {
		t.Fatal("典型 JSONL が採用されなかった")
	}
	rt, err := ReconstructJSONL(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, data) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	orig := probeLen(data)
	col := probeLen(u.Chunked)
	t.Logf("probe: 行指向=%d 列指向=%d (-%.1f%%)", orig, col, float64(orig-col)*100/float64(orig))
	if col >= orig {
		t.Fatal("列指向が縮んでいない")
	}
}

// TestJSONLNested は入れ子(オブジェクト/配列)を含む行が不透明スパンとして
// 畳まれ、往復一致し、採用されることを確認する(k8s/OTel ログの主対象)。
func TestJSONLNested(t *testing.T) {
	// 往復一致(構文バリエーション)。
	cases := map[string][]byte{
		"nested-obj":   bytes.Repeat([]byte(`{"ts":1,"k8s":{"pod":"p","ns":"prod"},"msg":"x"}`+"\n"), 20),
		"nested-arr":   bytes.Repeat([]byte(`{"ts":1,"tags":["a","b","c"],"n":5}`+"\n"), 20),
		"deep-nest":    bytes.Repeat([]byte(`{"a":{"b":{"c":[1,2,{"d":3}]}},"e":9}`+"\n"), 20),
		"brace-in-str": bytes.Repeat([]byte(`{"o":{"m":"has } and ] inside"},"n":1}`+"\n"), 20),
		"empty-nest":   bytes.Repeat([]byte(`{"o":{},"a":[],"n":1}`+"\n"), 20),
	}
	for name, data := range cases {
		u, ok := TryUnwrapJSONL(data, 1<<20)
		if !ok {
			t.Logf("%s: 非採用(安全)", name)
			continue
		}
		rt, err := ReconstructJSONL(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, data) {
			t.Fatalf("%s: 入れ子の往復不一致: err=%v", name, err)
		}
	}
	// 実ログ様(可変な入れ子値)で採用され、行指向より縮むこと。
	rng := rand.New(rand.NewSource(3))
	svcs := []string{"checkout", "payments", "inventory", "gateway"}
	var b bytes.Buffer
	ts := 1700000000
	for i := 0; i < 4000; i++ {
		ts += rng.Intn(50)
		fmt.Fprintf(&b, `{"ts":%d,"level":"info","k8s":{"pod":"%s-%d","ns":"prod"},"trace":{"id":"%08x"},"code":%d}`+"\n",
			ts, svcs[rng.Intn(len(svcs))], rng.Intn(20), rng.Uint32(), 200+rng.Intn(3)*100)
	}
	data := b.Bytes()
	u, ok := TryUnwrapJSONL(data, 1<<20)
	if !ok {
		t.Fatal("入れ子構造化ログが採用されなかった(以前は素通しだった)")
	}
	rt, err := ReconstructJSONL(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, data) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	if probeLen(u.Chunked) >= probeLen(data) {
		t.Fatal("入れ子ログの列指向が縮んでいない")
	}
	t.Logf("入れ子ログ: 列指向が行指向より縮んだ(%d 値/行)", u.Recipe.Cols)
}

func TestJSONLRejects(t *testing.T) {
	reject := map[string][]byte{
		"nested":       bytes.Repeat([]byte(`{"a":{"b":1},"c":2}`+"\n"), 20),
		"array-val":    bytes.Repeat([]byte(`{"a":[1,2,3],"c":2}`+"\n"), 20),
		"schema-drift": []byte("{\"a\":1,\"b\":2}\n{\"a\":1}\n{\"x\":9,\"y\":8}\n{\"a\":1,\"b\":2}\n{\"a\":1,\"b\":2}\n{\"a\":1,\"b\":2}\n{\"a\":1,\"b\":2}\n{\"a\":1,\"b\":2}\n"),
		"not-json":     bytes.Repeat([]byte("hello world this is not json\n"), 20),
	}
	for name, data := range reject {
		if u, ok := TryUnwrapJSONL(data, 1<<20); ok {
			rt, err := ReconstructJSONL(u.Recipe, u.Chunked)
			if err != nil || !bytes.Equal(rt, data) {
				t.Fatalf("%s: 採用されたが往復不一致", name)
			}
			t.Logf("%s: 採用されたが往復一致(安全)", name)
		}
	}
}

func FuzzTryUnwrapJSONL(f *testing.F) {
	f.Add(genJSONL(20, 1))
	f.Add([]byte(`{"a":1,"b":"x"}` + "\n" + `{"a":2,"b":"y"}` + "\n"))
	f.Add([]byte(`{"a":{"b":1}}` + "\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapJSONL(data, 1<<20)
		if !ok {
			return
		}
		rt, err := ReconstructJSONL(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("採用されたが復元エラー: %v", err)
		}
		if !bytes.Equal(rt, data) {
			t.Fatalf("採用されたが往復不一致\n orig=%q\n  got=%q", data, rt)
		}
	})
}
