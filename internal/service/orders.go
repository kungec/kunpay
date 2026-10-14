package service

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
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
	ErrTypeNotOpen      = errors.New("该支付方式未开放,请联系商户")
	ErrAmountBusy       = errors.New("当前支付金额存在进行中的订单,请稍后重试或修改金额")
	ErrOrderPaid        = errors.New("订单号已存在且已支付,请勿重复提交")
	ErrBadMoney         = errors.New("金额格式错误,最多两位小数")
)

type Store struct {
	DB            *db.DB
	DataDir       string    // 数据目录(自动备份用)
	deviceAlertOn *sync.Map // 已告警的离线设备 id -> 告警时间
}

func NewStore(d *db.DB) *Store { return &Store{DB: d} }

func scanUser(row interface{ Scan(...any) error }) (*model.User, error) {
	u := &model.User{}
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.QQ, &u.Role, &u.Status,
		&u.Rate, &u.Balance, &u.Pid, &u.Skey, &u.SettleType, &u.SettleAccount, &u.SettleName,
		&u.InviteCode, &u.InvitedBy, &u.CreatedAt, &u.LastLoginAt, &u.LastLoginIP, &u.Avatar, &u.ApiIPs,
		&u.TotpSecret, &u.NotifyWebhook, &u.LimitSingle, &u.LimitDaily,
		&u.RsaPublic, &u.PayTypes, &u.NotifyEmailOn, &u.RecoveryCodes, &u.NotifyPost, &u.ApiRate, &u.Logo, &u.EventWebhook, &u.CashierNotice)
	if err != nil {
		return nil, err
	}
	return u, nil
}

const userCols = `id,username,email,password_hash,qq,role,status,rate,balance,pid,skey,
	settle_type,settle_account,settle_name,invite_code,invited_by,created_at,last_login_at,last_login_ip,avatar,COALESCE(api_ips,''),COALESCE(totp_secret,''),COALESCE(notify_webhook,''),COALESCE(limit_single,0),COALESCE(limit_daily,0),COALESCE(rsa_public,''),COALESCE(pay_types,''),COALESCE(notify_email_on,0),COALESCE(recovery_codes,''),COALESCE(notify_post,0),COALESCE(api_rate,0),COALESCE(logo,''),COALESCE(event_webhook,''),COALESCE(cashier_notice,'')`

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

// AllowLocalCallbacks 测试/本机联调开关: true 时回调地址允许回环地址(默认 false,生产禁止内网回调)
var AllowLocalCallbacks = false

// CallbackBlocked 校验商户可控回调地址是否应被拒绝(下单/发送时都调用,发送时复核防存储后策略变更)
func CallbackBlocked(rawURL string) bool {
	if rawURL == "" {
		return false
	}
	if AllowLocalCallbacks {
		return IsInternalURL(rawURL)
	}
	return IsInternalURLStrict(rawURL)
}

// SafeHTTPClient 严格 HTTP 客户端: 禁止跟随重定向(302 跳内网是常见 SSRF 绕过手段)
func SafeHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// PushEvent 推送订单生命周期事件到商户 Event Webhook(JSON + MD5 签名)
func (s *Store) PushEvent(merchant *model.User, event, tradeNo string, data map[string]any) {
	if merchant == nil {
		return
	}
	hook := strings.TrimSpace(merchant.EventWebhook)
	if hook == "" || CallbackBlocked(hook) {
		return
	}
	payload := map[string]any{
		"event":     event,
		"trade_no":  tradeNo,
		"timestamp": model.Now(),
	}
	for k, v := range data {
		payload[k] = v
	}
	sign := Sign(map[string]string{
		"event":    event,
		"trade_no": tradeNo,
	}, merchant.Skey)
	payload["sign"] = sign
	payload["sign_type"] = "MD5"
	body, _ := json.Marshal(payload)
	client := SafeHTTPClient(10 * time.Second)
	resp, err := client.Post(hook, "application/json", bytes.NewReader(body))
	if err != nil {
		return
	}
	resp.Body.Close()
}

