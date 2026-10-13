package model

import "time"

type User struct {
	ID            int64
	Username      string
	Email         string
	PasswordHash  string
	QQ            string
	Role          string  // user / admin
	Status        int     // 1正常 0封禁
	Rate          float64 // 费率%, 0=跟随全局
	Balance       int64   // 分
	Pid           int64
	Skey          string
	SettleType    int // 1支付宝 2微信 3QQ 4银行卡
	SettleAccount string
	SettleName    string
	InviteCode    string
	InvitedBy     int64
	CreatedAt     int64
	LastLoginAt   int64
	LastLoginIP   string
	Avatar        string
	ApiIPs        string // API IP 白名单(逗号分隔/CIDR,空=不限制)
	TotpSecret    string // TOTP 两步验证密钥(base32,空=未开启)
	NotifyWebhook string // 收款成功 IM 推送(飞书/钉钉/企微机器人地址)
	LimitSingle   int64  // 单笔收款限额(分,0=不限)
	LimitDaily    int64  // 单日收款限额(分,0=不限)
	RsaPublic     string // 商户 RSA 公钥(PEM,空=使用 MD5 签名)
	PayTypes      string // 商户可用支付方式(CSV,空=全部)
	NotifyEmailOn int    // 收款成功邮件通知开关(需已填邮箱)
	NotifyPost    int    // 异步通知使用 POST 方式(0=GET)
	ApiRate       int64  // 下单接口限流(次/分钟,0=跟随全局)
	RecoveryCodes string // TOTP 恢复代码(SHA256 十六进制 CSV,空=未生成)
}

type Order struct {
	ID           int64
	TradeNo      string
	OutTradeNo   string
	Pid          int64
	Type         string // alipay / wxpay / qqpay
	Name         string
	Money        int64 // 商户提交金额(分)
	PayMoney     int64 // 买家实际应付金额(分,含尾数偏移)
	Param        string
	NotifyURL    string
	ReturnURL    string
	Sitename     string
	Device       string
	ChannelID    int64
	Status       int // 0待支付 1支付成功 2已过期 3手动补单
	Fee          int64
	Buyer        string
	Addtime      int64
	Endtime      int64
	ExpireAt     int64
	NotifyCount  int
	NotifyOK     int
	NextNotifyAt int64
	ClientIP     string

	MerchantItemID int64  // 使用的商户自有码
	PlatformItemID int64  // 使用的平台码
	Remark         string // 运营/客服备注
}

type Channel struct {
	ID        int64
	Type      string // alipay / wxpay / qqpay
	Name      string
	Enabled   int
	MinMoney  int64 // 分
	MaxMoney  int64 // 分
	Qrcode    string
	Account   string
	DeviceID  int64
	AlipayUID string // 支付宝收款账号UID(用于深链自动带金额)
	BreakerAt int64  // 熔断停用时间(0=正常,仅自动熔断写入)
}

type Device struct {
	ID            int64
	Name          string
	Token         string
	LastHeartbeat int64
	LastIP        string
	Status        int
	CreatedAt     int64
}

type PayLog struct {
	ID        int64
	DeviceID  int64
	Type      string
	Money     int64
	Content   string
	OrderID   int64
	CreatedAt int64
}

type SettleLog struct {
	ID         int64
	UserID     int64
	Amount     int64
	SettleType int
	Account    string
	Name       string
	Status     int // 0待处理 1已打款 2已拒绝
	Note       string
	CreatedAt  int64
	DoneAt     int64
}

type BalanceLog struct {
	ID        int64
	UserID    int64
	Type      string
	Amount    int64
	Before    int64
	After     int64
	Memo      string
	CreatedAt int64
}

type Session struct {
	Token     string
	UserID    int64
	ExpiresAt int64
	CreatedAt int64
}

type NotifyLog struct {
	ID        int64
	OrderID   int64
	URL       string
	Response  string
	OK        int
	CreatedAt int64
}

// MerchantChanPref 商户某类型通道的偏好(轮询开关)
type MerchantChanPref struct {
	UserID    int64
	Type      string
	Rotate    int
	UpdatedAt int64
}

// MerchantItem 商户绑定的单个收款码(同类型可添加多个,轮询使用)
type MerchantItem struct {
	ID         int64
	UserID     int64
	Type       string
	Name       string
	Qrcode     string
	Enabled    int
	Weight     int64
	CreatedAt  int64
	LastUsedAt int64
}

func Now() int64 { return time.Now().Unix() }
