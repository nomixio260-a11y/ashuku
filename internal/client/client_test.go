package client

import (
	"bytes"
	"crypto/sha256"
	"github.com/nomixio260-a11y/ashuku/internal/precomp"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nomixio260-a11y/ashuku/internal/api"
	"github.com/nomixio260-a11y/ashuku/internal/store"
)

func newServerAndClient(t *testing.T, opts api.Options) (*store.Store, *Client) {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Config{})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(api.New(st, opts))
	t.Cleanup(func() {
		srv.Close()
		st.Close()
	})
	return st, &Client{Base: srv.URL}
}

func mixedData(t *testing.T, size int) []byte {
	t.Helper()
	rng := rand.New(rand.NewSource(31))
	data := make([]byte, size)
	rng.Read(data[:size/2])
	line := []byte("client-side compression test line with repetitive content\n")
	for i := size / 2; i < size; i += len(line) {
		copy(data[i:], line)
	}
	return data
}

func TestClientPutGetRoundTrip(t *testing.T) {
	_, c := newServerAndClient(t, api.Options{})
	data := mixedData(t, 5<<20)

	res, err := c.Put("mixed.bin", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if res.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", res.Size, len(data))
	}
	if res.ChunksUploaded == 0 || res.ChunksUploaded != res.ChunksTotal {
		t.Fatalf("初回は全チャンク転送のはず: %d/%d", res.ChunksUploaded, res.ChunksTotal)
	}
	// 圧縮はクライアント側: 送信バイトは元より小さい(半分はテキスト)
	if res.BytesUploaded >= res.Size {
		t.Fatalf("送信 %d >= 元 %d: クライアント圧縮が効いていません", res.BytesUploaded, res.Size)
	}

	var out bytes.Buffer
	if err := c.Get(res.ID, &out); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(out.Bytes()) != sha256.Sum256(data) {
		t.Fatal("復元データが一致しません")
	}
}

