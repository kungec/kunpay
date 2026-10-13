package service

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"kunzhifu/internal/db"
	"kunzhifu/internal/model"
)

var (
	ErrMerchantNotFound = errors.New("商户ID不存在")
	ErrMerchantBanned   = errors.New("商户已被封禁,无法发起支付")
	ErrChannelDisabled  = errors.New("支付方式未开通或已关闭")
	ErrMoneyRange       = errors.New("支付金额超出渠道限额")
	ErrLimitSingle      = errors.New("支付金额超出商户单笔限额")
	ErrLimitDaily       = errors.New("超出商户当日收款限额")
	ErrOrderPaid        = errors.New("订单号已存在且已支付,请勿重复提交")
	ErrBadMoney         = errors.New("金额格式错误,最多两位小数")
)

type Store struct {
	DB            *db.DB
	deviceAlertOn *sync.Map // 已告警的离线设备 id -> 告警时间
}

func NewStore(d *db.DB) *Store { return &Store{DB: d} }

func scanUser(row interface{ Scan(...any) error }) (*model.User, error) {
	u := &model.User{}
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.QQ, &u.Role, &u.Status,
		&u.Rate, &u.Balance, &u.Pid, &u.Skey, &u.SettleType, &u.SettleAccount, &u.SettleName,
		&u.InviteCode, &u.InvitedBy, &u.CreatedAt, &u.LastLoginAt, &u.LastLoginIP, &u.Avatar, &u.ApiIPs,
		&u.TotpSecret, &u.NotifyWebhook, &u.LimitSingle, &u.LimitDaily)
	if err != nil {
		return nil, err
	}
	return u, nil
}

const userCols = `id,username,email,password_hash,qq,role,status,rate,balance,pid,skey,
	settle_type,settle_account,settle_name,invite_code,invited_by,created_at,last_login_at,last_login_ip,avatar,COALESCE(api_ips,''),COALESCE(totp_secret,''),COALESCE(notify_webhook,''),COALESCE(limit_single,0),COALESCE(limit_daily,0)`

