package web

import (
	"net/http"
	"sync"
	"time"
)

// securityHeaders 全站安全响应头
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-XSS-Protection", "0")
		next.ServeHTTP(w, r)
	})
}

// payRateLimit 支付接口限频: 同一 IP 每分钟最多 N 次(防刷)
type minuteWindow struct {
	minute int64
	count  int
}

var (
	rlMu    sync.Mutex
	rlMap   = map[string]*minuteWindow{}
	rlLimit = 120
)

func payRateLimited(ip string) bool {
	nowMinute := time.Now().Unix() / 60
	rlMu.Lock()
	defer rlMu.Unlock()
	w := rlMap[ip]
	if w == nil || w.minute != nowMinute {
		if len(rlMap) > 10000 { // 防止恶意 IP 撑爆内存
			rlMap = map[string]*minuteWindow{}
		}
		w = &minuteWindow{minute: nowMinute}
		rlMap[ip] = w
	}
	w.count++
	return w.count > rlLimit
}

func payRateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if payRateLimited(clientIP(r)) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"code":-1,"msg":"请求过于频繁,请稍后再试"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
