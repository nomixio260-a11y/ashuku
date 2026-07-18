package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nomixio260-a11y/ashuku/internal/store"
)

func newAuthServer(t *testing.T, opts Options) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, opts))
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
	return srv
}

func req(t *testing.T, method, url, key string, body []byte) *http.Response {
	t.Helper()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	r, _ := http.NewRequest(method, url, rd)
	if key != "" {
		r.Header.Set("X-API-Key", key)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAuthRequired(t *testing.T) {
	srv := newAuthServer(t, Options{Users: map[string]User{"key-a": {Name: "A"}}})

	resp := req(t, "GET", srv.URL+"/api/v1/files", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("キーなし: status = %d, want 401", resp.StatusCode)
	}
	resp = req(t, "GET", srv.URL+"/api/v1/files", "wrong-key", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("不正キー: status = %d, want 401", resp.StatusCode)
	}
	resp = req(t, "GET", srv.URL+"/api/v1/files", "key-a", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正しいキー: status = %d, want 200", resp.StatusCode)
	}
}

// 所有者分離: 他人のファイルは一覧に出ず、取得も削除も 404。
func TestOwnerIsolation(t *testing.T) {
	srv := newAuthServer(t, Options{Users: map[string]User{
		"key-a": {Name: "A"}, "key-b": {Name: "B"},
	}})

	resp := req(t, "POST", srv.URL+"/api/v1/files?name=a.txt", "key-a", []byte("data of user A"))
	var m store.FileManifest
	json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: status = %d", resp.StatusCode)
	}

	// B の一覧には出ない
	resp = req(t, "GET", srv.URL+"/api/v1/files", "key-b", nil)
	var list struct {
		Files []store.FileManifest `json:"files"`
	}
	json.NewDecoder(resp.Body).Decode(&list)
	resp.Body.Close()
	if len(list.Files) != 0 {
		t.Fatalf("他人のファイルが一覧に出ました: %d 件", len(list.Files))
	}

	// B は取得も削除もできない(404 = 存在も漏らさない)
	for _, method := range []string{"GET", "DELETE"} {
		resp = req(t, method, srv.URL+"/api/v1/files/"+m.ID, "key-b", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s 他人ファイル: status = %d, want 404", method, resp.StatusCode)
		}
	}

	// A 本人は取得できる
	resp = req(t, "GET", srv.URL+"/api/v1/files/"+m.ID, "key-a", nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "data of user A" {
		t.Fatal("本人の取得が失敗しました")
	}
}

// クォータ: 超過アップロードは 507、削除で使用量が戻る。
func TestQuotaEnforcement(t *testing.T) {
	srv := newAuthServer(t, Options{Users: map[string]User{
		"key-a": {Name: "A", Quota: 1 << 20}, // 1MiB
	}})

	// 700KB → OK
	resp := req(t, "POST", srv.URL+"/api/v1/files?name=one", "key-a", bytes.Repeat([]byte("x"), 700<<10))
	var m1 store.FileManifest
	json.NewDecoder(resp.Body).Decode(&m1)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("1本目: status = %d", resp.StatusCode)
	}

	// さらに 700KB → クォータ超過
	resp = req(t, "POST", srv.URL+"/api/v1/files?name=two", "key-a", bytes.Repeat([]byte("y"), 700<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("超過アップロード: status = %d, want 507", resp.StatusCode)
	}

	// /me で使用量確認
	resp = req(t, "GET", srv.URL+"/api/v1/me", "key-a", nil)
	var me struct {
		Used  int64 `json:"used_bytes"`
		Quota int64 `json:"quota_bytes"`
	}
	json.NewDecoder(resp.Body).Decode(&me)
	resp.Body.Close()
	if me.Used != 700<<10 || me.Quota != 1<<20 {
		t.Fatalf("me = used %d quota %d", me.Used, me.Quota)
	}

	// 削除すると使用量が戻り、再アップロードできる
	resp = req(t, "DELETE", srv.URL+"/api/v1/files/"+m1.ID, "key-a", nil)
	resp.Body.Close()
	resp = req(t, "POST", srv.URL+"/api/v1/files?name=three", "key-a", bytes.Repeat([]byte("z"), 700<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("削除後の再アップロード: status = %d, want 201", resp.StatusCode)
	}
}

// アップロードサイズ上限: 超過は 413。
func TestMaxUploadLimit(t *testing.T) {
	srv := newAuthServer(t, Options{MaxUploadBytes: 512 << 10})

	resp := req(t, "POST", srv.URL+"/api/v1/files?name=ok", "", bytes.Repeat([]byte("a"), 256<<10))
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("上限内: status = %d", resp.StatusCode)
	}
	resp = req(t, "POST", srv.URL+"/api/v1/files?name=big", "", bytes.Repeat([]byte("a"), 1<<20))
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("上限超過: status = %d, want 413", resp.StatusCode)
	}
	// 失敗したアップロードは容量を消費しない
	resp = req(t, "GET", srv.URL+"/api/v1/stats", "", nil)
	var st store.Stats
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.FileCount != 1 {
		t.Fatalf("file_count = %d, want 1(失敗分はロールバック)", st.FileCount)
	}
}

// スクラブエンドポイントは健全なストアで破損なしを返す。
func TestScrubEndpoint(t *testing.T) {
	srv := newAuthServer(t, Options{})
	req(t, "POST", srv.URL+"/api/v1/files?name=a", "", bytes.Repeat([]byte("x"), 1<<20)).Body.Close()

	resp := req(t, "POST", srv.URL+"/api/v1/scrub", "", nil)
	var res struct {
		ChunksChecked int      `json:"chunks_checked"`
		Corrupt       []string `json:"corrupt"`
	}
	json.NewDecoder(resp.Body).Decode(&res)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scrub status = %d", resp.StatusCode)
	}
	if len(res.Corrupt) != 0 {
		t.Fatalf("健全なストアで破損 %d 件", len(res.Corrupt))
	}
}

