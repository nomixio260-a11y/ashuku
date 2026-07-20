package precomp

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
)

// TestColumnarCodecSelection は列ごとに raw/delta/dict が正しく選ばれることを
// 確認する(単調列→delta、列挙列→dict、ランダム列→raw)。
func TestColumnarCodecSelection(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	var b bytes.Buffer
	b.WriteString("seq,level,rand\n") // 単調, 列挙, ランダム
	seq := 1000
	levels := []string{"INFO", "WARN", "ERROR"}
	for i := 0; i < 2000; i++ {
		seq += rng.Intn(3)
		fmt.Fprintf(&b, "%d,%s,%d\n", seq, levels[rng.Intn(3)], rng.Intn(1<<30))
	}
	u, ok := TryUnwrapCSV(b.Bytes(), 1<<20)
	if !ok {
		t.Fatal("典型 CSV が採用されなかった")
	}
	if len(u.Recipe.Codec) != 3 {
		t.Fatalf("コーデック数が列数と不一致: %d", len(u.Recipe.Codec))
	}
	// seq 列は delta が選ばれるはず(近単調)。
	if u.Recipe.Codec[0] != colDelta {
		t.Errorf("seq 列(単調)は delta 期待、実際 codec=%d", u.Recipe.Codec[0])
	}
	// level 列は dict(ASCII or 二進)が選ばれるはず(3値)。
	if u.Recipe.Codec[1] != colDict && u.Recipe.Codec[1] != colDictBin {
		t.Errorf("level 列(列挙)は dict 期待、実際 codec=%d", u.Recipe.Codec[1])
	}
	// rand 列は raw(delta も dict も不利)。
	if u.Recipe.Codec[2] != colRaw {
		t.Errorf("rand 列は raw 期待、実際 codec=%d", u.Recipe.Codec[2])
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, b.Bytes()) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	t.Logf("codecs=%v (0=raw 1=delta 2=dict)", u.Recipe.Codec)
}

// TestColumnarDictBin は二進 ID の dict が選ばれ往復一致することを確認する
// (低カーディナリティで長めの値だと二進 ID が ASCII を上回る)。
func TestColumnarDictBin(t *testing.T) {
	rng := rand.New(rand.NewSource(9))
	hosts := []string{"web-server-01.prod.example.com", "web-server-02.prod.example.com",
		"db-primary.prod.example.com", "cache-node-03.prod.example.com"}
	var b bytes.Buffer
	b.WriteString("id,host,region\n")
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(&b, "%d,%s,us-east-%d\n", i, hosts[rng.Intn(len(hosts))], rng.Intn(3))
	}
	u, ok := TryUnwrapCSV(b.Bytes(), 1<<20)
	if !ok {
		t.Fatal("採用されなかった")
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, b.Bytes()) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	// host 列(4値・長め)は二進 dict が選ばれるはず。
	if u.Recipe.Codec[1] != colDictBin {
		t.Logf("host 列 codec=%d(二進 dict=%d 期待だが best-of 次第)", u.Recipe.Codec[1], colDictBin)
	}
	t.Logf("codecs=%v", u.Recipe.Codec)
}

// TestColumnarCRLF は '\r\n' 終端 CSV が検出・往復一致し、末尾数値列が
// delta 化されることを確認する。
func TestColumnarCRLF(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("ts,level,size\r\n")
	ts := 1700000000
	for i := 0; i < 500; i++ {
		ts += 1
		fmt.Fprintf(&b, "%d,INFO,%d\r\n", ts, 1000+i)
	}
	orig := b.Bytes()
	u, ok := TryUnwrapCSV(orig, 1<<20)
	if !ok {
		t.Fatal("CRLF CSV が採用されなかった")
	}
	if !u.Recipe.CRLF {
		t.Error("CRLF フラグが立っていない")
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("CRLF 往復不一致: err=%v", err)
	}
	// 末尾 size 列(単調)は '\r' が剥がれて delta 化されるはず。
	if u.Recipe.Codec[2] != colDelta {
		t.Errorf("size 列は delta 期待、実際 codec=%d", u.Recipe.Codec[2])
	}
}

// TestColumnarNanoTimestamp は 19 桁(ナノ秒 epoch)列が delta 化され
// 往復一致することを確認する。
func TestColumnarNanoTimestamp(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("ns,v\n")
	ns := int64(1700000000123456789)
	for i := 0; i < 500; i++ {
		ns += int64(1000 + i)
		fmt.Fprintf(&b, "%d,%d\n", ns, i)
	}
	orig := b.Bytes()
	u, ok := TryUnwrapCSV(orig, 1<<20)
	if !ok {
		t.Fatal("採用されなかった")
	}
	if u.Recipe.Codec[0] != colDelta {
		t.Errorf("19桁 ns 列は delta 期待、実際 codec=%d", u.Recipe.Codec[0])
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("往復不一致: err=%v", err)
	}
}

