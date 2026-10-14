package web

import (
	"kunzhifu/internal/service"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"kunzhifu/internal/db"
	"kunzhifu/internal/model"
)

const sessionCookie = "kunpay_sid"

// AllowDefaultAdmin 关闭"默认管理员密码强制修改"(回归测试/演示环境用)
var AllowDefaultAdmin = false

// defPwCache 缓存"该密码哈希是否为默认密码"的 bcrypt 校验结果,避免每个请求都做慢哈希
var defPwCache sync.Map // passwordHash -> bool

// usingDefaultAdminPw 管理员是否仍在使用初始密码 admin123
func (s *Server) usingDefaultAdminPw(u *model.User) bool {
	if AllowDefaultAdmin || u == nil || u.Role != "admin" {
		return false
	}
	if s.DB.MustSetting("admin_default_pw") != "1" {
		return false
	}
	h := u.PasswordHash
	if v, ok := defPwCache.Load(h); ok {
		return v.(bool)
	}
	isDef := bcrypt.CompareHashAndPassword([]byte(h), []byte("admin123")) == nil
	defPwCache.Store(h, isDef)
	return isDef
}

// isPwChangePath 强制改密期间仍可访问的路径
func isPwChangePath(p string) bool {
	return p == "/user/settings" || p == "/user/settings/password" || p == "/logout" || p == "/user"
}

// ClearDefaultAdminPwFlag 管理员修改密码后清除强制改密标记
func (s *Server) ClearDefaultAdminPwFlag() {
	_, _ = s.DB.Exec("UPDATE settings SET v='0' WHERE k='admin_default_pw'")
}

var (
	reUsername   = regexp.MustCompile(`^[a-zA-Z0-9_]{3,20}$`)
	reEmail      = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
	reTOTPSecret = regexp.MustCompile(`^[A-Z2-7]{16,64}$`)
)

