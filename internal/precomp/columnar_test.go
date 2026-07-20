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
	// level 列は dict が選ばれるはず(3値)。
	if u.Recipe.Codec[1] != colDict {
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
		for _, codec := range []uint8{colRaw, colDelta, colDict, 99} {
			_ = colDecode(seg, codec, 0, nrows, grid) // パニックしないこと
		}
	})
}
