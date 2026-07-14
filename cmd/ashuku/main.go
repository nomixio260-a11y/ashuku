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
	compression := flag.String("compression", "balanced", "圧縮レベル: fast | balanced | max")
	delta := flag.Bool("delta", true, "類似チャンクへのデルタ圧縮を有効にする")
	chunkAvg := flag.Int("chunk-avg", 0, "平均チャンクサイズ(バイト, 0=デフォルト1MiB)。初回起動時のみ有効")
	deltaDepth := flag.Int("delta-depth", 0, "デルタチェーンの深さ上限(0=デフォルト32)。深いほど多世代バックアップが縮む")
	cacheMB := flag.Int64("cache-mb", 0, "伸長済みチャンクキャッシュ容量 MiB(0=デフォルト128)")
	flag.Parse()

	st, err := store.Open(*dataDir, store.Config{
		Compression:   *compression,
		DisableDelta:  !*delta,
		AvgChunkSize:  *chunkAvg,
		MaxDeltaDepth: *deltaDepth,
		CacheBytes:    *cacheMB << 20,
	})
	if err != nil {
		log.Fatalf("ストアを開けません: %v", err)
	}
	defer st.Close()

	log.Printf("ashuku サーバー起動: %s (データ: %s)", *addr, *dataDir)
	if err := http.ListenAndServe(*addr, api.New(st)); err != nil {
		log.Fatal(err)
	}
}
