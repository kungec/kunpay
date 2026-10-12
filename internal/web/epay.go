package web

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/skip2/go-qrcode"

	"kunzhifu/internal/model"
	"kunzhifu/internal/service"
)

var allowTypes = map[string]bool{"alipay": true, "wxpay": true, "qqpay": true}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

func atoint64(s string) int64 {
	var v int64
	fmt.Sscanf(strings.TrimSpace(s), "%d", &v)
	return v
}

func atoint64Or(s string, def int64) int64 {
	var v int64
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &v); err != nil || v == 0 {
		return def
	}
	return v
}

func todayStart() int64 {
	t := time.Now()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Unix()
}

func (s *Server) absoluteURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}
	host := r.Host
	if host == "" {
		host = "localhost:" + s.Port
	}
	return scheme + "://" + host
}

// paramsOfForm 汇总 GET/POST 参数(易支付兼容两种方式;兼容 multipart 表单)
func paramsOfForm(r *http.Request) map[string]string {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		_ = r.ParseMultipartForm(32 << 20)
	} else {
		_ = r.ParseForm()
	}
	p := map[string]string{}
	for k := range r.PostForm {
		p[k] = r.PostForm.Get(k)
	}
	if r.MultipartForm != nil {
		for k := range r.MultipartForm.Value {
			if _, ok := p[k]; !ok {
				p[k] = r.MultipartForm.Value[k][0]
			}
		}
	}
	for k := range r.URL.Query() {
		if _, ok := p[k]; !ok {
			p[k] = r.URL.Query().Get(k)
		}
	}
	return p
}

// validateEpayRequest 校验公共参数与签名,返回 (merchant, req, errInfo)
func (s *Server) validateEpayRequest(p map[string]string) (*model.User, *service.OrderReq, string) {
	pidStr := p["pid"]
	if pidStr == "" {
		return nil, nil, "缺少商户ID(pid)"
	}
	pid := atoint64(pidStr)
	if pid <= 0 {
		return nil, nil, "商户ID格式错误"
	}
	merchant, err := s.Store.UserByPid(pid)
	if err != nil || merchant == nil {
		return nil, nil, "商户ID不存在"
	}
	if !service.VerifySign(p, merchant.Skey) {
		return nil, nil, "签名校验失败,请检查商户密钥与签名算法"
	}
	typ := p["type"]
	if !allowTypes[typ] {
		return nil, nil, "支付方式(type)不合法,仅支持 alipay/wxpay/qqpay"
	}
	req := &service.OrderReq{
		Type:       typ,
		OutTradeNo: strings.TrimSpace(p["out_trade_no"]),
		NotifyURL:  strings.TrimSpace(p["notify_url"]),
		ReturnURL:  strings.TrimSpace(p["return_url"]),
		Name:       strings.TrimSpace(p["name"]),
		Money:      strings.TrimSpace(p["money"]),
		Sitename:   strings.TrimSpace(p["sitename"]),
		Param:      strings.TrimSpace(p["param"]),
		Device:     strings.TrimSpace(p["device"]),
	}
	if req.OutTradeNo == "" {
		return nil, nil, "缺少商户订单号(out_trade_no)"
	}
	if req.Name == "" {
		return nil, nil, "缺少商品名称(name)"
	}
	if req.Money == "" {
		return nil, nil, "缺少商品金额(money)"
	}
	if req.Device == "" {
		req.Device = "pc"
	}
	return merchant, req, ""
}

func writeJSON(w http.ResponseWriter, out map[string]any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}

func renderMsg(s *Server, w http.ResponseWriter, r *http.Request, title, msg string, status int) {
	d := s.pageData(r)
	d["Page"] = "message"
	d["Title"] = title
	d["Msg"] = msg
	s.render(w, "pub_message", status, d)
}

// handleEpaySubmit GET/POST /submit.php —— 页面跳转支付,校验后 302 到收银台
func (s *Server) handleEpaySubmit(w http.ResponseWriter, r *http.Request) {
	p := paramsOfForm(r)
	merchant, req, errMsg := s.validateEpayRequest(p)
	if errMsg != "" {
		renderMsg(s, w, r, "支付失败", errMsg, http.StatusOK)
		return
	}
	o, err := s.Store.CreateOrder(merchant, req, clientIP(r))
	if err != nil {
		renderMsg(s, w, r, "下单失败", err.Error(), http.StatusOK)
		return
	}
	http.Redirect(w, r, "/pay/"+o.TradeNo, http.StatusFound)
}

