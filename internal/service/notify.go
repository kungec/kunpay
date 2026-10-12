package service

import (
	crand "crypto/rand"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"kunzhifu/internal/model"
)

// 异步通知重试间隔(秒),共 8 次
var retrySchedule = []int{15, 30, 60, 180, 600, 1800, 3600, 7200}

func fastrandN(n int64) int64 {
	v, err := crand.Int(crand.Reader, big.NewInt(n))
	if err != nil {
		return time.Now().UnixNano() % n
	}
	return v.Int64()
}

// TriggerNotify 立即异步发送通知(不阻塞调用方)
func (s *Store) TriggerNotify(orderID int64) {
	go func() {
		s.sendNotify(orderID)
	}()
}

func (s *Store) sendNotify(orderID int64) {
	o := s.OrderByID(orderID)
	if o == nil || o.Status != 1 || o.NotifyOK == 1 || o.NotifyURL == "" {
		return
	}
	merchant, err := s.UserByPid(o.Pid)
	if err != nil || merchant == nil {
		return
	}
	params := map[string]string{
		"pid":          itoa(o.Pid),
		"trade_no":     o.TradeNo,
		"out_trade_no": o.OutTradeNo,
		"type":         o.Type,
		"name":         o.Name,
		"money":        MoneyStr(o.Money),
		"trade_status": "TRADE_SUCCESS",
	}
	if o.Param != "" {
		params["param"] = o.Param
	}
	params["sign"] = Sign(params, merchant.Skey)
	params["sign_type"] = "MD5"

	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	target := o.NotifyURL
	if strings.Contains(target, "?") {
		target += "&" + q.Encode()
	} else {
		target += "?" + q.Encode()
	}

	client := &http.Client{Timeout: 10 * time.Second}
	ok := 0
	resp := ""
	r, err := client.Get(target)
	if err != nil {
		resp = "请求失败: " + err.Error()
	} else {
		defer r.Body.Close()
		buf := make([]byte, 512)
		n, _ := r.Body.Read(buf)
		resp = string(buf[:n])
		if strings.EqualFold(strings.TrimSpace(resp), "success") {
			ok = 1
		}
	}
	now := model.Now()
	_, _ = s.DB.Exec("INSERT INTO notify_logs(order_id,url,response,ok,created_at) VALUES(?,?,?,?,?)",
		o.ID, truncate(target, 500), truncate(resp, 300), ok, now)
	if ok == 1 {
		_, _ = s.DB.Exec("UPDATE orders SET notify_ok=1 WHERE id=?", o.ID)
		log.Printf("[notify] 成功 trade_no=%s -> %s", o.TradeNo, o.NotifyURL)
		return
	}
	// 失败:安排下一次重试
	cnt := o.NotifyCount
	if cnt >= len(retrySchedule) {
		return
	}
	_, _ = s.DB.Exec("UPDATE orders SET notify_count=?, next_notify_at=? WHERE id=?",
		cnt+1, now+int64(retrySchedule[cnt]), o.ID)
	log.Printf("[notify] 失败 trade_no=%s 第%d次: %s", o.TradeNo, cnt+1, truncate(resp, 100))
}

// BuildReturnURL 构造支付完成后的跳转通知地址(带签名)
func (s *Store) BuildReturnURL(o *model.Order) string {
	if o.ReturnURL == "" {
		return ""
	}
	merchant, err := s.UserByPid(o.Pid)
	if err != nil || merchant == nil {
		return ""
	}
	params := map[string]string{
		"pid":          itoa(o.Pid),
		"trade_no":     o.TradeNo,
		"out_trade_no": o.OutTradeNo,
		"type":         o.Type,
		"name":         o.Name,
		"money":        MoneyStr(o.Money),
		"trade_status": "TRADE_SUCCESS",
	}
	if o.Param != "" {
		params["param"] = o.Param
	}
	params["sign"] = Sign(params, merchant.Skey)
	params["sign_type"] = "MD5"
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	sep := "?"
	if strings.Contains(o.ReturnURL, "?") {
		sep = "&"
	}
	return o.ReturnURL + sep + q.Encode()
}

// RunBackground 启动后台任务: 订单过期、通知重试、会话清理、旧流水维护、设备离线告警
func (s *Store) RunBackground() {
	lastMaint := time.Now()
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for range t.C {
			s.expireOrders()
			s.retryNotifies()
			s.cleanSessions()
			s.checkDeviceAlerts()
			if time.Since(lastMaint) >= 6*time.Hour {
				lastMaint = time.Now()
				s.cleanOldLogs(90)
			}
		}
	}()
}

// AlertFn 由上层注入(发送告警邮件),为空时仅记录日志
var AlertFn func(msg string)

// checkDeviceAlerts 监控设备离线超过 5 分钟时告警一次,恢复在线自动解除
func (s *Store) checkDeviceAlerts() {
	if s.deviceAlertOn == nil {
		s.deviceAlertOn = &sync.Map{}
	}
	rows, err := s.DB.Query("SELECT id,name,last_heartbeat FROM devices WHERE status=1")
	if err != nil {
		return
	}
	type dev struct {
		id     int64
		name   string
		lastHb int64
	}
	var devs []dev
	for rows.Next() {
		var d dev
		if rows.Scan(&d.id, &d.name, &d.lastHb) == nil {
			devs = append(devs, d)
		}
	}
	rows.Close()
	now := time.Now().Unix()
	for _, d := range devs {
		offline := d.lastHb > 0 && now-d.lastHb > 300
		key := d.id
		if _, ok := s.deviceAlertOn.Load(key); ok && !offline {
			s.deviceAlertOn.Delete(key) // 恢复在线,解除告警状态
			continue
		}
		if offline {
			if _, alerted := s.deviceAlertOn.Load(key); !alerted {
				s.deviceAlertOn.Store(key, now)
				msg := "监控设备离线: " + d.name + " (超过 5 分钟无心跳,时间 " +
					time.Unix(d.lastHb, 0).Format("15:04:05") + "),请检查收款手机与监控APP。"
				log.Println("[alert] " + msg)
				if AlertFn != nil {
					AlertFn(msg)
				}
			}
		}
	}
}

// cleanOldLogs 清理 90 天前的到账流水与通知日志(数据量控制)
func (s *Store) cleanOldLogs(keepDays int) {
	cut := time.Now().AddDate(0, 0, -keepDays).Unix()
	r1, _ := s.DB.Exec("DELETE FROM pay_logs WHERE created_at<?", cut)
	r2, _ := s.DB.Exec("DELETE FROM notify_logs WHERE created_at<?", cut)
	n1, _ := r1.RowsAffected()
	n2, _ := r2.RowsAffected()
	if n1+n2 > 0 {
		log.Printf("[maint] 已清理 %d 条过期流水/通知日志", n1+n2)
	}
}

func (s *Store) expireOrders() {
	now := model.Now()
	if _, err := s.DB.Exec("UPDATE orders SET status=2 WHERE status=0 AND expire_at<=?", now); err != nil {
		log.Printf("[cron] 过期订单处理失败: %v", err)
	}
}

func (s *Store) retryNotifies() {
	rows, err := s.DB.Query(`SELECT id FROM orders WHERE status=1 AND notify_ok=0 AND notify_url<>''
		AND notify_count<8 AND next_notify_at>0 AND next_notify_at<=? LIMIT 20`, model.Now())
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		s.sendNotify(id)
	}
}

func (s *Store) cleanSessions() {
	_, _ = s.DB.Exec("DELETE FROM sessions WHERE expires_at < ?", model.Now())
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
