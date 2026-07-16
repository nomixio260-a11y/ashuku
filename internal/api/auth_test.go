package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// Authorization: Bearer 形式でも認証できる。
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