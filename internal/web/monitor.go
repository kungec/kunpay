package web

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

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

// handleMonitorHeart GET /monitor/heart?token=xxx 设备心跳
func (s *Server) handleMonitorHeart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.deviceByToken(r.URL.Query().Get("token"))
	if !ok {
		monitorJSON(w, -1, "设备token无效")
		return
	}
	_, _ = s.DB.Exec("UPDATE devices SET last_heartbeat=?, last_ip=? WHERE id=?", model.Now(), clientIP(r), id)
	monitorJSON(w, 1, "心跳成功")
}

// handleMonitorPush GET /monitor/push?token=xxx&type=alipay&money=1.23&content=xxx 到账上报
func (s *Server) handleMonitorPush(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, ok := s.deviceByToken(q.Get("token"))
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
	ts := atoint64Or(q.Get("time"), model.Now())
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

func (s *Server) vmqAuth(q url.Values) (int64, bool) {
	t := q.Get("t")
	sign := q.Get("sign")
	if t == "" || sign == "" {
		return 0, false
	}
	// 遍历设备校验(设备量小,可接受)
	rows, err := s.DB.Query("SELECT id,token,status FROM devices")
	if err != nil {
		return 0, false
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var token string
		var status int
		if rows.Scan(&id, &token, &status) == nil && status == 1 && vmqSign(t, token) == strings.ToLower(sign) {
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