// 2回目のアップロードは重複排除交渉により1チャンクも転送しない。
func TestClientDedupSkipsTransfer(t *testing.T) {
	_, c := newServerAndClient(t, api.Options{})
	data := mixedData(t, 3<<20)

	if _, err := c.Put("v1", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	res2, err := c.Put("v2", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if res2.ChunksUploaded != 0 || res2.BytesUploaded != 0 {
		t.Fatalf("2回目に %d チャンク %d bytes 転送: dedup交渉が効いていません",
			res2.ChunksUploaded, res2.BytesUploaded)
	}
	// 両ファイルとも読める
	var out bytes.Buffer
	if err := c.Get(res2.ID, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("dedup後の復元が一致しません")
	}
}

// チャンク汚染防止: 偽の内容を正しいハッシュ名で登録しようとすると拒否される。
func TestChunkPoisoningRejected(t *testing.T) {
	st, c := newServerAndClient(t, api.Options{})
	if err := c.init(); err != nil {
		t.Fatal(err)
	}
	victim := sha256.Sum256([]byte("victim content"))
	victimHash := "0000000000000000000000000000000000000000000000000000000000000000"
	_ = victim

	req, _ := c.req("PUT", "/api/v1/chunks/"+victimHash, bytes.NewReader([]byte("evil data")))
	req.Header.Set("X-Compression", "none")
	req.Header.Set("X-Raw-Size", "9")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("汚染チャンクが拒否されませんでした: status %d", resp.StatusCode)
	}
	// ストアにも入っていない
	missing, err := st.HasChunks([]string{victimHash})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 1 {
		t.Fatal("汚染チャンクがストアに登録されています")
	}
}

// 未コミットチャンクを参照しないマニフェストは 409 + missing 一覧。
func TestManifestMissingChunks(t *testing.T) {
	_, c := newServerAndClient(t, api.Options{})
	if err := c.init(); err != nil {
		t.Fatal(err)
	}
	fake := sha256.Sum256([]byte("never uploaded"))
	fakeHash := bytesToHex(fake[:])
	body := []byte(`{"name":"x","chunks":["` + fakeHash + `"]}`)
	req, _ := c.req("POST", "/api/v1/manifests", bytes.NewReader(body))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

func bytesToHex(b []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, x := range b {
		out = append(out, hexdigits[x>>4], hexdigits[x&15])
	}
	return string(out)
}

// クライアント経路でも認証・クォータが効く。
func TestClientAuthAndQuota(t *testing.T) {
	_, c := newServerAndClient(t, api.Options{Users: map[string]api.User{
		"key-a": {Quota: 1 << 20},
	}})

	// キーなし → init(/api/v1/config)の時点で 401
	if _, err := c.Put("x", bytes.NewReader([]byte("data"))); err == nil {
		t.Fatal("認証なしで通ってしまいました")
	}

	c2 := &Client{Base: c.Base, Key: "key-a"}
	data := mixedData(t, 700<<10)
	if _, err := c2.Put("one", bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	// クォータ超過(マニフェスト確定時に拒否される)
	if _, err := c2.Put("two", bytes.NewReader(mixedData(t, 900<<10))); err == nil {
		t.Fatal("クォータ超過が拒否されませんでした")
	}
}

// server-side-uploads=off ではサーバー側圧縮経路が拒否され、
// クライアント経路だけが機能する。
func TestServerSideUploadsOff(t *testing.T) {
	_, c := newServerAndClient(t, api.Options{ServerSideUploads: "off"})
	data := mixedData(t, 1<<20)

	// クライアント経路は動く
	res, err := c.Put("ok.bin", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := c.Get(res.ID, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatal("クライアント経路の復元が一致しません")
	}

	// 従来経路(サーバー側圧縮・展開)は 403
	req, _ := c.req("POST", "/api/v1/files?name=x", bytes.NewReader(data))
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("サーバー側アップロード: status = %d, want 403", resp.StatusCode)
	}
	req, _ = c.req("GET", "/api/v1/files/"+res.ID, nil)
	resp, err = c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("サーバー側ダウンロード: status = %d, want 403", resp.StatusCode)
	}
}

// クライアント経路のチャンクはオフラインデルタパスで後追い圧縮される。
func TestOfflineDeltaUpgradesClientChunks(t *testing.T) {
	st, c := newServerAndClient(t, api.Options{})

	base := make([]byte, 2<<20)
	rand.New(rand.NewSource(77)).Read(base)
	edited := append([]byte(nil), base...)
	copy(edited[1<<20:], []byte("EDITED-REGION"))

	r1, err := c.Put("v1", bytes.NewReader(base))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Put("v2", bytes.NewReader(edited)); err != nil {
		t.Fatal(err)
	}

	before, _ := st.Stats()
	res, err := st.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	after, _ := st.Stats()
	if res.DeltaUpgraded == 0 {
		t.Fatalf("オフラインデルタが適用されていません (before physical=%d after=%d)",
			before.PhysicalBytes, after.PhysicalBytes)
	}
	if after.PhysicalBytes >= before.PhysicalBytes {
		t.Fatalf("物理容量が減っていません: %d → %d", before.PhysicalBytes, after.PhysicalBytes)
	}
	// 復元整合性
	var out bytes.Buffer
	if err := c.Get(r1.ID, &out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), base) {
		t.Fatal("デルタ化後の復元が一致しません")
	}
}

// precompression されたファイル(サーバー経路でアップロードされた gzip 等)を
// クライアント経路でダウンロードしても、元のバイト列がビット一致で返る
// (チャンク列=展開データの結合ではなく、サーバー経路への自動フォール
// バックで再構成される)。修正前は展開データがエラーなしで返るバグだった。
func TestClientGetPrecompFileFallsBack(t *testing.T) {
	if !precomp.Supported() {
		t.Skip("CGO 無効")
	}
	st, c := newServerAndClient(t, api.Options{})

	// zlib 産 gzip を合成し(Reconstruct はシステム zlib を使う)、
	// サーバー経路(store.Put)で保存 → precomp が適用される
	header := []byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3}
	plain := bytes.Repeat([]byte("precomp fallback test data! "), 20000)
	orig, err := precomp.Reconstruct(header, 6, plain)
	if err != nil {
		t.Fatal(err)
	}
	m, err := st.Put("test.gz", bytes.NewReader(orig))
	if err != nil {
		t.Fatal(err)
	}
	if m.Encoding == "" {
		t.Fatal("precomp が適用されていません(テスト前提が崩れています)")
	}

	var out bytes.Buffer
	if err := c.Get(m.ID, &out); err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(out.Bytes()) != sha256.Sum256(orig) {
		t.Fatalf("クライアント経路の復元が元と一致しません: got %d bytes, want %d",
			out.Len(), len(orig))
	}
}