// SetCustomAmount 任意金额订单: 买家提交金额后重新定价(含尾数偏移)并重新计时
func (s *Store) SetCustomAmount(tradeNo string, money int64) (*model.Order, error) {
	o := s.OrderByTradeNo(tradeNo)
	if o == nil || o.Custom != 1 || o.Status != 0 {
		return nil, errors.New("订单不存在或不支持自定金额")
	}
	if o.ExpireAt < model.Now() {
		return nil, errors.New("订单已过期,请重新发起")
	}
	if money < 1 {
		return nil, errors.New("金额格式错误")
	}
	ch, _ := s.ChannelByType(o.Type)
	if ch != nil {
		if money < ch.MinMoney {
			return nil, errors.New(fmt.Sprintf("最低支付 %s", MoneyStr(ch.MinMoney)))
		}
		if money > ch.MaxMoney {
			return nil, errors.New(fmt.Sprintf("最高支付 %s", MoneyStr(ch.MaxMoney)))
		}
	}
	// 买家自填金额同样受商户风控限额约束(0=不限)
	if u, _ := s.UserByPid(o.Pid); u != nil {
		if err := s.checkUserLimits(u, money); err != nil {
			return nil, err
		}
	}
	now := model.Now()
	timeoutMin := atoiDefault(s.DB.MustSetting("order_timeout"), 5)
	payMoney := money
	if s.DB.MustSetting("money_offset") == "1" {
		payMoney = s.uniquePayMoney(o.Type, money, now, int64(timeoutMin)*60)
	}
	// 单条原子 UPDATE 完成定价+去标记(custom 1→0): 并发提交只有一个生效,防双重定价
	res, err := s.DB.Exec("UPDATE orders SET money=?, pay_money=?, expire_at=?, custom=0 WHERE id=? AND status=0 AND custom=1",
		money, payMoney, now+int64(timeoutMin)*60, o.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, errors.New("订单状态已变化,请刷新收银台")
	}
	return s.OrderByTradeNo(tradeNo), nil
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
	Custom     bool // 任意金额收款(买家在收银台自填金额)
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
	if u.PayTypes != "" && !payTypeAllowed(u.PayTypes, req.Type) {
		return nil, ErrTypeNotOpen
	}
	if money < ch.MinMoney || money > ch.MaxMoney {
		return nil, ErrMoneyRange
	}
	// 商户风控限额(0=不限)
	if u.LimitSingle > 0 && money > u.LimitSingle {
		return nil, ErrLimitSingle
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
	// (-allow-local-notify 仅供本机联调,生产默认严格拒绝)
	for _, cbu := range []string{req.NotifyURL, req.ReturnURL} {
		lu := strings.ToLower(cbu)
		if cbu != "" && !strings.HasPrefix(lu, "http://") && !strings.HasPrefix(lu, "https://") {
			return nil, errors.New("通知地址仅支持 http/https 协议")
		}
		if cbu != "" && CallbackBlocked(cbu) {
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
	if o.PayMoney == money && offsetOn {
		// 整数金额: 尾数偏移 60 次仍冲突(在途极端拥挤) → 明确拒绝,避免错配
		var n int
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE status=0 AND COALESCE(custom,0)=0 AND type=? AND pay_money=? AND expire_at>?",
			req.Type, money, now+int64(timeoutMin)*60).Scan(&n)
		if n > 0 {
			return nil, ErrAmountBusy
		}
	}
	// 单日限额 + 插入放同一事务,避免并发下单同时通过限额检查(单连接池下事务天然串行)
	tx, err := s.DB.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := checkLimitDailyTx(tx, u, money); err != nil {
		return nil, err
	}
	res, err := tx.Exec(`INSERT INTO orders(trade_no,out_trade_no,pid,type,name,money,pay_money,param,
		notify_url,return_url,sitename,device,channel_id,status,addtime,expire_at,client_ip,custom)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,0,?,?,?,?)`,
		o.TradeNo, o.OutTradeNo, o.Pid, o.Type, o.Name, o.Money, o.PayMoney, o.Param,
		o.NotifyURL, o.ReturnURL, o.Sitename, o.Device, o.ChannelID, o.Addtime, o.ExpireAt, o.ClientIP, boolToInt(req.Custom))
	if err != nil {
		// 并发竞态: UNIQUE(pid,out_trade_no) 冲突时回落到复用已有订单
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			tx.Rollback()
			if existing := s.OrderByOutTradeNo(u.Pid, outTradeNo); existing != nil {
				if existing.Status == 0 && existing.ExpireAt > model.Now() {
					return existing, nil
				}
				return nil, ErrOrderPaid
			}
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	o.ID, _ = res.LastInsertId()
	if !req.Custom { // 自定义金额订单定价前是占位金额,禁止回溯匹配
		s.backfillUnmatched(o)
	}
	return o, nil
}

// checkLimitDailyTx 在事务内检查商户单日收款限额(0=不限)
func checkLimitDailyTx(tx *sql.Tx, u *model.User, money int64) error {
	if u.LimitDaily <= 0 {
		return nil
	}
	lt := time.Unix(model.Now(), 0)
	dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location()).Unix()
	var todayPaid int64
	if err := tx.QueryRow("SELECT COALESCE(SUM(money),0) FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=?",
		u.Pid, dayStart).Scan(&todayPaid); err != nil {
		return err
	}
	if todayPaid+money > u.LimitDaily {
		return ErrLimitDaily
	}
	return nil
}

// checkUserLimits 商户风控限额(单笔+单日),CreateOrder 与 SetCustomAmount 共用
func (s *Store) checkUserLimits(u *model.User, money int64) error {
	if u.LimitSingle > 0 && money > u.LimitSingle {
		return ErrLimitSingle
	}
	if u.LimitDaily > 0 {
		lt := time.Unix(model.Now(), 0)
		dayStart := time.Date(lt.Year(), lt.Month(), lt.Day(), 0, 0, 0, 0, lt.Location()).Unix()
		var todayPaid int64
		_ = s.DB.QueryRow("SELECT COALESCE(SUM(money),0) FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=?",
			u.Pid, dayStart).Scan(&todayPaid)
		if todayPaid+money > u.LimitDaily {
			return ErrLimitDaily
		}
	}
	return nil
}

// backfillUnmatched 回溯匹配: 新订单创建后,若近期(订单超时窗口内)存在同类型同金额的
// 未匹配到账流水(付款发生在下单之前),自动完成该订单并入账通知。
// 先原子认领流水(order_id 0→订单ID)再入账,防止并发订单重复消费同一笔真实付款;
// 流水须通过订单的设备绑定校验,且回溯窗口上下限都受约束(上限=拒绝未来时间戳)。
func (s *Store) backfillUnmatched(o *model.Order) {
	timeoutMin := atoiDefault(s.DB.MustSetting("order_timeout"), 5)
	now := model.Now()
	windowStart := now - int64(timeoutMin)*60
	var logID, logDevice int64
	err := s.DB.QueryRow(`SELECT id,device_id FROM pay_logs
		WHERE order_id=0 AND type=? AND money=? AND created_at>=? AND created_at<=? ORDER BY id ASC LIMIT 1`,
		o.Type, o.PayMoney, windowStart, now).Scan(&logID, &logDevice)
	if err != nil {
		return // 没有可回溯的流水
	}
	if !s.orderAllowsDevice(o, logDevice) {
		return // 流水来源设备与订单渠道绑定不符
	}
	res, err := s.DB.Exec("UPDATE pay_logs SET order_id=? WHERE id=? AND order_id=0", o.ID, logID)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return // 已被其他订单认领
	}
	if err := s.CompleteOrder(o, "回溯匹配"); err != nil {
		s.DB.Exec("UPDATE pay_logs SET order_id=0 WHERE id=?", logID) // 入账失败退还流水
		log.Printf("[match] 回溯匹配完成订单失败 trade_no=%s: %v", o.TradeNo, err)
	}
}

// uniquePayMoney 生成带随机尾数的应付金额,尽量避免与在途订单撞额。
func (s *Store) uniquePayMoney(typ string, money, now, ttl int64) int64 {
	inFlight := func(cand int64) bool {
		var n int
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE status=0 AND COALESCE(custom,0)=0 AND type=? AND pay_money=? AND expire_at>?",
			typ, cand, now+ttl-3600).Scan(&n)
		return n > 0
	}
	if money%100 != 0 {
		// 金额自带小数: 同一元内微调分位避开在途同额(9.90 冲突 → 9.91/9.89…),
		// 全部被占则返回原值,由创建方按「同额在途」明确拒绝
		cents, yuan := money%100, money-money%100
		if !inFlight(money) {
			return money
		}
		for d := int64(1); d <= 99; d++ {
			if up := cents + d; up <= 99 && !inFlight(yuan+up) {
				return yuan + up
			}
			if dn := cents - d; dn >= 1 && !inFlight(yuan+dn) {
				return yuan + dn
			}
		}
		return money
	}
	for i := 0; i < 60; i++ {
		c := int64(1 + fastrandN(99)) // 1~99 分
		cand := money + c
		if !inFlight(cand) {
			return cand
		}
	}
	return money + 1 + fastrandN(99)
}

