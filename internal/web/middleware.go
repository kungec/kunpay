package web

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
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
		h.Set("X-Permitted-Cross-Domain-Policies", "none")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: https:; font-src 'self'; connect-src 'self'; frame-ancestors 'self'")

		// HSTS(仅HTTPS请求)
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}

		next.ServeHTTP(w, r)
	})
}

// limitBody 请求体大小限制(10MB)
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		}
		next.ServeHTTP(w, r)
	})
}

// isHTTPS 判断当前请求是否走 HTTPS
func isHTTPSRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// sessionIdleTimeoutSec 会话空闲超时(秒)
const sessionIdleTimeoutSec = 1800 // 30分钟

// maxSessionsPerUser 单用户最大并发会话数
const maxSessionsPerUser = 5

// ---- 密码复杂度校验 ----

func validatePasswordStrength(pw string) error {
	if len(pw) < 6 {
		return fmt.Errorf("密码长度至少 6 位")
	}
	hasLetter := false
	hasDigit := false
	for _, ch := range pw {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') {
			hasLetter = true
		}
		if ch >= '0' && ch <= '9' {
			hasDigit = true
		}
	}
	if !hasLetter {
		return fmt.Errorf("密码需包含字母")
	}
	if !hasDigit && len(pw) < 10 {
		return fmt.Errorf("纯数字密码需至少 10 位")
	}
	return nil
}

// ---- 管理端 IP 白名单 ----

func isAdminIPAllowed(ip, whitelist string) bool {
	if whitelist == "" {
		return true
	}
	for _, allowed := range strings.Split(whitelist, ",") {
		allowed = strings.TrimSpace(allowed)
		if allowed == ip {
			return true
		}
	}
	return false
}

// ---- 常量时间比较(防时序攻击) ----

func constantTimeEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// payRateLimitMiddleware 支付接口限频: 同一 IP 每分钟最多 N 次(防刷)
var (
	rlMu    sync.Mutex
	rlMap   = map[string]*rlWindow{}
	rlLimit = 120
)

type rlWindow struct {
	minute int64
	count  int
}

func payRateLimited(ip string) bool {
	nowMinute := time.Now().Unix() / 60
	rlMu.Lock()
	defer rlMu.Unlock()
	w := rlMap[ip]
	if w == nil || w.minute != nowMinute {
		if len(rlMap) > 10000 {
			rlMap = map[string]*rlWindow{}
		}
		w = &rlWindow{minute: nowMinute}
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

// payRateLimitWrap 支付接口限频包装器
func payRateLimitWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只限频支付相关路径
		if strings.Contains(r.URL.Path, "submit.php") ||
			strings.Contains(r.URL.Path, "mapi.php") ||
			strings.Contains(r.URL.Path, "api.php") {
			if payRateLimited(clientIP(r)) {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.WriteHeader(http.StatusTooManyRequests)
				w.Write([]byte(`{"code":-1,"msg":"请求过于频繁,请稍后再试"}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
