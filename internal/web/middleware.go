package web

import (
	"fmt"
	"kunzhifu/internal/model"
	"net"
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

// limitBody 请求体大小限制(默认 10MB;下载中心文件上传路由放宽到 200MB)
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			limit := int64(10 << 20)
			if r.Method == http.MethodPost && r.URL.Path == "/admin/downloads/upload" {
				limit = 200 << 20
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// isHTTPS 判断当前请求是否走 HTTPS
// X-Forwarded-Proto 仅在直连对端为内网/回环(即真实反代)时采信,与 clientIP 同策略
func isHTTPSRequest(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return TrustProxyHeaders && isTrustedProxyPeer(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
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

// 支付接口限频窗口: 同一 IP 每分钟最多 rlLimit 次(防刷)
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

// payRateLimitWrap 支付接口限频包装器
func payRateLimitWrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 只限频支付相关路径(demo 下单公开且创建真实订单,一并纳入限频防刷)
		if strings.Contains(r.URL.Path, "submit.php") ||
			strings.Contains(r.URL.Path, "mapi.php") ||
			strings.Contains(r.URL.Path, "api.php") ||
			strings.HasSuffix(r.URL.Path, "/amount") ||
			strings.HasSuffix(r.URL.Path, "/demo/create") {
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

// apiIPAllowed 判断来源 IP 是否命中商户 API 白名单。
// 支持精确 IP、CIDR(如 203.0.113.0/24)、通配前缀(如 203.0.113.*);白名单为空表示不限制。
func apiIPAllowed(list, ip string) bool {
	list = strings.TrimSpace(list)
	if list == "" {
		return true
	}
	pip := net.ParseIP(strings.TrimSpace(ip))
	if pip == nil {
		return false
	}
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			if _, ipnet, err := net.ParseCIDR(entry); err == nil && ipnet.Contains(pip) {
				return true
			}
			continue
		}
		if strings.HasSuffix(entry, "*") {
			if strings.HasPrefix(ip+".", strings.TrimSuffix(entry, "*")) {
				return true
			}
			continue
		}
		if e := net.ParseIP(entry); e != nil && e.Equal(pip) {
			return true
		}
	}
	return false
}

// ---- 商户级下单限流(内存滑动窗口,按分钟) ----
var apiThrottleMu sync.Mutex
var apiThrottle = map[int64]*apiWindow{}

type apiWindow struct {
	winStart int64
	count    int
}

// merchantApiThrottle 商户限流: api_rate>0 时按该值限流,0=跟随全局默认(120/分钟)
func merchantApiThrottle(m *model.User) bool {
	limit := m.ApiRate
	if limit <= 0 {
		return true // 跟随全局 IP 限频,不再叠加
	}
	now := model.Now()
	win := now / 60
	apiThrottleMu.Lock()
	defer apiThrottleMu.Unlock()
	w := apiThrottle[m.ID]
	if w == nil || w.winStart != win {
		w = &apiWindow{winStart: win, count: 0}
		apiThrottle[m.ID] = w
		// 顺带清理其他过期窗口,防 map 无限增长
		if len(apiThrottle) > 4096 {
			for k, v := range apiThrottle {
				if v.winStart < win-2 {
					delete(apiThrottle, k)
				}
			}
		}
	}
	if w.count >= int(limit) {
		return false
	}
	w.count++
	return true
}
