package zstdc

import (
	"bytes"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// 本家 libzstd の出力が純Goデコーダで伸長できる(相互運用)ことと、
// 純Go最高レベルより小さくなることを確認する。
func TestInteropAndRatio(t *testing.T) {
	if !Available() {
		t.Skip("CGO 無効")
	}
	data := []byte(strings.Repeat("the quick brown fox jumps over the lazy dog. ", 50000))

	out, err := Compress(data)
	if err != nil {
		t.Fatal(err)
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	back, err := dec.DecodeAll(out, nil)
	if err != nil {
		t.Fatalf("純Goデコーダで伸長できません: %v", err)
	}
	if !bytes.Equal(back, data) {
		t.Fatal("往復が一致しません")
	}

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		t.Fatal(err)
	}
	defer enc.Close()
	pure := enc.EncodeAll(data, nil)
	if len(out) > len(pure) {
		t.Fatalf("libzstd-19 (%d) が純Go best (%d) より大きい", len(out), len(pure))
	}
	t.Logf("libzstd-19: %d bytes / 純Go best: %d bytes (%.1f%% 削減)",
		len(out), len(pure), 100*(1-float64(len(out))/float64(len(pure))))
}
