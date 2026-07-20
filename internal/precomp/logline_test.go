package precomp

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

func genNginx(rows int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	methods := []string{"GET", "GET", "POST", "HEAD"}
	paths := []string{"/", "/index.html", "/api/v1/users", "/static/app.js", "/favicon.ico"}
	codes := []int{200, 200, 304, 404, 500}
	uas := []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/120.0", "curl/7.68.0", "Mozilla/5.0 (iPhone; CPU iPhone OS 14_0)"}
	var b bytes.Buffer
	sec := 0
	for i := 0; i < rows; i++ {
		sec += rng.Intn(3)
		fmt.Fprintf(&b, "10.%d.%d.%d - - [%02d/Jul/2026:%02d:%02d:%02d +0000] \"%s %s HTTP/1.1\" %d %d \"-\" \"%s\"\n",
			rng.Intn(256), rng.Intn(256), rng.Intn(256), 1+rng.Intn(28),
			(sec/3600)%24, (sec/60)%60, sec%60, methods[rng.Intn(len(methods))], paths[rng.Intn(len(paths))],
			codes[rng.Intn(len(codes))], rng.Intn(50000), uas[rng.Intn(len(uas))])
	}
	return b.Bytes()
}

func TestLogRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"nginx":          genNginx(500, 1),
		"no-trailing-nl": bytes.TrimRight(genNginx(300, 2), "\n"),
		"tabs": bytes.Repeat([]byte("2026-07-20\tINFO\tauth\tuser logged in\t42\n"), 20),
		"quoted-escapes": bytes.Repeat([]byte(`1.2.3.4 - - [x] "GET / HTTP/1.1" 200 5 "-" "UA \"weird\" v1"`+"\n"), 20),
		"empty-quote":    bytes.Repeat([]byte(`1.2.3.4 - - [x] "GET / HTTP/1.1" 200 5 "" ""`+"\n"), 20),
	}
	for name, data := range cases {
		u, ok := TryUnwrapLog(data, 1<<20)
		if !ok {
			t.Logf("%s: 非採用(安全)", name)
			continue
		}
		rt, err := ReconstructLog(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("%s: 復元エラー: %v", name, err)
		}
		if !bytes.Equal(rt, data) {
			t.Fatalf("%s: 往復不一致\n orig=%q\n  got=%q", name, data, rt)
		}
	}
}

func TestLogCompresses(t *testing.T) {
	data := genNginx(6000, 7)
	u, ok := TryUnwrapLog(data, 1<<20)
	if !ok {
		t.Fatal("nginx ログが採用されなかった(以前は素通しだった)")
	}
	rt, err := ReconstructLog(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, data) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	if u.Recipe.Cols != 9 {
		t.Errorf("nginx combined は 9 フィールド期待、実際 %d", u.Recipe.Cols)
	}
	orig := probeLen(data)
	col := probeLen(u.Chunked)
	t.Logf("probe: 行指向=%d 列指向=%d (-%.1f%%)", orig, col, float64(orig-col)*100/float64(orig))
	if col >= orig {
		t.Fatal("ログ列指向が縮んでいない")
	}
}

func TestLogRejects(t *testing.T) {
	reject := map[string][]byte{
		"prose":     bytes.Repeat([]byte("The quick brown fox jumps over the lazy dog today.\n"), 20),
		"json":      bytes.Repeat([]byte(`{"a":1,"b":2,"c":3,"d":4}`+"\n"), 20),
		"one-field": bytes.Repeat([]byte("singletokennospaces\n"), 20),
		"ragged":    []byte("a b c d\ne f\ng h i j k\nl m n o\np q r s\nt u v w\nx y z a\nb c d e\n"),
	}
	for name, data := range reject {
		if u, ok := TryUnwrapLog(data, 1<<20); ok {
			rt, err := ReconstructLog(u.Recipe, u.Chunked)
			if err != nil || !bytes.Equal(rt, data) {
				t.Fatalf("%s: 採用されたが往復不一致", name)
			}
			t.Logf("%s: 採用されたが往復一致(安全)", name)
		}
	}
}
