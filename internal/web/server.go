package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"kunzhifu/internal/db"
	"kunzhifu/internal/model"
	"kunzhifu/internal/service"
	webfs "kunzhifu/web"
)

type Server struct {
	Store          *service.Store
	DB             *db.DB
	DataDir        string
	Port           string
	StartedAt      time.Time
	CaptchaEnabled bool // 图形验证码开关(注册/登录/找回)
	Tpl            map[string]*template.Template
}

func NewServer(store *service.Store, dataDir, port string) *Server {
	return &Server{Store: store, DB: store.DB, DataDir: dataDir, Port: port, StartedAt: time.Now()}
}

// 模板传参可能为 any(来自 map),统一做类型转换
func toI64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}

func typeName(t any) string {
	switch s := fmt.Sprint(t); s {
	case "alipay":
		return "支付宝"
	case "wxpay":
		return "微信支付"
	case "qqpay":
		return "QQ钱包"
	case "bank":
		return "网银"
	default:
		return s
	}
}

func statusName(s any) string {
	switch toI64(s) {
	case 0:
		return "待支付"
	case 1:
		return "支付成功"
	case 2:
		return "已过期"
	case 3:
		return "手动补单"
	case 4:
		return "已退款"
	}
	return "未知"
}

func settleTypeName(t any) string {
	switch toI64(t) {
	case 1:
		return "支付宝"
	case 2:
		return "微信"
	case 3:
		return "QQ"
	case 4:
		return "银行卡"
	}
	return "未知"
}

func yuan(c any) string { return service.MoneyStr(toI64(c)) }

func signedYuan(c int64) string {
	if c >= 0 {
		return "+" + service.MoneyStr(c)
	}
	return service.MoneyStr(c)
}

func fdate(ts any) string {
	t := toI64(ts)
	if t <= 0 {
		return "-"
	}
	return timeFormat(t, "2006-01-02 15:04:05")
}

func shortdate(ts any) string {
	t := toI64(ts)
	if t <= 0 {
		return "-"
	}
	return timeFormat(t, "01-02 15:04")
}

func subStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func timeFormat(ts int64, layout string) string {
	return time.Unix(ts, 0).Format(layout)
}

func jsonFor(v any) template.JS {
	b, _ := json.Marshal(v)
	return template.JS(b)
}

// StatDay 每日成交统计
type StatDay struct {
	Date  string
	Count int64
	Money int64
}

func daysLabels(v any) []string {
	if ds, ok := v.([]StatDay); ok {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = d.Date
		}
		return out
	}
	return nil
}

func daysValues(v any) []int64 {
	if ds, ok := v.([]StatDay); ok {
		out := make([]int64, len(ds))
		for i, d := range ds {
			out[i] = d.Money / 100 // 图表以元为单位
		}
		return out
	}
	return nil
}

// dict 模板内构造 map
func dict(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			m[k] = kv[i+1]
		}
	}
	return m
}

func addInt(a, b any) int64 { return toI64(a) + toI64(b) }

func pagerQuery(parts ...string) string {
	q := ""
	for _, p := range parts {
		if p != "" {
			q += p + "&"
		}
	}
	return q
}

// pollSec 收银台轮询间隔(秒),读取系统设置,限制 1~30
func pollSec(settings map[string]string) int {
	var v int
	if _, err := fmt.Sscanf(strings.TrimSpace(settings["poll_interval"]), "%d", &v); err != nil || v < 1 {
		v = 3
	}
	if v > 30 {
		v = 30
	}
	return v
}

var funcMap = template.FuncMap{
	"stringsContains": strings.Contains,
	"typename":        typeName,
	"statusname":      statusName,
	"settlename":      settleTypeName,
	"yuan":            yuan,
	"signedYuan":      signedYuan,
	"fdate":           fdate,
	"shortdate":       shortdate,
	"substr":          subStr,
	"safehtml":        func(s string) template.HTML { return template.HTML(s) },
	"json":            jsonFor,
	"daysLabels":      daysLabels,
	"daysValues":      daysValues,
	"dict":            dict,
	"addInt":          addInt,
	"pagerQuery":      pagerQuery,
	"pollSec":         pollSec,
	"minus":           func(a, b int64) int64 { return a - b },
	"divSize":         func(b int64) string { return fmt.Sprintf("%.2f", float64(b)/1024/1024) },
}

