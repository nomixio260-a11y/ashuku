// Package api はストレージエンジンを REST API として公開する HTTP レイヤ。
//
// 認証(APIキー)・所有者分離・クォータ・アップロードサイズ上限・
// /stats の TTL キャッシュを持つ。認証を設定しない場合は従来どおり
// 全操作が匿名(所有者 "")で行える(開発・単一ユーザー運用向け)。
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"sync"
	"time"

	"github.com/nomixio260-a11y/ashuku/internal/store"
)

// User は APIキー1つぶんの認証情報。
type User struct {
	// Name は表示名(ログ・/me 用)。
	Name string
	// Quota は論理容量上限(バイト)。0 なら無制限。
	Quota int64
}

// Options はサーバーの動作設定。
type Options struct {
	// Users は APIキー → ユーザー情報。空なら認証なし(全操作が匿名)。
	Users map[string]User
	// MaxUploadBytes は1アップロードのサイズ上限。0 なら無制限。
	MaxUploadBytes int64
	// StatsTTL は /stats の集計キャッシュ期間。0 ならデフォルト(10秒)。
	// 統計はチャンク全走査 O(N) なので、大規模ストアで /stats を叩かれ
	// 続けても集計は TTL ごとに1回で済む。
	StatsTTL time.Duration
	// MinFreeBytes はこの空き容量を切ったらアップロードを 507 で拒否する
	// ディスク予約(0 ならデフォルト 1GiB)。満杯直前で書き込みを止め、
	// ディスク枯渇によるサービス停止・破損を防ぐ。
	MinFreeBytes int64
	// AccessLog を true にすると1リクエストごとにアクセスログを出力する。
	AccessLog bool
	// ServerSideUploads はサーバー側で圧縮・展開を行う従来経路
	// (POST/GET /api/v1/files)の扱い:
	//   "full"(デフォルト) = 従来どおり(auto圧縮・precomp あり)
	//   "fast" = 受け付けるが fast 圧縮固定・precomp なし(CPU軽量)
	//   "off"  = 拒否(403)。クライアント支援プロトコル(ashuku-cli)専用にし、
	//            圧縮・展開を完全にユーザーデバイス側へ寄せる
	ServerSideUploads string
}

// DefaultMinFreeBytes はディスク予約のデフォルト(1GiB)。
const DefaultMinFreeBytes = 1 << 30

// Server は REST API サーバー。
type Server struct {
	store   *store.Store
	mux     *http.ServeMux
	opts    Options
	metrics *metrics

	statsMu   sync.Mutex
	statsAt   time.Time
	statsLast *store.Stats
}

// New は store を公開する HTTP ハンドラを作る。
func New(st *store.Store, opts Options) *Server {
	if opts.StatsTTL <= 0 {
		opts.StatsTTL = 10 * time.Second
	}
	if opts.MinFreeBytes <= 0 {
		opts.MinFreeBytes = DefaultMinFreeBytes
	}
	s := &Server{store: st, mux: http.NewServeMux(), opts: opts, metrics: newMetrics()}
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("POST /api/v1/files", s.auth(s.handleUpload))
	s.mux.HandleFunc("GET /api/v1/files", s.auth(s.handleList))
	s.mux.HandleFunc("GET /api/v1/files/{id}", s.auth(s.handleDownload))
	s.mux.HandleFunc("DELETE /api/v1/files/{id}", s.auth(s.handleDelete))
	s.mux.HandleFunc("GET /api/v1/me", s.auth(s.handleMe))
	s.mux.HandleFunc("GET /api/v1/stats", s.auth(s.handleStats))
	s.mux.HandleFunc("POST /api/v1/optimize", s.auth(s.handleOptimize))
	s.mux.HandleFunc("POST /api/v1/scrub", s.auth(s.handleScrub))
	s.mux.HandleFunc("POST /api/v1/fsck", s.auth(s.handleFsck))
	// ヘルスチェック(認証不要。ロードバランサ・監視用)
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	// クライアント支援プロトコル(圧縮・展開・分割をクライアント側で行う)
	s.mux.HandleFunc("GET /api/v1/config", s.auth(s.handleConfig))
	s.mux.HandleFunc("POST /api/v1/chunks/missing", s.auth(s.handleChunksMissing))
	s.mux.HandleFunc("PUT /api/v1/chunks/{hash}", s.auth(s.handleChunkPut))
	s.mux.HandleFunc("GET /api/v1/chunks/{hash}", s.auth(s.handleChunkGet))
	s.mux.HandleFunc("POST /api/v1/manifests", s.auth(s.handleManifestCommit))
	s.mux.HandleFunc("GET /api/v1/manifests/{id}", s.auth(s.handleManifestGet))
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	// パニック分離: 1リクエストのパニックがサーバー全体を落とさないように、
	// ここで捕捉して 500 を返す(ハンドラ内の想定外バグに対する最終防衛線)。
	defer func() {
		if rec := recover(); rec != nil {
			if s.metrics != nil {
				s.metrics.panics.Add(1)
			}
			log.Printf("panic in %s %s: %v", r.Method, r.URL.Path, rec)
			defer func() { recover() }()
			sw.status = http.StatusInternalServerError
			writeError(sw, http.StatusInternalServerError, "内部エラーが発生しました")
		}
		if s.metrics != nil {
			s.metrics.observeRequest(r.Method, sw.status)
		}
		if s.opts.AccessLog {
			log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
		}
	}()
	s.mux.ServeHTTP(sw, r)
}

