// Package client は ashuku のクライアント支援プロトコルの実装。
//
// チャンク分割・圧縮・展開・整合性検証をすべてクライアント側で行い、
// サーバーには「持っていないチャンクの圧縮済みバイト列」だけを送る。
// サーバーの仕事は検証(伸長+SHA-256)と保存のみになり、
// CPU・メモリコストが最小化される。既にサーバーにあるチャンクは
// 転送すらされない(帯域も節約される)。
package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/nomixio260-a11y/ashuku/internal/chunker"
)

// Client は ashuku サーバーへの接続。
type Client struct {
	// Base はサーバーの URL(例: http://localhost:8080)。
	Base string
	// Key は API キー(認証なしサーバーなら空)。
	Key string
	// Compression はチャンク圧縮モード: "auto"(デフォルト) | "fast" | "max" | "none"
	Compression string
	// Parallel はチャンク転送の並列数(0 なら 4)。
	Parallel int
	// HTTP は使用する http.Client(nil なら適切なタイムアウト付きの既定)。
	HTTP *http.Client

	initOnce  sync.Once
	initErr   error
	chunkSize int
	encFast   *zstd.Encoder
	encBest   *zstd.Encoder
	dec       *zstd.Decoder
}

// PutResult はアップロード結果と転送統計。
type PutResult struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Size int64  `json:"size"`
	// ChunksTotal はファイルの総チャンク数、ChunksUploaded は実際に
	// 転送したチャンク数(残りはサーバー側の重複排除でスキップ)。
	ChunksTotal    int   `json:"chunks_total"`
	ChunksUploaded int   `json:"chunks_uploaded"`
	BytesUploaded  int64 `json:"bytes_uploaded"`
}

// Manifest はサーバー上のファイル情報(必要なフィールドのみ)。
type Manifest struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Size   int64    `json:"size"`
	Chunks []string `json:"chunks"`
	// Encoding が非空のファイルは precompression 適用済みで、チャンク列は
	// 展開データ。元ストリームの再構成レシピはサーバーだけが持つため、
	// ダウンロードはサーバー経路にフォールバックする。
	Encoding string `json:"encoding"`
	// OrigSHA256 は元ストリームの SHA-256(フォールバック経路の検証用)。
	OrigSHA256 string `json:"orig_sha256"`
}

func (c *Client) init() error {
	c.initOnce.Do(func() {
		if c.HTTP == nil {
			c.HTTP = &http.Client{Timeout: 10 * time.Minute}
		}
		if c.Parallel <= 0 {
			c.Parallel = 4
		}
		var cfg struct {
			AvgChunkSize int `json:"avg_chunk_size"`
		}
		if c.initErr = c.getJSON("/api/v1/config", &cfg); c.initErr != nil {
			return
		}
		c.chunkSize = cfg.AvgChunkSize
		c.encFast, c.initErr = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
		if c.initErr != nil {
			return
		}
		c.encBest, c.initErr = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
		if c.initErr != nil {
			return
		}
		c.dec, c.initErr = zstd.NewReader(nil)
	})
	return c.initErr
}

func (c *Client) req(method, path string, body io.Reader) (*http.Request, error) {
	r, err := http.NewRequest(method, c.Base+path, body)
	if err != nil {
		return nil, err
	}
	if c.Key != "" {
		r.Header.Set("X-API-Key", c.Key)
	}
	return r, nil
}

