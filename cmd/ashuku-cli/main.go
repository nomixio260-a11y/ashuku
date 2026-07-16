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
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "エラー:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "使い方: ashuku-cli [-server URL] [-key KEY] put <ファイル> [名前] | get <ID> <出力先> | ls | rm <ID>")
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

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