// パニックする内部ハンドラでもサーバーは 500 を返して生き続ける
// (ServeHTTP のパニック分離)。
func TestPanicRecovery(t *testing.T) {
	panicSrv := &Server{mux: newMuxThatPanics()}
	rec := &recordingWriter{header: make(map[string][]string)}
	r, _ := http.NewRequest("GET", "/boom", nil)
	// パニックが伝播せず(テストが落ちず)500 になることを確認
	panicSrv.ServeHTTP(rec, r)
	if rec.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.status)
	}
}

// Authorization: Bearer 形式でも認証できる。
// /metrics は Prometheus 形式でカウンタを返す。
func TestMetricsEndpoint(t *testing.T) {
	srv := newAuthServer(t, Options{})
	req(t, "POST", srv.URL+"/api/v1/files?name=a", "", bytes.Repeat([]byte("x"), 1<<20)).Body.Close()

	resp := req(t, "GET", srv.URL+"/metrics", "", nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics status = %d", resp.StatusCode)
	}
	for _, want := range []string{
		"ashuku_http_requests_total",
		"ashuku_uploads_total",
		"ashuku_bytes_uploaded_total",
		"ashuku_disk_free_bytes",
		"# TYPE ashuku_uploads_total counter",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("metrics に %q がありません:\n%s", want, text)
		}
	}
}

// ディスク予約を極端に大きくすると、アップロードが 507 で拒否され、
// health が 503(readiness NG)を返す。
func TestDiskGuard(t *testing.T) {
	// 現実のディスク空きより大きい予約(必ず不足扱いになる)
	srv := newAuthServer(t, Options{MinFreeBytes: 1 << 62})

	resp := req(t, "POST", srv.URL+"/api/v1/files?name=x", "", []byte("data"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusInsufficientStorage {
		t.Fatalf("ディスク不足時のアップロード status = %d, want 507", resp.StatusCode)
	}
	// health は 503 + low_disk
	resp = req(t, "GET", srv.URL+"/healthz", "", nil)
	var h struct {
		Status string `json:"status"`
	}
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || h.Status != "low_disk" {
		t.Fatalf("health status = %d %q, want 503 low_disk", resp.StatusCode, h.Status)
	}
}

// 通常時の health は 200 ok。
func TestHealthOK(t *testing.T) {
	srv := newAuthServer(t, Options{})
	resp := req(t, "GET", srv.URL+"/healthz", "", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want 200", resp.StatusCode)
	}
}

// newMuxThatPanics は必ずパニックする ServeMux を返す(テスト用)。
func newMuxThatPanics() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) {
		panic("test panic")
	})
	return mux
}

// recordingWriter は http.ResponseWriter の最小実装(ステータス記録用)。
type recordingWriter struct {
	header http.Header
	status int
}

func (w *recordingWriter) Header() http.Header { return w.header }
func (w *recordingWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(b), nil
}
func (w *recordingWriter) WriteHeader(s int) { w.status = s }

func TestBearerToken(t *testing.T) {
	srv := newAuthServer(t, Options{Users: map[string]User{"tok": {}}})
	r, _ := http.NewRequest("GET", srv.URL+"/api/v1/files", nil)
	r.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer: status = %d, want 200", resp.StatusCode)
	}
}

