package service

import (
	crand "crypto/rand"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// signNotifyFor 按商户配置选择通知签名方式: 配置了 RSA 公钥的商户用平台私钥 RSA-SHA256,否则 MD5
func (s *Store) signNotifyFor(merchant *model.User, params map[string]string) (string, string) {
	if merchant.RsaPublic != "" {
		if priv := s.DB.MustSetting("platform_rsa_priv"); priv != "" {
			if sig, err := SignRSA(priv, SortedRaw(params)); err == nil {
				return sig, "RSA"
			}
		}
	}
	return Sign(params, merchant.Skey), "MD5"
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
	sign, signType := s.signNotifyFor(merchant, params)
	params["sign"] = sign
	params["sign_type"] = signType

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
	sign, signType := s.signNotifyFor(merchant, params)
	params["sign"] = sign
	params["sign_type"] = signType
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
			s.checkLowBalance()
			s.checkChannelHealth()
			s.autoSettle()
			if time.Since(lastMaint) >= 6*time.Hour {
				lastMaint = time.Now()
				s.cleanOldLogs(90)
			}
			s.autoBackup()
			s.autoStatement()
		}
	}()
}

// AlertFn 由上层注入(发送告警邮件),为空时仅记录日志
var AlertFn func(msg string)

// MailHook 通用邮件发送注入(收件人/主题/HTML 正文),为空时不可发信
var MailHook func(to, subject, htmlBody string) error

// checkLowBalance 商户余额低于阈值时邮件提醒(每商户每次离线周期只发一次)
var lowBalanceAlerted sync.Map

func (s *Store) checkLowBalance() {
	type lowUser struct {
		uid      int64
		username string
	}
	var lows []lowUser
	rows, err := s.DB.Query(`SELECT id,username FROM users
		WHERE role='user' AND status=1 AND balance < 1000 AND balance > 0 AND email != ''`)
	if err != nil {
		return
	}
	for rows.Next() {
		var u lowUser
		if rows.Scan(&u.uid, &u.username) == nil {
			lows = append(lows, u)
		}
	}
	rows.Close() // 先关闭结果集再发告警(连接池仅 1 个连接,回调里再查库会自锁)
	for _, u := range lows {
		key := fmt.Sprintf("lowbal_%d", u.uid)
		if _, ok := lowBalanceAlerted.Load(key); !ok {
			lowBalanceAlerted.Store(key, time.Now())
			if AlertFn != nil {
				AlertFn(fmt.Sprintf("商户 %s 余额不足 ¥10.00,请及时充值。", u.username))
			}
		}
	}
}

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

