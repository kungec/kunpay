package main

import (
	"log"
	"net/http"

	"kunzhifu/internal/config"
	"kunzhifu/internal/db"
	"kunzhifu/internal/service"
	"kunzhifu/internal/web"
)

func main() {
	cfg := config.Load()
	store := service.NewStore(mustDB(cfg))
	store.DataDir = cfg.DataDir

	srv := web.NewServer(store, cfg.DataDir, cfg.Port)
	srv.CaptchaEnabled = cfg.Captcha
	if err := srv.Init(); err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	// 启动后台任务: 订单过期 / 通知重试 / 设备离线告警
	srv.InjectAlert()
	store.RunBackground()

	addr := "0.0.0.0:" + cfg.Port
	log.Printf("========================================")
	log.Printf("  KunPay 码支付系统 已启动")
	log.Printf("  监听地址: %s", addr)
	log.Printf("  管理后台: http://localhost:%s/admin  (admin / admin123)", cfg.Port)
	log.Printf("  商户中心: http://localhost:%s/login", cfg.Port)
	log.Printf("  支付测试: http://localhost:%s/demo", cfg.Port)
	log.Printf("========================================")
	if err := http.ListenAndServe(addr, srv.Routes()); err != nil {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
}

func mustDB(cfg *config.Config) *db.DB {
	d, err := db.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	if err := d.Migrate(); err != nil {
		log.Fatalf("数据库初始化失败: %v", err)
	}
	return d
}