func (s *Server) setSession(w http.ResponseWriter, r *http.Request, userID int64) {
	token := db.RandKey(32)
	now := model.Now()
	ua := r.UserAgent()
	if len(ua) > 120 {
		ua = ua[:120]
	}
	// 单用户最大并发会话数: 保留最近 maxSessionsPerUser-1 个,腾位给新会话
	if maxSessionsPerUser > 0 {
		_, _ = s.DB.Exec(`DELETE FROM sessions WHERE user_id=? AND token NOT IN (
			SELECT token FROM sessions WHERE user_id=?
			ORDER BY COALESCE(last_seen,0) DESC, created_at DESC LIMIT ?)`,
			userID, userID, maxSessionsPerUser-1)
	}
	_, err := s.DB.Exec("INSERT INTO sessions(token,user_id,expires_at,created_at,ip,ua,last_seen) VALUES(?,?,?,?,?,?,?)",
		token, userID, now+7*86400, now, clientIP(r), ua, now)
	if err != nil {
		http.Error(w, "创建会话失败", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPSRequest(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   7 * 86400,
	})
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_, _ = s.DB.Exec("DELETE FROM sessions WHERE token=?", c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: isHTTPSRequest(r), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

var lastSeenMu sync.Mutex
var lastSeenAt = map[string]int64{}

func (s *Server) currentUser(r *http.Request) *model.User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	var userID, expires, lastSeen, createdAt int64
	var sip string
	err = s.DB.QueryRow("SELECT user_id,expires_at,COALESCE(last_seen,0),created_at,COALESCE(ip,'') FROM sessions WHERE token=?", c.Value).
		Scan(&userID, &expires, &lastSeen, &createdAt, &sip)
	if err != nil || expires < model.Now() {
		return nil
	}
	// 空闲超时: 超过 sessionIdleTimeoutSec 无活动(以 last_seen/created_at 较新者计)即销毁会话
	now := model.Now()
	effLast := lastSeen
	if effLast < createdAt {
		effLast = createdAt
	}
	if sessionIdleTimeoutSec > 0 && now-effLast > sessionIdleTimeoutSec {
		_, _ = s.DB.Exec("DELETE FROM sessions WHERE token=?", c.Value)
		return nil
	}
	// 活跃心跳限频写库: 距上次 >60s 才更新,并记录最新 IP
	lastSeenMu.Lock()
	last := lastSeenAt[c.Value]
	lastSeenMu.Unlock()
	if now-last > 60 {
		ip := clientIP(r)
		if ip != sip || now-lastSeen > 60 {
			_, _ = s.DB.Exec("UPDATE sessions SET last_seen=?, ip=? WHERE token=?", now, ip, c.Value)
		}
		lastSeenMu.Lock()
		lastSeenAt[c.Value] = now
		// 防无界增长: 条目过多时清掉陈旧 token 的记录
		if len(lastSeenAt) > 4096 {
			for tk, ts := range lastSeenAt {
				if now-ts > 3600 {
					delete(lastSeenAt, tk)
				}
			}
		}
		lastSeenMu.Unlock()
	}
	u, _ := s.Store.UserByID(userID)
	if u == nil || u.Status != 1 {
		return nil
	}
	return u
}

// SessionDevice 当前用户会话设备列表(标记当前会话)
type SessionDevice struct {
	Token    string
	IP       string
	UA       string
	Created  int64
	LastSeen int64
	Current  bool
}

func (s *Server) sessionDevices(r *http.Request) []SessionDevice {
	c, err := r.Cookie(sessionCookie)
	cur := ""
	if err == nil {
		cur = c.Value
	}
	rows, err := s.DB.Query(`SELECT token,COALESCE(ip,''),COALESCE(ua,''),created_at,COALESCE(last_seen,created_at)
		FROM sessions WHERE user_id=? AND expires_at>? ORDER BY COALESCE(last_seen,created_at) DESC`,
		s.currentUser(r).ID, model.Now())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []SessionDevice
	for rows.Next() {
		var d SessionDevice
		if rows.Scan(&d.Token, &d.IP, &d.UA, &d.Created, &d.LastSeen) == nil {
			d.Current = d.Token == cur
			out = append(out, d)
		}
	}
	return out
}

// RevokeOtherSessions 下线当前账号除当前会话外的所有会话
func (s *Server) RevokeOtherSessions(userID int64, curToken string) int64 {
	res, _ := s.DB.Exec("DELETE FROM sessions WHERE user_id=? AND token<>?", userID, curToken)
	n, _ := res.RowsAffected()
	return n
}

// requireLogin 包装: 未登录跳转 /login
func (s *Server) requireLogin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusFound)
			return
		}
		// 管理员仍在使用默认密码: 除改密相关页面外全部拦截
		if s.usingDefaultAdminPw(u) && !isPwChangePath(r.URL.Path) {
			http.Redirect(w, r, "/user/settings?err="+urlEscape("安全提示: 请先修改管理员默认密码后再进行其他操作"), http.StatusFound)
			return
		}
		next(w, r)
	}
}

// requireAdmin 包装: 需要管理员
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := s.currentUser(r)
		if u == nil {
			http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusFound)
			return
		}
		if u.Role != "admin" {
			http.Error(w, "无权访问", http.StatusForbidden)
			return
		}
		if s.usingDefaultAdminPw(u) && !isPwChangePath(r.URL.Path) {
			http.Redirect(w, r, "/user/settings?err="+urlEscape("安全提示: 请先修改管理员默认密码后再进行其他操作"), http.StatusFound)
			return
		}
		if wl := s.DB.MustSetting("admin_ip_whitelist"); wl != "" {
			if !isAdminIPAllowed(clientIP(r), wl) {
				http.Error(w, "IP 不在白名单中", http.StatusForbidden)
				return
			}
		}
		next(w, r)
	}
}