func genTradeNo() string {
	return time.Now().Format("20060102150405") + db.RandDigits(10)
}

func (s *Store) OrderByID(id int64) *model.Order {
	o := &model.Order{}
	err := s.DB.QueryRow(`SELECT id,trade_no,out_trade_no,pid,type,name,money,pay_money,param,notify_url,
		return_url,sitename,device,channel_id,status,fee,buyer,addtime,endtime,expire_at,notify_count,notify_ok,next_notify_at,client_ip,m_item_id,p_item_id,COALESCE(remark,''),COALESCE(custom,0)
		FROM orders WHERE id=?`, id).
		Scan(&o.ID, &o.TradeNo, &o.OutTradeNo, &o.Pid, &o.Type, &o.Name, &o.Money, &o.PayMoney, &o.Param,
			&o.NotifyURL, &o.ReturnURL, &o.Sitename, &o.Device, &o.ChannelID, &o.Status, &o.Fee, &o.Buyer,
			&o.Addtime, &o.Endtime, &o.ExpireAt, &o.NotifyCount, &o.NotifyOK, &o.NextNotifyAt, &o.ClientIP,
			&o.MerchantItemID, &o.PlatformItemID, &o.Remark, &o.Custom)
	if err != nil {
		return nil
	}
	return o
}

