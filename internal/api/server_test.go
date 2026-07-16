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
