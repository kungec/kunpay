package web

import (
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunzhifu/internal/model"
	"kunzhifu/internal/service"
)

func monitorJSON(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"code": code, "msg": msg})
}

// deviceByToken 校验设备 token
func (s *Server) deviceByToken(token string) (int64, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return 0, false
	}
	var id int64
	var status int
	err := s.DB.QueryRow("SELECT id,status FROM devices WHERE token=?", token).Scan(&id, &status)
	if err != nil || status != 1 {
		return 0, false
	}
	return id, true
}

// monitorParams 汇总 GET query 与 POST 表单(新版监控APP用 POST,body 中携带 token,避免进访问日志)
func monitorParams(r *http.Request) url.Values {
	_ = r.ParseForm()
	return r.Form
}

// handleMonitorHeart /monitor/heart?token=xxx 设备心跳(GET/POST 均可)
func (s *Server) handleMonitorHeart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.deviceByToken(monitorParams(r).Get("token"))
	if !ok {
		monitorJSON(w, -1, "设备token无效")
		return
	}
	_, _ = s.DB.Exec("UPDATE devices SET last_heartbeat=?, last_ip=? WHERE id=?", model.Now(), clientIP(r), id)
	monitorJSON(w, 1, "心跳成功")
}

// handleMonitorPair GET /monitor/pair?t={token} — APP 扫码配对
func (s *Server) handleMonitorPair(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("t"))
	if token == "" {
		monitorJSON(w, -1, "缺少 token 参数")
		return
	}
	id, ok := s.deviceByToken(token)
	if !ok {
		monitorJSON(w, -1, "设备token无效")
		return
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	host := r.Host
	if host == "" {
		host = "localhost"
	}
	_ = id
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"code":   1,
		"msg":    "配对成功",
		"server": scheme + "://" + host,
		"token":  token,
	})
}

// handleMonitorPush /monitor/push 到账上报(GET/POST 均可)
//
// v2 签名(推荐,官方监控APP已启用): 附加 ts(秒级时间戳) 与
// sig = hex(HMAC-SHA256(token, ts+"\n"+type+"\n"+money+"\n"+content))。
// 服务端校验 ±300s 时间窗 + nonce 缓存 —— Token 即使被截获,截获的签名也无法
// 复用(_nonce 一次性)或改金额重放(签名覆盖金额)。
// 未携带签名的旧版客户端回落 token-only 模式(兼容,安全强度依赖传输层)。
func (s *Server) handleMonitorPush(w http.ResponseWriter, r *http.Request) {
	q := monitorParams(r)
	token := q.Get("token")
	id, ok := s.deviceByToken(token)
	if !ok {
		monitorJSON(w, -1, "设备token无效")
		return
	}
	typ := q.Get("type")
	if !allowTypes[typ] {
		monitorJSON(w, -1, "type不合法")
		return
	}
	money, err := service.ParseMoney(q.Get("money"))
	if err != nil || money <= 0 {
		monitorJSON(w, -1, "金额格式错误")
		return
	}
	content := q.Get("content")
	now := model.Now()
	// v2 签名校验: HMAC-SHA256 覆盖全部业务字段 + ±300s 时间窗 + nonce 一次性
	if sig := q.Get("sig"); sig != "" {
		tsStr := q.Get("ts")
		tsN, err := strconv.ParseInt(tsStr, 10, 64)
		if err != nil || tsN > now+30 || tsN < now-300 {
			monitorJSON(w, -1, "签名时间戳无效")
			return
		}
		mac := hmac.New(sha256.New, []byte(token))
		// 签名材料使用客户端原始 money 字符串(与安卓端一致),避免格式化差异
		mac.Write([]byte(tsStr + "\n" + typ + "\n" + q.Get("money") + "\n" + content))
		if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(strings.ToLower(sig))) != 1 {
			monitorJSON(w, -1, "签名校验失败")
			return
		}
		nonceKey := "push:" + strconv.FormatInt(id, 10) + ":" + sig[:16]
		pushNonceMu.Lock()
		if pushNonces == nil {
			pushNonces = map[string]bool{}
		}
		if pushNonces[nonceKey] {
			pushNonceMu.Unlock()
			monitorJSON(w, -1, "重复推送")
			return
		}
		pushNonces[nonceKey] = true
		if len(pushNonces) > 8192 {
			for k := range pushNonces {
				delete(pushNonces, k)
			}
		}
		pushNonceMu.Unlock()
	}
	ts := atoint64Or(q.Get("time"), now)
	// 时间戳钳制: 客户端时间不可信,拒绝未来时间戳与超龄时间戳(防预注入未来流水回溯匹配)
	if ts > now {
		ts = now
	}
	if ts < now-86400 {
		ts = now
	}
	o := s.Store.ReportPayment(id, typ, money, content, ts)
	_, _ = s.DB.Exec("UPDATE devices SET last_heartbeat=?, last_ip=? WHERE id=?", model.Now(), clientIP(r), id)
	if o != nil {
		monitorJSON(w, 1, "已匹配订单 "+o.TradeNo)
		return
	}
	monitorJSON(w, 2, "已记录,未匹配到订单")
}

// vmqSign V免签兼容: sign = md5(t + token)
func vmqSign(t, token string) string {
	sum := md5.Sum([]byte(t + token))
	return hex.EncodeToString(sum[:])
}