func (s *Store) OrderByTradeNo(tradeNo string) *model.Order {
	o := &model.Order{}
	err := s.DB.QueryRow(`SELECT id,trade_no,out_trade_no,pid,type,name,money,pay_money,param,notify_url,
		return_url,sitename,device,channel_id,status,fee,buyer,addtime,endtime,expire_at,notify_count,notify_ok,next_notify_at,client_ip,m_item_id,p_item_id,COALESCE(remark,''),COALESCE(custom,0)
		FROM orders WHERE trade_no=?`, tradeNo).
		Scan(&o.ID, &o.TradeNo, &o.OutTradeNo, &o.Pid, &o.Type, &o.Name, &o.Money, &o.PayMoney, &o.Param,
			&o.NotifyURL, &o.ReturnURL, &o.Sitename, &o.Device, &o.ChannelID, &o.Status, &o.Fee, &o.Buyer,
			&o.Addtime, &o.Endtime, &o.ExpireAt, &o.NotifyCount, &o.NotifyOK, &o.NextNotifyAt, &o.ClientIP,
			&o.MerchantItemID, &o.PlatformItemID, &o.Remark, &o.Custom)
	if err != nil {
		return nil
	}
	return o
}

func (s *Store) OrderByOutTradeNo(pid int64, outTradeNo string) *model.Order {
	o := &model.Order{}
	err := s.DB.QueryRow(`SELECT id,trade_no,out_trade_no,pid,type,name,money,pay_money,param,notify_url,
		return_url,sitename,device,channel_id,status,fee,buyer,addtime,endtime,expire_at,notify_count,notify_ok,next_notify_at,client_ip,m_item_id,p_item_id,COALESCE(remark,''),COALESCE(custom,0)
		FROM orders WHERE pid=? AND out_trade_no=?`, pid, outTradeNo).
		Scan(&o.ID, &o.TradeNo, &o.OutTradeNo, &o.Pid, &o.Type, &o.Name, &o.Money, &o.PayMoney, &o.Param,
			&o.NotifyURL, &o.ReturnURL, &o.Sitename, &o.Device, &o.ChannelID, &o.Status, &o.Fee, &o.Buyer,
			&o.Addtime, &o.Endtime, &o.ExpireAt, &o.NotifyCount, &o.NotifyOK, &o.NextNotifyAt, &o.ClientIP,
			&o.MerchantItemID, &o.PlatformItemID, &o.Remark, &o.Custom)
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
	// 推送去重: 监控APP超时重试/截获重放会重复推送同一笔到账;窗口内同设备/同方式/同金额/同内容
	// 且已成功匹配过的推送直接忽略,防止误匹配到第二笔同额订单(窗口覆盖 VMQ 的新鲜度区间)。
	var dup int
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM pay_logs
		WHERE device_id=? AND type=? AND money=? AND content=? AND order_id>0 AND created_at>?`,
		deviceID, typ, money, truncate(content, 500), now-300).Scan(&dup)
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
			return_url,sitename,device,channel_id,status,fee,buyer,addtime,endtime,expire_at,notify_count,notify_ok,next_notify_at,client_ip,m_item_id,p_item_id,COALESCE(remark,''),COALESCE(custom,0)
			FROM orders WHERE status=0 AND COALESCE(custom,0)=0 AND type=? AND pay_money=? AND expire_at>? ORDER BY id ASC LIMIT 5`,
			typ, money, now-300)
		if err == nil {
			for rows.Next() {
				o := &model.Order{}
				if err := rows.Scan(&o.ID, &o.TradeNo, &o.OutTradeNo, &o.Pid, &o.Type, &o.Name, &o.Money, &o.PayMoney,
					&o.Param, &o.NotifyURL, &o.ReturnURL, &o.Sitename, &o.Device, &o.ChannelID, &o.Status, &o.Fee,
					&o.Buyer, &o.Addtime, &o.Endtime, &o.ExpireAt, &o.NotifyCount, &o.NotifyOK, &o.NextNotifyAt, &o.ClientIP,
					&o.MerchantItemID, &o.PlatformItemID, &o.Remark, &o.Custom); err == nil {
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
	if _, err := s.DB.Exec("INSERT INTO pay_logs(device_id,type,money,content,order_id,created_at) VALUES(?,?,?,?,?,?)",
		deviceID, typ, money, truncate(content, 500), orderID, ts); err != nil {
		log.Printf("[match] 写入到账流水失败: %v", err)
	}
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

	// 事务前置读取(事务内不得再碰 s.DB: 连接池仅 1,会自锁)
	merchant, _ := s.UserByPid(o.Pid)
	rate := 0.0
	if merchant != nil {
		rate = merchant.Rate
	}
	if rate <= 0 {
		rate = atofDefault(s.DB.MustSetting("fee_rate"), 0)
	}
	inviteOn := s.DB.MustSetting("invite_open") == "1"
	inviteRate := atofDefault(s.DB.MustSetting("invite_rate"), 0)

	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec("UPDATE orders SET status=1, endtime=?, buyer=? WHERE id=? AND status=0", now, buyer, o.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil // 已处理过(幂等)
	}
	o.Status = 1
	o.Endtime = now

	if merchant != nil {
		fee := int64(float64(o.Money)*rate/100 + 0.5)
		income := o.Money - fee
		if income < 0 {
			income = 0
		}
		if _, err := tx.Exec("UPDATE orders SET fee=? WHERE id=?", fee, o.ID); err != nil {
			return err
		}
		var before int64
		if err := tx.QueryRow("SELECT balance FROM users WHERE id=?", merchant.ID).Scan(&before); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE users SET balance=balance+? WHERE id=?", income, merchant.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
			VALUES(?,?,?,?,?,?,?)`,
			merchant.ID, "order", income, before, before+income,
			fmt.Sprintf("订单入账 %s (手续费%s)", MoneyStr(income), MoneyStr(fee)), now); err != nil {
			return err
		}
		o.Fee = fee

		// 邀请返佣: 平台从手续费中按比例返给邀请人
		if fee > 0 && merchant.InvitedBy > 0 && inviteOn && inviteRate > 0 {
			if rebate := int64(float64(fee)*inviteRate/100 + 0.5); rebate > 0 {
				var invBefore int64
				if err := tx.QueryRow("SELECT balance FROM users WHERE id=?", merchant.InvitedBy).Scan(&invBefore); err == nil {
					if _, err := tx.Exec("UPDATE users SET balance=balance+? WHERE id=?", rebate, merchant.InvitedBy); err == nil {
						if _, err := tx.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
							VALUES(?,?,?,?,?,?,?)`,
							merchant.InvitedBy, "invite", rebate, invBefore, invBefore+rebate,
							fmt.Sprintf("邀请返佣: 商户 %s 订单手续费 %s 的 %s%%", merchant.Username, MoneyStr(fee), trimRate(inviteRate)), now); err != nil {
							return err
						}
					}
				}
			}
		}
	}
	// 立即异步通知 + 安排重试
	if _, err := tx.Exec("UPDATE orders SET next_notify_at=? WHERE id=?", now, o.ID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}

	// ---- 事务提交后的旁路动作(网络/异步,失败不影响入账) ----
	if merchant != nil {
		// 订单支付成功事件(签名)推送到商户 Event Webhook
		if strings.TrimSpace(merchant.EventWebhook) != "" {
			go s.PushEvent(merchant, "order.paid", o.TradeNo, map[string]any{
				"out_trade_no": o.OutTradeNo, "type": o.Type,
				"name": o.Name, "money": MoneyStr(o.Money),
			})
		}
		// 商户自配 IM 机器人推送(异步,不阻塞)
		if hook := strings.TrimSpace(merchant.NotifyWebhook); hook != "" {
			msg := fmt.Sprintf("✅ 收款到账 %s\n订单: %s\n商品: %s", MoneyStr(o.Money), o.TradeNo, o.Name)
			go SendWebhookAlert(hook, s.DB.MustSetting("site_name"), msg)
		}
		// 商户收款邮件通知(异步,由 web 层注入发送实现)
		if merchant.NotifyEmailOn == 1 && merchant.Email != "" && PayEmailHook != nil {
			go PayEmailHook(merchant, o)
		}
	}
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

