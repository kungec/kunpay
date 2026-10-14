package config

import (
	"flag"
	"log"
	"os"
	"path/filepath"
)

type Config struct {
	Port              string
	DBPath            string
	DataDir           string
	SiteName          string
	Captcha           bool
	TrustProxy        bool
	AllowLocalNotify  bool // 允许回调地址指向回环/内网(仅本机联调用,生产必须关闭)
	AllowDefaultAdmin bool // 关闭管理员默认密码强制修改(仅回归测试/演示用)
}

func Load() *Config {
	execDir := ""
	if exe, err := os.Executable(); err == nil {
		execDir = filepath.Dir(exe)
	}
	defDB := filepath.Join(".", "kunpay.db")
	defData := filepath.Join(".", "data")
	if execDir != "" {
		if wd, err := os.Getwd(); err == nil && wd != execDir {
			// keep defaults relative to working directory
			_ = wd
		}
	}

	cfg := &Config{}
	flag.StringVar(&cfg.Port, "port", envOr("KUNPAY_PORT", "8080"), "HTTP 监听端口")
	flag.StringVar(&cfg.DBPath, "db", envOr("KUNPAY_DB", defDB), "SQLite 数据库文件路径")
	flag.StringVar(&cfg.DataDir, "data", envOr("KUNPAY_DATA", defData), "上传文件(收款码等)存储目录")
	flag.BoolVar(&cfg.TrustProxy, "trust-proxy", true, "信任反向代理的 X-Real-IP/X-Forwarded-For 头(仅当直连对端为内网/回环时生效)")
	flag.BoolVar(&cfg.Captcha, "captcha", true, "注册/登录/找回密码启用图形验证码(回归测试可关闭)")
	flag.BoolVar(&cfg.AllowLocalNotify, "allow-local-notify", envBool("KUNPAY_ALLOW_LOCAL_NOTIFY"), "允许异步通知/回调地址指向内网回环地址(仅本机联调,生产必须关闭)")
	flag.BoolVar(&cfg.AllowDefaultAdmin, "allow-default-admin", envBool("KUNPAY_ALLOW_DEFAULT_ADMIN"), "关闭管理员默认密码强制修改(仅回归测试/演示用)")
	flag.Parse()

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("创建数据目录失败: %v", err)
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string) bool {
	v := os.Getenv(key)
	return v == "1" || v == "true"
}
