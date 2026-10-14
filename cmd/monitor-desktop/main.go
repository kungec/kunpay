// KunPay 监控助手 —— Windows / macOS / Linux 桌面监控终端
// 功能: 设备心跳保活、手动到账上报、模拟到账测试、本地 Web 控制台
// 运行后自动打开浏览器控制台,与 KunPay 平台 /monitor/heart、/monitor/push 协议对接。
package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

//go:embed console.html
var consoleHTML string

type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	Port   int    `json:"port"`
}

type LogEntry struct {
	Time   string `json:"time"`
	Kind   string `json:"kind"` // heart / push / test
	Type   string `json:"type"`
	Detail string `json:"detail"`
	OK     bool   `json:"ok"`
}

type App struct {
	mu        sync.Mutex
	cfg       Config
	logs      []LogEntry
	hbOK      bool
	hbErr     string
	hbLast    time.Time
	pushCount int
	startAt   time.Time
}

var pageTpl = template.Must(template.New("c").Parse(consoleHTML))

func main() {
	cfgPath := flag.String("config", "kunpay-monitor.json", "配置文件路径")
	port := flag.Int("port", 8210, "控制台端口")
	server := flag.String("server", "", "平台地址(初始化配置)")
	token := flag.String("token", "", "设备Token(初始化配置)")
	noOpen := flag.Bool("no-open", false, "不自动打开浏览器")
	flag.Parse()

	app := &App{startAt: time.Now()}
	app.loadConfig(*cfgPath)
	if *server != "" {
		app.cfg.Server = strings.TrimRight(*server, "/")
	}
	if *token != "" {
		app.cfg.Token = *token
	}
	if app.cfg.Port == 0 {
		app.cfg.Port = *port
	}
	app.saveConfig(*cfgPath)

	go app.heartbeatLoop()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", app.handleConsole)
	mux.HandleFunc("GET /api/status", app.handleStatus)
	mux.HandleFunc("POST /api/config", app.handleSaveConfig)
	mux.HandleFunc("POST /api/push", app.handlePush)
	mux.HandleFunc("POST /api/simulate", app.handleSimulate)
	mux.HandleFunc("POST /api/heartbeat", app.handleHeartbeatNow)

	// 端口被占用(可能已有实例在跑)时自动顺延,保证双击启动总能打开控制台
	addr := ""
	url := ""
	for p := app.cfg.Port; p < app.cfg.Port+10; p++ {
		addr = fmt.Sprintf("127.0.0.1:%d", p)
		url = fmt.Sprintf("http://127.0.0.1:%d/", p)
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			ln.Close()
			app.cfg.Port = p
			break
		}
		log.Printf("端口 %d 被占用,尝试下一个", p)
		addr = ""
	}
	if addr == "" {
		log.Printf("端口 %d-%d 均被占用,退出", app.cfg.Port, app.cfg.Port+9)
		return
	}
	log.Printf("KunPay 监控助手已启动: %s", url)
	if !*noOpen {
		go openBrowser(url)
	}
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Printf("监听失败: %v", err)
	}
}

// ---------- 配置 ----------

func (a *App) loadConfig(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &a.cfg)
}

func (a *App) saveConfig(path string) {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	_ = os.WriteFile(path, b, 0o600)
}

// ---------- 协议 ----------

func (a *App) doHeartbeat() (bool, string) {
	cfg := a.snapshotCfg()
	if cfg.Server == "" || cfg.Token == "" {
		return false, "请先配置平台地址与设备Token"
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(cfg.Server + "/monitor/heart?token=" + cfg.Token)
	if err != nil {
		return false, "心跳失败: " + err.Error()
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	if out.Code == 1 {
		return true, "心跳成功"
	}
	return false, "平台返回: " + body
}

func (a *App) doPush(typ, money, content string) (bool, string) {
	cfg := a.snapshotCfg()
	if cfg.Server == "" || cfg.Token == "" {
		return false, "请先配置平台地址与设备Token"
	}
	url := fmt.Sprintf("%s/monitor/push?token=%s&type=%s&money=%s&content=%s",
		cfg.Server, cfg.Token, typ, money, content)
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false, "上报失败: " + err.Error()
	}
	defer resp.Body.Close()
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	body := string(buf[:n])
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	_ = json.Unmarshal([]byte(body), &out)
	return out.Code >= 1, body
}

// ---------- 后台心跳 ----------

func (a *App) heartbeatLoop() {
	for {
		ok, msg := a.doHeartbeat()
		a.mu.Lock()
		a.hbOK, a.hbErr, a.hbLast = ok, msg, time.Now()
		a.mu.Unlock()
		time.Sleep(30 * time.Second)
	}
}

// ---------- HTTP ----------

func (a *App) snapshotCfg() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *App) addLog(e LogEntry) {
	a.mu.Lock()
	e.Time = time.Now().Format("15:04:05")
	a.logs = append([]LogEntry{e}, a.logs...)
	if len(a.logs) > 100 {
		a.logs = a.logs[:100]
	}
	if e.OK {
		a.pushCount++
	}
	a.mu.Unlock()
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(v)
}

