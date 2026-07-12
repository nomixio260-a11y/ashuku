// ashuku — 重複排除+zstd圧縮付きデータ保存サービス
package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/nomixio260-a11y/ashuku/internal/api"
	"github.com/nomixio260-a11y/ashuku/internal/store"
)

func main() {
	addr := flag.String("addr", ":8080", "待ち受けアドレス")
	dataDir := flag.String("data", "./data", "データディレクトリ")
	flag.Parse()

	st, err := store.Open(*dataDir)
	if err != nil {
		log.Fatalf("ストアを開けません: %v", err)
	}
	defer st.Close()

	log.Printf("ashuku サーバー起動: %s (データ: %s)", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, api.New(st)); err != nil {
		log.Fatal(err)
	}
}