// statusWriter は書き込まれたステータスコードを記録する ResponseWriter。
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// authed はリクエストの認証結果。
type authed struct {
	owner string // 所有者ID(= APIキー)。認証なし運用では ""
	user  User
}

// auth は APIキーを検証するミドルウェア。キー未設定なら素通し(匿名)。
// キーは Authorization: Bearer <key> または X-API-Key ヘッダで渡す。
func (s *Server) auth(next func(http.ResponseWriter, *http.Request, authed)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(s.opts.Users) == 0 {
			next(w, r, authed{})
			return
		}
		key := r.Header.Get("X-API-Key")
		if key == "" {
			if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
				key = h[7:]
			}
		}
		u, ok := s.lookupUser(key)
		if !ok {
			writeError(w, http.StatusUnauthorized, "APIキーが無効です")
			return
		}
		next(w, r, authed{owner: key, user: u})
	}
}

// lookupUser は APIキーを定時間比較で照合する。マップの直接引きと違い、
// 比較時間がキー内容に依存しないため、応答時間からキーを1文字ずつ
// 推測するタイミング攻撃が成立しない(全登録キーと必ず比較する)。
func (s *Server) lookupUser(key string) (User, bool) {
	var found User
	ok := false
	for k, u := range s.opts.Users {
		if subtle.ConstantTimeCompare([]byte(k), []byte(key)) == 1 {
			found, ok = u, true
		}
	}
	return found, ok
}

