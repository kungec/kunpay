package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"

	_ "modernc.org/sqlite"

	"golang.org/x/crypto/bcrypt"
	"kunzhifu/internal/model"
)

type DB struct {
	*sql.DB
}

func Open(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	d, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite 支持多连接,但写并发受限;限制为单写连接避免 SQLITE_BUSY
	d.SetMaxOpenConns(1)
	if err := d.Ping(); err != nil {
		return nil, err
	}
	return &DB{d}, nil
}

var schema = `
CREATE TABLE IF NOT EXISTS users (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  username TEXT UNIQUE NOT NULL,
  email TEXT DEFAULT '',
  password_hash TEXT NOT NULL,
  qq TEXT DEFAULT '',
  role TEXT DEFAULT 'user',
  status INTEGER DEFAULT 1,
  rate REAL DEFAULT 0,
  balance INTEGER DEFAULT 0,
  pid INTEGER UNIQUE NOT NULL,
  skey TEXT NOT NULL,
  settle_type INTEGER DEFAULT 1,
  settle_account TEXT DEFAULT '',
  settle_name TEXT DEFAULT '',
  invite_code TEXT DEFAULT '',
  invited_by INTEGER DEFAULT 0,
  created_at INTEGER NOT NULL,
  last_login_at INTEGER DEFAULT 0,
  last_login_ip TEXT DEFAULT ''
);
CREATE TABLE IF NOT EXISTS orders (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  trade_no TEXT UNIQUE NOT NULL,
  out_trade_no TEXT NOT NULL,
  pid INTEGER NOT NULL,
  type TEXT NOT NULL,
  name TEXT NOT NULL,
  money INTEGER NOT NULL,
  pay_money INTEGER NOT NULL,
  param TEXT DEFAULT '',
  notify_url TEXT DEFAULT '',
  return_url TEXT DEFAULT '',
  sitename TEXT DEFAULT '',
  device TEXT DEFAULT 'pc',
  channel_id INTEGER DEFAULT 0,
  status INTEGER DEFAULT 0,
  fee INTEGER DEFAULT 0,
  buyer TEXT DEFAULT '',
  addtime INTEGER NOT NULL,
  endtime INTEGER DEFAULT 0,
  expire_at INTEGER NOT NULL,
  notify_count INTEGER DEFAULT 0,
  notify_ok INTEGER DEFAULT 0,
  next_notify_at INTEGER DEFAULT 0,
  client_ip TEXT DEFAULT '',
  UNIQUE(pid, out_trade_no)
);
CREATE INDEX IF NOT EXISTS idx_orders_match ON orders(status, type, pay_money, expire_at);
CREATE INDEX IF NOT EXISTS idx_orders_pid ON orders(pid, addtime);
CREATE INDEX IF NOT EXISTS idx_orders_notify ON orders(status, notify_ok, next_notify_at);
CREATE TABLE IF NOT EXISTS channels (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT UNIQUE NOT NULL,
  name TEXT NOT NULL,
  enabled INTEGER DEFAULT 1,
  min_money INTEGER DEFAULT 1,
  max_money INTEGER DEFAULT 500000,
  qrcode TEXT DEFAULT '',
  account TEXT DEFAULT '',
  device_id INTEGER DEFAULT 0,
  rotate INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS platform_channel_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  type TEXT NOT NULL,
  name TEXT DEFAULT '',
  qrcode TEXT NOT NULL,
  enabled INTEGER DEFAULT 1,
  weight INTEGER DEFAULT 1,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_pci ON platform_channel_items(type, enabled);
CREATE TABLE IF NOT EXISTS devices (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  name TEXT NOT NULL,
  token TEXT UNIQUE NOT NULL,
  last_heartbeat INTEGER DEFAULT 0,
  last_ip TEXT DEFAULT '',
  status INTEGER DEFAULT 1,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pay_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  device_id INTEGER DEFAULT 0,
  type TEXT NOT NULL,
  money INTEGER NOT NULL,
  content TEXT DEFAULT '',
  order_id INTEGER DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settle_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  amount INTEGER NOT NULL,
  settle_type INTEGER NOT NULL,
  account TEXT NOT NULL,
  name TEXT NOT NULL,
  status INTEGER DEFAULT 0,
  note TEXT DEFAULT '',
  created_at INTEGER NOT NULL,
  done_at INTEGER DEFAULT 0
);
CREATE TABLE IF NOT EXISTS balance_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  type TEXT NOT NULL,
  amount INTEGER NOT NULL,
  before INTEGER NOT NULL,
  after INTEGER NOT NULL,
  memo TEXT DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS pwd_resets (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  admin_id INTEGER NOT NULL,
  admin TEXT NOT NULL,
  action TEXT NOT NULL,
  detail TEXT DEFAULT '',
  ip TEXT DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
  k TEXT PRIMARY KEY,
  v TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  token TEXT PRIMARY KEY,
  user_id INTEGER NOT NULL,
  expires_at INTEGER NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS notify_logs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  order_id INTEGER NOT NULL,
  url TEXT NOT NULL,
  response TEXT DEFAULT '',
  ok INTEGER DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS merchant_channels (
  user_id INTEGER NOT NULL,
  type TEXT NOT NULL,
  rotate INTEGER DEFAULT 0,
  updated_at INTEGER DEFAULT 0,
  UNIQUE(user_id, type)
);
CREATE TABLE IF NOT EXISTS merchant_channel_items (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  type TEXT NOT NULL,
  name TEXT DEFAULT '',
  qrcode TEXT NOT NULL,
  enabled INTEGER DEFAULT 1,
  weight INTEGER DEFAULT 1,
  created_at INTEGER NOT NULL,
  last_used_at INTEGER DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_mci ON merchant_channel_items(user_id, type, enabled);
CREATE TABLE IF NOT EXISTS tickets (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id INTEGER NOT NULL,
  subject TEXT NOT NULL,
  status INTEGER DEFAULT 0,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tickets ON tickets(user_id, status);
CREATE TABLE IF NOT EXISTS ticket_msgs (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ticket_id INTEGER NOT NULL,
  is_admin INTEGER DEFAULT 0,
  content TEXT NOT NULL,
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tmsgs ON ticket_msgs(ticket_id);
CREATE INDEX IF NOT EXISTS idx_balance_user ON balance_logs(user_id, type, created_at);
CREATE INDEX IF NOT EXISTS idx_nlogs_order ON notify_logs(order_id);
CREATE INDEX IF NOT EXISTS idx_plogs_time ON pay_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_audits_time ON audit_logs(created_at);
`

