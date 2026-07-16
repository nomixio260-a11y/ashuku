// Package api はストレージエンジンを REST API として公開する HTTP レイヤ。
//
// 認証(APIキー)・所有者分離・クォータ・アップロードサイズ上限・
// /stats の TTL キャッシュを持つ。認証を設定しない場合は従来どおり
// 全操作が匿名(所有者 "")で行える(開発・単一ユーザー運用向け)。
package api

import (
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
	// ServerSideUploads はサーバー側で圧縮・展開を行う従来経路
	// (POST/GET /api/v1/files)の扱い:
	//   "full"(デフォルト) = 従来どおり(auto圧縮・precomp あり)
	//   "fast" = 受け付けるが fast 圧縮固定・precomp なし(CPU軽量)
	//   "off"  = 拒否(403)。クライアント支援プロトコル(ashuku-cli)専用にし、
	//            圧縮・展開を完全にユーザーデバイス側へ寄せる
	ServerSideUploads string
}

// Server は REST API サーバー。
type Server struct {
	store *store.Store
	mux   *http.ServeMux
	opts  Options

	statsMu   sync.Mutex
	statsAt   time.Time
	statsLast *store.Stats
}

// New は store を公開する HTTP ハンドラを作る。
func New(st *store.Store, opts Options) *Server {
	if opts.StatsTTL <= 0 {
		opts.StatsTTL = 10 * time.Second
	}
	s := &Server{store: st, mux: http.NewServeMux(), opts: opts}
	s.mux.HandleFunc("POST /api/v1/files", s.auth(s.handleUpload))
	s.mux.HandleFunc("GET /api/v1/files", s.auth(s.handleList))
	s.mux.HandleFunc("GET /api/v1/files/{id}", s.auth(s.handleDownload))
	s.mux.HandleFunc("DELETE /api/v1/files/{id}", s.auth(s.handleDelete))
	s.mux.HandleFunc("GET /api/v1/me", s.auth(s.handleMe))
	s.mux.HandleFunc("GET /api/v1/stats", s.auth(s.handleStats))
	s.mux.HandleFunc("POST /api/v1/optimize", s.auth(s.handleOptimize))
	s.mux.HandleFunc("POST /api/v1/scrub", s.auth(s.handleScrub))
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
	// パニック分離: 1リクエストのパニックがサーバー全体を落とさないように、
	// ここで捕捉して 500 を返す(ハンドラ内の想定外バグに対する最終防衛線)。
	defer func() {
		if rec := recover(); rec != nil {
			log.Printf("panic in %s %s: %v", r.Method, r.URL.Path, rec)
			// ヘッダ未送信なら 500 を返す(送信済みなら接続が閉じられる)
			defer func() { recover() }()
			writeError(w, http.StatusInternalServerError, "内部エラーが発生しました")
		}
	}()
	s.mux.ServeHTTP(w, r)
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
		u, ok := s.opts.Users[key]
		if !ok {
			writeError(w, http.StatusUnauthorized, "APIキーが無効です")
			return
		}
		next(w, r, authed{owner: key, user: u})
	}
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
	writeJSON(w, http.StatusCreated, m)
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
	if _, err := io.Copy(w, body); err != nil {
		// ヘッダ送信後はエラーレスポンスを返せないのでログのみ
		log.Printf("download %s: %v", m.ID, err)
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request, a authed) {
	files, err := s.store.List(a.owner)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if files == nil {
		files = []*store.FileManifest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
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

// handleHealth はサーバーの稼働確認を返す(認証不要)。
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
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

// handleStats はストア全体の統計を返す。集計はチャンク全走査 O(N) のため
// TTL キャッシュする(多数ユーザーが叩いても集計は TTL ごとに1回)。
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, _ authed) {
	s.statsMu.Lock()
	if s.statsLast != nil && time.Since(s.statsAt) < s.opts.StatsTTL {
		st := *s.statsLast
		s.statsMu.Unlock()
		writeJSON(w, http.StatusOK, &st)
		return
	}
	s.statsMu.Unlock()

	st, err := s.store.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.statsMu.Lock()
	s.statsLast, s.statsAt = st, time.Now()
	s.statsMu.Unlock()
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
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
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
	w.Write(data)
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