// handleUpload はリクエストボディをそのまま保存する。
// ファイル名は X-File-Name ヘッダまたは ?name= で指定(省略可)。
// 圧縮モードは X-Compression ヘッダまたは ?compression= でアップロード単位に
// 上書きできる(auto | fast | balanced | max)。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request, a authed) {
	if s.opts.ServerSideUploads == "off" {
		writeError(w, http.StatusForbidden,
			"このサーバーはサーバー側圧縮を無効化しています。ashuku-cli(クライアント側圧縮)を使ってください")
		return
	}
	if !s.hasFreeSpace() {
		s.metrics.uploadErrors.Add(1)
		writeError(w, http.StatusInsufficientStorage,
			"サーバーのディスク空き容量が不足しています")
		return
	}
	// Content-Length が申告されている場合はボディを読む前に事前拒否する
	// (上限やクォータを大幅に超えるアップロードの帯域・CPUを浪費しない)。
	// 申告なし/虚偽申告は従来どおり取り込み中の上限とコミット時のクォータ
	// 検査(権威判定)で守られる。
	if cl := r.ContentLength; cl > 0 {
		if s.opts.MaxUploadBytes > 0 && cl > s.opts.MaxUploadBytes {
			s.metrics.uploadErrors.Add(1)
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("アップロードサイズが上限(%d bytes)を超えています", s.opts.MaxUploadBytes))
			return
		}
		if a.user.Quota > 0 {
			if used, err := s.store.OwnerUsage(a.owner); err == nil && used+cl > a.user.Quota {
				s.metrics.uploadErrors.Add(1)
				writeError(w, http.StatusInsufficientStorage,
					"容量クォータを超過しています(不要なファイルを削除してください)")
				return
			}
		}
	}
	s.metrics.uploadsTotal.Add(1)
	name := r.Header.Get("X-File-Name")
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	if name == "" {
		name = "unnamed"
	}
	name = path.Base(name) // パス区切りは受け付けない

	mode := r.Header.Get("X-Compression")
	if mode == "" {
		mode = r.URL.Query().Get("compression")
	}
	if !store.ValidCompression(mode) {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("不明な圧縮モード %q (auto | fast | balanced | max)", mode))
		return
	}

	disablePrecomp := false
	if s.opts.ServerSideUploads == "fast" {
		// CPU軽量モード: fast 圧縮固定・precomp なし
		mode = "fast"
		disablePrecomp = true
	}

	body := r.Body
	if s.opts.MaxUploadBytes > 0 {
		body = http.MaxBytesReader(w, body, s.opts.MaxUploadBytes)
	}
	m, err := s.store.PutWithOptions(name, body, store.PutOptions{
		Compression:    mode,
		Owner:          a.owner,
		Quota:          a.user.Quota,
		MaxBytes:       s.opts.MaxUploadBytes,
		DisablePrecomp: disablePrecomp,
	})
	if err != nil {
		s.metrics.uploadErrors.Add(1)
		var maxErr *http.MaxBytesError
		switch {
		case errors.Is(err, store.ErrQuotaExceeded):
			writeError(w, http.StatusInsufficientStorage,
				"容量クォータを超過しています(不要なファイルを削除してください)")
		case errors.Is(err, store.ErrTooLarge), errors.As(err, &maxErr):
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("アップロードサイズが上限(%d bytes)を超えています", s.opts.MaxUploadBytes))
		default:
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("保存に失敗しました: %v", err))
		}
		return
	}
	s.metrics.bytesUploaded.Add(m.Size)
	writeJSON(w, http.StatusCreated, m)
}

// hasFreeSpace はディスク予約を満たしているかを返す(取得失敗時は許可)。
func (s *Server) hasFreeSpace() bool {
	free, err := s.store.FreeBytes()
	if err != nil {
		return true // 取得できないときは書き込みを止めない
	}
	return free >= s.opts.MinFreeBytes
}

