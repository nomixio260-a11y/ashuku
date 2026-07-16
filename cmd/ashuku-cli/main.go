// ashuku-cli — クライアント側で圧縮・展開を行う ashuku クライアント
//
// サーバーには「持っていないチャンクの圧縮済みバイト列」だけが送られ、
// サーバーの CPU・メモリコストは検証と保存だけに抑えられる。
//
// 使い方:
//
//	ashuku-cli -server http://host:8080 [-key APIキー] put <ファイル> [名前]
//	ashuku-cli ... get <ID> <出力先>
//	ashuku-cli ... ls
//	ashuku-cli ... rm <ID>
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nomixio260-a11y/ashuku/internal/client"
)

func main() {
	server := flag.String("server", "http://localhost:8080", "サーバーURL")
	key := flag.String("key", os.Getenv("ASHUKU_KEY"), "APIキー(環境変数 ASHUKU_KEY でも可)")
	compression := flag.String("compression", "auto", "チャンク圧縮: auto | fast | max | none")
	parallel := flag.Int("parallel", 4, "チャンク転送の並列数")
	flag.Parse()

	c := &client.Client{
		Base:        *server,
		Key:         *key,
		Compression: *compression,
		Parallel:    *parallel,
	}

	args := flag.Args()
	if len(args) == 0 {
		usage()
	}
	var err error
	switch args[0] {
	case "put":
		err = cmdPut(c, args[1:])
	case "get":
		err = cmdGet(c, args[1:])
	case "ls":
		err = cmdLs(c)
	case "rm":
		err = cmdRm(c, args[1:])
	case "stats":
		err = cmdStats(c)
	case "me":
		err = cmdMe(c)
	case "scrub":
		err = cmdScrub(c)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "エラー:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "使い方: ashuku-cli [-server URL] [-key KEY] put <ファイル> [名前] | get <ID> <出力先> | ls | rm <ID> | stats | me | scrub")
	os.Exit(2)
}

func cmdPut(c *client.Client, args []string) error {
	if len(args) < 1 {
		usage()
	}
	f, err := os.Open(args[0])
	if err != nil {
		return err
	}
	defer f.Close()
	name := args[0]
	if len(args) >= 2 {
		name = args[1]
	}
	res, err := c.Put(name, f)
	if err != nil {
		return err
	}
	fmt.Printf("ID: %s\nサイズ: %d bytes / チャンク %d 個中 %d 個を転送 (%d bytes 送信、%.1f%% 削減)\n",
		res.ID, res.Size, res.ChunksTotal, res.ChunksUploaded, res.BytesUploaded,
		100*(1-float64(res.BytesUploaded)/float64(max64(res.Size, 1))))
	return nil
}

func cmdGet(c *client.Client, args []string) error {
	if len(args) < 2 {
		usage()
	}
	f, err := os.Create(args[1])
	if err != nil {
		return err
	}
	defer f.Close()
	return c.Get(args[0], f)
}

func cmdLs(c *client.Client) error {
	files, err := c.List()
	if err != nil {
		return err
	}
	for _, m := range files {
		fmt.Printf("%s  %12d  %s\n", m.ID, m.Size, m.Name)
	}
	return nil
}

func cmdRm(c *client.Client, args []string) error {
	if len(args) < 1 {
		usage()
	}
	return c.Delete(args[0])
}

func cmdStats(c *client.Client) error {
	st, err := c.Stats()
	if err != nil {
		return err
	}
	ratio, _ := st["total_ratio"].(float64)
	logical, _ := st["logical_bytes"].(float64)
	physical, _ := st["physical_bytes"].(float64)
	fmt.Printf("論理: %s / 物理: %s / 総削減 %.1fx\n",
		humanF(logical), humanF(physical), ratio)
	return nil
}

func cmdMe(c *client.Client) error {
	me, err := c.Me()
	if err != nil {
		return err
	}
	used, _ := me["used_bytes"].(float64)
	quota, _ := me["quota_bytes"].(float64)
	name, _ := me["name"].(string)
	if quota > 0 {
		fmt.Printf("%s: 使用 %s / %s (%.1f%%)\n", name, humanF(used), humanF(quota), 100*used/quota)
	} else {
		fmt.Printf("%s: 使用 %s / 無制限\n", name, humanF(used))
	}
	return nil
}

func cmdScrub(c *client.Client) error {
	res, err := c.Scrub()
	if err != nil {
		return err
	}
	checked, _ := res["chunks_checked"].(float64)
	corrupt, _ := res["corrupt"].([]any)
	missing, _ := res["missing"].([]any)
	affected, _ := res["affected_files"].([]any)
	if len(corrupt) == 0 && len(missing) == 0 {
		fmt.Printf("完全性OK: %.0f チャンク検証、破損なし\n", checked)
		return nil
	}
	fmt.Printf("⚠️ 破損 %d / 欠損 %d チャンク検出(影響ファイル %d 件)\n",
		len(corrupt), len(missing), len(affected))
	for _, f := range affected {
		if m, ok := f.(map[string]any); ok {
			fmt.Printf("  影響: %v  %v\n", m["id"], m["name"])
		}
	}
	return nil
}

func humanF(b float64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", b/(1<<10))
	default:
		return fmt.Sprintf("%.0f B", b)
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