// requireCSRF 包装: 校验 POST 表单 CSRF
func (s *Server) requireCSRF(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && !s.checkCSRF(r) {
			http.Error(w, "会话已过期,请刷新页面重试", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// TrustProxyHeaders 是否信任反代头(默认 true)。仅在直连对端为内网/回环地址时才
// 真正采纳转发头 —— 反向代理几乎总是部署在本机或内网,而公网直连者提供的
// X-Real-IP/X-Forwarded-For 不可信(可伪造绕过 IP 白名单与限流)。
var TrustProxyHeaders = true

func isTrustedProxyPeer(r *http.Request) bool {
	ra := r.RemoteAddr
	if h, _, err := net.SplitHostPort(ra); err == nil {
		ra = h
	}
	ip := net.ParseIP(ra)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func clientIP(r *http.Request) string {
	if TrustProxyHeaders && isTrustedProxyPeer(r) {
		if xf := r.Header.Get("X-Real-IP"); xf != "" {
			return strings.TrimSpace(strings.Split(xf, ",")[0])
		}
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			return strings.TrimSpace(strings.Split(xf, ",")[0])
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndexByte(host, ':'); i > 0 {
		host = host[:i]
	}
	return host
}

// ---- 注册 ----

func (s *Server) handleRegisterPage(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/user", http.StatusFound)
		return
	}
	if s.DB.MustSetting("register_open") != "1" {
		s.render(w, "pub_message", http.StatusOK, map[string]any{
			"Page": "message", "Title": "暂未开放注册",
			"Msg": "系统暂未开放注册,请联系管理员。", "Settings": s.pageData(r)["Settings"],
		})
		return
	}
	d := s.pageData(r)
	d["Page"] = "register"
	s.newCaptchaData(d)
	s.render(w, "pub_register", http.StatusOK, d)
}

// 注册防滥用: 同一 IP 每小时最多注册 5 个账号
var regCounts sync.Map // ip -> *regCount

type regCount struct {
	n      int
	window time.Time
}

func registerAllowed(ip string) bool {
	v, _ := regCounts.LoadOrStore(ip, &regCount{window: time.Now()})
	rc := v.(*regCount)
	if time.Since(rc.window) > time.Hour {
		rc.n = 0
		rc.window = time.Now()
	}
	rc.n++
	return rc.n <= 5
}

func (s *Server) handleRegisterPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if !registerAllowed(clientIP(r)) {
		d := s.pageData(r)
		d["Page"] = "register"
		d["Error"] = "该 IP 注册过于频繁,请稍后再试"
		s.render(w, "pub_register", http.StatusTooManyRequests, d)
		return
	}
	username := strings.TrimSpace(r.FormValue("username"))
	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	password2 := r.FormValue("password2")
	qq := strings.TrimSpace(r.FormValue("qq"))
	invite := strings.TrimSpace(r.FormValue("invite_code"))

	if s.CaptchaEnabled && !verifyCaptcha(r.FormValue("captcha_id"), r.FormValue("captcha_code")) {
		failCaptcha(s, w, r)
		return
	}

	fail := func(msg string) {
		d := s.pageData(r)
		d["Page"] = "register"
		d["Error"] = msg
		d["Form"] = map[string]string{"username": username, "email": email, "qq": qq}
		s.newCaptchaData(d)
		s.render(w, "pub_register", http.StatusOK, d)
	}

	if s.DB.MustSetting("register_open") != "1" {
		fail("系统暂未开放注册")
		return
	}
	if !reUsername.MatchString(username) {
		fail("用户名须为 3-20 位字母、数字或下划线")
		return
	}
	if email != "" && !reEmail.MatchString(email) {
		fail("邮箱格式不正确")
		return
	}
	if len(password) < 6 {
		fail("密码长度至少 6 位")
		return
	}
	if password != password2 {
		fail("两次输入的密码不一致")
		return
	}
	if s.DB.MustSetting("invite_open") == "1" && invite != "" {
		var n int
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM users WHERE invite_code=?", invite).Scan(&n)
		if n == 0 {
			fail("邀请码无效")
			return
		}
	}
	if exists, _ := s.Store.UserByUsername(username); exists != nil {
		fail("用户名已被注册")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		fail("注册失败,请稍后再试")
		return
	}
	// 分配递增 PID
	var maxPid int64
	_ = s.DB.QueryRow("SELECT MAX(pid) FROM users").Scan(&maxPid)
	newPid := maxPid + 1
	if newPid < 1001 {
		newPid = 1001
	}
	var inviter int64
	if invite != "" {
		_ = s.DB.QueryRow("SELECT id FROM users WHERE invite_code=?", invite).Scan(&inviter)
	}
	res, err := s.DB.Exec(`INSERT INTO users(username,email,password_hash,qq,role,status,pid,skey,invite_code,invited_by,created_at)
		VALUES(?,?,?,?,'user',1,?,?,?,?,?)`,
		username, email, string(hash), qq, newPid, db.RandKey(32), db.RandKey(8), inviter, model.Now())
	if err != nil {
		fail("注册失败: 用户名或邮箱可能已被占用")
		return
	}
	uid, _ := res.LastInsertId()
	// 邀请人奖励(可选): 不做资金奖励,仅记录
	s.setSession(w, r, uid)
	http.Redirect(w, r, "/user", http.StatusFound)
}

// ---- 登录 ----

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	if s.currentUser(r) != nil {
		http.Redirect(w, r, "/user", http.StatusFound)
		return
	}
	d := s.pageData(r)
	d["Page"] = "login"
	s.newCaptchaData(d)
	s.render(w, "pub_login", http.StatusOK, d)
}

// 登录防爆破: 同一 IP+用户名 15 分钟内失败 5 次则锁定 15 分钟
var loginFails sync.Map // key -> *failState

type failState struct {
	count    int
	lockedAt time.Time
}

func loginKey(ip, username string) string { return ip + "|" + username }

func loginLocked(ip, username string) (bool, time.Duration) {
	if v, ok := loginFails.Load(loginKey(ip, username)); ok {
		fs := v.(*failState)
		if fs.count >= 5 && time.Since(fs.lockedAt) < 15*time.Minute {
			return true, 15*time.Minute - time.Since(fs.lockedAt)
		}
		if fs.count >= 5 {
			fs.count = 0 // 锁定期结束,重新计数
		}
	}
	return false, 0
}

func recordLoginFail(ip, username string) {
	key := loginKey(ip, username)
	v, _ := loginFails.LoadOrStore(key, &failState{})
	fs := v.(*failState)
	fs.count++
	if fs.count == 1 {
		fs.lockedAt = time.Now()
	}
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")

	if locked, remain := loginLocked(clientIP(r), username); locked {
		// IP 维度锁定

		d := s.pageData(r)
		d["Page"] = "login"
		d["Error"] = "尝试次数过多,请 " + strconv.Itoa(int(remain.Minutes())+1) + " 分钟后再试"
		s.render(w, "pub_login", http.StatusTooManyRequests, d)
		return
	}

	d := s.pageData(r)
	d["Page"] = "login"
	s.newCaptchaData(d)
	if s.CaptchaEnabled && !verifyCaptcha(r.FormValue("captcha_id"), r.FormValue("captcha_code")) {
		d["Error"] = "验证码错误或已过期"
		s.render(w, "pub_login", http.StatusOK, d)
		return
	}
	// 用户名维度锁定: 防止伪造 X-Forwarded-For 绕过 IP 锁定
	if locked, remain := loginLocked("u", username); locked {
		d["Error"] = "尝试次数过多,请 " + strconv.Itoa(int(remain.Minutes())+1) + " 分钟后再试"
		s.render(w, "pub_login", http.StatusTooManyRequests, d)
		return
	}
	failMsg := "用户名或密码错误"

	u, err := s.Store.UserByUsername(username)
	if err != nil || u == nil {
		recordLoginFail(clientIP(r), username)
		recordLoginFail("u", username)
		d["Error"] = failMsg
		s.newCaptchaData(d)
		s.render(w, "pub_login", http.StatusOK, d)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)) != nil {
		recordLoginFail(clientIP(r), username)
		recordLoginFail("u", username)
		d["Error"] = failMsg
		s.newCaptchaData(d)
		s.render(w, "pub_login", http.StatusOK, d)
		return
	}
	if u.Status != 1 {
		d["Error"] = "账号已被封禁,请联系管理员"
		s.render(w, "pub_login", http.StatusOK, d)
		return
	}
	// 两步验证: 开启后必须携带有效动态码(或一次性恢复代码)
	if u.TotpSecret != "" {
		code := r.FormValue("totp_code")
		if !service.VerifyTOTP(u.TotpSecret, code) && !s.Store.ConsumeRecoveryCode(u.ID, code) {
			recordLoginFail(clientIP(r), username)
			recordLoginFail("u", username)
			d["Error"] = "两步验证码不正确"
			d["NeedTOTP"] = true
			s.newCaptchaData(d)
			s.render(w, "pub_login", http.StatusOK, d)
			return
		}
	}
	loginFails.Delete(loginKey(clientIP(r), username))
	loginFails.Delete(loginKey("u", username)) // 成功登录同时清除用户名维度失败计数,避免被第三方恶意锁死
	prevIP := u.LastLoginIP
	curIP := clientIP(r)
	_, _ = s.DB.Exec("UPDATE users SET last_login_at=?, last_login_ip=? WHERE id=?",
		model.Now(), curIP, u.ID)
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	_, _ = s.DB.Exec("INSERT INTO login_logs(user_id,ip,user_agent,created_at) VALUES(?,?,?,?)",
		u.ID, curIP, ua, model.Now())
	// 登录 IP 变化邮件提醒(异步,不影响登录)
	if prevIP != "" && prevIP != curIP && u.Email != "" {
		go func(mail, name, from, to string, site string) {
			body := mailHTML("登录安全提醒", []string{
				"您的账号 " + name + " 刚刚在新 IP 地址登录:",
				"本次登录 IP:" + to,
				"上次登录 IP:" + from,
				"如非本人操作,请立即修改密码并检查账户安全。",
			})
			_ = s.SendMail(mail, "【"+site+"】登录安全提醒", body)
		}(u.Email, u.Username, prevIP, curIP, s.DB.MustSetting("site_name"))
	}
	s.setSession(w, r, u.ID)
	next := r.FormValue("next")
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/user"
	}
	http.Redirect(w, r, next, http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w, r)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// ---- 修改密码辅助 ----

func hashPassword(pw string) string {
	h, _ := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	return string(h)
}

// ---------- 忘记密码(邮件重置) ----------

var resetCounts sync.Map // ip -> *regCount (复用计数结构)

func resetAllowed(ip string) bool {
	v, _ := resetCounts.LoadOrStore(ip, &regCount{window: time.Now()})
	rc := v.(*regCount)
	if time.Since(rc.window) > time.Hour {
		rc.n = 0
		rc.window = time.Now()
	}
	rc.n++
	return rc.n <= 3
}

func (s *Server) handleForgotPage(w http.ResponseWriter, r *http.Request) {
	d := s.pageData(r)
	d["Page"] = "forgot"
	d["Done"] = false
	s.newCaptchaData(d)
	s.render(w, "pub_forgot", http.StatusOK, d)
}

func (s *Server) handleForgotPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	if s.CaptchaEnabled && !verifyCaptcha(r.FormValue("captcha_id"), r.FormValue("captcha_code")) {
		failCaptcha(s, w, r)
		return
	}
	account := strings.TrimSpace(r.FormValue("account"))
	d := s.pageData(r)
	d["Page"] = "forgot"
	if !resetAllowed(clientIP(r)) {
		d["Msg"] = "请求过于频繁,请稍后再试"
		d["Done"] = true
		s.newCaptchaData(d)
		s.render(w, "pub_forgot", http.StatusOK, d)
		return
	}
	// 防枚举: 无论账号是否存在都显示同一提示
	generic := "如果该账号存在且已绑定邮箱,重置链接已发送,请在 15 分钟内完成重置。"
	var userID int64
	var email string
	err := s.DB.QueryRow("SELECT id,COALESCE(NULLIF(email,''),'') FROM users WHERE username=? OR email=?",
		account, account).Scan(&userID, &email)
	if err != nil || email == "" || s.smtpConf().Host == "" {
		d["Msg"] = generic
		d["Done"] = true
		s.newCaptchaData(d)
		s.render(w, "pub_forgot", http.StatusOK, d)
		return
	}
	token := db.RandKey(32)
	_, _ = s.DB.Exec("INSERT INTO pwd_resets(token,user_id,expires_at,created_at) VALUES(?,?,?,?)",
		token, userID, model.Now()+900, model.Now())
	// 重置链接使用配置的站点地址(防 Host 头投毒窃取 Token),未配置时才回落请求推断
	resetURL := s.baseURL(r) + "/reset?token=" + token
	subject := "【" + s.DB.MustSetting("site_name") + "】密码重置"
	body := mailHTML("重置登录密码", []string{
		"您(或他人)正在重置账号 <b>" + htmlEscape(account) + "</b> 的登录密码。",
		"点击下方链接设置新密码(15 分钟内有效):",
		`<a href="` + resetURL + `" style="word-break:break-all;color:#2e63f6">` + resetURL + `</a>`,
		"如非本人操作,请忽略本邮件,密码不会变更。",
	})
	if err := s.SendMail(email, subject, body); err != nil {
		d["Msg"] = "邮件发送失败,请联系管理员。"
		d["Done"] = true
		s.newCaptchaData(d)
		s.render(w, "pub_forgot", http.StatusOK, d)
		return
	}
	d["Msg"] = generic
	d["Done"] = true
	s.render(w, "pub_forgot", http.StatusOK, d)
}