// handleEpayMapi GET/POST /mapi.php —— 后端接口支付,返回 JSON
func (s *Server) handleEpayMapi(w http.ResponseWriter, r *http.Request) {
	p := paramsOfForm(r)
	out := map[string]any{}
	merchant, req, errMsg := s.validateEpayRequest(p)
	if errMsg != "" {
		out["code"] = -1
		out["msg"] = errMsg
		writeJSON(w, out)
		return
	}
	o, err := s.Store.CreateOrder(merchant, req, clientIP(r))
	if err != nil {
		out["code"] = -1
		out["msg"] = err.Error()
		writeJSON(w, out)
		return
	}
	payURL := s.absoluteURL(r) + "/pay/" + o.TradeNo
	img := ""
	if png, err := qrcode.Encode(payURL, qrcode.Medium, 300); err == nil {
		img = "data:image/png;base64," + b64(png)
	}
	out["code"] = 1
	out["msg"] = "成功"
	out["trade_no"] = o.TradeNo
	out["payurl"] = payURL
	out["qrcode"] = payURL
	out["img"] = img
	out["money"] = service.MoneyStr(o.PayMoney)
	writeJSON(w, out)
}

// handleEpayApi GET /api.php —— 商户信息/订单查询
func (s *Server) handleEpayApi(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	out := map[string]any{}
	act := r.FormValue("act")
	pid := atoint64(r.FormValue("pid"))
	key := r.FormValue("key")
	merchant, err := s.Store.UserByPid(pid)
	if err != nil || merchant == nil || key == "" || key != merchant.Skey {
		out["code"] = -1
		out["msg"] = "商户ID或密钥错误"
		writeJSON(w, out)
		return
	}
	switch act {
	case "query": // 查询商户信息
		var orders, today, yesterday int64
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE pid=?", pid).Scan(&orders)
		dayStart := todayStart()
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=?", pid, dayStart).Scan(&today)
		_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE pid=? AND status IN(1,3) AND addtime<? AND addtime>=?", pid, dayStart, dayStart-86400).Scan(&yesterday)
		out["code"] = 1
		out["pid"] = merchant.Pid
		out["key"] = merchant.Skey
		out["active"] = merchant.Status
		out["money"] = service.MoneyStr(merchant.Balance)
		out["type"] = merchant.SettleType
		out["account"] = merchant.SettleAccount
		out["username"] = merchant.SettleName
		out["orders"] = orders
		out["order_today"] = today
		out["order_lastday"] = yesterday
	case "order": // 查询单个订单
		tradeNo := r.FormValue("trade_no")
		outTradeNo := r.FormValue("out_trade_no")
		var o *model.Order
		if tradeNo != "" {
			o = s.Store.OrderByTradeNo(tradeNo)
		} else if outTradeNo != "" {
			o = s.Store.OrderByOutTradeNo(pid, outTradeNo)
		}
		if o == nil || o.Pid != pid {
			out["code"] = -1
			out["msg"] = "订单不存在"
			writeJSON(w, out)
			return
		}
		status := 0
		if o.Status == 1 || o.Status == 3 {
			status = 1
		}
		out["code"] = 1
		out["msg"] = "查询订单号成功！"
		out["trade_no"] = o.TradeNo
		out["out_trade_no"] = o.OutTradeNo
		out["api_trade_no"] = ""
		out["type"] = o.Type
		out["pid"] = o.Pid
		out["addtime"] = fdate(o.Addtime)
		out["endtime"] = fdate(o.Endtime)
		out["name"] = o.Name
		out["money"] = service.MoneyStr(o.Money)
		out["status"] = status
		out["param"] = o.Param
		out["buyer"] = o.Buyer
	case "orders": // 批量查询订单
		limit := atoint64Or(r.FormValue("limit"), 20)
		if limit <= 0 || limit > 50 {
			limit = 20
		}
		page := atoint64Or(r.FormValue("page"), 1)
		if page <= 0 {
			page = 1
		}
		offset := (page - 1) * limit
		rows, err := s.DB.Query(`SELECT trade_no,out_trade_no,type,pid,addtime,endtime,name,money,status,param
			FROM orders WHERE pid=? ORDER BY id DESC LIMIT ? OFFSET ?`, pid, limit, offset)
		if err != nil {
			out["code"] = -1
			out["msg"] = "查询失败"
			writeJSON(w, out)
			return
		}
		defer rows.Close()
		list := []map[string]any{}
		for rows.Next() {
			var tradeNo, outTradeNo, typ, name, param string
			var opid, money, status, addtime, endtime int64
			if err := rows.Scan(&tradeNo, &outTradeNo, &typ, &opid, &addtime, &endtime, &name, &money, &status, &param); err == nil {
				st := 0
				if status == 1 || status == 3 {
					st = 1
				}
				list = append(list, map[string]any{
					"trade_no": tradeNo, "out_trade_no": outTradeNo, "type": typ, "pid": opid,
					"addtime": fdate(addtime), "endtime": fdate(endtime), "name": name,
					"money": service.MoneyStr(money), "status": st, "param": param,
				})
			}
		}
		out["code"] = 1
		out["msg"] = "查询订单列表成功！"
		out["data"] = list
	default:
		out["code"] = -1
		out["msg"] = "不支持的 act"
	}
	writeJSON(w, out)
}