// autoSettle 自动结算: 开启后,余额达到最低结算金额、已填收款方式且无待审核结算单的商户,
// 每 15 分钟自动提交一笔结算申请(原子扣款,防止并发重复)。
func (s *Store) autoSettle() {
	if s.DB.MustSetting("auto_settle") != "1" {
		return
	}
	minSettle := atofDefault(s.DB.MustSetting("min_settle"), 1000)
	if minSettle <= 0 {
		return
	}
	type cand struct {
		id      int64
		balance int64
		typ     int
		account string
		name    string
	}
	var cands []cand
	rows, err := s.DB.Query(`SELECT u.id,u.balance,u.settle_type,COALESCE(u.settle_account,''),COALESCE(u.settle_name,'')
		FROM users u WHERE u.role='user' AND u.status=1 AND u.balance>=?
		AND u.settle_account<>'' AND u.id NOT IN
		(SELECT user_id FROM settle_logs WHERE status=0) LIMIT 10`, int64(minSettle))
	if err != nil {
		return
	}
	for rows.Next() {
		var c cand
		if rows.Scan(&c.id, &c.balance, &c.typ, &c.account, &c.name) == nil {
			cands = append(cands, c)
		}
	}
	rows.Close()
	if len(cands) == 0 {
		return
	}
	now := model.Now()
	for _, c := range cands {
		amount := c.balance // 全额结算
		res, err := s.DB.Exec("UPDATE users SET balance=balance-? WHERE id=? AND balance>=?", amount, c.id, amount)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if c.typ <= 0 {
			c.typ = 1
		}
		_, _ = s.DB.Exec(`INSERT INTO settle_logs(user_id,amount,settle_type,account,name,status,note,created_at)
			VALUES(?,?,?,?,?,0,?,?)`, c.id, amount, c.typ, c.account, c.name, "自动结算", now)
		var before int64
		_ = s.DB.QueryRow("SELECT balance FROM users WHERE id=?", c.id).Scan(&before)
		_, _ = s.DB.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
			VALUES(?,?,?,?,?,?,?)`, c.id, "settle", -amount, before+amount, before,
			"自动结算申请 "+MoneyStr(amount), now)
		log.Printf("[settle] 自动结算: 用户#%d %s", c.id, MoneyStr(amount))
	}
}

// cleanOldLogs 清理 90 天前的到账流水与通知日志(数据量控制)
func (s *Store) cleanOldLogs(keepDays int) {
	cut := time.Now().AddDate(0, 0, -keepDays).Unix()
	r1, _ := s.DB.Exec("DELETE FROM pay_logs WHERE created_at<?", cut)
	r2, _ := s.DB.Exec("DELETE FROM notify_logs WHERE created_at<?", cut)
	// 已关闭超过保留期的工单及其会话一并清理
	r3, _ := s.DB.Exec("DELETE FROM tickets WHERE status=2 AND updated_at<?", cut)
	if n3, _ := r3.RowsAffected(); n3 > 0 {
		s.DB.Exec("DELETE FROM ticket_msgs WHERE ticket_id NOT IN (SELECT id FROM tickets)")
		log.Printf("[maint] 已清理 %d 个过期关闭工单", n3)
	}
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

// breakerAlerted 避免冷却恢复重复告警
var breakerAlerted sync.Map

// autoStatement 月度对账单自动邮件: 每月首次 tick,把上月汇总发给填了邮箱的商户
func (s *Store) autoStatement() {
	if s.DB.MustSetting("statement_auto") != "1" {
		return
	}
	if MailHook == nil {
		return
	}
	now := time.Unix(model.Now(), 0)
	prevMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).AddDate(0, -1, 0)
	key := prevMonth.Format("2006-01")
	if s.DB.MustSetting("statement_last") == key {
		return
	}
	start := prevMonth.Unix()
	end := prevMonth.AddDate(0, 1, 0).Unix()

	type merchant struct {
		id       int64
		email    string
		username string
	}
	var ms []merchant
	rows, err := s.DB.Query("SELECT id,COALESCE(email,''),username FROM users WHERE role='user' AND status=1 AND email<>''")
	if err != nil {
		return
	}
	for rows.Next() {
		var m merchant
		if rows.Scan(&m.id, &m.email, &m.username) == nil {
			ms = append(ms, m)
		}
	}
	rows.Close()

	sent := 0
	for _, m := range ms {
		var okN int64
		var money, fee int64
		_ = s.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(money),0),COALESCE(SUM(fee),0)
			FROM orders WHERE pid=(SELECT pid FROM users WHERE id=?) AND status IN(1,3) AND addtime>=? AND addtime<?`,
			m.id, start, end).Scan(&okN, &money, &fee)
		income := money - fee
		var b strings.Builder
		b.WriteString("<p>" + prevMonth.Format("2006年01月") + " 对账汇总:</p>")
		b.WriteString("<table style=\"border-collapse:collapse\" border=\"1\" cellpadding=\"6\"><tr><th>成功笔数</th><th>成交额</th><th>手续费</th><th>净入账</th></tr>")
		b.WriteString(fmt.Sprintf("<tr><td style=\"text-align:center\">%d</td><td>¥%s</td><td>¥%s</td><td>¥%s</td></tr></table>", okN, MoneyStr(money), MoneyStr(fee), MoneyStr(income)))
		b.WriteString("<p style=\"color:#888;font-size:12px\">详细按日数据可在商户中心 → 每日对账 导出。</p>")
		if err := MailHook(m.email, "【"+s.DB.MustSetting("site_name")+"】"+key+" 月度对账单", b.String()); err == nil {
			sent++
		}
	}
	_ = s.DB.SetSetting("statement_last", key)
	log.Printf("[statement] 月度对账单已发送 %d 位商户(%s)", sent, key)
}

var lastBackupDay int64

// autoBackup 定时自动备份: 每日第一轮 tick 执行 VACUUM INTO,按保留份数清理旧自动备份
func (s *Store) autoBackup() {
	if s.DB.MustSetting("backup_auto") != "1" {
		return
	}
	lt := time.Unix(model.Now(), 0)
	today := lt.Format("20060102")
	if lastBackupDay == today_i64(today) {
		return
	}
	lastBackupDay = today_i64(today)
	dir := filepath.Join(s.DataDir, "backups")
	_ = os.MkdirAll(dir, 0o755)
	name := fmt.Sprintf("auto-%s-%s.db", today, lt.Format("150405"))
	target := filepath.Join(dir, name)
	if _, err := s.DB.Exec("VACUUM INTO ?", target); err != nil {
		log.Printf("[backup] 自动备份失败: %v", err)
		return
	}
	keep := int(atoiDefault(s.DB.MustSetting("backup_keep"), 10))
	if keep < 1 {
		keep = 1
	}
	if ents, err := os.ReadDir(dir); err == nil {
		var autos []string
		for _, e := range ents {
			if !e.IsDir() && strings.HasPrefix(e.Name(), "auto-") && strings.HasSuffix(e.Name(), ".db") {
				autos = append(autos, e.Name())
			}
		}
		sort.Strings(autos)
		for i := 0; i < len(autos)-keep; i++ {
			_ = os.Remove(filepath.Join(dir, autos[i]))
		}
	}
	log.Printf("[backup] 自动备份完成: %s", name)
}

func today_i64(s string) int64 { n, _ := strconv.ParseInt(s, 10, 64); return n }