func (s *Server) handleResetPage(w http.ResponseWriter, r *http.Request) {
	token := r.FormValue("token")
	var userID int64
	err := s.DB.QueryRow("SELECT user_id FROM pwd_resets WHERE token=? AND expires_at>?", token, model.Now()).Scan(&userID)
	if err != nil {
		renderMsg(s, w, r, "链接无效", "重置链接无效或已过期,请重新发起找回密码。", http.StatusOK)
		return
	}
	d := s.pageData(r)
	d["Page"] = "reset"
	d["Token"] = token
	s.render(w, "pub_reset", http.StatusOK, d)
}

func (s *Server) handleResetPost(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	token := r.FormValue("token")
	password := r.FormValue("password")
	password2 := r.FormValue("password2")
	var userID int64
	err := s.DB.QueryRow("SELECT user_id FROM pwd_resets WHERE token=? AND expires_at>?", token, model.Now()).Scan(&userID)
	if err != nil {
		renderMsg(s, w, r, "链接无效", "重置链接无效或已过期,请重新发起找回密码。", http.StatusOK)
		return
	}
	if len(password) < 6 || password != password2 {
		d := s.pageData(r)
		d["Page"] = "reset"
		d["Token"] = token
		d["Error"] = "密码长度至少 6 位且两次输入一致"
		s.render(w, "pub_reset", http.StatusOK, d)
		return
	}
	_, _ = s.DB.Exec("UPDATE users SET password_hash=? WHERE id=?", hashPassword(password), userID)
	_, _ = s.DB.Exec("DELETE FROM pwd_resets WHERE token=?", token)
	// 安全: 重置密码后踢掉该用户所有会话
	_, _ = s.DB.Exec("DELETE FROM sessions WHERE user_id=?", userID)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// newCaptchaData 生成验证码并注入页面数据(未启用时跳过)
func (s *Server) newCaptchaData(d map[string]any) {
	if !s.CaptchaEnabled {
		return
	}
	id, _ := newCaptcha()
	d["CaptchaID"] = id
}

// failCaptcha 渲染验证码错误页(login/register/forgot 共用)
func failCaptcha(s *Server, w http.ResponseWriter, r *http.Request) {
	page := "/register"
	if strings.Contains(r.URL.Path, "forgot") {
		page = "/forgot"
	} else if strings.Contains(r.URL.Path, "login") {
		page = "/login"
	}
	d := s.pageData(r)
	d["Page"] = strings.TrimPrefix(page, "/")
	d["Error"] = "验证码错误或已过期"
	d["Form"] = map[string]string{"username": r.FormValue("username"), "email": r.FormValue("email")}
	s.newCaptchaData(d)
	s.render(w, "pub_"+page[strings.LastIndex(page, "/")+1:], http.StatusOK, d)
}

func htmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;").Replace(s)
}

// isHTTPS 判断当前请求是否走 HTTPS(直连TLS或反代X-Forwarded-Proto)
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}
