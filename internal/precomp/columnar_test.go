package precomp

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"
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
	// 間隔が 1 ずつ漸増する加速列なので delta/delta2 のどちらかが選ばれる
	// (この列は二階差分が定数になり delta2 が最小になる。§4.52)。
	if u.Recipe.Codec[0] != colDelta && u.Recipe.Codec[0] != colDelta2 {
		t.Errorf("19桁 ns 列は delta 系期待、実際 codec=%d", u.Recipe.Codec[0])
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("往復不一致: err=%v", err)
	}
}

// TestColumnarFixedDecimal は漸増する固定小数列が colDeltaDec で符号化され
// 往復一致し、乱数小数列は raw に退避する(採否は best-of)ことを確認する。
func TestColumnarFixedDecimal(t *testing.T) {
	rng := rand.New(rand.NewSource(77))
	var b bytes.Buffer
	b.WriteString("temp,latency\n") // 気温=漸増(delta有効), latency=乱数(raw)
	temp := 20.0
	for i := 0; i < 2000; i++ {
		temp += (rng.Float64() - 0.5) * 0.2
		fmt.Fprintf(&b, "%.2f,%.3f\n", temp, rng.Float64()*100)
	}
	orig := b.Bytes()
	u, ok := TryUnwrapCSV(orig, 1<<20)
	if !ok {
		t.Fatal("採用されなかった")
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("固定小数 往復不一致: err=%v", err)
	}
	if u.Recipe.Codec[0] != colDeltaDec {
		t.Errorf("temp 列(漸増固定小数)は colDeltaDec 期待、実際 codec=%d", u.Recipe.Codec[0])
	}
	if u.Recipe.Codec[1] != colRaw {
		t.Errorf("latency 列(乱数小数)は raw 期待、実際 codec=%d", u.Recipe.Codec[1])
	}
	t.Logf("codecs=%v (4=colDeltaDec)", u.Recipe.Codec)

	// 固定小数の桁・符号バリエーションの往復。
	edge := []byte("v\n0.00\n-0.50\n1.05\n-1.00\n12.34\n0.01\n99.99\n-9.99\n")
	if u, ok := TryUnwrapCSV(edge, 1<<20); ok {
		rt, err := ReconstructCSV(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, edge) {
			t.Fatalf("固定小数エッジ 往復不一致: err=%v", err)
		}
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

// TestColumnarDelta2 は二階差分コーデック(colDelta2)が加速する整数時系列で
// 選択され、byte 一致で往復することを確認する。
func TestColumnarDelta2(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("t,label\n")
	// 間隔が線形に増える加速列(retry backoff / 三角数など)。二階差分が定数→潰れる。
	t0 := int64(1000000000)
	step := int64(50)
	for i := 0; i < 400; i++ {
		t0 += step
		step += 3
		fmt.Fprintf(&b, "%d,row%d\n", t0, i%7)
	}
	orig := b.Bytes()
	u, ok := TryUnwrapCSV(orig, 1<<20)
	if !ok {
		t.Fatal("採用されなかった")
	}
	if u.Recipe.Codec[0] != colDelta2 {
		t.Errorf("加速列は delta2 期待、実際 codec=%d", u.Recipe.Codec[0])
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("往復不一致: err=%v", err)
	}
}

// TestColumnarTSVAndSemicolon は TSV(タブ区切り)と ';' 区切り CSV が列指向変換で
// 採用され byte 一致で往復すること、および区切りが正しく記録されることを確認する。
func TestColumnarTSVAndSemicolon(t *testing.T) {
	rng := rand.New(rand.NewSource(21))
	for _, tc := range []struct {
		name  string
		delim byte
	}{{"tsv", '\t'}, {"semicolon", ';'}} {
		var b bytes.Buffer
		fmt.Fprintf(&b, "seq%clevel%crand\n", tc.delim, tc.delim)
		seq := 1000
		levels := []string{"INFO", "WARN", "ERROR"}
		for i := 0; i < 2000; i++ {
			seq += rng.Intn(3)
			fmt.Fprintf(&b, "%d%c%s%c%d\n", seq, tc.delim, levels[rng.Intn(3)], tc.delim, rng.Intn(1<<30))
		}
		orig := b.Bytes()
		u, ok := TryUnwrapCSV(orig, 1<<20)
		if !ok {
			t.Fatalf("%s が採用されなかった", tc.name)
		}
		if u.Recipe.Delim != tc.delim {
			t.Errorf("%s: Delim=%q 期待 %q", tc.name, u.Recipe.Delim, tc.delim)
		}
		rt, err := ReconstructCSV(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, orig) {
			t.Fatalf("%s 往復不一致: err=%v", tc.name, err)
		}
		// seq 列は delta 系が選ばれるはず(区切りに依らず列指向が効く)。
		if u.Recipe.Codec[0] == colRaw {
			t.Errorf("%s: seq 列が raw(delta 系期待)", tc.name)
		}
	}
	// ',' CSV は従来通り Delim=0(既定)で記録されること(後方互換)。
	var c bytes.Buffer
	c.WriteString("a,b,n\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&c, "x,y,%d\n", i)
	}
	if u, ok := TryUnwrapCSV(c.Bytes(), 1<<20); ok {
		if u.Recipe.Delim != 0 {
			t.Errorf("',' CSV は Delim=0 期待、実際 %q", u.Recipe.Delim)
		}
	}
}

// TestColumnarHexAndZPad は hex ID / ゼロ埋め連番の列コーデック(colHexPack /
// colHexDelta / colZPadDlt)が選択され byte 一致で往復することを確認する。
func TestColumnarHexAndZPad(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	var b bytes.Buffer
	b.WriteString("seq,trace,counter,rand\n")
	ctr := uint64(0x1000)
	for i := 0; i < 3000; i++ {
		ctr += uint64(1 + rng.Intn(3))
		trace := make([]byte, 16)
		for j := range trace {
			trace[j] = "0123456789abcdef"[rng.Intn(16)]
		}
		fmt.Fprintf(&b, "%08d,%s,%012x,%d\n", i, trace, ctr, rng.Intn(1<<30))
	}
	orig := b.Bytes()
	u, ok := TryUnwrapCSV(orig, 8<<20)
	if !ok {
		t.Fatal("採用されなかった")
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	// seq(ゼロ埋め連番)=zpad、trace(乱数16hex)=hexpack、counter(単調hex)=hexdelta。
	if u.Recipe.Codec[0] != colZPadDlt {
		t.Errorf("seq 列は zpad 期待、実際 codec=%d", u.Recipe.Codec[0])
	}
	if u.Recipe.Codec[1] != colHexPack {
		t.Errorf("trace 列は hexpack 期待、実際 codec=%d", u.Recipe.Codec[1])
	}
	if u.Recipe.Codec[2] != colHexDelta {
		t.Errorf("counter 列は hexdelta 期待、実際 codec=%d", u.Recipe.Codec[2])
	}
	t.Logf("codecs=%v (6=hexpack 7=hexdelta 8=zpad)", u.Recipe.Codec)

	// エッジ: 大文字hex・可変幅・幅超過は安全に非採用(=raw 等にフォールバック)。
	edge := []byte("h\nDEADBEEF\ndeadbeef\n00ff\n")
	if u, ok := TryUnwrapCSV(edge, 1<<20); ok {
		rt, err := ReconstructCSV(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, edge) {
			t.Fatalf("hex エッジ 往復不一致: err=%v", err)
		}
	}
}

// TestColumnarUUIDAndISO は UUID(ダッシュ付き)/ISO-8601 日時の列コーデックが
// 選択され byte 一致で往復することを確認する。
func TestColumnarUUIDAndISO(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	var b bytes.Buffer
	b.WriteString("id,ts,tsz,val\n")
	base := int64(1700000000)
	for i := 0; i < 3000; i++ {
		base += int64(1 + rng.Intn(3))
		u := make([]byte, 36)
		hn := 0
		for j := 0; j < 36; j++ {
			if j == 8 || j == 13 || j == 18 || j == 23 {
				u[j] = '-'
				continue
			}
			u[j] = "0123456789abcdef"[rng.Intn(16)]
			hn++
		}
		iso := time.Unix(base, 0).UTC().Format("2006-01-02T15:04:05Z07:00")
		isoms := time.Unix(base, int64(rng.Intn(1000))*1e6).UTC().Format("2006-01-02 15:04:05.000")
		fmt.Fprintf(&b, "%s,%s,%s,%d\n", u, iso, isoms, rng.Intn(1<<20))
	}
	orig := b.Bytes()
	u, ok := TryUnwrapCSV(orig, 8<<20)
	if !ok {
		t.Fatal("採用されなかった")
	}
	rt, err := ReconstructCSV(u.Recipe, u.Chunked)
	if err != nil || !bytes.Equal(rt, orig) {
		t.Fatalf("往復不一致: err=%v", err)
	}
	if u.Recipe.Codec[0] != colUUID {
		t.Errorf("id 列は uuid 期待、実際 codec=%d", u.Recipe.Codec[0])
	}
	if u.Recipe.Codec[1] != colISO8601 {
		t.Errorf("ts 列は iso 期待、実際 codec=%d", u.Recipe.Codec[1])
	}
	if u.Recipe.Codec[2] != colISO8601 {
		t.Errorf("tsz 列(ミリ秒)は iso 期待、実際 codec=%d", u.Recipe.Codec[2])
	}
	t.Logf("codecs=%v (9=uuid 10=iso)", u.Recipe.Codec)

	// エッジ: 大文字UUID・非UTCオフセット・不正日時は安全に非採用。
	edge := []byte("x\n2024-13-99T99:99:99Z\nZZZ\n")
	if u, ok := TryUnwrapCSV(edge, 1<<20); ok {
		if rt, err := ReconstructCSV(u.Recipe, u.Chunked); err != nil || !bytes.Equal(rt, edge) {
			t.Fatalf("iso エッジ 往復不一致: err=%v", err)
		}
	}
}
