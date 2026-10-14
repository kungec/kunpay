package web

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	webfs "kunzhifu/web"
)

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	// 公开页面
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /doc", s.handleDocs)
	mux.HandleFunc("GET /doc/{page}", s.handleDocs)
	mux.HandleFunc("GET /demo", s.handleDemoPage)
	mux.HandleFunc("POST /demo/create", s.handleDemoCreate)
	mux.HandleFunc("GET /demo/done", s.handleDemoDone)
	mux.HandleFunc("GET /check", s.handleOrderCheckPage)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var dbOK int
		s.DB.QueryRow("SELECT 1").Scan(&dbOK)
		st := "ok"
		code := 200
		if dbOK != 1 {
			st = "db_error"
			code = 503
		}
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{
			"status": st, "time": time.Now().Format(time.RFC3339),
		})
	})
	mux.HandleFunc("GET /receipt", s.handleOrderReceipt)
	mux.HandleFunc("GET /download", s.handleDownload)
	mux.HandleFunc("GET /status", s.handleStatus)
	mux.HandleFunc("GET /download/file", s.handleDownloadFile)

	// 认证
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLoginPost)
	mux.HandleFunc("GET /captcha/image", handleCaptchaImage)
	mux.HandleFunc("GET /forgot", s.handleForgotPage)
	mux.HandleFunc("POST /forgot", s.handleForgotPost)
	mux.HandleFunc("GET /reset", s.handleResetPage)
	mux.HandleFunc("POST /reset", s.handleResetPost)
	mux.HandleFunc("GET /register", s.handleRegisterPage)
	mux.HandleFunc("POST /register", s.handleRegisterPost)
	mux.HandleFunc("GET /logout", s.handleLogout)
	mux.HandleFunc("POST /logout", s.handleLogout)

	// 易支付兼容接口(根路径 + /epay/ 别名),带 IP 限频防刷
	for _, prefix := range []string{"", "/epay/"} {
		mux.HandleFunc(prefix+"/submit.php", s.handleEpaySubmit)
		mux.HandleFunc(prefix+"/mapi.php", s.handleEpayMapi)
		mux.HandleFunc(prefix+"/api.php", s.handleEpayApi)
	}

	// 收银台
	mux.HandleFunc("GET /pay/{tradeNo}", s.handlePayCashier)
	mux.HandleFunc("GET /pay/{tradeNo}/check", s.handlePayCheck)
	mux.HandleFunc("GET /pay/{tradeNo}/qr", s.handlePayQR)

	// 监控设备 API(自有协议 + V免签兼容)
	mux.HandleFunc("GET /monitor/heart", s.handleMonitorHeart)
	mux.HandleFunc("POST /monitor/heart", s.handleMonitorHeart)
	mux.HandleFunc("GET /monitor/push", s.handleMonitorPush)
	mux.HandleFunc("POST /monitor/push", s.handleMonitorPush)
	mux.HandleFunc("GET /appHeart", s.handleVmqHeart)
	mux.HandleFunc("GET /appPushV", s.handleVmqPush)

	// 用户中心
	mux.HandleFunc("GET /user", s.requireLogin(s.handleUserDashboard))
	mux.HandleFunc("GET /user/merchant", s.requireLogin(s.handleUserMerchant))
	mux.HandleFunc("POST /user/merchant/regen", s.requireLogin(s.requireCSRF(s.handleUserMerchantRegen)))
	mux.HandleFunc("POST /user/merchant/apiips", s.requireLogin(s.requireCSRF(s.handleUserApiIPs)))
	mux.HandleFunc("POST /user/merchant/rsa", s.requireLogin(s.requireCSRF(s.handleUserMerchantRSA)))
	mux.HandleFunc("POST /user/merchant/notifypost", s.requireLogin(s.requireCSRF(s.handleUserNotifyPost)))
	mux.HandleFunc("POST /user/paytypes", s.requireLogin(s.requireCSRF(s.handleUserPayTypes)))
	mux.HandleFunc("GET /user/channels", s.requireLogin(s.handleUserChannels))
	mux.HandleFunc("POST /user/channels/upload", s.requireLogin(s.requireCSRF(s.handleUserChannelsUpload)))
	mux.HandleFunc("POST /user/channels/item/toggle", s.requireLogin(s.requireCSRF(s.handleUserChannelsItemToggle)))
	mux.HandleFunc("POST /user/channels/item/del", s.requireLogin(s.requireCSRF(s.handleUserChannelsItemDel)))
	mux.HandleFunc("POST /user/channels/item/weight", s.requireLogin(s.requireCSRF(s.handleUserChannelsItemWeight)))
	mux.HandleFunc("POST /user/channels/rotate", s.requireLogin(s.requireCSRF(s.handleUserChannelsRotate)))
	mux.HandleFunc("GET /user/orders", s.requireLogin(s.handleUserOrders))
	mux.HandleFunc("GET /user/settle", s.requireLogin(s.handleUserSettle))
	mux.HandleFunc("GET /user/settle/export.csv", s.requireLogin(s.handleUserSettleExport))
	mux.HandleFunc("POST /user/settle/apply", s.requireLogin(s.requireCSRF(s.handleUserSettleApply)))
	mux.HandleFunc("GET /user/settings", s.requireLogin(s.handleUserSettings))
	mux.HandleFunc("POST /user/settings/profile", s.requireLogin(s.requireCSRF(s.handleUserSettingsProfile)))
	mux.HandleFunc("POST /user/settings/password", s.requireLogin(s.requireCSRF(s.handleUserSettingsPassword)))
	mux.HandleFunc("GET /user/settings/totp/setup", s.requireLogin(s.handleUserTOTPSetup))
	mux.HandleFunc("POST /user/settings/totp", s.requireLogin(s.requireCSRF(s.handleUserTOTPEnable)))
	mux.HandleFunc("POST /user/settings/totp/off", s.requireLogin(s.requireCSRF(s.handleUserTOTPDisable)))
	mux.HandleFunc("POST /user/settings/totp/recovery", s.requireLogin(s.requireCSRF(s.handleUserTOTPRecoveryRegen)))
	mux.HandleFunc("POST /user/settings/sessions/revoke", s.requireLogin(s.requireCSRF(s.handleUserSessionsRevoke)))
	mux.HandleFunc("GET /user/totp/qr", s.requireLogin(s.handleUserTOTPQR))
	mux.HandleFunc("POST /user/notify/webhook", s.requireLogin(s.requireCSRF(s.handleUserNotifyWebhook)))
	mux.HandleFunc("POST /user/avatar/upload", s.requireLogin(s.requireCSRF(s.handleUserAvatarUpload)))
	mux.HandleFunc("POST /user/avatar/clear", s.requireLogin(s.requireCSRF(s.handleUserAvatarClear)))
	mux.HandleFunc("GET /user/balance", s.requireLogin(s.handleUserBalance))
	mux.HandleFunc("GET /user/logs", s.requireLogin(s.handleUserLoginLogs))

	// 管理后台
	mux.HandleFunc("GET /admin", s.requireAdmin(s.handleAdminHome))
	mux.HandleFunc("GET /admin/todo.json", s.requireAdmin(s.handleAdminTodo))
	mux.HandleFunc("GET /admin/users", s.requireAdmin(s.handleAdminUsers))
	mux.HandleFunc("POST /admin/users/save", s.requireAdmin(s.requireCSRF(s.handleAdminUsersSave)))
	mux.HandleFunc("GET /admin/orders", s.requireAdmin(s.handleAdminOrders))
	mux.HandleFunc("POST /admin/orders/repair", s.requireAdmin(s.requireCSRF(s.handleAdminOrdersRepair)))
	mux.HandleFunc("GET /admin/channels", s.requireAdmin(s.handleAdminChannels))
	mux.HandleFunc("POST /admin/channels/save", s.requireAdmin(s.requireCSRF(s.handleAdminChannelsSave)))
	mux.HandleFunc("POST /admin/channels/item/add", s.requireAdmin(s.requireCSRF(s.handleAdminChannelsItemAdd)))
	mux.HandleFunc("POST /admin/channels/item/toggle", s.requireAdmin(s.requireCSRF(s.handleAdminChannelsItemToggle)))
	mux.HandleFunc("POST /admin/channels/item/del", s.requireAdmin(s.requireCSRF(s.handleAdminChannelsItemDel)))
	mux.HandleFunc("POST /admin/channels/item/weight", s.requireAdmin(s.requireCSRF(s.handleAdminChannelsItemWeight)))
	mux.HandleFunc("POST /admin/order/remark", s.requireAdmin(s.requireCSRF(s.handleAdminOrderRemark)))
	mux.HandleFunc("POST /admin/channels/rotate", s.requireAdmin(s.requireCSRF(s.handleAdminChannelsRotate)))
	mux.HandleFunc("GET /admin/devices", s.requireAdmin(s.handleAdminDevices))
	mux.HandleFunc("POST /admin/devices/add", s.requireAdmin(s.requireCSRF(s.handleAdminDevicesAdd)))
	mux.HandleFunc("POST /admin/devices/toggle", s.requireAdmin(s.requireCSRF(s.handleAdminDevicesToggle)))
	mux.HandleFunc("POST /admin/devices/del", s.requireAdmin(s.requireCSRF(s.handleAdminDevicesDel)))
	mux.HandleFunc("GET /admin/settles", s.requireAdmin(s.handleAdminSettles))
	mux.HandleFunc("POST /admin/settles/do", s.requireAdmin(s.requireCSRF(s.handleAdminSettlesDo)))
	mux.HandleFunc("GET /admin/settings", s.requireAdmin(s.handleAdminSettings))
	mux.HandleFunc("POST /admin/settings/save", s.requireAdmin(s.requireCSRF(s.handleAdminSettingsSave)))
	mux.HandleFunc("POST /admin/settings/rsa", s.requireAdmin(s.requireCSRF(s.handleAdminRSA)))
	mux.HandleFunc("POST /admin/update/check", s.requireAdmin(s.requireCSRF(s.handleAdminUpdateCheck)))
	mux.HandleFunc("POST /admin/settings/smtp-test", s.requireAdmin(s.requireCSRF(s.handleAdminSMTPTest)))
	mux.HandleFunc("POST /admin/downloads/upload", s.requireAdmin(s.requireCSRF(s.handleAdminDownloadsUpload)))
	mux.HandleFunc("POST /admin/downloads/delete", s.requireAdmin(s.requireCSRF(s.handleAdminDownloadsDelete)))
	mux.HandleFunc("POST /admin/settings/seed", s.requireAdmin(s.requireCSRF(s.handleAdminSeed)))
	mux.HandleFunc("POST /admin/backup/create", s.requireAdmin(s.requireCSRF(s.handleAdminBackupCreate)))
	mux.HandleFunc("GET /admin/backup/download", s.requireAdmin(s.handleAdminBackupDownload))
	mux.HandleFunc("POST /admin/mail/test", s.requireAdmin(s.requireCSRF(s.handleAdminMailTest)))
	mux.HandleFunc("POST /admin/logo/upload", s.requireAdmin(s.requireCSRF(s.handleAdminLogoUpload)))
	mux.HandleFunc("POST /admin/logo/clear", s.requireAdmin(s.requireCSRF(s.handleAdminLogoClear)))
	mux.HandleFunc("GET /admin/paylogs", s.requireAdmin(s.handleAdminPayLogs))
	mux.HandleFunc("GET /admin/audit", s.requireAdmin(s.handleAdminAudit))
	mux.HandleFunc("GET /api/poll", s.requireLogin(s.handleOrderPoll))
	mux.HandleFunc("GET /admin/cleanup", s.requireAdmin(s.handleAdminCleanup))
	mux.HandleFunc("GET /admin/channelstats", s.requireAdmin(s.handleAdminChannelStats))
	mux.HandleFunc("GET /admin/revenue", s.requireAdmin(s.handleAdminRevenue))
	mux.HandleFunc("GET /admin/screen", s.requireAdmin(s.handleAdminScreen))
	mux.HandleFunc("POST /admin/cleanup/run", s.requireAdmin(s.requireCSRF(s.handleAdminCleanupRun)))
	mux.HandleFunc("POST /admin/paylogs/match", s.requireAdmin(s.requireCSRF(s.handleAdminPayLogMatch)))
	mux.HandleFunc("POST /admin/orders/notify", s.requireAdmin(s.requireCSRF(s.handleAdminOrderNotify)))
	mux.HandleFunc("GET /admin/users/{id}/channels", s.requireAdmin(s.handleAdminUserChannels))
	mux.HandleFunc("GET /user/report", s.requireLogin(s.handleUserReport))
	mux.HandleFunc("GET /user/report/export.csv", s.requireLogin(s.handleUserReportExport))
	mux.HandleFunc("POST /user/report/mail", s.requireLogin(s.requireCSRF(s.handleUserReportMail)))
	mux.HandleFunc("POST /user/debug", s.requireLogin(s.requireCSRF(s.handleUserDebug)))
	mux.HandleFunc("POST /user/debug/notify", s.requireLogin(s.requireCSRF(s.handleUserDebugNotify)))
	mux.HandleFunc("GET /user/orders/export.csv", s.requireLogin(s.handleUserOrdersExport))
	mux.HandleFunc("GET /user/orders/{id}", s.requireLogin(s.handleUserOrderDetail))
	mux.HandleFunc("POST /user/paylink", s.requireLogin(s.requireCSRF(s.handleUserPayLink)))
	mux.HandleFunc("GET /user/quickpay", s.requireLogin(s.handleUserQuickPay))
	mux.HandleFunc("POST /pay/{tradeNo}/amount", s.handlePaySetAmount)
	mux.HandleFunc("GET /user/invite", s.requireLogin(s.handleUserInvite))
	mux.HandleFunc("POST /user/orders/refund", s.requireLogin(s.requireCSRF(s.handleUserOrderRefund)))
	mux.HandleFunc("POST /user/orders/notify", s.requireLogin(s.requireCSRF(s.handleUserOrderNotify)))
	mux.HandleFunc("GET /user/tickets", s.requireLogin(s.handleUserTickets))
	mux.HandleFunc("POST /user/orders/close", s.requireLogin(s.requireCSRF(s.handleUserOrderClose)))
	mux.HandleFunc("POST /user/tickets/create", s.requireLogin(s.requireCSRF(s.handleUserTicketCreate)))
	mux.HandleFunc("GET /user/messages", s.requireLogin(s.handleUserMessages))
	mux.HandleFunc("POST /user/logo/upload", s.requireLogin(s.requireCSRF(s.handleUserLogoUpload)))
	mux.HandleFunc("POST /user/logo/delete", s.requireLogin(s.requireCSRF(s.handleUserLogoDelete)))
	mux.HandleFunc("POST /user/messages/mark_read", s.requireLogin(s.handleUserMessages))
	mux.HandleFunc("GET /user/tickets/{id}", s.requireLogin(s.handleUserTicketView))
	mux.HandleFunc("POST /user/tickets/reply", s.requireLogin(s.requireCSRF(s.handleUserTicketReply)))
	mux.HandleFunc("POST /user/tickets/close", s.requireLogin(s.requireCSRF(s.handleUserTicketClose)))
	mux.HandleFunc("GET /admin/tickets", s.requireAdmin(s.handleAdminTickets))
	mux.HandleFunc("GET /admin/tickets/{id}", s.requireAdmin(s.handleAdminTicketView))
	mux.HandleFunc("POST /admin/tickets/reply", s.requireAdmin(s.requireCSRF(s.handleAdminTicketReply)))
	mux.HandleFunc("POST /admin/tickets/close", s.requireAdmin(s.requireCSRF(s.handleAdminTicketClose)))
	mux.HandleFunc("GET /user/report/statement.csv", s.requireLogin(s.handleUserStatementExport))
	mux.HandleFunc("POST /admin/orders/refund", s.requireAdmin(s.requireCSRF(s.handleAdminOrderRefund)))
	mux.HandleFunc("GET /admin/orders/export.csv", s.requireAdmin(s.handleAdminOrdersExport))
	mux.HandleFunc("GET /admin/orders/{id}", s.requireAdmin(s.handleAdminOrderDetail))

	// 静态资源(返回 no-cache,保证样式更新即时生效)
	staticFS := http.FileServer(http.FS(webfs.Static()))
	mux.Handle("GET /static/", noCacheHeader(http.StripPrefix("/static/", staticFS)))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/img/favicon.svg", http.StatusFound)
	})

	// 运行时上传目录(收款码): 仅允许访问具体文件,禁用目录列表
	if s.DataDir != "" {
		fs := http.FileServer(http.Dir(s.DataDir))
		mux.Handle("GET /uploads/", http.StripPrefix("/uploads/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if p == "" || p == "/" || strings.HasSuffix(p, "/") {
				http.NotFound(w, r) // 禁用目录列表
				return
			}
			fs.ServeHTTP(w, r)
		})))
	}

	// 兜底 404
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			renderMsg(s, w, r, "404", "页面不存在: "+r.URL.Path, http.StatusNotFound)
			return
		}
		s.handleHome(w, r)
	})

	return securityHeaders(limitBody(payRateLimitWrap(s.logMiddleware(mux))))
}

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/static/") || strings.HasPrefix(r.URL.Path, "/uploads/") {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func noCacheHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// Init 初始化: 加载模板等
func (s *Server) Init() error {
	s.injectPayEmail()
	TrustProxyHeaders = s.TrustProxy
	if err := s.loadTemplates(); err != nil {
		return err
	}
	// 确保会话密钥存在
	s.sessionSecret()
	log.Printf("[init] 模板加载完成,共 %d 个页面", len(s.Tpl))
	return nil
}
