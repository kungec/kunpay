package web

import (
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"kunzhifu/internal/db"
	"kunzhifu/internal/service"
)

// 白盒 HTTP 回路测试: 复现浏览器完整流程
// GET /login(取验证码 id)→ 白盒读取服务端真实 code → POST /login(正确账号+验证码)→ 302 登录成功
// 背景: e2e 以 -captcha=false 运行,验证码 HTTP 回路仅由此测试覆盖;
// 用户曾反馈 Windows 部署"输入正确仍提示验证码错误",此测试守卫该回路。
func TestCaptchaHTTPRoundTrip(t *testing.T) {
	s := newCaptchaTestServer(t)
	handler := s.Routes()

	rec := httptest.NewRequest("GET", "/login", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, rec)
	if rec2.Code != 200 {
		t.Fatalf("GET /login = %d, body=%s", rec2.Code, rec2.Body.String()[:minInt(200, rec2.Body.Len())])
	}
	id := regexpFindCaptchaID(rec2.Body.String())
	if id == "" {
		t.Fatal("页面未包含 captcha_id")
	}
	captchaMu.Lock()
	entry, ok := captchaStore[id]
	captchaMu.Unlock()
	if !ok {
		t.Fatal("验证码 id 未入存储")
	}

	form := url.Values{
		"username":     {"admin"},
		"password":     {"admin123"},
		"captcha_id":   {id},
		"captcha_code": {entry.code},
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req)
	if rec3.Code != 302 {
		t.Fatalf("正确验证码登录应 302,得到 %d, body=%s", rec3.Code, rec3.Body.String()[:minInt(200, rec3.Body.Len())])
	}
	if loc := rec3.Header().Get("Location"); !strings.HasPrefix(loc, "/user") {
		t.Errorf("登录成功应跳 /user,实际 %s", loc)
	}
}

// 用户场景复现: 提交不存在的验证码 id(代理缓存旧页面/过期 id)→ 应渲染错误页并提示
func TestCaptchaHTTPInvalidID(t *testing.T) {
	s := newCaptchaTestServer(t)
	handler := s.Routes()

	rec := httptest.NewRequest("GET", "/login", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, rec)

	form := url.Values{
		"username":     {"admin"},
		"password":     {"admin123"},
		"captcha_id":   {"nonexistent123"},
		"captcha_code": {"aaaa"},
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req)
	if rec3.Code != 200 {
		t.Errorf("无效 id 应渲染错误页(200),实际 %d", rec3.Code)
	}
	if !strings.Contains(rec3.Body.String(), "验证码错误或已过期") {
		t.Error("应提示 验证码错误或已过期")
	}
}

func newCaptchaTestServer(t *testing.T) *Server {
	t.Helper()
	store := service.NewStore(mustTestDB(t))
	s := NewServer(store, t.TempDir(), "8080")
	s.CaptchaEnabled = true
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return s
}

func regexpFindCaptchaID(body string) string {
	m := regexp.MustCompile(`name="captcha_id" value="([a-z0-9]+)"`).FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func mustTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir()+"/t.db", "NORMAL")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