func (s *Store) UserByID(id int64) (*model.User, error) {
	u, err := scanUser(s.DB.QueryRow("SELECT "+userCols+" FROM users WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func (s *Store) UserByPid(pid int64) (*model.User, error) {
	u, err := scanUser(s.DB.QueryRow("SELECT "+userCols+" FROM users WHERE pid=?", pid))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func (s *Store) UserByUsername(name string) (*model.User, error) {
	u, err := scanUser(s.DB.QueryRow("SELECT "+userCols+" FROM users WHERE username=?", name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func (s *Store) ChannelByType(t string) (*model.Channel, error) {
	c := &model.Channel{}
	err := s.DB.QueryRow("SELECT id,type,name,enabled,min_money,max_money,qrcode,account,device_id,alipay_uid,COALESCE(breaker_at,0) FROM channels WHERE type=?", t).
		Scan(&c.ID, &c.Type, &c.Name, &c.Enabled, &c.MinMoney, &c.MaxMoney, &c.Qrcode, &c.Account, &c.DeviceID, &c.AlipayUID, &c.BreakerAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return c, err
}

func (s *Store) AllChannels() ([]model.Channel, error) {
	rows, err := s.DB.Query("SELECT id,type,name,enabled,min_money,max_money,qrcode,account,device_id,alipay_uid,COALESCE(breaker_at,0) FROM channels ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Channel
	for rows.Next() {
		c := model.Channel{}
		if err := rows.Scan(&c.ID, &c.Type, &c.Name, &c.Enabled, &c.MinMoney, &c.MaxMoney, &c.Qrcode, &c.Account, &c.DeviceID, &c.AlipayUID, &c.BreakerAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// OrderReq 汇总外部下单请求参数
type OrderReq struct {
	Type       string
	OutTradeNo string
	NotifyURL  string
	ReturnURL  string
	Name       string
	Money      string
	Sitename   string
	Param      string
	Device     string
}

// CreateOrder 创建或复用支付订单,返回订单。
func (s *Store) CreateOrder(u *model.User, req *OrderReq, clientIP string) (*model.Order, error) {
	money, err := ParseMoney(req.Money)
	if err != nil || money <= 0 {
		return nil, ErrBadMoney
	}
	if u.Status != 1 {
		return nil, ErrMerchantBanned
	}
	ch, err := s.ChannelByType(req.Type)
	if err != nil {
		return nil, err
	}
	if ch == nil || ch.Enabled != 1 {
		return nil, ErrChannelDisabled
	}
	if money < ch.MinMoney || money > ch.MaxMoney {
		return nil, ErrMoneyRange
	}
	// 商户风控限额(0=不限)
	if u.LimitSingle > 0 && money > u.LimitSingle {
		return nil, ErrLimitSingle
	}
	if u.LimitDaily > 0 {
		lt := time.Unix(model.Now(), 0)
		dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location()).Unix()
		var todayPaid int64
		_ = s.DB.QueryRow("SELECT COALESCE(SUM(money),0) FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=?",
			u.Pid, dayStart).Scan(&todayPaid)
		if todayPaid+money > u.LimitDaily {
			return nil, ErrLimitDaily
		}
	}
	name := truncate(req.Name, 127)
	outTradeNo := truncate(req.OutTradeNo, 64)

	// 同一商户同一订单号:待支付未过期则复用;已支付则报错
	var existID int64
	var existStatus int
	var existExpire int64
	err = s.DB.QueryRow("SELECT id,status,expire_at FROM orders WHERE pid=? AND out_trade_no=?", u.Pid, outTradeNo).
		Scan(&existID, &existStatus, &existExpire)
	if err == nil {
		if existStatus == 0 && existExpire > model.Now() {
			if o := s.OrderByID(existID); o != nil {
				return o, nil
			}
		}
		if existStatus == 1 || existStatus == 3 {
			return nil, ErrOrderPaid
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	timeoutMin := atoiDefault(s.DB.MustSetting("order_timeout"), 5)
	offsetOn := s.DB.MustSetting("money_offset") == "1"

	// 回调地址仅允许 http/https(防非HTTP协议SSRF),并阻断内网/回环地址
	for _, cbu := range []string{req.NotifyURL, req.ReturnURL} {
		lu := strings.ToLower(cbu)
		if cbu != "" && !strings.HasPrefix(lu, "http://") && !strings.HasPrefix(lu, "https://") {
			return nil, errors.New("通知地址仅支持 http/https 协议")
		}
		if cbu != "" && isInternalURL(cbu) {
			return nil, errors.New("通知地址不允许指向内网地址")
		}
	}

	now := model.Now()
	o := &model.Order{
		TradeNo:    genTradeNo(),
		OutTradeNo: outTradeNo,
		Pid:        u.Pid,
		Type:       req.Type,
		Name:       name,
		Money:      money,
		Param:      req.Param,
		NotifyURL:  req.NotifyURL,
		ReturnURL:  req.ReturnURL,
		Sitename:   truncate(req.Sitename, 60),
		Device:     req.Device,
		ChannelID:  ch.ID,
		Addtime:    now,
		ExpireAt:   now + int64(timeoutMin)*60,
		ClientIP:   clientIP,
	}
	o.PayMoney = money
	if offsetOn {
		o.PayMoney = s.uniquePayMoney(req.Type, money, now, int64(timeoutMin)*60)
	}
	res, err := s.DB.Exec(`INSERT INTO orders(trade_no,out_trade_no,pid,type,name,money,pay_money,param,
		notify_url,return_url,sitename,device,channel_id,status,addtime,expire_at,client_ip)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?)`,
		o.TradeNo, o.OutTradeNo, o.Pid, o.Type, o.Name, o.Money, o.PayMoney, o.Param,
		o.NotifyURL, o.ReturnURL, o.Sitename, o.Device, o.ChannelID, o.Addtime, o.ExpireAt, o.ClientIP)
	if err != nil {
		// 并发竞态: UNIQUE(pid,out_trade_no) 冲突时回落到复用已有订单
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			if existing := s.OrderByOutTradeNo(u.Pid, outTradeNo); existing != nil {
				if existing.Status == 0 && existing.ExpireAt > model.Now() {
					return existing, nil
				}
				return nil, ErrOrderPaid
			}
		}
		return nil, err
	}
	o.ID, _ = res.LastInsertId()
	s.backfillUnmatched(o)
	return o, nil
}

// backfillUnmatched 回溯匹配: 新订单创建后,若近期(订单超时窗口内)存在同类型同金额的
// 未匹配到账流水(付款发生在下单之前),自动完成该订单并入账通知。
func (s *Store) backfillUnmatched(o *model.Order) {
	timeoutMin := atoiDefault(s.DB.MustSetting("order_timeout"), 5)
	windowStart := model.Now() - int64(timeoutMin)*60
	var logID int64
	err := s.DB.QueryRow(`SELECT id FROM pay_logs
		WHERE order_id=0 AND type=? AND money=? AND created_at>=? ORDER BY id ASC LIMIT 1`,
		o.Type, o.PayMoney, windowStart).Scan(&logID)
	if err != nil {
		return // 没有可回溯的流水
	}
	if err := s.CompleteOrder(o, "回溯匹配"); err != nil {
		return
	}
	_, _ = s.DB.Exec("UPDATE pay_logs SET order_id=? WHERE id=?", o.ID, logID)
}

// uniquePayMoney 生成带随机尾数的应付金额,尽量避免与在途订单撞额。
func (s *Store) uniquePayMoney(typ string, money, now, ttl int64) int64 {
	if money%100 != 0 {
		// 金额本身已带小数,保持原值
		return money
	}
	for i := 0; i < 60; i++ {
		c := int64(1 + fastrandN(99)) // 1~99 分
		cand := money + c
		var n int
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE status=0 AND type=? AND pay_money=? AND expire_at>?",
			typ, cand, now+ttl-3600).Scan(&n)
		if n == 0 {
			return cand
		}
	}
	return money + 1 + fastrandN(99)
}

func genTradeNo() string {
	return time.Now().Format("20060102150405") + db.RandDigits(5)
}

func (s *Store) OrderByID(id int64) *model.Order {
	o := &model.Order{}
	err := s.DB.QueryRow(`SELECT id,trade_no,out_trade_no,pid,type,name,money,pay_money,param,notify_url,
		return_url,sitename,device,channel_id,status,fee,buyer,addtime,endtime,expire_at,notify_count,notify_ok,next_notify_at,client_ip,m_item_id,p_item_id,COALESCE(remark,'')
		FROM orders WHERE id=?`, id).
		Scan(&o.ID, &o.TradeNo, &o.OutTradeNo, &o.Pid, &o.Type, &o.Name, &o.Money, &o.PayMoney, &o.Param,
			&o.NotifyURL, &o.ReturnURL, &o.Sitename, &o.Device, &o.ChannelID, &o.Status, &o.Fee, &o.Buyer,
			&o.Addtime, &o.Endtime, &o.ExpireAt, &o.NotifyCount, &o.NotifyOK, &o.NextNotifyAt, &o.ClientIP,
			&o.MerchantItemID, &o.PlatformItemID, &o.Remark)
	if err != nil {
		return nil
	}
	return o
}

func (s *Store) OrderByTradeNo(tradeNo string) *model.Order {
	o := &model.Order{}
	err := s.DB.QueryRow(`SELECT id,trade_no,out_trade_no,pid,type,name,money,pay_money,param,notify_url,
		return_url,sitename,device,channel_id,status,fee,buyer,addtime,endtime,expire_at,notify_count,notify_ok,next_notify_at,client_ip,m_item_id,p_item_id,COALESCE(remark,'')
		FROM orders WHERE trade_no=?`, tradeNo).
		Scan(&o.ID, &o.TradeNo, &o.OutTradeNo, &o.Pid, &o.Type, &o.Name, &o.Money, &o.PayMoney, &o.Param,
			&o.NotifyURL, &o.ReturnURL, &o.Sitename, &o.Device, &o.ChannelID, &o.Status, &o.Fee, &o.Buyer,
			&o.Addtime, &o.Endtime, &o.ExpireAt, &o.NotifyCount, &o.NotifyOK, &o.NextNotifyAt, &o.ClientIP,
			&o.MerchantItemID, &o.PlatformItemID, &o.Remark)
	if err != nil {
		return nil
	}
	return o
}

func (s *Store) OrderByOutTradeNo(pid int64, outTradeNo string) *model.Order {
	o := &model.Order{}
	err := s.DB.QueryRow(`SELECT id,trade_no,out_trade_no,pid,type,name,money,pay_money,param,notify_url,
		return_url,sitename,device,channel_id,status,fee,buyer,addtime,endtime,expire_at,notify_count,notify_ok,next_notify_at,client_ip,m_item_id,p_item_id,COALESCE(remark,'')
		FROM orders WHERE pid=? AND out_trade_no=?`, pid, outTradeNo).
		Scan(&o.ID, &o.TradeNo, &o.OutTradeNo, &o.Pid, &o.Type, &o.Name, &o.Money, &o.PayMoney, &o.Param,
			&o.NotifyURL, &o.ReturnURL, &o.Sitename, &o.Device, &o.ChannelID, &o.Status, &o.Fee, &o.Buyer,
			&o.Addtime, &o.Endtime, &o.ExpireAt, &o.NotifyCount, &o.NotifyOK, &o.NextNotifyAt, &o.ClientIP,
			&o.MerchantItemID, &o.PlatformItemID, &o.Remark)
	if err != nil {
		return nil
	}
	return o
}

// SetOrderRemark 设置订单备注(管理后台用)
func (s *Store) SetOrderRemark(id int64, remark string) error {
	_, err := s.DB.Exec("UPDATE orders SET remark=? WHERE id=?", remark, id)
	return err
}

// ReportPayment 监控设备上报一笔到账,尝试匹配待支付订单。
// 返回匹配到的订单(可为 nil 表示未匹配)。
func (s *Store) ReportPayment(deviceID int64, typ string, money int64, content string, ts int64) *model.Order {
	if money <= 0 {
		return nil
	}
	now := model.Now()
	// 推送去重: 监控APP超时重试会重复推送同一笔到账;30 秒内同设备/同方式/同金额/同内容
	// 且已成功匹配过的推送直接忽略,防止误匹配到第二笔同额订单。
	var dup int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM pay_logs
		WHERE device_id=? AND type=? AND money=? AND content=? AND order_id>0 AND created_at>?`,
		deviceID, typ, money, truncate(content, 500), ts-30).Scan(&dup)
	if dup > 0 {
		return nil
	}
	// 1) 内容里若带有本平台订单号,优先精确匹配
	var matched *model.Order
	for _, t := range extractOrderNos(content) {
		if o := s.OrderByTradeNo(t); o != nil && o.Status == 0 && o.PayMoney == money {
			matched = o
			break
		}
	}
	// 2) 金额精确匹配最早的待支付订单;渠道绑定了监控设备时,仅接受该设备的上报(防串单)
	if matched == nil {
		var candidates []*model.Order
		rows, err := s.DB.Query(`SELECT id,trade_no,out_trade_no,pid,type,name,money,pay_money,param,notify_url,
			return_url,sitename,device,channel_id,status,fee,buyer,addtime,endtime,expire_at,notify_count,notify_ok,next_notify_at,client_ip,m_item_id,p_item_id,COALESCE(remark,'')
			FROM orders WHERE status=0 AND type=? AND pay_money=? AND expire_at>? ORDER BY id ASC LIMIT 5`,
			typ, money, now-300)
		if err == nil {
			for rows.Next() {
				o := &model.Order{}
				if err := rows.Scan(&o.ID, &o.TradeNo, &o.OutTradeNo, &o.Pid, &o.Type, &o.Name, &o.Money, &o.PayMoney,
					&o.Param, &o.NotifyURL, &o.ReturnURL, &o.Sitename, &o.Device, &o.ChannelID, &o.Status, &o.Fee,
					&o.Buyer, &o.Addtime, &o.Endtime, &o.ExpireAt, &o.NotifyCount, &o.NotifyOK, &o.NextNotifyAt, &o.ClientIP,
					&o.MerchantItemID, &o.PlatformItemID, &o.Remark); err == nil {
					candidates = append(candidates, o)
				}
			}
			rows.Close() // 先关闭结果集再做内嵌查询(单连接池防死锁)
			for _, o := range candidates {
				if s.orderAllowsDevice(o, deviceID) {
					matched = o
					break
				}
			}
		}
	}
	orderID := int64(0)
	if matched != nil {
		orderID = matched.ID
	}
	_, _ = s.DB.Exec("INSERT INTO pay_logs(device_id,type,money,content,order_id,created_at) VALUES(?,?,?,?,?,?)",
		deviceID, typ, money, truncate(content, 500), orderID, ts)
	if matched != nil {
		if err := s.CompleteOrder(matched, "监控到账"); err != nil {
			log.Printf("[match] 完成订单失败 trade_no=%s: %v", matched.TradeNo, err)
			return nil
		}
	}
	return matched
}

// orderAllowsDevice 渠道绑定监控设备后,仅该设备的上报可匹配其订单(0=不限制)。
// 主备接管: 绑定的主设备离线超过 5 分钟(无心跳)时,任意设备的上报自动接管匹配,
// 避免云端挂机场景下主设备掉线导致全部掉单。
func (s *Store) orderAllowsDevice(o *model.Order, deviceID int64) bool {
	if o.ChannelID == 0 || deviceID == 0 {
		return true
	}
	var devID int64
	if err := s.DB.QueryRow("SELECT device_id FROM channels WHERE id=?", o.ChannelID).Scan(&devID); err != nil {
		return true
	}
	if devID == 0 || devID == deviceID {
		return true
	}
	// 上报设备不是绑定设备: 若绑定设备已离线超过 5 分钟,则允许接管
	var lastHb int64
	var status int
	if err := s.DB.QueryRow("SELECT COALESCE(last_heartbeat,0),status FROM devices WHERE id=?", devID).Scan(&lastHb, &status); err != nil {
		return true // 绑定设备已被删除,视为不限制
	}
	if status != 1 {
		return true // 绑定设备已停用,允许其他设备接管
	}
	return lastHb == 0 || (model.Now()-lastHb) > 300
}

// CompleteOrder 标记订单成功并入账(幂等),随后触发异步通知。
func (s *Store) CompleteOrder(o *model.Order, buyer string) error {
	now := model.Now()
	res, err := s.DB.Exec("UPDATE orders SET status=1, endtime=?, buyer=? WHERE id=? AND status=0", now, buyer, o.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // 已处理过
	}
	o.Status = 1
	o.Endtime = now

	// 入账: 余额 += money - fee
	merchant, _ := s.UserByPid(o.Pid)
	if merchant != nil {
		rate := merchant.Rate
		if rate <= 0 {
			rate = atofDefault(s.DB.MustSetting("fee_rate"), 0)
		}
		fee := int64(float64(o.Money)*rate/100 + 0.5)
		income := o.Money - fee
		if income < 0 {
			income = 0
		}
		_, _ = s.DB.Exec("UPDATE orders SET fee=? WHERE id=?", fee, o.ID)
		_, _ = s.DB.Exec("UPDATE users SET balance=balance+? WHERE id=?", income, merchant.ID)
		var before int64
		_ = s.DB.QueryRow("SELECT balance FROM users WHERE id=?", merchant.ID).Scan(&before)
		_, _ = s.DB.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
			VALUES(?,?,?,?,?,?,?)`,
			merchant.ID, "order", income, before-income, before,
			fmt.Sprintf("订单入账 %s (手续费%s)", MoneyStr(income), MoneyStr(fee)), now)

		// 邀请返佣: 平台从手续费中按比例返给邀请人
		if fee > 0 && merchant.InvitedBy > 0 && s.DB.MustSetting("invite_open") == "1" {
			irate := atofDefault(s.DB.MustSetting("invite_rate"), 0)
			if rebate := int64(float64(fee)*irate/100 + 0.5); rebate > 0 {
				if _, err := s.DB.Exec("UPDATE users SET balance=balance+? WHERE id=?", rebate, merchant.InvitedBy); err == nil {
					var invBefore int64
					_ = s.DB.QueryRow("SELECT balance FROM users WHERE id=?", merchant.InvitedBy).Scan(&invBefore)
					_, _ = s.DB.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
						VALUES(?,?,?,?,?,?,?)`,
						merchant.InvitedBy, "invite", rebate, invBefore-rebate, invBefore,
						fmt.Sprintf("邀请返佣: 商户 %s 订单手续费 %s 的 %s%%", merchant.Username, MoneyStr(fee), trimRate(irate)), now)
				}
			}
		}
	}
	// 商户自配 IM 机器人推送(异步,不阻塞)
	if hook := merchant.NotifyWebhook; strings.TrimSpace(hook) != "" {
		msg := fmt.Sprintf("✅ 收款到账 %s\n订单: %s\n商品: %s", MoneyStr(o.Money), o.TradeNo, o.Name)
		go SendWebhookAlert(hook, s.DB.MustSetting("site_name"), msg)
	}
	// 立即异步通知 + 安排重试
	_, _ = s.DB.Exec("UPDATE orders SET next_notify_at=? WHERE id=?", now, o.ID)
	s.TriggerNotify(o.ID)
	return nil
}

// extractOrderNos 从通知内容中提取疑似订单号的数字串
func extractOrderNos(content string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() >= 14 { // trade_no 至少 18 位,商户单号通常也较长
			out = append(out, cur.String())
		}
		cur.Reset()
	}
	for _, r := range content {
		if r >= '0' && r <= '9' {
			cur.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// 避免切断 UTF-8 字符
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

func atoiDefault(s string, def int) int {
	var v int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v); err != nil || v <= 0 {
		return def
	}
	return v
}

// SetUserNotifyWebhook 设置商户收款成功 IM 推送地址(空=关闭)
func (s *Store) SetUserNotifyWebhook(userID int64, hook string) error {
	hook = strings.TrimSpace(hook)
	if len(hook) > 300 {
		return errors.New("webhook 地址过长")
	}
	_, err := s.DB.Exec("UPDATE users SET notify_webhook=? WHERE id=?", hook, userID)
	return err
}

// SetUserTOTP 开启/关闭两步验证
func (s *Store) SetUserTOTP(userID int64, secret string) error {
	_, err := s.DB.Exec("UPDATE users SET totp_secret=? WHERE id=?", secret, userID)
	return err
}

// SetUserLimits 设置商户收款限额(分,0=不限)
func (s *Store) SetUserLimits(userID int64, single, daily int64) error {
	if single < 0 {
		single = 0
	}
	if daily < 0 {
		daily = 0
	}
	_, err := s.DB.Exec("UPDATE users SET limit_single=?, limit_daily=? WHERE id=?", single, daily, userID)
	return err
}

// trimRate 费率展示: 去掉多余的小数尾零(10 → "10", 7.5 → "7.5")
func trimRate(r float64) string {
	s := strconv.FormatFloat(r, 'f', -1, 64)
	return s
}

func atofDefault(s string, def float64) float64 {
	var v float64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%f", &v); err != nil || v < 0 {
		return def
	}
	return v
}

// isInternalURL 检查URL是否指向内网/回环/链路本地地址(SSRF防护)
// 允许 127.0.0.1 和 10.0.2.2(Android模拟器宿主)用于本地开发/测试
func isInternalURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "" {
		return true
	}
	// 本地开发豁免
	if host == "127.0.0.1" || host == "localhost" || host == "10.0.2.2" {
		return false
	}
	// 检查私网IP段
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	// 域名解析后检查IP
	ips, err := net.LookupIP(host)
	if err != nil {
		return false // 无法解析,放行(可能是外网域名)
	}
	for _, resolved := range ips {
		if resolved.IsPrivate() || resolved.IsLoopback() || resolved.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}
