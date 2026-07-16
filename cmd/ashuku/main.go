// ashuku — 重複排除+zstd圧縮付きデータ保存サービス
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nomixio260-a11y/ashuku/internal/api"
	"github.com/nomixio260-a11y/ashuku/internal/store"
)

func main() {
	addr := flag.String("addr", ":8080", "待ち受けアドレス")
	dataDir := flag.String("data", "./data", "データディレクトリ")
	compression := flag.String("compression", "auto",
		"圧縮モード: auto(探査して縮むものだけ最高レベル) | fast | balanced | max")
	delta := flag.Bool("delta", true, "類似チャンクへのデルタ圧縮を有効にする")
	chunkAvg := flag.Int("chunk-avg", 0, "平均チャンクサイズ(バイト, 0=デフォルト1MiB)。初回起動時のみ有効")
	deltaDepth := flag.Int("delta-depth", 0, "デルタチェーンの深さ上限(0=デフォルト32)。深いほど多世代バックアップが縮む")
	cacheMB := flag.Int64("cache-mb", 0, "伸長済みチャンクキャッシュ容量 MiB(0=デフォルト128)")
	optimizeEvery := flag.Duration("optimize-every", time.Hour,
		"chain repack(デルタチェーン再編成)の自動実行間隔。0 で無効")
	precompFlag := flag.Bool("precomp", true,
		"gzip precompression(zlib産gzipを展開して保存、ビット一致復元)。CGO無効ビルドでは自動オフ")
	precompMax := flag.String("precomp-max", "64M",
		"precompression が扱う展開データの上限(例: 64M, 256M)。この値×並行数がメモリ上限")
	precompPar := flag.Int("precomp-parallel", 0,
		"precompression の同時実行数上限(0=デフォルト2)。超過分は素通しで通常保存")
	authKeys := flag.String("auth-keys", "",
		"APIキーファイル(1行: <キー> [クォータ 例:10G] [名前])。未指定なら認証なし")
	maxUpload := flag.String("max-upload", "0",
		"1アップロードのサイズ上限(例: 50G)。0 で無制限")
	serverSide := flag.String("server-side-uploads", "full",
		"サーバー側圧縮経路(/api/v1/files)の扱い: full | fast(軽量圧縮のみ) | off(ashuku-cli専用)")
	flag.Parse()

	switch *serverSide {
	case "full", "fast", "off":
	default:
		log.Fatalf("-server-side-uploads は full | fast | off のいずれかです")
	}

	precompMaxBytes, err := parseBytes(*precompMax)
	if err != nil {
		log.Fatalf("-precomp-max: %v", err)
	}
	maxUploadBytes, err := parseBytes(*maxUpload)
	if err != nil {
		log.Fatalf("-max-upload: %v", err)
	}

	st, err := store.Open(*dataDir, store.Config{
		Compression:     *compression,
		DisableDelta:    !*delta,
		AvgChunkSize:    *chunkAvg,
		MaxDeltaDepth:   *deltaDepth,
		CacheBytes:      *cacheMB << 20,
		DisablePrecomp:  !*precompFlag,
		PrecompMaxPlain: precompMaxBytes,
		PrecompParallel: *precompPar,
	})
	if err != nil {
		log.Fatalf("ストアを開けません: %v", err)
	}
	defer st.Close()

	users, err := loadAuthKeys(*authKeys)
	if err != nil {
		log.Fatalf("APIキーファイルを読めません: %v", err)
	}
	if len(users) > 0 {
		log.Printf("認証: 有効 (%d キー)", len(users))
	} else {
		log.Printf("認証: 無効(全操作が匿名で可能。公開運用では -auth-keys を設定してください)")
	}

	// 自動最適化: ドリフトが蓄積した星形チェーンを定期的に再編成する。
	// Optimize は改善がある場合のみ書き換えるので、何度呼んでも安全。
	if *optimizeEvery > 0 {
		go func() {
			for range time.Tick(*optimizeEvery) {
				res, err := st.Optimize()
				if err != nil {
					log.Printf("自動最適化に失敗: %v", err)
					continue
				}
				if res.ChunksRepacked > 0 {
					log.Printf("自動最適化: %d チャンクを再編成 (%d → %d bytes)",
						res.ChunksRepacked, res.BytesBefore, res.BytesAfter)
				}
			}
		}()
	}

	handler := api.New(st, api.Options{
		Users:             users,
		MaxUploadBytes:    maxUploadBytes,
		ServerSideUploads: *serverSide,
	})
	// タイムアウト: 大容量のアップロード/ダウンロードは何分もかかりうるので
	// Read/WriteTimeout は設定せず、ヘッダ読取とアイドル接続だけを制限する
	// (接続を掴んだまま何もしないクライアントの蓄積を防ぐ)。
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	log.Printf("ashuku サーバー起動: %s (データ: %s)", *addr, *dataDir)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// parseBytes は "500M" "10G" のような人間可読のサイズ表記を解析する。
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K', 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1<<30, s[:len(s)-1]
	case 'T', 't':
		mult, s = 1<<40, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("サイズ表記が不正です: %q", s)
	}
	return n * mult, nil
}

// loadAuthKeys は APIキーファイルを読む。
// 形式(1行1キー): <キー> [クォータ(例: 10G, 0=無制限)] [名前]
// '#' で始まる行と空行は無視する。
func loadAuthKeys(path string) (map[string]api.User, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	users := make(map[string]api.User)
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		u := api.User{}
		if len(fields) >= 2 {
			q, err := parseBytes(fields[1])
			if err != nil {
				return nil, fmt.Errorf("%d行目: %v", line, err)
			}
			u.Quota = q
		}
		if len(fields) >= 3 {
			u.Name = strings.Join(fields[2:], " ")
		}
		users[fields[0]] = u
	}
	return users, sc.Err()
}
