package config

import (
	"flag"
	"log"
	"os"
	"path/filepath"
)

type Config struct {
	Port     string
	DBPath   string
	DataDir  string
	SiteName string
	Captcha  bool
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
	flag.BoolVar(&cfg.Captcha, "captcha", true, "注册/登录/找回密码启用图形验证码(回归测试可关闭)")
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