func (s *Server) loadTemplates() error {
	tfs := webfs.Templates()
	s.Tpl = map[string]*template.Template{}
	pubPages, err := fs.Glob(tfs, "pages/pub/*.html")
	if err != nil {
		return err
	}
	for _, p := range pubPages {
		name := "pub_" + strings.TrimSuffix(baseName(p), ".html")
		t, err := template.New("base").Funcs(funcMap).ParseFS(tfs, "layouts/base.html", "partials/ui.html", p)
		if err != nil {
			return fmt.Errorf("解析模板 %s: %w", p, err)
		}
		s.Tpl[name] = t
	}
	dashPages, err := fs.Glob(tfs, "pages/dash/*.html")
	if err != nil {
		return err
	}
	for _, p := range dashPages {
		name := "dash_" + strings.TrimSuffix(baseName(p), ".html")
		t, err := template.New("base").Funcs(funcMap).ParseFS(tfs, "layouts/dash.html", "partials/ui.html", p)
		if err != nil {
			return fmt.Errorf("解析模板 %s: %w", p, err)
		}
		s.Tpl[name] = t
	}
	standalone, err := fs.Glob(tfs, "pages/standalone/*.html")
	if err != nil {
		return err
	}
	for _, p := range standalone {
		name := "sa_" + strings.TrimSuffix(baseName(p), ".html")
		t, err := template.New("page").Funcs(funcMap).ParseFS(tfs, p)
		if err != nil {
			return fmt.Errorf("解析模板 %s: %w", p, err)
		}
		s.Tpl[name] = t
	}
	return nil
}

func (s *Server) render(w http.ResponseWriter, name string, status int, data any) {
	t, ok := s.Tpl[name]
	if !ok {
		http.Error(w, "模板不存在: "+name, http.StatusInternalServerError)
		return
	}
	entry := "base"
	if strings.HasPrefix(name, "sa_") {
		entry = "page"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, entry, data); err != nil {
		log.Printf("[tpl] 渲染 %s 失败: %v", name, err)
	}
}

func (s *Server) pageData(r *http.Request) map[string]any {
	st, _ := s.DB.AllSettings()
	d := map[string]any{
		"Settings": st,
		"Path":     r.URL.Path,
		"Next":     "",
		"Form":     map[string]string{},
		"Error":    "",
		"Flash":    "",
		"FlashErr": "",
		"Title":    "",
	}
	if u := s.currentUser(r); u != nil {
		d["Me"] = u
		d["CSRF"] = s.csrfToken(u)
	}
	return d
}

// ---- CSRF: 基于 session token 的 HMAC ----

func (s *Server) csrfToken(u *model.User) string {
	mac := hmac.New(sha256.New, []byte("kunpay-csrf"))
	mac.Write([]byte(fmt.Sprintf("%d:%s", u.ID, s.sessionSecret())))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}

func (s *Server) checkCSRF(r *http.Request) bool {
	u := s.currentUser(r)
	if u == nil {
		return false
	}
	tok := r.FormValue("_csrf")
	return tok != "" && hmac.Equal([]byte(tok), []byte(s.csrfToken(u)))
}

func (s *Server) sessionSecret() string {
	sec := s.DB.MustSetting("_secret")
	if sec == "" {
		sec = db.RandKey(32)
		_ = s.DB.SetSetting("_secret", sec)
	}
	return sec
}

func baseName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// baseURL 站点对外地址: 优先取系统设置 site_url(防 Host 头注入),未配置回落请求推断
func (s *Server) baseURL(r *http.Request) string {
	if u := strings.TrimRight(s.DB.MustSetting("site_url"), "/"); u != "" {
		return u
	}
	return s.absoluteURL(r)
}

// InjectAlert 将告警接入邮件 + IM 机器人双通道(按设置开关)
func (s *Server) InjectAlert() {
	service.AlertFn = func(msg string) {
		if s.DB.MustSetting("device_alert") != "1" {
			return
		}
		siteName := s.DB.MustSetting("site_name")
		if to := s.DB.MustSetting("alert_email"); to != "" {
			body := mailHTML("系统告警通知", []string{msg})
			if err := s.SendMail(to, "【"+siteName+"】系统告警", body); err != nil {
				log.Printf("[alert] 告警邮件发送失败: %v", err)
			}
		}
		if hook := s.DB.MustSetting("alert_webhook"); hook != "" {
			if err := service.SendWebhookAlert(hook, siteName, "【"+siteName+"】"+msg); err != nil && err != service.ErrInvalidWebhook {
				log.Printf("[alert] 机器人告警发送失败: %v", err)
			}
		}
	}
}
