package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	srv.TrustProxy = cfg.TrustProxy
	service.AllowLocalCallbacks = cfg.AllowLocalNotify
	web.AllowDefaultAdmin = cfg.AllowDefaultAdmin
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
	httpSrv := &http.Server{
		Addr:    addr,
		Handler: srv.Routes(),
		// 慢连接防护: 头部必须在 15s 内发完,空连接 120s 回收;
		// 读体不设总超时(200MB 安装包上传走 MaxBytesReader 限流)
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	// 优雅关停: SIGINT/SIGTERM 后停止接新请求,给在途请求 10s 收尾
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		log.Println("[shutdown] 收到退出信号,正在优雅关停...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("HTTP 服务启动失败: %v", err)
	}
	log.Println("[shutdown] 服务已停止")
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