// checkOwner は id のファイルが所有者のものであることを確認する。
// 他人のファイルは存在自体を漏らさないため 404 相当の扱いにする。
func (s *Server) checkOwner(id string, a authed) error {
	m, err := s.store.Manifest(id)
	if err != nil {
		return err
	}
	if m.Owner != a.owner {
		return store.ErrNotFound
	}
	return nil
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request, a authed) {
	if s.opts.ServerSideUploads == "off" {
		writeError(w, http.StatusForbidden,
			"このサーバーはサーバー側展開を無効化しています。ashuku-cli get(クライアント側展開)を使ってください")
		return
	}
	id := r.PathValue("id")
	if err := s.checkOwner(id, a); err != nil {
		writeStoreError(w, err)
		return
	}
	m, body, err := s.store.Get(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	defer body.Close()

	w.Header().Set("Content-Type", contentType(m.Name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", m.Size))
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename*=UTF-8''%s", url.PathEscape(m.Name)))
	n, err := io.Copy(w, body)
	s.metrics.bytesDownloaded.Add(n)
	if err != nil {
		// ヘッダ送信後はエラーレスポンスを返せないのでログのみ
		log.Printf("download %s: %v", m.ID, err)
	}
}

// maxListLimit は1ページの最大件数。
const maxListLimit = 10000

// handleList はファイル一覧を作成日時の降順で返す。
// ?limit=N でページ件数を制限でき(最大10000)、続きがある場合は
// レスポンスの next_cursor を次の ?after= に渡す(カーソルページング)。
// limit 省略時は全件(後方互換)。
func (s *Server) handleList(w http.ResponseWriter, r *http.Request, a authed) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeError(w, http.StatusBadRequest, "limit は正の整数で指定してください")
			return
		}
		limit = min(n, maxListLimit)
	}
	files, next, err := s.store.ListPage(a.owner, r.URL.Query().Get("after"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if files == nil {
		files = []*store.FileManifest{}
	}
	resp := map[string]any{"files": files}
	if next != "" {
		resp["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, a authed) {
	id := r.PathValue("id")
	if err := s.checkOwner(id, a); err != nil {
		writeStoreError(w, err)
		return
	}
	if err := s.store.Delete(id); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleMe は呼び出しユーザーの使用量とクォータを返す。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request, a authed) {
	used, err := s.store.OwnerUsage(a.owner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        a.user.Name,
		"used_bytes":  used,
		"quota_bytes": a.user.Quota,
	})
}

// handleHealth はサーバーの稼働確認とディスク空きを返す(認証不要)。
// ディスク予約を切っている場合は 503 を返す(readiness プローブ用:
// 書き込めない状態をロードバランサに知らせる)。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	free, _ := s.store.FreeBytes()
	ready := free == 0 || free >= s.opts.MinFreeBytes
	status := "ok"
	code := http.StatusOK
	if !ready {
		status = "low_disk"
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "free_bytes": free})
}

// handleMetrics は Prometheus 形式でメトリクスを返す(認証不要)。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	gauges := map[string]int64{}
	if free, err := s.store.FreeBytes(); err == nil {
		gauges["ashuku_disk_free_bytes"] = free
	}
	if st, err := s.cachedStats(); err == nil {
		gauges["ashuku_logical_bytes"] = st.LogicalBytes
		gauges["ashuku_physical_bytes"] = st.PhysicalBytes
		gauges["ashuku_chunk_count"] = int64(st.ChunkCount)
		gauges["ashuku_file_count"] = int64(st.FileCount)
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write([]byte(s.metrics.render(gauges)))
}

// handleScrub は全チャンクの完全性を検証し、破損・欠損を報告する。
// 破損があれば 200 で結果を返す(検出は成功しているため)。
func (s *Server) handleScrub(w http.ResponseWriter, r *http.Request, _ authed) {
	res, err := s.store.Scrub()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleFsck はメタデータ整合性を検証する。?repair=1 で修復も行う。
func (s *Server) handleFsck(w http.ResponseWriter, r *http.Request, _ authed) {
	repair := r.URL.Query().Get("repair") == "1"
	res, err := s.store.Fsck(repair)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleOptimize は chain repack(デルタチェーン再編成)を実行し、
// 削減結果を返す。長期の世代保持でドリフトが蓄積したストアの物理容量を
// 回収する。実行中も読み書きは可能。
func (s *Server) handleOptimize(w http.ResponseWriter, r *http.Request, _ authed) {
	res, err := s.store.Optimize()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// cachedStats はストア統計を TTL キャッシュ付きで返す(集計はチャンク
// 全走査 O(N) なので、多数の呼び出しでも集計は TTL ごとに1回で済む)。
func (s *Server) cachedStats() (*store.Stats, error) {
	s.statsMu.Lock()
	if s.statsLast != nil && time.Since(s.statsAt) < s.opts.StatsTTL {
		st := *s.statsLast
		s.statsMu.Unlock()
		return &st, nil
	}
	s.statsMu.Unlock()

	st, err := s.store.Stats()
	if err != nil {
		return nil, err
	}
	s.statsMu.Lock()
	s.statsLast, s.statsAt = st, time.Now()
	s.statsMu.Unlock()
	return st, nil
}

// handleStats はストア全体の統計を返す。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, _ authed) {
	st, err := s.cachedStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// ---- クライアント支援プロトコル ----
//
// 圧縮・展開・チャンク分割をクライアント側で行い、サーバーは検証と保存
// だけを担う(サーバーCPU: 圧縮 10〜50MB/s → 検証用伸長 500MB/s 超)。
// 流れ: GET /config → クライアントが FastCDC 分割+SHA-256 →
// POST /chunks/missing で無いチャンクを特定 → PUT /chunks/{hash} で
// 圧縮済みチャンクを送信 → POST /manifests で確定。
// ダウンロードは GET /manifests/{id} + GET /chunks/{hash} を並列取得し、
// クライアントが伸長・結合する。

// handleConfig はクライアントが分割・圧縮パラメータを揃えるための情報を返す。
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, _ authed) {
	writeJSON(w, http.StatusOK, map[string]any{
		"avg_chunk_size":   s.store.AvgChunkSize(),
		"max_upload_bytes": s.opts.MaxUploadBytes,
	})
}

// maxMissingQuery は1回の missing 問い合わせで受け付けるハッシュ数の上限。
const maxMissingQuery = 10000

func (s *Server) handleChunksMissing(w http.ResponseWriter, r *http.Request, _ authed) {
	var req struct {
		Hashes []string `json:"hashes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "JSONを解析できません")
		return
	}
	if len(req.Hashes) > maxMissingQuery {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("1回の問い合わせは %d ハッシュまでです", maxMissingQuery))
		return
	}
	missing, err := s.store.HasChunks(req.Hashes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if missing == nil {
		missing = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"missing": missing})
}

func (s *Server) handleChunkPut(w http.ResponseWriter, r *http.Request, _ authed) {
	if !s.hasFreeSpace() {
		writeError(w, http.StatusInsufficientStorage, "サーバーのディスク空き容量が不足しています")
		return
	}
	hash := r.PathValue("hash")
	rawSize, _ := strconv.ParseInt(r.Header.Get("X-Raw-Size"), 10, 64)
	compression := r.Header.Get("X-Compression")
	if compression == "" {
		compression = "zstd"
	}
	// チャンクは高々 平均×4+圧縮ヘッダ ぶんしか受け取らない(メモリ上限)
	limit := int64(s.store.AvgChunkSize())*4 + 8192
	stored, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if int64(len(stored)) > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "チャンクが大きすぎます")
		return
	}
	if err := s.store.PutChunkVerified(hash, stored, compression, rawSize); err != nil {
		s.metrics.uploadErrors.Add(1)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// クライアント支援経路も転送量メトリクスに計上する(圧縮済みサイズ)。
	s.metrics.bytesUploaded.Add(int64(len(stored)))
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) handleChunkGet(w http.ResponseWriter, r *http.Request, a authed) {
	// 読み出し権チェック: 呼び出しユーザーが自分のマニフェスト経由で参照
	// しているチャンクだけを返す。「存在確認に成功した」ことと「読み出せる」
	// ことを分離し、ハッシュだけを知る攻撃者のデータ窃取(Dark Clouds 型)を
	// 防ぐ(USENIX Sec'11、RESEARCH.md 参照)。
	hash := r.PathValue("hash")
	ok, err := s.store.OwnerHasChunk(a.owner, hash)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		writeStoreError(w, store.ErrNotFound)
		return
	}
	data, compression, rawSize, err := s.store.ChunkRep(hash)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	w.Header().Set("X-Compression", compression)
	w.Header().Set("X-Raw-Size", strconv.FormatInt(rawSize, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
	s.metrics.bytesDownloaded.Add(int64(len(data)))
}

func (s *Server) handleManifestCommit(w http.ResponseWriter, r *http.Request, a authed) {
	var req struct {
		Name   string   `json:"name"`
		Chunks []string `json:"chunks"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 256<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "JSONを解析できません")
		return
	}
	if req.Name == "" {
		req.Name = "unnamed"
	}
	req.Name = path.Base(req.Name)

	m, missing, err := s.store.CommitClientManifest(
		req.Name, a.owner, req.Chunks, a.user.Quota, s.opts.MaxUploadBytes)
	if err != nil {
		if !errors.Is(err, store.ErrChunksMissing) {
			// 409(missing 交渉)は正常なプロトコルの一部なのでエラーに数えない
			s.metrics.uploadErrors.Add(1)
		}
		switch {
		case errors.Is(err, store.ErrChunksMissing):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": err.Error(), "missing": missing,
			})
		case errors.Is(err, store.ErrQuotaExceeded):
			writeError(w, http.StatusInsufficientStorage, "容量クォータを超過しています")
		case errors.Is(err, store.ErrTooLarge):
			writeError(w, http.StatusRequestEntityTooLarge, "アップロードサイズが上限を超えています")
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	// クライアント支援経路のアップロード完了もメトリクスに計上する。
	s.metrics.uploadsTotal.Add(1)
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleManifestGet(w http.ResponseWriter, r *http.Request, a authed) {
	id := r.PathValue("id")
	m, err := s.store.Manifest(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	if m.Owner != a.owner {
		writeStoreError(w, store.ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func writeStoreError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "ファイルが見つかりません")
		return
	}
	writeError(w, http.StatusInternalServerError, err.Error())
}

func contentType(name string) string {
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