// 默认配置项
var defaultSettings = map[string]string{
	"site_name":          "KunPay 码支付",
	"site_title":         "专业可靠的码支付解决方案",
	"announcement":       "请务必按上方金额付款(包含小数尾数),否则系统无法自动确认到账。",
	"kefu":               "",
	"fee_rate":           "1.0",
	"order_timeout":      "5",
	"money_offset":       "1",
	"register_open":      "1",
	"min_settle":         "1000",
	"auto_settle":        "0",
	"pay_notice":         "扫码后请输入上方金额付款,付款完成后本页面会自动跳转。",
	"poll_interval":      "3",
	"site_url":           "",
	"smtp_host":          "",
	"smtp_port":          "465",
	"smtp_user":          "",
	"smtp_pass":          "",
	"smtp_tls":           "ssl",
	"smtp_from":          "",
	"alert_email":        "",
	"device_alert":       "0",
	"merchant_notice":    "",
	"admin_ip_whitelist": "",
	"demo_pid":           "1001",
	"invite_open":        "1",
	"invite_rate":        "10",
	"alert_webhook":      "",
	"circuit_breaker":    "0",
	"backup_auto":        "0",
	"backup_keep":        "10",
	"statement_auto":     "0",
	"ip_blacklist":       "",
	"statement_last":     "",
	"digest_auto":        "0",
	"digest_last":        "",
	"update_check":       "1",
	"update_repo":        "",
	"update_latest":      "",
	"update_check_at":    "0",
	"footer_text":        "© 2026 KunPay. 保留所有权利.",
}

