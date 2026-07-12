// Package api はストレージエンジンを REST API として公開する HTTP レイヤ。
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

	"github.com/nomixio260-a11y/ashuku/internal/store"
)

// Server は REST API サーバー。
type Server struct {
	store *store.Store
	mux   *http.ServeMux
}

// New は store を公開する HTTP ハンドラを作る。
func New(st *store.Store) *Server {
	s := &Server{store: st, mux: http.NewServeMux()}
	s.mux.HandleFunc("POST /api/v1/files", s.handleUpload)
	s.mux.HandleFunc("GET /api/v1/files", s.handleList)
	s.mux.HandleFunc("GET /api/v1/files/{id}", s.handleDownload)
	s.mux.HandleFunc("DELETE /api/v1/files/{id}", s.handleDelete)
	s.mux.HandleFunc("GET /api/v1/stats", s.handleStats)
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleUpload はリクエストボディをそのまま保存する。
// ファイル名は X-File-Name ヘッダまたは ?name= で指定(省略可)。
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	name := r.Header.Get("X-File-Name")
	if name == "" {
		name = r.URL.Query().Get("name")
	}
	if name == "" {
		name = "unnamed"
	}
	name = path.Base(name) // パス区切りは受け付けない

	m, err := s.store.Put(name, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("保存に失敗しました: %v", err))
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	m, body, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "ファイルが見つかりません")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
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

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	files, err := s.store.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if files == nil {
		files = []*store.FileManifest{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	err := s.store.Delete(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "ファイルが見つかりません")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
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