// resolvePayQR 解析收银台展示的收款码: 商户自有码优先(支持多码轮询),回落平台码。
// 选中的码 ID 会写回订单,用于收款码使用统计。
func (s *Server) resolvePayQR(o *model.Order) (qr string, source string) {
	if merchant, err := s.Store.UserByPid(o.Pid); err == nil && merchant != nil {
		if qr, itemID := s.Store.PickMerchantQR(o.ID, merchant.ID, o.Type); qr != "" {
			_, _ = s.DB.Exec("UPDATE orders SET m_item_id=? WHERE id=?", itemID, o.ID)
			if s.Store.MerchantRotate(merchant.ID, o.Type) {
				return qr, "merchant_rotate"
			}
			return qr, "merchant"
		}
	}
	if qr, itemID := s.Store.PickPlatformQR(o.ID, o.Type); qr != "" {
		_, _ = s.DB.Exec("UPDATE orders SET p_item_id=? WHERE id=?", itemID, o.ID)
		if s.Store.PlatformRotate(o.Type) {
			return qr, "platform_rotate"
		}
		return qr, "platform"
	}
	return "", ""
}

// handlePayCashier 收银台页面
func (s *Server) handlePayCashier(w http.ResponseWriter, r *http.Request) {
	tradeNo := r.PathValue("tradeNo")
	o := s.Store.OrderByTradeNo(tradeNo)
	if o == nil {
		renderMsg(s, w, r, "订单不存在", "支付订单不存在或已删除", http.StatusNotFound)
		return
	}
	qr, qrSource := s.resolvePayQR(o)

	// 移动端唤起: 生成各通道深链(支付宝配置UID后可自动带金额与备注)
	ch, _ := s.Store.ChannelByType(o.Type)
	ua := strings.ToLower(r.UserAgent())
	isMobile := strings.Contains(ua, "mobile") || o.Device == "mobile" ||
		o.Device == "qq" || o.Device == "wechat" || o.Device == "alipay"
	amount := service.MoneyStr(o.PayMoney)
	memo := o.TradeNo
	if len(memo) > 6 {
		memo = memo[len(memo)-6:]
	}
	var alipayScheme, wechatScheme, qqScheme string
	if isMobile && o.Status == 0 {
		if ch != nil && ch.AlipayUID != "" {
			alipayScheme = "alipays://platformapi/startapp?appId=09999999&actionType=toAccount&userId=" +
				url.QueryEscape(ch.AlipayUID) + "&amount=" + amount + "&memo=" + url.QueryEscape(memo)
		} else if o.Type == "alipay" {
			alipayScheme = "alipays://"
		}
		if o.Type == "wxpay" {
			wechatScheme = "weixin://"
		}
		if o.Type == "qqpay" {
			qqScheme = "mqq://"
		}
	}
	st, _ := s.DB.AllSettings()
	d := map[string]any{
		"Settings":     st,
		"Order":        o,
		"QrPath":       qr,
		"QrSource":     qrSource,
		"IsMobileUA":   isMobile,
		"AmountYuan":   amount,
		"AlipayScheme": template.URL(alipayScheme),
		"WechatScheme": template.URL(wechatScheme),
		"QqScheme":     template.URL(qqScheme),
		"SiteName":     st["site_name"],
		"IsPaid":       o.Status == 1 || o.Status == 3,
		"Expired":      o.Status == 2 || (o.Status == 0 && o.ExpireAt < model.Now()),
		"LeftSec":      o.ExpireAt - model.Now(),
		"PayURL":       s.absoluteURL(r) + "/pay/" + o.TradeNo,
	}
	d["Page"] = "cashier"
	s.render(w, "sa_cashier", http.StatusOK, d)
}

// handlePayCheck 收银台轮询: 订单状态 + 支付成功后返回带签名的跳转地址
func (s *Server) handlePayCheck(w http.ResponseWriter, r *http.Request) {
	tradeNo := r.PathValue("tradeNo")
	o := s.Store.OrderByTradeNo(tradeNo)
	out := map[string]any{}
	if o == nil {
		out["code"] = -1
		out["msg"] = "订单不存在"
		writeJSON(w, out)
		return
	}
	out["code"] = 1
	status := o.Status
	if o.Status == 3 {
		status = 1
	}
	out["status"] = status
	if o.Status == 1 || o.Status == 3 {
		out["return_url"] = s.Store.BuildReturnURL(o)
	}
	writeJSON(w, out)
}

// handlePayQR 收银台二维码图片: 商户自有码优先,回落平台收款码
func (s *Server) handlePayQR(w http.ResponseWriter, r *http.Request) {
	tradeNo := r.PathValue("tradeNo")
	o := s.Store.OrderByTradeNo(tradeNo)
	if o == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	if qr, _ := s.resolvePayQR(o); qr != "" {
		http.Redirect(w, r, qr, http.StatusFound)
		return
	}
	text := "通道未配置收款码,请联系平台管理员"
	data, err := qrcode.Encode(text, qrcode.Medium, 320)
	if err != nil {
		http.Error(w, "生成二维码失败", 500)
		return
	}
	w.Write(data)
}