func (a *App) handleConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = pageTpl.Execute(w, nil)
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	cfg := a.cfg
	logs := a.logs
	pushCount := a.pushCount
	hbOK, hbErr, hbLast := a.hbOK, a.hbErr, a.hbLast
	startAt := a.startAt
	a.mu.Unlock()
	hostname, _ := os.Hostname()
	writeJSON(w, map[string]any{
		"server": cfg.Server, "token_set": cfg.Token != "",
		"token_mask": maskToken(cfg.Token),
		"hb_ok":      hbOK, "hb_err": hbErr,
		"hb_last": func() string {
			if hbLast.IsZero() {
				return ""
			}
			return hbLast.Format("15:04:05")
		}(),
		"logs": logs, "push_count": pushCount,
		"device": hostname, "os": runtime.GOOS + "/" + runtime.GOARCH,
		"uptime_min": int(time.Since(startAt).Minutes()),
	})
}

func (a *App) handleSaveConfig(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Server string `json:"server"`
		Token  string `json:"token"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	a.mu.Lock()
	a.cfg.Server = strings.TrimRight(strings.TrimSpace(in.Server), "/")
	a.cfg.Token = strings.TrimSpace(in.Token)
	cfgPath := flagConfigPath()
	a.mu.Unlock()
	a.saveConfig(cfgPath)
	ok, msg := a.doHeartbeat()
	a.mu.Lock()
	a.hbOK, a.hbErr, a.hbLast = ok, msg, time.Now()
	a.mu.Unlock()
	a.addLog(LogEntry{Kind: "test", Detail: msg, OK: ok})
	writeJSON(w, map[string]any{"ok": ok, "msg": msg})
}

func (a *App) handlePush(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Type    string `json:"type"`
		Money   string `json:"money"`
		Content string `json:"content"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	if in.Type == "" {
		in.Type = "alipay"
	}
	if in.Money == "" {
		writeJSON(w, map[string]any{"ok": false, "msg": "请输入金额"})
		return
	}
	if in.Content == "" {
		in.Content = "手动上报 " + in.Money + "元"
	}
	ok, msg := a.doPush(in.Type, in.Money, in.Content)
	a.addLog(LogEntry{Kind: "push", Type: in.Type, Detail: "¥" + in.Money + " " + msg, OK: ok})
	writeJSON(w, map[string]any{"ok": ok, "msg": msg})
}

func (a *App) handleSimulate(w http.ResponseWriter, r *http.Request) {
	money := fmt.Sprintf("%d.%02d", 1+rand.Intn(50), 1+rand.Intn(99))
	types := []string{"alipay", "wxpay", "qqpay"}
	typ := types[rand.Intn(len(types))]
	ok, msg := a.doPush(typ, money, "模拟到账 "+money+"元")
	a.addLog(LogEntry{Kind: "test", Type: typ, Detail: "模拟 ¥" + money + " " + msg, OK: ok})
	writeJSON(w, map[string]any{"ok": ok, "msg": "模拟到账 ¥" + money + " (" + typ + ") " + msg})
}

func (a *App) handleHeartbeatNow(w http.ResponseWriter, r *http.Request) {
	ok, msg := a.doHeartbeat()
	a.mu.Lock()
	a.hbOK, a.hbErr, a.hbLast = ok, msg, time.Now()
	a.mu.Unlock()
	a.addLog(LogEntry{Kind: "heart", Detail: msg, OK: ok})
	writeJSON(w, map[string]any{"ok": ok, "msg": msg})
}

// ---------- 工具 ----------

func flagConfigPath() string {
	return "kunpay-monitor.json"
}

func maskToken(t string) string {
	if t == "" {
		return "未配置"
	}
	if len(t) <= 8 {
		return t[:2] + "****"
	}
	return t[:6] + "****" + t[len(t)-4:]
}

func openBrowser(url string) {
	time.Sleep(400 * time.Millisecond)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("请手动打开控制台: %s", url)
	}
}