func TestHashedAPIKey(t *testing.T) {
	// キーファイルに sha256:<hex> 形式で置いたキーで認証できること
	rawKey := "secret-key-12345"
	sum := sha256.Sum256([]byte(rawKey))
	hashed := "sha256:" + hex.EncodeToString(sum[:])
	srv := newAuthServer(t, Options{Users: map[string]User{
		hashed: {Name: "hashed-user", ID: "u1"},
	}})
	// 生キーで認証成功
	resp := req(t, "GET", srv.URL+"/api/v1/stats", rawKey, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("生キー認証が失敗: %d", resp.StatusCode)
	}
	// ハッシュ文字列そのものでは認証できない(ハッシュはキーではない)
	resp = req(t, "GET", srv.URL+"/api/v1/stats", hashed, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("ハッシュ文字列で認証できてしまった: %d", resp.StatusCode)
	}
	// 誤ったキーは拒否
	resp = req(t, "GET", srv.URL+"/api/v1/stats", "wrong", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("誤ったキーが通った: %d", resp.StatusCode)
	}
}

func TestMetricsRequiresAdmin(t *testing.T) {
	srv := newAuthServer(t, Options{Users: map[string]User{
		"adminkey": {Name: "op", Admin: true},
		"userkey":  {Name: "user"},
	}})
	// 無認証は 403
	resp := req(t, "GET", srv.URL+"/metrics", "", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("無認証で metrics が見えた: %d", resp.StatusCode)
	}
	// 一般ユーザーも 403
	resp = req(t, "GET", srv.URL+"/metrics", "userkey", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("一般キーで metrics が見えた: %d", resp.StatusCode)
	}
	// 管理者は 200
	resp = req(t, "GET", srv.URL+"/metrics", "adminkey", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("管理者キーで metrics が見えない: %d", resp.StatusCode)
	}
}

func TestHealthzHidesDiskBytes(t *testing.T) {
	srv := newAuthServer(t, Options{})
	resp := req(t, "GET", srv.URL+"/healthz", "", nil)
	body, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, leaked := m["free_bytes"]; leaked {
		t.Fatal("healthz が free_bytes を漏らしている")
	}
	if m["status"] == nil {
		t.Fatal("status がない")
	}
}

// TestManifestDoesNotLeakInternals はマニフェスト/一覧 API が内部表現
// (再構成レシピ・符号化方式名・分解後チャンク構造)を漏らさないことを検証。
func TestManifestDoesNotLeakInternals(t *testing.T) {
	srv := newAuthServer(t, Options{})
	// zlib 産 gzip をサーバー経路でアップロード(precomp 適用対象)
	var gz bytes.Buffer
	zw, _ := zlibNewGzip(&gz)
	for i := 0; i < 3000; i++ {
		fmt.Fprintf(zw, "line %d of highly compressible content\n", i)
	}
	zw.Close()
	resp := req(t, "POST", srv.URL+"/api/v1/files?name=a.gz", "", gz.Bytes())
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload: %d", resp.StatusCode)
	}
	var up map[string]any
	json.NewDecoder(resp.Body).Decode(&up)
	id := up["id"].(string)

	for _, path := range []string{"/api/v1/manifests/" + id, "/api/v1/files"} {
		resp = req(t, "GET", srv.URL+path, "", nil)
		body, _ := io.ReadAll(resp.Body)
		low := strings.ToLower(string(body))
		for _, banned := range []string{"precomp", "zlib", "jpeg", "recipe", "coder", "prefix", "suffix", "gzip-"} {
			if strings.Contains(low, banned) {
				t.Fatalf("%s の応答に内部情報 %q が含まれる: %s", path, banned, body[:min(len(body), 400)])
			}
		}
		// エンコード済みファイルのマニフェストはチャンク列も返さない
		if strings.HasPrefix(path, "/api/v1/manifests/") {
			var m map[string]any
			json.Unmarshal(body, &m)
			if enc, _ := m["encoding"].(string); enc != "" && enc != "server" {
				t.Fatalf("encoding が不透明でない: %q", enc)
			}
			if _, has := m["chunks"]; has && m["encoding"] == "server" {
				t.Fatal("エンコード済みファイルのチャンク列が露出")
			}
		}
	}
}

// zlibNewGzip は zlib 産 gzip 相当のライタ(Go 標準は zlib ベース)。
func zlibNewGzip(w io.Writer) (io.WriteCloser, error) {
	return gzip.NewWriterLevel(w, 6)
}