// payTypeAllowed 检查支付方式是否在商户开放列表(CSV)
func payTypeAllowed(csvList, typ string) bool {
	for _, t := range strings.Split(csvList, ",") {
		if strings.TrimSpace(t) == typ {
			return true
		}
	}
	return false
}

// SetUserPayTypes 设置商户可用支付方式(空=全部开放)
func (s *Store) SetUserPayTypes(userID int64, types []string) error {
	clean := make([]string, 0, len(types))
	for _, t := range types {
		t = strings.TrimSpace(t)
		if t == "alipay" || t == "wxpay" || t == "qqpay" {
			clean = append(clean, t)
		}
	}
	_, err := s.DB.Exec("UPDATE users SET pay_types=? WHERE id=?", strings.Join(clean, ","), userID)
	return err
}

// SetUserRSAPublic 保存商户 RSA 公钥(空=回落 MD5)
func (s *Store) SetUserRSAPublic(userID int64, pubPEM string) error {
	_, err := s.DB.Exec("UPDATE users SET rsa_public=? WHERE id=?", pubPEM, userID)
	return err
}

// PayEmailHook 由 web 层注入: 商户开启收款邮件通知后的发送实现
var PayEmailHook func(merchant *model.User, o *model.Order)

// SetUserApiRate 设置商户下单接口限流(次/分钟,0=跟随全局)
func (s *Store) SetUserApiRate(userID int64, rate int64) error {
	if rate < 0 {
		rate = 0
	}
	if rate > 100000 {
		rate = 100000
	}
	_, err := s.DB.Exec("UPDATE users SET api_rate=? WHERE id=?", rate, userID)
	return err
}

// SetUserNotifyPost 设置异步通知请求方式
func (s *Store) SetUserNotifyPost(userID int64, post bool) error {
	v := 0
	if post {
		v = 1
	}
	_, err := s.DB.Exec("UPDATE users SET notify_post=? WHERE id=?", v, userID)
	return err
}

// SetUserNotifyEmail 设置收款邮件通知开关
func (s *Store) SetUserNotifyEmail(userID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := s.DB.Exec("UPDATE users SET notify_email_on=? WHERE id=?", v, userID)
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
// IsInternalURLStrict 严格判定(无本地开发豁免): 用于商户可控的 Webhook 地址
func IsInternalURLStrict(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return true
	}
	host := u.Hostname()
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil {
		return ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return true // 无法解析的域名一律拒绝,不给探测机会
	}
	for _, resolved := range ips {
		if resolved.IsPrivate() || resolved.IsLoopback() || resolved.IsLinkLocalUnicast() || resolved.IsUnspecified() {
			return true
		}
	}
	return false
}

func IsInternalURL(rawURL string) bool {
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
