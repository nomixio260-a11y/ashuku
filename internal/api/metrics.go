package api

// 最小の Prometheus 互換メトリクス(外部依存なし)。
//
// カウンタとゲージだけを持ち、Prometheus テキスト形式で /metrics に出力する。
// 本番運用の可観測性(リクエスト数・エラー率・転送量・ストレージ増加・
// ディスク空き)を、client_golang を持ち込まずに提供する。

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// metrics はサーバーのメトリクスレジストリ。
type metrics struct {
	// requests[method+" "+status] = 回数
	mu       sync.Mutex
	requests map[string]int64

	bytesUploaded   atomic.Int64
	bytesDownloaded atomic.Int64
	uploadsTotal    atomic.Int64
	uploadErrors    atomic.Int64
	panics          atomic.Int64
}

func newMetrics() *metrics {
	return &metrics{requests: make(map[string]int64)}
}

func (m *metrics) observeRequest(method string, status int) {
	key := method + " " + statusClass(status)
	m.mu.Lock()
	m.requests[key]++
	m.mu.Unlock()
}

// statusClass は 200→"2xx" のようにステータスをクラスへ丸める(カーディナリティ抑制)。
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "other"
	}
}

// render は Prometheus テキスト形式でメトリクスを書き出す。
// gauges は /metrics 時に集めたライブ値(ストレージ統計・ディスク空き)。
func (m *metrics) render(gauges map[string]int64) string {
	var b []string
	add := func(name, help, typ string, lines []string) {
		b = append(b, "# HELP "+name+" "+help)
		b = append(b, "# TYPE "+name+" "+typ)
		b = append(b, lines...)
	}

	m.mu.Lock()
	var reqLines []string
	keys := make([]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		method, class := splitReqKey(k)
		reqLines = append(reqLines, fmt.Sprintf(
			"ashuku_http_requests_total{method=%q,status=%q} %d", method, class, m.requests[k]))
	}
	m.mu.Unlock()
	add("ashuku_http_requests_total", "HTTP リクエスト数(ステータスクラス別)", "counter", reqLines)

	add("ashuku_bytes_uploaded_total", "受信バイト総数", "counter",
		[]string{fmt.Sprintf("ashuku_bytes_uploaded_total %d", m.bytesUploaded.Load())})
	add("ashuku_bytes_downloaded_total", "送信バイト総数", "counter",
		[]string{fmt.Sprintf("ashuku_bytes_downloaded_total %d", m.bytesDownloaded.Load())})
	add("ashuku_uploads_total", "アップロード試行総数", "counter",
		[]string{fmt.Sprintf("ashuku_uploads_total %d", m.uploadsTotal.Load())})
	add("ashuku_upload_errors_total", "アップロード失敗総数", "counter",
		[]string{fmt.Sprintf("ashuku_upload_errors_total %d", m.uploadErrors.Load())})
	add("ashuku_panics_total", "捕捉したパニック総数", "counter",
		[]string{fmt.Sprintf("ashuku_panics_total %d", m.panics.Load())})

	// ライブゲージ(ストレージ統計・ディスク空き)
	gkeys := make([]string, 0, len(gauges))
	for k := range gauges {
		gkeys = append(gkeys, k)
	}
	sort.Strings(gkeys)
	for _, k := range gkeys {
		add(k, k, "gauge", []string{fmt.Sprintf("%s %d", k, gauges[k])})
	}

	out := ""
	for _, line := range b {
		out += line + "\n"
	}
	return out
}

func splitReqKey(k string) (method, class string) {
	for i := 0; i < len(k); i++ {
		if k[i] == ' ' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}
