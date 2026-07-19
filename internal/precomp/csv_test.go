package precomp

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// genCSV は数値列(近単調ts含む)・列挙・文字列が混ざる矩形 CSV を作る。
func genCSV(rows int, seed int64) []byte {
	rng := rand.New(rand.NewSource(seed))
	var b bytes.Buffer
	b.WriteString("ts,user,action,latency_ms,bytes,ok,region\n")
	regions := []string{"us-east", "us-west", "eu-central", "ap-south"}
	acts := []string{"get", "put", "del", "list"}
	ts := 1700000000
	for i := 0; i < rows; i++ {
		ts += rng.Intn(5)
		fmt.Fprintf(&b, "%d,user%d,%s,%d,%d,%d,%s\n",
			ts, rng.Intn(5000), acts[rng.Intn(4)], rng.Intn(2000),
			rng.Intn(1000000), rng.Intn(2), regions[rng.Intn(4)])
	}
	return b.Bytes()
}

func TestCSVRoundTrip(t *testing.T) {
	cases := map[string][]byte{
		"typical":           genCSV(500, 1),
		"no-trailing-nl":    bytes.TrimRight(genCSV(300, 2), "\n"),
		"two-col":           []byte("a,b\n1,2\n3,4\n5,6\n7,8\n9,10\n11,12\n13,14\n15,16\n"),
		"empty-fields":      []byte("x,y,z\n1,,3\n,,\n4,5,6\n,7,\n8,,9\n1,2,3\n4,5,6\n7,8,9\n"),
		"leading-zeros":     []byte("id,code\n1,007\n2,042\n3,100\n4,003\n5,050\n6,060\n7,070\n8,080\n"),
		"negative-ints":     []byte("t,delta\n0,-5\n1,-3\n2,10\n3,-100\n4,0\n5,7\n6,-1\n7,2\n"),
		"quotes-as-content": []byte("a,b\n\"x\",\"y\"\n\"1\",\"2\"\n\"p\",\"q\"\n\"m\",\"n\"\n\"i\",\"j\"\n\"k\",\"l\"\n\"s\",\"t\"\n\"u\",\"v\"\n"),
		"crlf-as-content":   []byte("a,b\r\n1,2\r\n3,4\r\n5,6\r\n7,8\r\n9,10\r\n11,12\r\n13,14\r\n"),
	}
	for name, data := range cases {
		u, ok := TryUnwrapCSV(data, 1<<20)
		if !ok {
			t.Logf("%s: 非採用(縮まないか矩形でない)— 素通しは安全", name)
			continue
		}
		rt, err := ReconstructCSV(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("%s: 復元エラー: %v", name, err)
		}
		if !bytes.Equal(rt, data) {
			t.Fatalf("%s: 往復不一致\n orig=%q\n  got=%q", name, data, rt)
		}
	}
}

// TestCSVCompresses は典型 CSV が採用され、列指向が確実に縮むことを確認する。
func TestCSVCompresses(t *testing.T) {
	data := genCSV(4000, 7)
	u, ok := TryUnwrapCSV(data, 1<<20)
	if !ok {
		t.Fatal("典型 CSV が採用されなかった")
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
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

// TestCSVRejectsNonRectangular は非矩形・小規模・非CSV を安全に素通しする。
func TestCSVRejectsNonRectangular(t *testing.T) {
	reject := map[string][]byte{
		"ragged":     []byte("a,b,c\n1,2\n3,4,5\n6,7,8,9\n1,2,3\n4,5,6\n7,8,9\n1,2,3\n"),
		"too-few":    []byte("a,b\n1,2\n3,4\n"),
		"one-col":    bytes.Repeat([]byte("justoneline\n"), 20),
		"prose":      []byte(strings.Repeat("Hello, this is a sentence with, commas.\n", 20)),
		"binary-ish": append([]byte("a,b\n"), bytes.Repeat([]byte{0, 1, 2, 3}, 50)...),
	}
	for name, data := range reject {
		if u, ok := TryUnwrapCSV(data, 1<<20); ok {
			// 採用されても復元がバイト一致すれば安全(prose 等はたまたま矩形かも)
			rt, err := ReconstructCSV(u.Recipe, u.Chunked)
			if err != nil || !bytes.Equal(rt, data) {
				t.Fatalf("%s: 採用されたが往復不一致", name)
			}
			t.Logf("%s: 採用されたが往復一致(安全)", name)
		}
	}
}

func FuzzTryUnwrapCSV(f *testing.F) {
	f.Add(genCSV(20, 1))
	f.Add([]byte("a,b\n1,2\n3,4\n5,6\n7,8\n9,10\n11,12\n13,14\n"))
	f.Add([]byte("x,y,z\n,,\n1,2,3\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapCSV(data, 1<<20)
		if !ok {
			return
		}
		// 採用されたら必ずバイト一致で復元できること(安全性の核)。
		rt, err := ReconstructCSV(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("採用されたが復元エラー: %v", err)
		}
		if !bytes.Equal(rt, data) {
			t.Fatalf("採用されたが往復不一致\n orig=%q\n  got=%q", data, rt)
		}
	})
}
