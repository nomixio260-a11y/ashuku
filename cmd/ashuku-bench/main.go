// ashuku-bench — 削減率の研究用ベンチマークツール
//
// 合成データセット(ログ / テキスト / 乱数 / 多世代バックアップ)を
// 決定的に生成し、圧縮レベル×デルタ有無の各設定でストアに投入して
// 削減率と速度を Markdown 表で出力する。
//
// 使い方:
//
//	go run ./cmd/ashuku-bench                 # 全データセット × 全設定
//	go run ./cmd/ashuku-bench -input file.tar # 実データでベンチ
//	go run ./cmd/ashuku-bench -gens 30        # バックアップ世代数の変更
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/nomixio260-a11y/ashuku/internal/store"
)

type dataset struct {
	name string
	// parts は「アップロード単位」の列(多世代バックアップは1世代=1ファイル)。
	parts [][]byte
}

type config struct {
	name  string
	cfg   store.Config
}

// runOptimize は -optimize フラグの値(投入後に chain repack を実行)。
var runOptimize bool

func main() {
	input := flag.String("input", "", "実データファイルでベンチする(省略時は合成データセット)")
	gens := flag.Int("gens", 20, "バックアップ世代数")
	size := flag.Int("size", 32<<20, "各合成データセットの基準サイズ(バイト)")
	only := flag.String("only", "", "名前にこの部分文字列を含むデータセットだけ実行")
	chunk := flag.Int("chunk", 0, "平均チャンクサイズ(バイト, 0=デフォルト1MiB)")
	depth := flag.Int("depth", 0, "デルタチェーン深さ上限(0=デフォルト)")
	optimize := flag.Bool("optimize", false, "投入後に chain repack(Optimize)を実行して結果も表示")
	flag.Parse()
	runOptimize = *optimize

	var sets []dataset
	if *input != "" {
		raw, err := os.ReadFile(*input)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		sets = []dataset{{name: filepath.Base(*input), parts: [][]byte{raw}}}
	} else {
		sets = []dataset{
			{name: "ログ(反復構造)", parts: [][]byte{genLogs(*size)}},
			{name: "疑似テキスト", parts: [][]byte{genText(*size)}},
			{name: "乱数(圧縮不能)", parts: [][]byte{genRandom(*size)}},
			{name: fmt.Sprintf("バックアップ%d世代(全域変更)", *gens), parts: genBackupGens(*size/2, *gens, false)},
			{name: fmt.Sprintf("バックアップ%d世代(局所変更)", *gens), parts: genBackupGens(*size/2, *gens, true)},
		}
	}

	configs := []config{
		{"auto", store.Config{Compression: "auto"}},
		{"fast", store.Config{Compression: "fast"}},
		{"balanced", store.Config{Compression: "balanced"}},
		{"max", store.Config{Compression: "max"}},
		{"balanced+delta無効", store.Config{Compression: "balanced", DisableDelta: true}},
	}
	for i := range configs {
		configs[i].cfg.AvgChunkSize = *chunk
		configs[i].cfg.MaxDeltaDepth = *depth
	}

	fmt.Println("| データセット | 設定 | 論理サイズ | 物理サイズ | 実ディスク | 削減倍率 | スループット |")
	fmt.Println("|---|---|---:|---:|---:|---:|---:|")
	for _, ds := range sets {
		if *only != "" && !strings.Contains(ds.name, *only) {
			continue
		}
		for _, c := range configs {
			if err := run(ds, c); err != nil {
				fmt.Fprintf(os.Stderr, "%s/%s: %v\n", ds.name, c.name, err)
				os.Exit(1)
			}
		}
	}
}

func run(ds dataset, c config) error {
	dir, err := os.MkdirTemp("", "ashuku-bench-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	st, err := store.Open(dir, c.cfg)
	if err != nil {
		return err
	}
	defer st.Close()

	start := time.Now()
	for i, part := range ds.parts {
		if _, err := st.Put(fmt.Sprintf("part-%d", i), bytes.NewReader(part)); err != nil {
			return err
		}
	}
	elapsed := time.Since(start)

	stats, err := st.Stats()
	if err != nil {
		return err
	}
	mbps := float64(stats.LogicalBytes) / (1 << 20) / elapsed.Seconds()
	fmt.Printf("| %s | %s | %s | %s | %s | **%.1fx** | %.0f MB/s |\n",
		ds.name, c.name, human(stats.LogicalBytes), human(stats.PhysicalBytes),
		human(diskUsage(dir)), stats.TotalRatio, mbps)

	if runOptimize {
		optStart := time.Now()
		if _, err := st.Optimize(); err != nil {
			return err
		}
		optElapsed := time.Since(optStart)
		stats, err = st.Stats()
		if err != nil {
			return err
		}
		fmt.Printf("| %s | %s +optimize | %s | %s | %s | **%.1fx** | (repack %s) |\n",
			ds.name, c.name, human(stats.LogicalBytes), human(stats.PhysicalBytes),
			human(diskUsage(dir)), stats.TotalRatio, optElapsed.Round(time.Second))
	}
	return nil
}

// diskUsage はデータディレクトリの実ディスク使用量(ブロック単位)を返す。
func diskUsage(dir string) int64 {
	var total int64
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			total += st.Blocks * 512
		} else {
			total += info.Size()
		}
		return nil
	})
	return total
}

