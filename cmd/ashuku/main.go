// ashuku — 重複排除+zstd圧縮付きデータ保存サービス
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	scrubEvery := flag.Duration("scrub-every", 24*time.Hour,
		"データ完全性スクラブ(bit rot 検出)の自動実行間隔。0 で無効")
	metaBackupEvery := flag.Duration("meta-backup-every", time.Hour,
		"メタデータ(bbolt)の自動バックアップ間隔。0 で無効")
	metaBackupKeep := flag.Int("meta-backup-keep", 24, "保持するメタバックアップ世代数")
	precompFlag := flag.Bool("precomp", true,
		"gzip precompression(zlib産gzipを展開して保存、ビット一致復元)。CGO無効ビルドでは自動オフ")
	precompMax := flag.String("precomp-max", "64M",
		"precompression が扱う展開データの上限(例: 64M, 256M)。この値×並行数がメモリ上限")
	precompPar := flag.Int("precomp-parallel", 0,
		"precompression の同時実行数上限(0=デフォルト2)。超過分は素通しで通常保存")
	authKeys := flag.String("auth-keys", "",
		"APIキーファイル(1行: <キー> [クォータ 例:10G] [id=ユーザーID] [@admin] [名前])。未指定なら認証なし")
	maxConcurrent := flag.Int("max-concurrent", 0,
		"同時処理リクエスト数の上限(0=デフォルト512、負値=無制限)。超過分は 503 で拒否")
	maxUpload := flag.String("max-upload", "0",
		"1アップロードのサイズ上限(例: 50G)。0 で無制限")
	serverSide := flag.String("server-side-uploads", "full",
		"サーバー側圧縮経路(/api/v1/files)の扱い: full | fast(軽量圧縮のみ) | off(ashuku-cli専用)")
	minFree := flag.String("min-free", "1G",
		"ディスク空きがこの値を下回ったらアップロードを 507 で拒否(枯渇によるサービス停止防止)")
	accessLog := flag.Bool("access-log", false, "リクエストごとのアクセスログを出力する")
	tlsCert := flag.String("tls-cert", "", "TLS 証明書ファイル(PEM)。tls-key と併せて指定で HTTPS")
	tlsKey := flag.String("tls-key", "", "TLS 秘密鍵ファイル(PEM)")
	flag.Parse()

	minFreeBytes, err := parseBytes(*minFree)
	if err != nil {
		log.Fatalf("-min-free: %v", err)
	}

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
		MinFreeBytes:    minFreeBytes, // 取り込み途中・Optimize もディスク予約を守る
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

	// 定期ジョブは context で停止を通知し、シャットダウン時は実行中の
	// ジョブが終わるのを待ってからストアを閉じる(閉じたストアを掴んだまま
	// バックグラウンド処理が走り続ける事故の防止)。
	jobsCtx, stopJobs := context.WithCancel(context.Background())
	var jobs sync.WaitGroup
	startJob := func(name string, interval time.Duration, fn func()) {
		if interval <= 0 {
			return
		}
		jobs.Add(1)
		go func() {
			defer jobs.Done()
			runPeriodic(jobsCtx, name, interval, fn)
		}()
	}

	// 自動最適化: ドリフトが蓄積した星形チェーンを定期的に再編成する。
	// Optimize は改善がある場合のみ書き換えるので、何度呼んでも安全。
	startJob("自動最適化", *optimizeEvery, func() {
		res, err := st.Optimize()
		if err != nil {
			log.Printf("自動最適化に失敗: %v", err)
			return
		}
		if res.ChunksRepacked > 0 || res.RegionsBuilt > 0 {
			log.Printf("自動最適化: %d チャンク再編成 / %d リージョン化",
				res.ChunksRepacked, res.RegionsBuilt)
		}
	})

	// 定期スクラブ: 保存データの完全性(bit rot 等)を検証する。
	startJob("スクラブ", *scrubEvery, func() {
		res, err := st.Scrub()
		if err != nil {
			log.Printf("スクラブに失敗: %v", err)
			return
		}
		if !res.Healthy() {
			log.Printf("⚠️ スクラブ: 破損 %d / 欠損 %d チャンク検出(影響ファイル %d 件)",
				len(res.Corrupt), len(res.Missing), len(res.AffectedFiles))
		} else {
			log.Printf("スクラブ: %d チャンク検証、破損なし", res.ChunksChecked)
		}
	})

	// 定期メタバックアップ: meta.db は単一障害点なので一貫スナップショットを
	// 別ディレクトリへ取り、世代保持する。
	startJob("メタバックアップ", *metaBackupEvery, func() {
		path, n, err := st.BackupMetaRotating(time.Now(), *metaBackupKeep)
		if err != nil {
			log.Printf("メタバックアップに失敗: %v", err)
			return
		}
		log.Printf("メタバックアップ: %s (%d bytes)", path, n)
	})

	handler := api.New(st, api.Options{
		Users:             users,
		MaxUploadBytes:    maxUploadBytes,
		ServerSideUploads: *serverSide,
		MinFreeBytes:      minFreeBytes,
		AccessLog:         *accessLog,
		MaxConcurrent:     *maxConcurrent,
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

	// グレースフルシャットダウン: SIGINT/SIGTERM で新規接続を止め、
	// 進行中のリクエスト(大容量転送を含む)を最大30秒待ってから閉じる。
	// bbolt と zstd エンコーダは defer で確実にクローズされ、データは無事。
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Printf("シャットダウン中(進行中リクエストの完了を待機)...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	scheme := "http"
	serve := srv.ListenAndServe
	if *tlsCert != "" && *tlsKey != "" {
		scheme = "https"
		serve = func() error { return srv.ListenAndServeTLS(*tlsCert, *tlsKey) }
	} else if *tlsCert != "" || *tlsKey != "" {
		log.Fatalf("TLS には -tls-cert と -tls-key の両方が必要です")
	}
	log.Printf("ashuku サーバー起動: %s://%s (データ: %s)", scheme, *addr, *dataDir)
	if err := serve(); err != nil && err != http.ErrServerClosed {
		stopJobs()
		log.Fatal(err)
	}
	// HTTP は停止済み。定期ジョブを止め、実行中のものの完了を待ってから
	// ストアを閉じる(defer st.Close はこの後に走る)。
	stopJobs()
	jobs.Wait()
	log.Printf("停止しました")
}

// runPeriodic は fn を interval ごとに実行する。fn がパニックしても
// ゴルーチン(とサーバー)は死なず、次の周期で再試行する(バックグラウンド
// ジョブの想定外バグに対する分離)。ctx のキャンセルで停止する。
func runPeriodic(ctx context.Context, name string, interval time.Duration, fn func()) {
	safe := func() {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("%s でパニック(継続します): %v", name, rec)
			}
		}()
		fn()
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			safe()
		}
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
// 形式(1行1キー): <キー> [クォータ(例: 10G, 0=無制限)] [id=ユーザーID] [@admin] [名前]
// '#' で始まる行と空行は無視する。
//
//   - id=xxx を付けると所有者IDがキーではなく xxx になる。同じ id で複数行
//     (複数キー)を発行でき、キーのローテーション(漏洩時の差し替え)が
//     ファイルへのアクセスを失わずにできる。
//   - @admin を付けたキーは管理操作(optimize / scrub / fsck)を実行できる。
//     付いていないキーは通常操作のみ(重い管理操作でのDoSを防ぐ)。
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
		quotaSet := false
		var nameParts []string
		for _, tok := range fields[1:] {
			switch {
			case tok == "@admin":
				u.Admin = true
			case strings.HasPrefix(tok, "id="):
				u.ID = tok[len("id="):]
			case !quotaSet:
				q, err := parseBytes(tok)
				if err != nil {
					return nil, fmt.Errorf("%d行目: クォータの解析に失敗: %v", line, err)
				}
				u.Quota = q
				quotaSet = true
			default:
				nameParts = append(nameParts, tok)
			}
		}
		u.Name = strings.Join(nameParts, " ")
		users[fields[0]] = u
	}
	return users, sc.Err()
}