// checkCircuitBreaker 渠道自动熔断
func (s *Store) checkCircuitBreaker() {
	if s.DB.MustSetting("circuit_breaker") != "1" {
		return
	}
	now := model.Now()
	// 1) 冷却到期(10 分钟)自动恢复观察
	type rec struct {
		id    int64
		name  string
		typ   string
		until int64
	}
	var recs []rec
	rows, err := s.DB.Query(`SELECT id,name,type,breaker_at FROM channels WHERE enabled=0 AND breaker_at>0 AND breaker_at<=?`, now-600)
	if err != nil {
		return
	}
	for rows.Next() {
		var rc rec
		if rows.Scan(&rc.id, &rc.name, &rc.typ, &rc.until) == nil {
			recs = append(recs, rc)
		}
	}
	rows.Close()
	for _, rc := range recs {
		if _, err := s.DB.Exec("UPDATE channels SET enabled=1, breaker_at=0 WHERE id=? AND enabled=0 AND breaker_at=?", rc.id, rc.until); err == nil {
			log.Printf("[breaker] 渠道 %s(%s) 冷却结束,自动恢复观察", rc.name, rc.typ)
			key := "breaker_" + rc.typ
			if AlertFn != nil {
				if _, dup := breakerAlerted.LoadOrStore(key+"_"+fmt.Sprint(rc.until), 1); !dup {
					AlertFn(fmt.Sprintf("渠道 %s(%s) 熔断冷却结束,已自动恢复,请关注成功率。", rc.name, rc.typ))
				}
			}
		}
	}
	// 2) 近1小时 ≥10 笔且成功率 <30% → 熔断停用
	type bad struct {
		typ, name string
		total     int64
		paid      int64
	}
	var bads []bad
	brows, err := s.DB.Query(`SELECT c.type, c.name, COUNT(o.id),
			SUM(CASE WHEN o.status IN(1,3) THEN 1 ELSE 0 END)
		FROM channels c JOIN orders o ON o.type=c.type AND o.addtime>=?
		WHERE c.enabled=1 AND COALESCE(c.breaker_at,0)=0
		GROUP BY c.type, c.name HAVING COUNT(o.id)>=10`, now-3600)
	if err != nil {
		return
	}
	for brows.Next() {
		var cb bad
		if brows.Scan(&cb.typ, &cb.name, &cb.total, &cb.paid) == nil {
			bads = append(bads, cb)
		}
	}
	brows.Close()
	for _, cb := range bads {
		if cb.total < 10 || cb.paid*100 >= cb.total*30 {
			continue
		}
		res, err := s.DB.Exec("UPDATE channels SET enabled=0, breaker_at=? WHERE type=? AND enabled=1 AND COALESCE(breaker_at,0)=0", now, cb.typ)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			rate := float64(cb.paid) / float64(cb.total) * 100
			msg := fmt.Sprintf("渠道 %s(%s) 近1小时成功率仅 %.1f%%(%d/%d),已自动熔断停用,10 分钟后自动恢复。",
				cb.name, cb.typ, rate, cb.paid, cb.total)
			log.Println("[breaker] " + msg)
			s.auditBySystem("channel.breaker", msg)
			if AlertFn != nil {
				AlertFn(msg)
			}
		}
	}
}

// auditBySystem 后台任务产生的审计记录(操作人记为 system)
func (s *Store) auditBySystem(action, detail string) {
	_, _ = s.DB.Exec(`INSERT INTO audit_logs(admin_id,admin,action,detail,ip,created_at) VALUES(0,'system',?,?, '', ?)`,
		action, detail, model.Now())
}

// checkChannelHealth 渠道成功率自动监控: 近1小时≥10笔且成功率<50%时告警;
// 开启熔断开关后,成功率<30% 自动停用渠道,10 分钟冷却后自动恢复观察。
func (s *Store) checkChannelHealth() {
	s.checkCircuitBreaker()
	rows, err := s.DB.Query(`SELECT c.type, c.name,
		COUNT(o.id) as total,
		SUM(CASE WHEN o.status IN(1,3) THEN 1 ELSE 0 END) as paid
		FROM channels c
		JOIN orders o ON o.type=c.type AND o.addtime>strftime('%s','now')-3600
		WHERE c.enabled=1
		GROUP BY c.type, c.name HAVING COUNT(o.id) >= 10`)
	if err != nil {
		return
	}
	type chStat struct {
		typ, name string
		total     int64
		paid      int64
	}
	var stats []chStat
	for rows.Next() {
		var c chStat
		if rows.Scan(&c.typ, &c.name, &c.total, &c.paid) == nil {
			stats = append(stats, c)
		}
	}
	rows.Close() // 先关闭结果集再发告警(同上,防连接自锁)
	for _, c := range stats {
		if c.total < 10 {
			continue
		}
		rate := float64(c.paid) / float64(c.total) * 100
		key := "chan_" + c.typ
		if _, ok := lowBalanceAlerted.Load(key); !ok && rate < 50 {
			lowBalanceAlerted.Store(key, time.Now())
			msg := fmt.Sprintf("渠道 %s(%s) 近1小时成功率仅 %.1f%%(%d/%d),请检查收款码和监控设备。", c.name, c.typ, rate, c.paid, c.total)
			log.Println("[alert] " + msg)
			if AlertFn != nil {
				AlertFn(msg)
			}
		}
	}
}