func human(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// --- 合成データ生成(すべて決定的) ---

// genLogs は実サービスのアクセスログに似た反復構造のデータを作る。
func genLogs(size int) []byte {
	rng := rand.New(rand.NewSource(1))
	paths := []string{"/api/v1/files", "/api/v1/stats", "/health", "/api/v1/files/abc123", "/login"}
	levels := []string{"INFO", "INFO", "INFO", "WARN", "ERROR"}
	var buf bytes.Buffer
	buf.Grow(size + 256)
	for buf.Len() < size {
		fmt.Fprintf(&buf, "2026-07-12T%02d:%02d:%02dZ %s method=GET path=%s status=%d duration=%dms bytes=%d trace=%08x\n",
			rng.Intn(24), rng.Intn(60), rng.Intn(60),
			levels[rng.Intn(len(levels))], paths[rng.Intn(len(paths))],
			200+rng.Intn(4)*100, rng.Intn(500), rng.Intn(100000), rng.Uint32())
	}
	return buf.Bytes()[:size]
}

// genText は語彙を再利用する疑似自然文テキストを作る。
func genText(size int) []byte {
	rng := rand.New(rand.NewSource(2))
	vocab := make([]string, 2000)
	letters := "abcdefghijklmnopqrstuvwxyz"
	for i := range vocab {
		n := 3 + rng.Intn(8)
		w := make([]byte, n)
		for j := range w {
			w[j] = letters[rng.Intn(len(letters))]
		}
		vocab[i] = string(w)
	}
	var buf bytes.Buffer
	buf.Grow(size + 64)
	for buf.Len() < size {
		// Zipf 的に先頭の語彙ほど頻出させる
		idx := int(float64(len(vocab)) * rng.Float64() * rng.Float64())
		buf.WriteString(vocab[idx])
		if rng.Intn(12) == 0 {
			buf.WriteString(".\n")
		} else {
			buf.WriteByte(' ')
		}
	}
	return buf.Bytes()[:size]
}

func genRandom(size int) []byte {
	rng := rand.New(rand.NewSource(3))
	buf := make([]byte, size)
	rng.Read(buf)
	return buf
}

// genBackupGens は「毎日変化するデータのフルバックアップを gens 世代取る」
// ワークロードを再現する。各世代は前世代に約0.1%の編集と少量の挿入
// (全体をシフトさせる)を加えたもの。
//
// localized=false: 編集を全体に1バイトずつ散らす(全チャンクが変化する最悪ケース。
// 完全一致dedupは全滅し、類似デルタだけが救える)。
// localized=true: 編集を少数の連続ブロックに集中させる(実際の日次バックアップに
// 近い。大半のチャンクは無変化でdedupが効く)。
func genBackupGens(size, gens int, localized bool) [][]byte {
	rng := rand.New(rand.NewSource(4))
	// ベースはテキストと乱数の混合(現実のディスクイメージ相当)
	base := append(genText(size*7/10), genRandom(size*3/10)...)
	parts := make([][]byte, 0, gens)
	cur := base
	for g := 0; g < gens; g++ {
		parts = append(parts, cur)
		next := append([]byte(nil), cur...)
		edits := len(next) / 1000 // 0.1%/世代
		if localized {
			// 8 箇所の連続ブロックにまとめて書き換え
			block := edits / 8
			for i := 0; i < 8; i++ {
				pos := rng.Intn(len(next) - block)
				rng.Read(next[pos : pos+block])
			}
		} else {
			for i := 0; i < edits; i++ {
				next[rng.Intn(len(next))] = byte(rng.Intn(256))
			}
		}
		// 先頭付近への小さな挿入(チャンク境界シフトを誘発)
		pos := rng.Intn(len(next) / 10)
		ins := []byte(fmt.Sprintf("## generation %d marker ##", g))
		next = append(next[:pos], append(ins, next[pos:]...)...)
		cur = next
	}
	return parts
}
