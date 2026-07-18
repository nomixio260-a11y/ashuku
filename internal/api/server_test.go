package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nomixio260-a11y/ashuku/internal/store"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, Options{}))
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
	return srv
}

func upload(t *testing.T, srv *httptest.Server, name string, body []byte) store.FileManifest {
	t.Helper()
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/files", bytes.NewReader(body))
	req.Header.Set("X-File-Name", name)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("upload status = %d: %s", resp.StatusCode, raw)
	}
	var m store.FileManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestUploadDownloadDelete(t *testing.T) {
	srv := newTestServer(t)
	content := []byte(strings.Repeat("こんにちは ashuku ", 1000))

	m := upload(t, srv, "greeting.txt", content)
	if m.Name != "greeting.txt" || m.Size != int64(len(content)) {
		t.Fatalf("manifest = %+v", m)
	}

	// ダウンロードして一致確認
	resp, err := http.Get(srv.URL + "/api/v1/files/" + m.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, content) {
		t.Fatalf("download status=%d, len=%d, want len=%d", resp.StatusCode, len(got), len(content))
	}

	// 削除
	req, _ := http.NewRequest("DELETE", srv.URL+"/api/v1/files/"+m.ID, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}

	// 削除後は 404
	resp, err = http.Get(srv.URL + "/api/v1/files/" + m.ID)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted file status = %d, want 404", resp.StatusCode)
	}
}

func TestListAndStats(t *testing.T) {
	srv := newTestServer(t)
	upload(t, srv, "a.log", []byte(strings.Repeat("log line\n", 10000)))
	upload(t, srv, "b.log", []byte(strings.Repeat("log line\n", 10000)))

	resp, err := http.Get(srv.URL + "/api/v1/files")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Files []store.FileManifest `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(list.Files) != 2 {
		t.Fatalf("len(files) = %d, want 2", len(list.Files))
	}

	resp, err = http.Get(srv.URL + "/api/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	var st store.Stats
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if st.FileCount != 2 {
		t.Fatalf("file_count = %d, want 2", st.FileCount)
	}
	// 同一内容2件+高冗長 → 大幅削減されているはず
	if st.TotalRatio < 2 {
		t.Fatalf("total_ratio = %.2f, want >= 2", st.TotalRatio)
	}
}

// 一覧の ?limit= と next_cursor によるページングで全件を漏れなく辿れる。
func TestListPagination(t *testing.T) {
	srv := newTestServer(t)
	for i := 0; i < 5; i++ {
		upload(t, srv, "p", []byte{byte(i)})
	}

	seen := map[string]bool{}
	cursor := ""
	for {
		u := srv.URL + "/api/v1/files?limit=2"
		if cursor != "" {
			u += "&after=" + cursor
		}
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		var page struct {
			Files      []store.FileManifest `json:"files"`
			NextCursor string               `json:"next_cursor"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, f := range page.Files {
			if seen[f.ID] {
				t.Fatalf("ページ間で %s が重複", f.ID)
			}
			seen[f.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("ページング合計 = %d, want 5", len(seen))
	}

	// 不正な limit は 400
	resp, err := http.Get(srv.URL + "/api/v1/files?limit=abc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("limit=abc の status = %d, want 400", resp.StatusCode)
	}
}

// Content-Length が上限超過のアップロードはボディを読まずに 413 で拒否される。
func TestUploadEarlyRejectByContentLength(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, Options{MaxUploadBytes: 1024}))
	t.Cleanup(func() { srv.Close(); st.Close() })

	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/files", bytes.NewReader(make([]byte, 4096)))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestNotFound(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/api/v1/files/deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// 管理操作(optimize/scrub/fsck)は @admin キーだけが実行できる。
func TestAdminEndpointsRequireAdminKey(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, Options{Users: map[string]User{
		"user-key":  {Name: "普通の人"},
		"admin-key": {Name: "管理者", Admin: true},
	}}))
	t.Cleanup(func() { srv.Close(); st.Close() })

	call := func(key, path string) int {
		req, _ := http.NewRequest("POST", srv.URL+path, nil)
		req.Header.Set("X-API-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, path := range []string{"/api/v1/optimize", "/api/v1/scrub", "/api/v1/fsck"} {
		if got := call("user-key", path); got != http.StatusForbidden {
			t.Fatalf("一般キーの %s = %d, want 403", path, got)
		}
		if got := call("admin-key", path); got != http.StatusOK {
			t.Fatalf("管理キーの %s = %d, want 200", path, got)
		}
	}
}

