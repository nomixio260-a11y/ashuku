package api

// Web コンソール。
//
// バイナリに埋め込んだ単一の自己完結 HTML(外部 CDN・フォント等への参照
// なし = 技術流出防止の方針どおり閉じたページ)をルートで配信する。
// ページ自体は静的で秘密を含まず、操作はブラウザから既存の REST API を
// APIキー付きで呼ぶだけ(認証は API 層がそのまま適用される)。

import (
	_ "embed"
	"net/http"
)

//go:embed console.html
var consoleHTML []byte

// handleConsole は Web コンソール(単一ページ)を返す。
// モバイルファーストのレスポンシブ設計で、スマホ・タブレット・PC の
// どれでもそのまま使える(ライト/ダークは OS 設定に自動追従+手動切替)。
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	// 自己完結ページ: 外部リソースを一切許可しない CSP(インラインのみ)。
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; manifest-src 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(consoleHTML)
}

// webManifest は PWA マニフェスト(スマホの「ホーム画面に追加」で
// アプリのように開ける)。アイコンは自己完結の SVG データURI。
var webManifest = []byte(`{
  "name": "ashuku 圧縮ストレージ",
  "short_name": "ashuku",
  "start_url": "/",
  "display": "standalone",
  "background_color": "#0f1420",
  "theme_color": "#4f8cff",
  "icons": [{
    "src": "data:image/svg+xml,%3Csvg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'%3E%3Crect width='100' height='100' rx='20' fill='%234f8cff'/%3E%3Ctext x='50' y='68' font-size='52' text-anchor='middle' fill='white' font-family='sans-serif'%3E%E5%9C%A7%3C/text%3E%3C/svg%3E",
    "sizes": "any",
    "type": "image/svg+xml",
    "purpose": "any"
  }]
}`)

// handleManifest は PWA マニフェストを返す(認証不要の静的リソース)。
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(webManifest)
}