func (d *DB) Migrate() error {
	if _, err := d.Exec(schema); err != nil {
		return err
	}
	// 旧库 channels 表补 rotate 列;orders 表补收款码关联列;users 表补头像列
	if _, err := d.Exec("ALTER TABLE channels ADD COLUMN rotate INTEGER DEFAULT 0"); err != nil {
		// 列已存在则忽略
	}
	_, _ = d.Exec("ALTER TABLE orders ADD COLUMN m_item_id INTEGER DEFAULT 0")
	_, _ = d.Exec("ALTER TABLE orders ADD COLUMN p_item_id INTEGER DEFAULT 0")
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN avatar TEXT DEFAULT ''")
	// 收款码权重(轮询按权重分配);订单备注
	_, _ = d.Exec("ALTER TABLE platform_channel_items ADD COLUMN weight INTEGER DEFAULT 1")
	_, _ = d.Exec("ALTER TABLE merchant_channel_items ADD COLUMN weight INTEGER DEFAULT 1")
	_, _ = d.Exec("ALTER TABLE orders ADD COLUMN remark TEXT DEFAULT ''")
	// 商户 API IP 白名单(空=不限制)
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN api_ips TEXT DEFAULT ''")
	// TOTP 两步验证密钥(空=未开启) / 商户收款成功 IM 推送 webhook
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN totp_secret TEXT DEFAULT ''")
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN notify_webhook TEXT DEFAULT ''")
	// 商户收款限额(分,0=不限) / 渠道熔断时间戳(0=正常)
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN limit_single INTEGER DEFAULT 0")
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN limit_daily INTEGER DEFAULT 0")
	_, _ = d.Exec("ALTER TABLE channels ADD COLUMN breaker_at INTEGER DEFAULT 0")
	// 商户 RSA 公钥(空=MD5) / 商户可用支付方式(空=全部)
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN rsa_public TEXT DEFAULT ''")
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN pay_types TEXT DEFAULT ''")
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN notify_email_on INTEGER DEFAULT 0")
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN notify_post INTEGER DEFAULT 0")
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN api_rate INTEGER DEFAULT 0")
	_, _ = d.Exec("ALTER TABLE orders ADD COLUMN custom INTEGER DEFAULT 0")
	// 热查询复合索引(依赖上方 ALTER 出的列,须在其后创建)
	_, _ = d.Exec("CREATE INDEX IF NOT EXISTS idx_orders_pitem ON orders(p_item_id)")
	_, _ = d.Exec("CREATE INDEX IF NOT EXISTS idx_orders_mitem ON orders(m_item_id)")
	_, _ = d.Exec("ALTER TABLE users ADD COLUMN recovery_codes TEXT DEFAULT ''")
	// 会话设备信息(登录设备管理)
	_, _ = d.Exec("ALTER TABLE sessions ADD COLUMN ip TEXT DEFAULT ''")
	_, _ = d.Exec("ALTER TABLE sessions ADD COLUMN ua TEXT DEFAULT ''")
	_, _ = d.Exec("ALTER TABLE sessions ADD COLUMN last_seen INTEGER DEFAULT 0")
	_, _ = d.Exec("ALTER TABLE channels ADD COLUMN alipay_uid TEXT DEFAULT ''")
	// 旧版平台单一收款码迁移为多码 items
	var itemN, qrN int
	_ = d.QueryRow("SELECT COUNT(*) FROM platform_channel_items").Scan(&itemN)
	_ = d.QueryRow("SELECT COALESCE(SUM(CASE WHEN qrcode<>'' THEN 1 ELSE 0 END),0) FROM channels").Scan(&qrN)
	if itemN == 0 && qrN > 0 {
		if _, err := d.Exec(`INSERT INTO platform_channel_items(type,name,qrcode,enabled,created_at,last_used_at)
			SELECT type,'',qrcode,enabled,strftime('%s','now'),0 FROM channels WHERE qrcode<>''`); err != nil {
			return err
		}
		if _, err := d.Exec("UPDATE channels SET qrcode=''"); err != nil {
			return err
		}
		log.Println("[migrate] 平台渠道单一收款码已迁移为多码列表")
	}
	// 旧版 merchant_channels(单码 qrcode 列)迁移为多码 items 表
	if cols, err := d.Query("PRAGMA table_info(merchant_channels)"); err == nil {
		hasQR := false
		for cols.Next() {
			var cid, notnull, pk int
			var name, typ string
			var dflt any
			if cols.Scan(&cid, &name, &typ, &notnull, &dflt, &pk) == nil && name == "qrcode" {
				hasQR = true
			}
		}
		cols.Close()
		if hasQR {
			if _, err := d.Exec(`INSERT INTO merchant_channel_items(user_id,type,name,qrcode,enabled,created_at,last_used_at)
				SELECT user_id,type,'',qrcode,enabled,updated_at,0 FROM merchant_channels WHERE qrcode<>''`); err != nil {
				return err
			}
			if _, err := d.Exec("DROP TABLE merchant_channels"); err != nil {
				return err
			}
			if _, err := d.Exec(schema); err != nil {
				return err
			}
		}
	}
	for k, v := range defaultSettings {
		if _, err := d.Exec("INSERT OR IGNORE INTO settings(k,v) VALUES(?,?)", k, v); err != nil {
			return err
		}
	}
	// 渠道
	for _, c := range []struct {
		t, n string
	}{
		{"alipay", "支付宝"},
		{"wxpay", "微信支付"},
		{"qqpay", "QQ钱包"},
	} {
		if _, err := d.Exec("INSERT OR IGNORE INTO channels(type,name) VALUES(?,?)", c.t, c.n); err != nil {
			return err
		}
	}
	// 管理员 admin / admin123
	var n int
	if err := d.QueryRow("SELECT COUNT(*) FROM users WHERE role='admin'").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		hash, err := bcrypt.GenerateFromPassword([]byte("admin123"), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		_, err = d.Exec(`INSERT INTO users(username,email,password_hash,role,status,pid,skey,created_at)
			VALUES('admin','admin@kunpay.local',?,'admin',1,1000,?,?)`,
			string(hash), RandKey(32), model.Now())
		if err != nil {
			return err
		}
		log.Println("[init] 已创建管理员账号 admin / admin123 (请尽快修改密码)")
	}
	// 演示商户 demo / demo123
	if err := d.QueryRow("SELECT COUNT(*) FROM users WHERE username='demo'").Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		hash, _ := bcrypt.GenerateFromPassword([]byte("demo123"), bcrypt.DefaultCost)
		_, err := d.Exec(`INSERT INTO users(username,email,password_hash,role,status,pid,skey,created_at)
			VALUES('demo','demo@kunpay.local',?,'user',1,1001,?,?)`,
			string(hash), RandKey(32), model.Now())
		if err != nil {
			return err
		}
	}
	return nil
}

func RandKey(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 不应失败;兜底避免 panic
		for i := range b {
			b[i] = byte(i * 7)
		}
	}
	return hex.EncodeToString(b)
}

func RandDigits(n int) string {
	out := make([]byte, n)
	max := big.NewInt(10)
	for i := range out {
		v, err := rand.Int(rand.Reader, max)
		if err != nil {
			out[i] = byte('0' + i%10)
			continue
		}
		out[i] = byte('0' + v.Int64())
	}
	return string(out)
}

// ---- Settings ----

func (d *DB) GetSetting(key string) (string, error) {
	var v string
	err := d.QueryRow("SELECT v FROM settings WHERE k=?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

func (d *DB) MustSetting(key string) string {
	v, _ := d.GetSetting(key)
	return v
}

func (d *DB) AllSettings() (map[string]string, error) {
	m := map[string]string{}
	rows, err := d.Query("SELECT k,v FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

func (d *DB) SetSetting(key, val string) error {
	_, err := d.Exec("INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", key, val)
	return err
}
