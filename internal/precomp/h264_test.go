package precomp

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestH264RoundTripTestdata は実 x264 産の CAVLC ストリームで
// 「採用 → 復元がバイト一致 → 縮んでいる」ことを確認する。
func TestH264RoundTripTestdata(t *testing.T) {
	files, _ := filepath.Glob("testdata/h264/v_*.h264")
	if len(files) == 0 {
		t.Skip("h264 testdata なし")
	}
	adopted := 0
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		u, ok := TryUnwrapH264(orig, 1<<30)
		base := filepath.Base(fn)
		if base == "v_cabac.h264" {
			if ok {
				t.Fatalf("%s: CABAC が採用された(対象外のはず)", base)
			}
			continue
		}
		if !ok {
			t.Logf("%s: 非採用(素通し)", base)
			continue
		}
		rt, err := ReconstructH264(u.Recipe, u.Chunked)
		if err != nil {
			t.Fatalf("%s: 復元エラー: %v", base, err)
		}
		if !bytes.Equal(rt, orig) {
			t.Fatalf("%s: 往復不一致", base)
		}
		t.Logf("%s: %d -> %d (-%.1f%%)", base, len(orig), len(u.Chunked),
			100*float64(len(orig)-len(u.Chunked))/float64(len(orig)))
		adopted++
	}
	if adopted == 0 {
		t.Fatal("CAVLC ストリームが1つも採用されなかった")
	}
}

// TestH264RejectsGarbage は壊れた・非対応入力を安全に拒否する。
func TestH264RejectsGarbage(t *testing.T) {
	cases := [][]byte{
		nil,
		{0, 0, 1},
		{0, 0, 0, 1, 0x67}, // SPS だけ(スライスなし)
		append([]byte{0, 0, 0, 1, 0x65}, bytes.Repeat([]byte{0xFF}, 64)...), // ゴミスライス
		bytes.Repeat([]byte{0xAB}, 100),                                     // スタートコードなし
	}
	for i, c := range cases {
		if _, ok := TryUnwrapH264(c, 1<<20); ok {
			t.Fatalf("case %d: 不正入力が採用された", i)
		}
	}
}

func FuzzTryUnwrapH264(f *testing.F) {
	if b, err := os.ReadFile("testdata/h264/v_qcif.h264"); err == nil {
		f.Add(b)
	}
	f.Add([]byte{0, 0, 0, 1, 0x67, 0x42, 0xC0, 0x0A})
	f.Fuzz(func(t *testing.T, data []byte) {
		u, ok := TryUnwrapH264(data, 1<<22)
		if !ok {
			return
		}
		// 採用されたら必ずバイト一致で復元できること(TryUnwrap 内でも検証済みだが二重に)
		rt, err := ReconstructH264(u.Recipe, u.Chunked)
		if err != nil || !bytes.Equal(rt, data) {
			t.Fatalf("採用されたが往復不一致: err=%v", err)
		}
	})
}
