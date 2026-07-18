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
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	// 自己完結ページ: 外部リソースを一切許可しない CSP(インラインのみ)。
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(consoleHTML)
}