// id= で所有者IDを分離すると、別のキーでも同じファイルにアクセスできる
// (キーローテーション)。id なしのキーはキー自身が所有者。
func TestKeyRotationViaUserID(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, Options{Users: map[string]User{
		"old-key":   {ID: "alice"},
		"new-key":   {ID: "alice"}, // ローテーション後の新キー
		"other-key": {ID: "bob"},
	}}))
	t.Cleanup(func() { srv.Close(); st.Close() })

	// 旧キーでアップロード
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/files", bytes.NewReader([]byte("rotate me")))
	req.Header.Set("X-API-Key", "old-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var m store.FileManifest
	json.NewDecoder(resp.Body).Decode(&m)
	resp.Body.Close()

	get := func(key string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/files/"+m.ID, nil)
		req.Header.Set("X-API-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := get("new-key"); got != http.StatusOK {
		t.Fatalf("新キーでのアクセス = %d, want 200(同じ id=alice)", got)
	}
	if got := get("other-key"); got != http.StatusNotFound {
		t.Fatalf("他人のキーでのアクセス = %d, want 404", got)
	}
}

// 長すぎるファイル名は 400 で拒否される。
func TestNameLengthLimit(t *testing.T) {
	srv := newTestServer(t)
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/files", bytes.NewReader([]byte("x")))
	req.Header.Set("X-File-Name", strings.Repeat("a", 300))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// ルートで Web コンソール(自己完結 HTML)が配信される。
func TestConsoleServed(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if !strings.Contains(string(body), "ashuku") || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatal("コンソールページが返っていません")
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("CSP ヘッダがありません")
	}
	// ルート以外の未知パスは 404 のまま
	resp2, err := http.Get(srv.URL + "/unknown-path")
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("未知パスの status = %d, want 404", resp2.StatusCode)
	}
}

// 認証失敗を繰り返す送信元は 429 でレート制限される。
func TestAuthFailureRateLimit(t *testing.T) {
	st, err := store.Open(t.TempDir(), store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(st, Options{Users: map[string]User{"good-key": {}}}))
	t.Cleanup(func() { srv.Close(); st.Close() })

	call := func(key string) int {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/files", nil)
		req.Header.Set("X-API-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	// しきい値まで失敗を積む
	for i := 0; i < authFailureLimit; i++ {
		if got := call("wrong-key"); got != http.StatusUnauthorized {
			t.Fatalf("失敗 %d 回目 = %d, want 401", i+1, got)
		}
	}
	// しきい値超過後は正しいキーでも 429(同一IPからの総当たりを遮断)
	if got := call("wrong-key"); got != http.StatusTooManyRequests {
		t.Fatalf("しきい値超過後 = %d, want 429", got)
	}
	if got := call("good-key"); got != http.StatusTooManyRequests {
		t.Fatalf("制限中の正キー = %d, want 429(IP単位の遮断)", got)
	}
}

// PWA マニフェストが配信され、コンソールにレスポンシブ/PWA の要素が含まれる。
func TestConsoleResponsiveAndManifest(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "\"display\": \"standalone\"") {
		t.Fatalf("manifest status=%d body=%s", resp.StatusCode, body[:min(len(body), 80)])
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "manifest+json") {
		t.Fatalf("manifest content-type = %q", resp.Header.Get("Content-Type"))
	}

	resp2, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	for _, want := range []string{
		"viewport-fit=cover",   // ノッチ端末対応
		"prefers-color-scheme", // テーマ自動追従
		"manifest.webmanifest", // PWA
		"safe-area-inset",      // セーフエリア
		"max-width:480px",      // モバイルレイアウト分岐
		"XMLHttpRequest",       // アップロード進捗
	} {
		if !strings.Contains(string(page), want) {
			t.Fatalf("コンソールに %q が含まれていません", want)
		}
	}
}
