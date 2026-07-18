package precomp

// JPEG コーダの実測ハーネス(開発用)。ASHUKU_JPEG_DIR に実 JPEG を置いて
// 実行すると、算術コーダ/平面+zstd の各サイズを出力する。未設定ならスキップ。

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestJPEGBenchReal(t *testing.T) {
	dir := os.Getenv("ASHUKU_JPEG_DIR")
	if dir == "" {
		t.Skip("ASHUKU_JPEG_DIR 未設定")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.jpg"))
	if len(files) == 0 {
		t.Skip("JPEG なし")
	}
	var sumOrig, sumBest int
	for _, fn := range files {
		orig, err := os.ReadFile(fn)
		if err != nil {
			t.Fatal(err)
		}
		u, ok := TryUnwrapJPEG(orig, 0)
		if !ok {
			fmt.Printf("%-20s orig=%7d 不採用\n", filepath.Base(fn), len(orig))
			sumOrig += len(orig)
			sumBest += len(orig)
			continue
		}
		// 採用サイズの見積り: 算術はそのまま、平面は probe 圧縮後
		frame, entropyStart, _ := parseFrame(orig)
		_ = entropyStart
		var est int
		if u.Recipe.Coder == jpegCoderArith {
			est = len(u.Chunked)
		} else {
			payload := u.Chunked[u.Recipe.PrefixLen:]
			probe := jpegProbeEncoder.EncodeAll(payload, nil)
			est = u.Recipe.PrefixLen + len(probe)
		}
		_ = frame
		coder := "planes"
		if u.Recipe.Coder == jpegCoderArith {
			coder = "arith"
		}
		fmt.Printf("%-20s orig=%7d best=%7d (%6.1f%%) coder=%s\n",
			filepath.Base(fn), len(orig), est,
			100*float64(est-len(orig))/float64(len(orig)), coder)
		sumOrig += len(orig)
		sumBest += est
	}
	fmt.Printf("==== 合計 orig=%d best=%d (%.2f%%) ====\n",
		sumOrig, sumBest, 100*float64(sumBest-sumOrig)/float64(sumOrig))
}
