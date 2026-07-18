package api

// 認証失敗のレート制限(総当たり対策)。
//
// APIキーの照合自体は定時間比較だが、無制限に試行できると総当たりの
// 試行回数を稼がれる。送信元IPごとの失敗回数を1分窓で数え、しきい値を
// 超えたIPからの認証付きリクエストを 429 で拒否する(窓が切り替わると
// 自動解除)。成功したユーザーには影響しない(失敗だけを数える)。
//
// メモリ上限: 記録するIPは maxTrackedIPs までで、超えたら全消去する
// (攻撃者が大量の偽装IPでメモリを膨らませるのを防ぐ。全消去は
// レート制限が一瞬緩むだけで安全側の失敗にはならない)。

import (
	"net"
	"sync"
	"time"
)

const (
	// authFailureLimit は1分窓あたりの許容失敗回数。
	authFailureLimit = 20
	// maxTrackedIPs を超えたら記録を全消去する(メモリ上限)。
	maxTrackedIPs = 65536
)

type authLimiter struct {
	mu     sync.Mutex
	window time.Time
	fails  map[string]int
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{fails: make(map[string]int)}
}

// rotate は1分窓の切り替え(呼び出し側が mu を保持)。
func (l *authLimiter) rotate(now time.Time) {
	if now.Sub(l.window) >= time.Minute {
		l.window = now
		l.fails = make(map[string]int)
	}
}

// blocked は ip からの認証試行を拒否すべきかを返す。
func (l *authLimiter) blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotate(time.Now())
	return l.fails[ip] >= authFailureLimit
}

// fail は ip の認証失敗を記録する。
func (l *authLimiter) fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotate(time.Now())
	if len(l.fails) >= maxTrackedIPs {
		l.fails = make(map[string]int)
	}
	l.fails[ip]++
}

// remoteIP は RemoteAddr からポートを除いたIPを返す。
func remoteIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}