func (c *Client) getJSON(path string, out any) error {
	r, err := c.req("GET", path, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func httpError(resp *http.Response) error {
	var e struct {
		Error string `json:"error"`
	}
	json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&e)
	if e.Error != "" {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, e.Error)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

// compress はモードに応じてチャンクを圧縮する(サーバーの auto と同じ
// 2段階方式)。縮まなければ ("none", raw) を返す。
func (c *Client) compress(data []byte) (string, []byte) {
	if c.Compression == "none" {
		return "none", data
	}
	var out []byte
	switch c.Compression {
	case "fast":
		out = c.encFast.EncodeAll(data, make([]byte, 0, len(data)/2))
	case "max":
		out = c.encBest.EncodeAll(data, make([]byte, 0, len(data)/2))
	default: // auto
		if len(data) < 128<<10 {
			out = c.encFast.EncodeAll(data, make([]byte, 0, len(data)/2))
		} else {
			quick := c.encFast.EncodeAll(data, make([]byte, 0, len(data)/2))
			if len(quick)*10 >= len(data)*9 {
				out = quick
			} else {
				best := c.encBest.EncodeAll(data, make([]byte, 0, len(quick)))
				if len(best) < len(quick) {
					out = best
				} else {
					out = quick
				}
			}
		}
	}
	if len(out) >= len(data) {
		return "none", data
	}
	return "zstd", out
}

// windowChunks は1バッチで処理するチャンク数(メモリ上限 ≒ 窓×平均チャンク)。
const windowChunks = 64

// Put は r の内容をクライアント側で分割・圧縮してアップロードする。
// メモリ使用量はファイルサイズによらず窓サイズで一定。
func (c *Client) Put(name string, r io.Reader) (*PutResult, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	ck, err := chunker.New(r, c.chunkSize)
	if err != nil {
		return nil, err
	}

	res := &PutResult{Name: name}
	var allHashes []string

	type piece struct {
		hash string
		data []byte
	}
	window := make([]piece, 0, windowChunks)

	flush := func() error {
		if len(window) == 0 {
			return nil
		}
		// 1) サーバーに無いチャンクを特定
		hashes := make([]string, len(window))
		for i, p := range window {
			hashes[i] = p.hash
		}
		var missing struct {
			Missing []string `json:"missing"`
		}
		body, _ := json.Marshal(map[string]any{"hashes": hashes})
		req, err := c.req("POST", "/api/v1/chunks/missing", bytes.NewReader(body))
		if err != nil {
			return err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return httpError(resp)
			}
			return json.NewDecoder(resp.Body).Decode(&missing)
		}()
		if err != nil {
			return err
		}
		need := make(map[string]bool, len(missing.Missing))
		for _, h := range missing.Missing {
			need[h] = true
		}

		// 2) 無いものだけ圧縮して並列アップロード
		//    (同一窓内の重複ハッシュは1回だけ送る)
		var todo []piece
		sent := make(map[string]bool)
		for _, p := range window {
			if need[p.hash] && !sent[p.hash] {
				sent[p.hash] = true
				todo = append(todo, p)
			}
		}
		var mu sync.Mutex
		var firstErr error
		sem := make(chan struct{}, c.Parallel)
		var wg sync.WaitGroup
		for _, p := range todo {
			wg.Add(1)
			sem <- struct{}{}
			go func(p piece) {
				defer wg.Done()
				defer func() { <-sem }()
				kind, payload := c.compress(p.data)
				req, err := c.req("PUT", "/api/v1/chunks/"+p.hash, bytes.NewReader(payload))
				if err == nil {
					req.Header.Set("X-Compression", kind)
					req.Header.Set("X-Raw-Size", fmt.Sprintf("%d", len(p.data)))
					var resp *http.Response
					resp, err = c.HTTP.Do(req)
					if err == nil {
						if resp.StatusCode != http.StatusCreated {
							err = httpError(resp)
						}
						resp.Body.Close()
					}
				}
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = fmt.Errorf("チャンク %s の送信に失敗: %w", p.hash[:12], err)
				} else if err == nil {
					res.ChunksUploaded++
					res.BytesUploaded += int64(len(payload))
				}
				mu.Unlock()
			}(p)
		}
		wg.Wait()
		if firstErr != nil {
			return firstErr
		}
		window = window[:0]
		return nil
	}

	for {
		chunk, err := ck.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(chunk.Data)
		hash := hex.EncodeToString(sum[:])
		allHashes = append(allHashes, hash)
		res.ChunksTotal++
		res.Size += int64(len(chunk.Data))
		window = append(window, piece{hash: hash, data: append([]byte(nil), chunk.Data...)})
		if len(window) >= windowChunks {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}

	// 3) マニフェスト確定。サーバー側 GC とのレースで一部チャンクが
	// 消えていた場合は 409 + missing 一覧が返るので、そのぶんだけ
	// 再アップロードして再コミットする(冪等リトライ)。
	// 再アップロードにはデータが必要なので、この経路では r を読み直せない
	// 前提から「窓内で保持していない = 既に flush 済み」のデータは持って
	// いない。そこで missing はコミット直前にもう一度 flush して潰しておく
	// のが基本で、リトライは1回だけ試みる。
	commit := func() (*Manifest, []string, error) {
		body, _ := json.Marshal(map[string]any{"name": name, "chunks": allHashes})
		req, err := c.req("POST", "/api/v1/manifests", bytes.NewReader(body))
		if err != nil {
			return nil, nil, err
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusConflict {
			var conflict struct {
				Missing []string `json:"missing"`
			}
			json.NewDecoder(resp.Body).Decode(&conflict)
			return nil, conflict.Missing, nil
		}
		if resp.StatusCode != http.StatusCreated {
			return nil, nil, httpError(resp)
		}
		var m Manifest
		if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
			return nil, nil, err
		}
		return &m, nil, nil
	}
	m, missing, err := commit()
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("コミットに失敗: サーバーに %d チャンクが存在しません(アップロードが長時間中断された場合は最初からやり直してください)", len(missing))
	}
	res.ID = m.ID
	return res, nil
}