// vmqFresh 校验 V免签推送时间戳新鲜度(±120s,拒绝未来时间)并缓存防重放。
// 重放键使用服务端解析出的设备ID+时间戳,不依赖客户端可控的 id 参数,
// 截获的签名在窗口内无法通过更换金额/内容/id 参数重放。
func (s *Server) vmqFresh(q url.Values, deviceID int64) bool {
	t := q.Get("t")
	ts, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return false
	}
	now := model.Now()
	if ts > now+30 || ts < now-120 {
		return false
	}
	key := fmt.Sprintf("%d|%d", deviceID, ts)
	vmqReplayMu.Lock()
	defer vmqReplayMu.Unlock()
	if vmqSeen == nil {
		vmqSeen = map[string]bool{}
	}
	if vmqSeen[key] {
		return false
	}
	vmqSeen[key] = true
	if len(vmqSeen) > 8192 {
		for k := range vmqSeen {
			delete(vmqSeen, k)
		}
	}
	return true
}

var (
	pushNonceMu sync.Mutex
	pushNonces  = map[string]bool{} // v2 签名 nonce: 设备+签名前缀 → 已用
)

var vmqReplayMu sync.Mutex
var vmqSeen = map[string]bool{}

func (s *Server) vmqAuth(q url.Values) (int64, bool) {
	t := q.Get("t")
	sign := q.Get("sign")
	if t == "" || sign == "" {
		return 0, false
	}
	// 遍历设备校验(设备量小,可接受);vmq_enabled=0 的设备已禁用 V免签兼容协议
	rows, err := s.DB.Query("SELECT id,token,status,COALESCE(vmq_enabled,1) FROM devices")
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var token string
		var status, vmqOn int
		if rows.Scan(&id, &token, &status, &vmqOn) == nil && status == 1 && vmqOn == 1 &&
			subtle.ConstantTimeCompare([]byte(vmqSign(t, token)), []byte(strings.ToLower(sign))) == 1 {
			return id, true
		}
	}
	return 0, false
}

// handleVmqHeart GET /appHeart —— V免签监控APP兼容心跳
func (s *Server) handleVmqHeart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.vmqAuth(r.URL.Query())
	if !ok {
		monitorJSON(w, -1, "验签失败")
		return
	}
	_, _ = s.DB.Exec("UPDATE devices SET last_heartbeat=?, last_ip=? WHERE id=?", model.Now(), clientIP(r), id)
	monitorJSON(w, 1, "心跳成功")
}

// handleVmqPush GET /appPushV —— V免签监控APP兼容到账推送
func (s *Server) handleVmqPush(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, ok := s.vmqAuth(q)
	if !ok {
		monitorJSON(w, -1, "验签失败")
		return
	}
	// 时间戳新鲜度 + 重放缓存(设备维度): 截获的旧签名无法改金额/内容重放
	if !s.vmqFresh(q, id) {
		monitorJSON(w, -1, "签名已过期或重复推送,请校准设备时间")
		return
	}
	typ := q.Get("type")
	if typ == "wxpay" || typ == "alipay" || typ == "qqpay" {
		// ok
	} else {
		monitorJSON(w, -1, "type不合法")
		return
	}
	money, err := service.ParseMoney(q.Get("money"))
	if err != nil || money <= 0 {
		monitorJSON(w, -1, "金额格式错误")
		return
	}
	content := q.Get("content")
	o := s.Store.ReportPayment(id, typ, money, content, model.Now())
	_, _ = s.DB.Exec("UPDATE devices SET last_heartbeat=?, last_ip=? WHERE id=?", model.Now(), clientIP(r), id)
	if o != nil {
		monitorJSON(w, 1, "已匹配订单 "+o.TradeNo)
		return
	}
	monitorJSON(w, 2, "已记录,未匹配到订单")
}

// ---------- 配对码: 用 6 位短码代替手动复制 Token,5 分钟有效 ----------

var (
	pairCodeMu   sync.Mutex
	pairCodeData = map[string]pairEntry{} // 6位码 → 设备信息
)

type pairEntry struct {
	token string
	exp   time.Time
}

// GeneratePairCode 为指定设备生成 6 位配对码(5 分钟有效)
func GeneratePairCode(token string) string {
	code := fmt.Sprintf("%06d", time.Now().UnixNano()%1000000)
	pairCodeMu.Lock()
	pairCodeData[code] = pairEntry{token: token, exp: time.Now().Add(5 * time.Minute)}
	pairCodeMu.Unlock()
	return code
}

// ExchangePairCode 用 6 位配对码换取设备 Token(一次性)
func ExchangePairCode(code string) (string, bool) {
	pairCodeMu.Lock()
	e, ok := pairCodeData[code]
	if ok {
		delete(pairCodeData, code)
	}
	pairCodeMu.Unlock()
	if !ok || e.exp.Before(time.Now()) {
		return "", false
	}
	return e.token, true
}

// handleMonitorPairExchange POST /monitor/pair/exchange — APP 用 6 位配对码换取 Token
func (s *Server) handleMonitorPairExchange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	code := strings.TrimSpace(in.Code)
	if len(code) != 6 {
		monitorJSON(w, -1, "配对码格式错误(6 位数字)")
		return
	}
	token, ok := ExchangePairCode(code)
	if !ok {
		monitorJSON(w, -1, "配对码无效或已过期")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"code":   1,
		"msg":    "配对成功",
		"token":  token,
		"server": fmt.Sprintf("%s://%s", schemeOf(r), r.Host),
	})
}

func schemeOf(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}