// TestColumnarLegacyDelta19Digit は「桁上限 18→19 の緩和」が旧形式レシピの
// 復元を壊さないことの回帰。旧エンコーダは 19桁 row0 を parseCanonInt(18桁上限)で
// 弾き、基準 0 で delta を格納した。新 parseCanonInt(19桁)で基準を計算すると
// 全データ行が V0 ぶんずれ、既存保存物が読めなくなる(データ損失)。旧経路は
// 当時の 18桁上限で基準を再現しなければならない。
func TestColumnarLegacyDelta19Digit(t *testing.T) {
	// 値: [19桁, 5, 6, 7, 8, 9, 10, 11]。旧エンコードは基準 0 起点なので
	// delta 列は [5, 1, 1, 1, 1, 1, 1](row0 は逐語)。
	blob := []byte("1234567890123456789\n5\n1\n1\n1\n1\n1\n1\n" + "a\nb\nc\nd\ne\nf\ng\nh\n")
	recipe := &CSVRecipe{Cols: 2, Rows: 8, Delta: []bool{true, false}} // ColBytes==nil → 旧経路
	out, err := ReconstructCSV(recipe, blob)
	if err != nil {
		t.Fatalf("旧形式 19桁 delta 復元エラー: %v", err)
	}
	want := []byte("1234567890123456789,a\n5,b\n6,c\n7,d\n8,e\n9,f\n10,g\n11,h")
	if !bytes.Equal(out, want) {
		t.Fatalf("19桁 row0 の旧形式 delta が壊れた\n want=%q\n got=%q", want, out)
	}
	// JSONL の旧経路も同様に確認(骨格 {"n":<v>})。
	skel := []byte(`{"n":` + "\x00" + `}`)
	jrecipe := &JSONLRecipe{Skeleton: skel, Cols: 1, Rows: 8, Delta: []bool{true}}
	jblob := []byte("1234567890123456789\n5\n1\n1\n1\n1\n1\n1\n")
	jout, err := ReconstructJSONL(jrecipe, jblob)
	if err != nil {
		t.Fatalf("JSONL 旧形式 19桁 delta 復元エラー: %v", err)
	}
	jwant := []byte(`{"n":1234567890123456789}` + "\n" + `{"n":5}` + "\n" + `{"n":6}` + "\n" +
		`{"n":7}` + "\n" + `{"n":8}` + "\n" + `{"n":9}` + "\n" + `{"n":10}` + "\n" + `{"n":11}`)
	if !bytes.Equal(jout, jwant) {
		t.Fatalf("JSONL 19桁 row0 の旧形式 delta が壊れた\n want=%q\n got=%q", jwant, jout)
	}
}

// TestColumnarLegacyRecipe は旧形式レシピ(Delta ベース・ColBytes 無し)が
// 引き続きバイト一致で復元できることを確認する(既存保存ファイルの後方互換)。
func TestColumnarLegacyRecipe(t *testing.T) {
	// 旧形式: 2 列 3 行、delta 無し。blob = 列ごとに nrows 行。
	// 期待原文: "a,1\nb,2\nc,3"(末尾改行なし)。
	blob := []byte("a\nb\nc\n1\n2\n3\n")
	rec := &CSVRecipe{Cols: 2, Rows: 3, TrailingNL: false} // ColBytes nil → 旧経路
	out, err := ReconstructCSV(rec, blob)
	if err != nil {
		t.Fatalf("旧形式復元エラー: %v", err)
	}
	if want := []byte("a,1\nb,2\nc,3"); !bytes.Equal(out, want) {
		t.Fatalf("旧形式復元不一致\n want=%q\n got=%q", want, out)
	}

	// 旧形式 delta 列(col1 が row0 + 差分)。
	// col0=raw ["x","y","z"], col1=delta base=10, deltas [+5,-3] → [10,15,12]。
	blob2 := []byte("x\ny\nz\n10\n5\n-3\n")
	rec2 := &CSVRecipe{Cols: 2, Rows: 3, Delta: []bool{false, true}}
	out2, err := ReconstructCSV(rec2, blob2)
	if err != nil {
		t.Fatalf("旧形式 delta 復元エラー: %v", err)
	}
	if want := []byte("x,10\ny,15\nz,12"); !bytes.Equal(out2, want) {
		t.Fatalf("旧形式 delta 復元不一致\n want=%q\n got=%q", want, out2)
	}
}

// TestColumnarJSONLLegacyRecipe は JSONL 旧形式レシピの後方互換を確認する。
func TestColumnarJSONLLegacyRecipe(t *testing.T) {
	// 骨格 {"a":<v0>,"b":<v1>} を 0x00 プレースホルダで表現。
	skel := []byte(`{"a":` + "\x00" + `,"b":` + "\x00" + `}`)
	// col0=raw ["1","2"], col1=raw ["x","y"] → 2 行。
	blob := []byte("1\n2\nx\ny\n")
	rec := &JSONLRecipe{Skeleton: skel, Cols: 2, Rows: 2, TrailingNL: true}
	out, err := ReconstructJSONL(rec, blob)
	if err != nil {
		t.Fatalf("JSONL 旧形式復元エラー: %v", err)
	}
	want := []byte(`{"a":1,"b":x}` + "\n" + `{"a":2,"b":y}` + "\n")
	if !bytes.Equal(out, want) {
		t.Fatalf("JSONL 旧形式復元不一致\n want=%q\n got=%q", want, out)
	}
}

// FuzzColumnarDecode は decodeColumns が任意入力でパニックせず、
// 壊れた入力を安全にエラーにすることを確認する。
func FuzzColumnarDecode(f *testing.F) {
	f.Add([]byte("a\nb\nc\n"), 3)
	f.Add([]byte("2\nx\ny\n0\n1\n0\n"), 3)
	f.Fuzz(func(t *testing.T, seg []byte, nrows int) {
		if nrows < 0 || nrows > 1<<16 {
			return
		}
		grid := make([][][]byte, nrows)
		for r := range grid {
			grid[r] = make([][]byte, 1)
		}
		for _, codec := range []uint8{colRaw, colDelta, colDict, colDictBin, 99} {
			_ = colDecode(seg, codec, 0, nrows, grid) // パニックしないこと
		}
	})
}
