//go:build cgo

package zstdc

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// 決定的な混合コーパス(語彙反復テキスト+乱数)。実データの中間的な性質。
func corpus(size int) []byte {
	rng := rand.New(rand.NewSource(9))
	vocab := make([][]byte, 3000)
	for i := range vocab {
		w := make([]byte, 4+rng.Intn(10))
		for j := range w {
			w[j] = byte('a' + rng.Intn(26))
		}
		vocab[i] = w
	}
	buf := make([]byte, 0, size+64)
	for len(buf) < size {
		if rng.Intn(10) == 0 { // 1割は圧縮しにくいバイナリ
			b := make([]byte, 200)
			rng.Read(b)
			buf = append(buf, b...)
		} else {
			buf = append(buf, vocab[rng.Intn(len(vocab))]...)
			buf = append(buf, ' ')
		}
	}
	return buf[:size]
}

// CompressMax(level 22 + 大窓)の出力が純Goデコーダで復元でき、
// level 19 以下のサイズになることを確認する。窓 > 8MiB のフレームが
// 既定デコーダ設定で読めることの検証を兼ねる(重要な安全確認)。
func TestCompressMaxRoundTripLargeWindow(t *testing.T) {
	data := corpus(16 << 20) // 16MiB → windowLog 24(> level22 既定の 8MiB)
	c19, err := Compress(data)
	if err != nil {
		t.Fatal(err)
	}
	c22, err := CompressMax(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(c22) > len(c19) {
		t.Fatalf("level22 (%d) が level19 (%d) より大きい", len(c22), len(c19))
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	got, err := dec.DecodeAll(c22, nil)
	if err != nil {
		t.Fatalf("大窓フレームの復号に失敗(デコーダ上限の問題): %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("復元が一致しません")
	}
	t.Logf("16MiB corpus: level19 = %d bytes / level22+大窓 = %d bytes (%.2f%% 削減)",
		len(c19), len(c22), 100*(1-float64(len(c22))/float64(len(c19))))
}

// 研究測定: リージョン束サイズ×エンコーダ設定の削減率(RESEARCH.md §4.15)。
// go test -run TestRegionSizeResearch -v で実行(-short ではスキップ)。
// ASHUKU_CORPUS=path で実データファイル(tar 等)を使って測定できる。
func TestRegionSizeResearch(t *testing.T) {
	if testing.Short() {
		t.Skip("研究測定(-short でスキップ)")
	}
	data := corpus(32 << 20)
	if path := os.Getenv("ASHUKU_CORPUS"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 64<<20 {
			raw = raw[:64<<20]
		}
		data = raw
		t.Logf("実コーパス %s (%d bytes)", path, len(data))
	}
	chunk := 1 << 20
	groupTotal := func(group int, max bool) int {
		total := 0
		for off := 0; off < len(data); off += group * chunk {
			end := off + group*chunk
			if end > len(data) {
				end = len(data)
			}
			var out []byte
			var err error
			if max {
				out, err = CompressMax(data[off:end])
			} else {
				out, err = Compress(data[off:end])
			}
			if err != nil {
				t.Fatal(err)
			}
			total += len(out)
		}
		return total
	}
	base := groupTotal(1, false) // 1MiB チャンク独立 level19(現行の取り込み)
	for _, g := range []int{8, 16, 32} {
		s19 := groupTotal(g, false)
		s22 := groupTotal(g, true)
		t.Logf("束=%2d チャンク: level19 = %8d (%.2f%%改善) / level22+大窓 = %8d (%.2f%%改善)",
			g, s19, 100*(1-float64(s19)/float64(base)),
			s22, 100*(1-float64(s22)/float64(base)))
	}
	t.Logf("基準(1MiB 独立 level19)= %d bytes", base)
	fmt.Println()
}