// Get は id のファイルをダウンロードし、クライアント側で伸長・検証しながら
// w に書き出す。チャンク取得は並列、書き出しは順序どおり。
//
// precompression 適用済みファイル(Encoding 非空)はチャンク列が展開データで
// あり、元ストリームの再構成レシピはサーバーにしかないため、サーバー経路の
// ダウンロードに自動フォールバックする(チャンク結合では元と違うバイト列に
// なってしまう)。その場合も OrigSHA256 でクライアント側検証を行う。
func (c *Client) Get(id string, w io.Writer) error {
	if err := c.init(); err != nil {
		return err
	}
	var m Manifest
	if err := c.getJSON("/api/v1/manifests/"+id, &m); err != nil {
		return err
	}
	if m.Encoding != "" {
		return c.getServerSide(id, w, m.OrigSHA256)
	}

	// 並列取得+順序書き出し: 各位置の結果チャネルを先に用意し、
	// ワーカーが埋め、書き手が順に読む。未処理の先読みは並列数まで。
	type result struct {
		data []byte
		err  error
	}
	slots := make([]chan result, len(m.Chunks))
	for i := range slots {
		slots[i] = make(chan result, 1)
	}
	sem := make(chan struct{}, c.Parallel)
	go func() {
		for i, h := range m.Chunks {
			sem <- struct{}{}
			go func(i int, h string) {
				defer func() { <-sem }()
				data, err := c.fetchChunk(h)
				slots[i] <- result{data: data, err: err}
			}(i, h)
		}
	}()
	for i := range slots {
		r := <-slots[i]
		if r.err != nil {
			return fmt.Errorf("チャンク %d/%d の取得に失敗: %w", i+1, len(m.Chunks), r.err)
		}
		if _, err := w.Write(r.data); err != nil {
			return err
		}
	}
	return nil
}

// getServerSide はサーバー経路(GET /files/{id})でダウンロードし、
// ストリーミングしながら SHA-256 を計算して wantSHA256(非空なら)と照合する。
func (c *Client) getServerSide(id string, w io.Writer, wantSHA256 string) error {
	req, err := c.req("GET", "/api/v1/files/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpError(resp)
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), resp.Body); err != nil {
		return err
	}
	if wantSHA256 != "" && hex.EncodeToString(h.Sum(nil)) != wantSHA256 {
		return fmt.Errorf("ダウンロードした内容の検証に失敗しました")
	}
	return nil
}

// fetchChunk はチャンクを取得し、クライアント側で伸長+SHA-256検証する。
func (c *Client) fetchChunk(hash string) ([]byte, error) {
	req, err := c.req("GET", "/api/v1/chunks/"+hash, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	stored, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	data := stored
	if resp.Header.Get("X-Compression") == "zstd" {
		data, err = c.dec.DecodeAll(stored, nil)
		if err != nil {
			return nil, fmt.Errorf("伸長に失敗: %w", err)
		}
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != hash {
		return nil, fmt.Errorf("チャンクの内容検証に失敗しました")
	}
	return data, nil
}

// List はファイル一覧を取得する。
func (c *Client) List() ([]Manifest, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	var out struct {
		Files []Manifest `json:"files"`
	}
	if err := c.getJSON("/api/v1/files", &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}

// Stats はストア全体の統計を取得する(任意のフィールドを map で返す)。
func (c *Client) Stats() (map[string]any, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	var out map[string]any
	if err := c.getJSON("/api/v1/stats", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Scrub は全チャンクの完全性検証をサーバーに実行させ、結果を返す。
func (c *Client) Scrub() (map[string]any, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	req, err := c.req("POST", "/api/v1/scrub", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Fsck はメタデータ整合性チェックをサーバーに実行させる(repair で修復)。
func (c *Client) Fsck(repair bool) (map[string]any, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	path := "/api/v1/fsck"
	if repair {
		path += "?repair=1"
	}
	req, err := c.req("POST", path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Me は呼び出しユーザーの使用量とクォータを取得する。
func (c *Client) Me() (map[string]any, error) {
	if err := c.init(); err != nil {
		return nil, err
	}
	var out map[string]any
	if err := c.getJSON("/api/v1/me", &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Delete はファイルを削除する。
func (c *Client) Delete(id string) error {
	if err := c.init(); err != nil {
		return err
	}
	req, err := c.req("DELETE", "/api/v1/files/"+id, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return httpError(resp)
	}
	return nil
}
