package web

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"kunzhifu/internal/db"
	"kunzhifu/internal/model"
	"kunzhifu/internal/service"
)

func (s *Server) handleAdminHome(w http.ResponseWriter, r *http.Request) {
	dayStart := todayStart()
	var todayOrders, todayOk, todayMoney, totalUsers, totalOrders, todayNewUsers int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE addtime>=?", dayStart).Scan(&todayOrders)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE status IN(1,3) AND addtime>=?", dayStart).Scan(&todayOk)
	_ = s.DB.QueryRow("SELECT COALESCE(SUM(money),0) FROM orders WHERE status IN(1,3) AND addtime>=?", dayStart).Scan(&todayMoney)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM users").Scan(&totalUsers)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM users WHERE created_at>=?", dayStart).Scan(&todayNewUsers)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders").Scan(&totalOrders)

	var days []StatDay
	for i := 6; i >= 0; i-- {
		start := dayStart - int64(i)*86400
		var c, m int64
		_ = s.DB.QueryRow("SELECT COUNT(*),COALESCE(SUM(money),0) FROM orders WHERE status IN(1,3) AND addtime>=? AND addtime<?",
			start, start+86400).Scan(&c, &m)
		days = append(days, StatDay{Date: timeFormat(start, "01-02"), Count: c, Money: m})
	}
	// 商户交易排行 Top5(近7日)
	type TopM struct {
		Pid      int64
		Username string
		Count    int64
		Money    int64
	}
	var top []TopM
	topRows, err := s.DB.Query(`SELECT o.pid, u.username, COUNT(*), COALESCE(SUM(o.money),0)
		FROM orders o JOIN users u ON u.pid=o.pid
		WHERE o.status IN(1,3) AND o.addtime>=?
		GROUP BY o.pid, u.username ORDER BY SUM(o.money) DESC LIMIT 5`, dayStart-6*86400)
	if err == nil {
		defer topRows.Close()
		for topRows.Next() {
			var t TopM
			if topRows.Scan(&t.Pid, &t.Username, &t.Count, &t.Money) == nil {
				top = append(top, t)
			}
		}
	}

	// 设备在线状态
	var devTotal, devOnline int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM devices").Scan(&devTotal)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM devices WHERE last_heartbeat>?", model.Now()-120).Scan(&devOnline)

	d := s.pageData(r)
	d["Page"] = "admin_home"
	d["Title"] = "平台概览"
	d["Stats"] = map[string]any{
		"TodayOrders": todayOrders, "TodayOk": todayOk, "TodayMoney": todayMoney,
		"TotalUsers": totalUsers, "TotalOrders": totalOrders, "TodayNewUsers": todayNewUsers,
		"DevTotal": devTotal, "DevOnline": devOnline,
	}
	d["Days"] = days
	d["TopMerchants"] = top
	s.render(w, "dash_admin_home", http.StatusOK, d)
}

func (s *Server) handleAdminTodo(w http.ResponseWriter, r *http.Request) {
	var pendingSettles, pendingOrders, unmatched, devOffline int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM settle_logs WHERE status=0").Scan(&pendingSettles)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE status=0 AND expire_at>?", model.Now()).Scan(&pendingOrders)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM pay_logs WHERE order_id=0 AND created_at>?", model.Now()-86400).Scan(&unmatched)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM devices WHERE status=1 AND last_heartbeat<=?", model.Now()-120).Scan(&devOffline)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{
		"pending_settles": pendingSettles,
		"pending_orders":  pendingOrders,
		"unmatched":       unmatched,
		"dev_offline":     devOffline,
	})
}

func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	page := atoint64Or(r.FormValue("p"), 1)
	if page < 1 {
		page = 1
	}
	const pageSize = 15
	kw := strings.TrimSpace(r.FormValue("kw"))
	where := "1=1"
	var args []any
	if kw != "" {
		where = "(username LIKE ? OR email LIKE ? OR CAST(pid AS TEXT)=?)"
		args = append(args, "%"+kw+"%", "%"+kw+"%", kw)
	}
	var total int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM users WHERE "+where, args...).Scan(&total)
	rows, err := s.DB.Query(`SELECT id,username,email,qq,role,status,rate,balance,pid,skey,created_at,last_login_at
		FROM users WHERE `+where+` ORDER BY id ASC LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	var users []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, balance, pid, createdAt, lastLogin int64
			var username, email, qq, role, skey string
			var status int
			var rate float64
			if rows.Scan(&id, &username, &email, &qq, &role, &status, &rate, &balance, &pid, &skey, &createdAt, &lastLogin) == nil {
				users = append(users, map[string]any{
					"ID": id, "Username": username, "Email": email, "QQ": qq, "Role": role,
					"Status": status, "Rate": rate, "Balance": balance, "Pid": pid, "Skey": skey,
					"CreatedAt": createdAt, "LastLogin": lastLogin,
				})
			}
		}
	}
	totalPages := (total + pageSize - 1) / pageSize
	d := s.pageData(r)
	d["Page"] = "admin_users"
	d["Title"] = "用户管理"
	d["Users"] = users
	d["Total"] = total
	d["PageNo"] = page
	d["TotalPages"] = totalPages
	d["Kw"] = kw
	d["Flash"] = r.URL.Query().Get("msg")
	s.render(w, "dash_admin_users", http.StatusOK, d)
}

func (s *Server) handleAdminUsersSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	id := atoint64(r.FormValue("id"))
	if id <= 0 {
		http.Redirect(w, r, "/admin/users", http.StatusFound)
		return
	}
	u, _ := s.Store.UserByID(id)
	if u == nil {
		http.Redirect(w, r, "/admin/users?msg="+urlEscape("用户不存在"), http.StatusFound)
		return
	}
	action := r.FormValue("action")
	msg := ""
	switch action {
	case "toggle":
		newStatus := 1
		if u.Status == 1 {
			newStatus = 0
		}
		_, _ = s.DB.Exec("UPDATE users SET status=? WHERE id=?", newStatus, id)
		if newStatus == 0 {
			msg = "已封禁 " + u.Username
		} else {
			msg = "已解封 " + u.Username
		}
	case "resetkey":
		_, _ = s.DB.Exec("UPDATE users SET skey=? WHERE id=?", db.RandKey(32), id)
		msg = "已重置 " + u.Username + " 的商户密钥"
	case "rate":
		rate, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue("rate")), 64)
		if rate < 0 || rate > 100 {
			rate = 0
		}
		_, _ = s.DB.Exec("UPDATE users SET rate=? WHERE id=?", rate, id)
		msg = fmt.Sprintf("已将 %s 费率设为 %.2f%%", u.Username, rate)
	case "balance":
		amount, err := service.ParseMoney(r.FormValue("amount"))
		if err != nil || amount == 0 {
			http.Redirect(w, r, "/admin/users?msg="+urlEscape("金额格式错误"), http.StatusFound)
			return
		}
		res, err := s.DB.Exec("UPDATE users SET balance=balance+? WHERE id=?", amount, id)
		if err != nil || rowsAffected(res) == 0 {
			http.Redirect(w, r, "/admin/users?msg="+urlEscape("调整失败"), http.StatusFound)
			return
		}
		var balance int64
		_ = s.DB.QueryRow("SELECT balance FROM users WHERE id=?", id).Scan(&balance)
		_, _ = s.DB.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
			VALUES(?,?,?,?,?,?,?)`,
			id, "adjust", amount, balance-amount, balance, "管理员调整余额", model.Now())
		msg = "已调整余额"
	case "password":
		pw := r.FormValue("password")
		if len(pw) < 6 {
			http.Redirect(w, r, "/admin/users?msg="+urlEscape("密码长度至少 6 位"), http.StatusFound)
			return
		}
		_, _ = s.DB.Exec("UPDATE users SET password_hash=? WHERE id=?", hashPassword(pw), id)
		msg = "已重置密码"
	}
	s.audit(r, "user."+action, u.Username+": "+msg)
	http.Redirect(w, r, "/admin/users?msg="+urlEscape(msg), http.StatusFound)
}

func (s *Server) handleAdminOrders(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	page := atoint64Or(r.FormValue("p"), 1)
	if page < 1 {
		page = 1
	}
	const pageSize = 20
	kw := strings.TrimSpace(r.FormValue("kw"))
	status := strings.TrimSpace(r.FormValue("status"))
	typ := strings.TrimSpace(r.FormValue("type"))
	dateFrom := strings.TrimSpace(r.FormValue("date_from"))
	dateTo := strings.TrimSpace(r.FormValue("date_to"))

	where := "1=1"
	var args []any
	if kw != "" {
		where += " AND (trade_no=? OR out_trade_no=? OR CAST(pid AS TEXT)=?)"
		args = append(args, kw, kw, kw)
	}
	if status != "" {
		if st, err := strconv.Atoi(status); err == nil && st >= 0 && st <= 3 {
			where += " AND status=?"
			args = append(args, st)
		}
	}
	if typ != "" && allowTypes[typ] {
		where += " AND type=?"
		args = append(args, typ)
	}
	if t, err := time.ParseInLocation("2006-01-02", dateFrom, time.Local); err == nil {
		where += " AND addtime>=?"
		args = append(args, t.Unix())
	}
	if t, err := time.ParseInLocation("2006-01-02", dateTo, time.Local); err == nil {
		where += " AND addtime<?"
		args = append(args, t.Unix()+86400)
	}
	var total int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE "+where, args...).Scan(&total)
	rows, err := s.DB.Query(`SELECT id,trade_no,out_trade_no,pid,type,name,money,pay_money,status,notify_ok,addtime,endtime
		FROM orders WHERE `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, pageSize, (page-1)*pageSize)...)
	var orders []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, pid, money, payMoney, addtime, endtime int64
			var tradeNo, outTradeNo, typ2, name string
			var status2, notifyOK int
			if rows.Scan(&id, &tradeNo, &outTradeNo, &pid, &typ2, &name, &money, &payMoney, &status2, &notifyOK, &addtime, &endtime) == nil {
				orders = append(orders, map[string]any{
					"ID": id, "TradeNo": tradeNo, "OutTradeNo": outTradeNo, "Pid": pid,
					"Type": typ2, "TypeName": typeName(typ2), "Name": name, "Money": money,
					"PayMoney": payMoney, "Status": status2, "NotifyOK": notifyOK,
					"Addtime": addtime, "Endtime": endtime,
				})
			}
		}
	}
	totalPages := (total + pageSize - 1) / pageSize
	d := s.pageData(r)
	d["Page"] = "admin_orders"
	d["Title"] = "订单管理"
	d["Orders"] = orders
	d["Total"] = total
	d["PageNo"] = page
	d["TotalPages"] = totalPages
	d["Kw"] = kw
	d["QStatus"] = status
	d["QType"] = typ
	d["DateFrom"] = dateFrom
	d["DateTo"] = dateTo
	d["Flash"] = r.URL.Query().Get("msg")
	s.render(w, "dash_admin_orders", http.StatusOK, d)
}

// handleAdminOrdersRepair 手动补单: 将订单标记成功并发起通知
func (s *Server) handleAdminOrdersRepair(w http.ResponseWriter, r *http.Request) {
	id := atoint64(r.FormValue("id"))
	o := s.Store.OrderByID(id)
	if o == nil {
		http.Redirect(w, r, "/admin/orders?msg="+urlEscape("订单不存在"), http.StatusFound)
		return
	}
	if o.Status == 1 || o.Status == 3 {
		http.Redirect(w, r, "/admin/orders?msg="+urlEscape("订单已是成功状态"), http.StatusFound)
		return
	}
	if err := s.Store.CompleteOrder(o, "管理员补单"); err != nil {
		http.Redirect(w, r, "/admin/orders?msg="+urlEscape("补单失败: "+err.Error()), http.StatusFound)
		return
	}
	_, _ = s.DB.Exec("UPDATE orders SET status=3 WHERE id=?", o.ID) // 标记为手动补单
	s.audit(r, "order.repair", o.TradeNo)
	http.Redirect(w, r, "/admin/orders?msg="+urlEscape("补单成功,已入账并发起异步通知"), http.StatusFound)
}

// audit 记录管理员敏感操作
func (s *Server) audit(r *http.Request, action, detail string) {
	u := s.currentUser(r)
	if u == nil {
		return
	}
	_, _ = s.DB.Exec(`INSERT INTO audit_logs(admin_id,admin,action,detail,ip,created_at) VALUES(?,?,?,?,?,?)`,
		u.ID, u.Username, action, detail, clientIP(r), model.Now())
}

func (s *Server) handleAdminChannels(w http.ResponseWriter, r *http.Request) {
	chs, err := s.Store.AllChannels()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	type platItemView struct {
		ID      int64
		Name    string
		Qrcode  string
		Enabled bool
		Count   int64
		Amount  int64
	}
	type chanView struct {
		model.Channel
		Items     []platItemView
		EnabledN  int
		Rotate    bool
		CanRotate bool
	}
	var views []chanView
	for _, c := range chs {
		v := chanView{Channel: c}
		items, _ := s.Store.PlatformItems(c.Type)
		for _, it := range items {
			if it.Enabled == 1 {
				v.EnabledN++
			}
			var cnt, amt int64
			_ = s.DB.QueryRow("SELECT COUNT(*),COALESCE(SUM(money-fee),0) FROM orders WHERE p_item_id=? AND status IN(1,3)", it.ID).
				Scan(&cnt, &amt)
			v.Items = append(v.Items, platItemView{ID: it.ID, Name: it.Name, Qrcode: it.Qrcode, Enabled: it.Enabled == 1, Count: cnt, Amount: amt})
		}
		v.Rotate = s.Store.PlatformRotate(c.Type)
		v.CanRotate = v.EnabledN >= 2
		views = append(views, v)
	}
	d := s.pageData(r)
	d["Page"] = "admin_channels"
	d["Title"] = "渠道管理"
	d["Views"] = views
	d["Flash"] = r.URL.Query().Get("msg")
	d["FlashErr"] = r.URL.Query().Get("err")
	s.render(w, "dash_admin_channels", http.StatusOK, d)
}

func (s *Server) handleAdminChannelsSave(w http.ResponseWriter, r *http.Request) {
	typ := r.FormValue("type")
	ch, err := s.Store.ChannelByType(typ)
	if err != nil || ch == nil {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("渠道不存在"), http.StatusFound)
		return
	}
	enabled := 0
	if r.FormValue("enabled") == "on" || r.FormValue("enabled") == "1" {
		enabledVal := 1
		_ = enabledVal
		enabled = 1
	}
	minMoney, _ := service.ParseMoney(r.FormValue("min_money"))
	maxMoney, _ := service.ParseMoney(r.FormValue("max_money"))
	if minMoney <= 0 {
		minMoney = 1
	}
	if maxMoney < minMoney {
		maxMoney = 500000
	}
	account := strings.TrimSpace(r.FormValue("account"))
	deviceID := atoint64(r.FormValue("device_id"))
	_, _ = s.DB.Exec(`UPDATE channels SET enabled=?, min_money=?, max_money=?, account=?, device_id=?, alipay_uid=? WHERE type=?`,
		enabled, minMoney, maxMoney, account, deviceID, strings.TrimSpace(r.FormValue("alipay_uid")), typ)
	http.Redirect(w, r, "/admin/channels?msg="+urlEscape("渠道设置已保存"), http.StatusFound)
}

// handleAdminChannelsItemAdd 添加平台收款码(同类型可多个,支持轮询)
func (s *Server) handleAdminChannelsItemAdd(w http.ResponseWriter, r *http.Request) {
	typ := r.FormValue("type")
	ch, err := s.Store.ChannelByType(typ)
	if err != nil || ch == nil {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("渠道不存在"), http.StatusFound)
		return
	}
	file, header, err := r.FormFile("qrcode")
	if err != nil {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("请选择图片文件"), http.StatusFound)
		return
	}
	defer file.Close()
	if header.Size > 4*1024*1024 {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("图片不能超过 4MB"), http.StatusFound)
		return
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".gif" && ext != ".webp" {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("仅支持 png/jpg/gif/webp 图片"), http.StatusFound)
		return
	}
	if !validImageMagic(file, ext) {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("图片内容与格式不符"), http.StatusFound)
		return
	}
	_, _ = file.Seek(0, 0)
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "收款码" + strconv.Itoa(int(model.Now())%1000)
	}
	fn := "qr_" + typ + "_" + db.RandKey(6) + ext
	dst, err := os.Create(filepath.Join(s.DataDir, fn))
	if err != nil {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("保存文件失败"), http.StatusFound)
		return
	}
	defer dst.Close()
	if _, err := dst.ReadFrom(file); err != nil {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("写入文件失败"), http.StatusFound)
		return
	}
	if err := s.Store.AddPlatformItem(typ, name, "/uploads/"+fn); err != nil {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("保存失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/channels?msg="+urlEscape("收款码已添加"), http.StatusFound)
}

func (s *Server) handleAdminChannelsItemToggle(w http.ResponseWriter, r *http.Request) {
	id := atoint64(r.FormValue("id"))
	enabled := 0
	if r.FormValue("enabled") == "1" {
		enabled = 1
	}
	if err := s.Store.SetPlatformItemState(id, enabled); err != nil {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape("操作失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/channels?msg="+urlEscape("收款码状态已更新"), http.StatusFound)
}

func (s *Server) handleAdminChannelsItemDel(w http.ResponseWriter, r *http.Request) {
	_ = s.Store.DeletePlatformItem(atoint64(r.FormValue("id")))
	http.Redirect(w, r, "/admin/channels?msg="+urlEscape("收款码已删除"), http.StatusFound)
}

func (s *Server) handleAdminChannelsRotate(w http.ResponseWriter, r *http.Request) {
	typ := r.FormValue("type")
	on := r.FormValue("rotate") == "1"
	if err := s.Store.SetPlatformRotate(typ, on); err != nil {
		http.Redirect(w, r, "/admin/channels?err="+urlEscape(err.Error()), http.StatusFound)
		return
	}
	if on {
		s.audit(r, "channel.rotate", typ+" 开启")
		http.Redirect(w, r, "/admin/channels?msg="+urlEscape("平台轮询已开启,订单将轮流使用各收款码"), http.StatusFound)
		return
	}
	s.audit(r, "channel.rotate", typ+" 关闭")
	http.Redirect(w, r, "/admin/channels?msg="+urlEscape("平台轮询已关闭"), http.StatusFound)
}

// handleAdminUserChannels 管理员只读查看某商户绑定的自有收款码
func (s *Server) handleAdminUserChannels(w http.ResponseWriter, r *http.Request) {
	uid := atoint64(r.PathValue("id"))
	u, _ := s.Store.UserByID(uid)
	if u == nil {
		http.Redirect(w, r, "/admin/users?msg="+urlEscape("用户不存在"), http.StatusFound)
		return
	}
	type itemView struct {
		ID       int64
		Type     string
		TypeName string
		Name     string
		Qrcode   string
		Enabled  bool
	}
	var items []itemView
	rows, err := s.DB.Query(`SELECT id,type,name,qrcode,enabled FROM merchant_channel_items WHERE user_id=? ORDER BY type,id`, uid)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id int64
			var typ, name, qrcode string
			var enabled int
			if rows.Scan(&id, &typ, &name, &qrcode, &enabled) == nil {
				items = append(items, itemView{ID: id, Type: typ, TypeName: typeName(typ), Name: name, Qrcode: qrcode, Enabled: enabled == 1})
			}
		}
	}
	rotateRows, _ := s.DB.Query("SELECT type FROM merchant_channels WHERE user_id=? AND rotate=1", uid)
	var rotating []string
	if rotateRows != nil {
		defer rotateRows.Close()
		for rotateRows.Next() {
			var typ string
			if rotateRows.Scan(&typ) == nil {
				rotating = append(rotating, typeName(typ))
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "admin_userchannels"
	d["Title"] = "自有通道 - " + u.Username
	d["Merchant"] = u
	d["Items"] = items
	d["Rotating"] = rotating
	s.render(w, "dash_admin_userchannels", http.StatusOK, d)
}

// exportOrdersCSV 导出订单 CSV(带 BOM,Excel 友好)
func (s *Server) exportOrdersCSV(w http.ResponseWriter, where string, args []any, filename string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	bw.WriteString("平台订单号,商户订单号,商户PID,支付方式,商品名称,金额,实付金额,状态,通知,创建时间,完成时间\n")
	rows, err := s.DB.Query(`SELECT trade_no,out_trade_no,pid,type,name,money,pay_money,status,notify_ok,addtime,endtime
		FROM orders WHERE `+where+` ORDER BY id DESC LIMIT 5000`, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tradeNo, outTradeNo, typ, name string
		var pid, money, payMoney, addtime, endtime int64
		var status, notifyOK int
		if rows.Scan(&tradeNo, &outTradeNo, &pid, &typ, &name, &money, &payMoney, &status, &notifyOK, &addtime, &endtime) != nil {
			continue
		}
		st := "待支付"
		if status == 1 {
			st = "支付成功"
		} else if status == 3 {
			st = "手动补单"
		} else if status == 2 {
			st = "已过期"
		}
		nt := "—"
		if status == 1 || status == 3 {
			if notifyOK == 1 {
				nt = "成功"
			} else {
				nt = "重试中"
			}
		}
		rec := []string{
			tradeNo, outTradeNo, strconv.FormatInt(pid, 10), typeName(typ), name,
			yuan(money), yuan(payMoney), st, nt, fdate(addtime), fdate(endtime),
		}
		for i, f := range rec {
			if i > 0 {
				bw.WriteString(",")
			}
			bw.WriteString(csvCell(f))
		}
		bw.WriteString("\n")
	}
}

func (s *Server) handleUserOrdersExport(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	s.exportOrdersCSV(w, "pid=?", []any{u.Pid},
		"orders_"+u.Username+"_"+timeFormat(model.Now(), "20060102")+".csv")
}

func (s *Server) handleAdminOrdersExport(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	where := "1=1"
	var args []any
	kw := strings.TrimSpace(r.FormValue("kw"))
	if kw != "" {
		where += " AND (trade_no=? OR out_trade_no=? OR CAST(pid AS TEXT)=?)"
		args = append(args, kw, kw, kw)
	}
	status := strings.TrimSpace(r.FormValue("status"))
	if status != "" {
		if st, err := strconv.Atoi(status); err == nil && st >= 0 && st <= 3 {
			where += " AND status=?"
			args = append(args, st)
		}
	}
	typ := strings.TrimSpace(r.FormValue("type"))
	if typ != "" && allowTypes[typ] {
		where += " AND type=?"
		args = append(args, typ)
	}
	if t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(r.FormValue("date_from")), time.Local); err == nil {
		where += " AND addtime>=?"
		args = append(args, t.Unix())
	}
	if t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(r.FormValue("date_to")), time.Local); err == nil {
		where += " AND addtime<?"
		args = append(args, t.Unix()+86400)
	}
	s.exportOrdersCSV(w, where, args, "orders_all_"+timeFormat(model.Now(), "20060102")+".csv")
}

func (s *Server) handleAdminDevices(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT id,name,token,last_heartbeat,last_ip,status,created_at FROM devices ORDER BY id DESC`)
	var devices []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, lastHb, status, createdAt int64
			var name, token, lastIP string
			if rows.Scan(&id, &name, &token, &lastHb, &lastIP, &status, &createdAt) == nil {
				online := lastHb > model.Now()-120
				devices = append(devices, map[string]any{
					"ID": id, "Name": name, "Token": token, "LastHb": lastHb,
					"LastIP": lastIP, "Status": status, "CreatedAt": createdAt, "Online": online,
				})
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "admin_devices"
	d["Title"] = "监控设备"
	d["Devices"] = devices
	d["Flash"] = r.URL.Query().Get("msg")
	s.render(w, "dash_admin_devices", http.StatusOK, d)
}

func (s *Server) handleAdminDevicesAdd(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "监控设备-" + db.RandDigits(3)
	}
	_, err := s.DB.Exec("INSERT INTO devices(name,token,status,created_at) VALUES(?,?,1,?)",
		name, db.RandKey(24), model.Now())
	if err != nil {
		http.Redirect(w, r, "/admin/devices?msg="+urlEscape("添加失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/devices?msg="+urlEscape("设备已添加,请将 Token 配置到监控APP"), http.StatusFound)
}

func (s *Server) handleAdminDevicesToggle(w http.ResponseWriter, r *http.Request) {
	id := atoint64(r.FormValue("id"))
	var status int
	_ = s.DB.QueryRow("SELECT status FROM devices WHERE id=?", id).Scan(&status)
	newStatus := 1
	if status == 1 {
		newStatus = 0
	}
	_, _ = s.DB.Exec("UPDATE devices SET status=? WHERE id=?", newStatus, id)
	http.Redirect(w, r, "/admin/devices?msg="+urlEscape("设备状态已更新"), http.StatusFound)
}

func (s *Server) handleAdminDevicesDel(w http.ResponseWriter, r *http.Request) {
	id := atoint64(r.FormValue("id"))
	_, _ = s.DB.Exec("DELETE FROM devices WHERE id=?", id)
	s.audit(r, "device.del", strconv.FormatInt(id, 10))
	http.Redirect(w, r, "/admin/devices?msg="+urlEscape("设备已删除"), http.StatusFound)
}

func (s *Server) handleAdminSettles(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	status := strings.TrimSpace(r.FormValue("status"))
	where := "1=1"
	var args []any
	if status != "" {
		if st, err := strconv.Atoi(status); err == nil && st >= 0 && st <= 2 {
			where = "sl.status=?"
			args = append(args, st)
		}
	}
	rows, err := s.DB.Query(`SELECT sl.id,sl.user_id,sl.amount,sl.settle_type,sl.account,sl.name,sl.status,sl.note,sl.created_at,sl.done_at,u.username
		FROM settle_logs sl LEFT JOIN users u ON u.id=sl.user_id WHERE `+where+` ORDER BY sl.id DESC LIMIT 100`, args...)
	var settles []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, uid, amount, settleType, createdAt, doneAt int64
			var account, name, note, username string
			var st int
			if rows.Scan(&id, &uid, &amount, &settleType, &account, &name, &st, &note, &createdAt, &doneAt, &username) == nil {
				settles = append(settles, map[string]any{
					"ID": id, "UserID": uid, "Username": username, "Amount": amount,
					"SettleType": settleType, "Account": account, "Name": name,
					"Status": st, "Note": note, "CreatedAt": createdAt, "DoneAt": doneAt,
				})
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "admin_settles"
	d["Title"] = "结算审核"
	d["Settles"] = settles
	d["QStatus"] = status
	d["Flash"] = r.URL.Query().Get("msg")
	s.render(w, "dash_admin_settles", http.StatusOK, d)
}

func (s *Server) handleAdminSettlesDo(w http.ResponseWriter, r *http.Request) {
	id := atoint64(r.FormValue("id"))
	action := r.FormValue("action")
	var slID, uid, amount int64
	var st int
	err := s.DB.QueryRow("SELECT id,user_id,amount,status FROM settle_logs WHERE id=?", id).Scan(&slID, &uid, &amount, &st)
	if err != nil {
		http.Redirect(w, r, "/admin/settles?msg="+urlEscape("记录不存在"), http.StatusFound)
		return
	}
	if st != 0 {
		http.Redirect(w, r, "/admin/settles?msg="+urlEscape("该申请已处理过"), http.StatusFound)
		return
	}
	if action == "pay" {
		_, _ = s.DB.Exec("UPDATE settle_logs SET status=1, done_at=? WHERE id=?", model.Now(), id)
		s.audit(r, "settle.pay", fmt.Sprintf("#%d ¥%s", id, service.MoneyStr(amount)))
		if m, _ := s.Store.UserByID(uid); m != nil {
			s.notifyMerchantSettle(m.Email, m.Username, true, amount, "")
		}
		http.Redirect(w, r, "/admin/settles?msg="+urlEscape("已标记打款"), http.StatusFound)
		return
	}
	if action == "reject" {
		_, _ = s.DB.Exec("UPDATE settle_logs SET status=2, note=?, done_at=? WHERE id=?",
			strings.TrimSpace(r.FormValue("note")), model.Now(), id)
		res, _ := s.DB.Exec("UPDATE users SET balance=balance+? WHERE id=?", amount, uid)
		if rowsAffected(res) > 0 {
			var balance int64
			_ = s.DB.QueryRow("SELECT balance FROM users WHERE id=?", uid).Scan(&balance)
			_, _ = s.DB.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
				VALUES(?,?,?,?,?,?,?)`,
				uid, "reject", amount, balance-amount, balance, "提现被拒绝,退还余额", model.Now())
		}
		s.audit(r, "settle.reject", fmt.Sprintf("#%d ¥%s", id, service.MoneyStr(amount)))
		note := strings.TrimSpace(r.FormValue("note"))
		if m, _ := s.Store.UserByID(uid); m != nil {
			s.notifyMerchantSettle(m.Email, m.Username, false, amount, note)
		}
		http.Redirect(w, r, "/admin/settles?msg="+urlEscape("已拒绝并退款"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/admin/settles", http.StatusFound)
}

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	st, _ := s.DB.AllSettings()
	d := s.pageData(r)
	d["Page"] = "admin_settings"
	d["Title"] = "系统设置"
	d["S"] = st
	d["Backups"] = s.backupList()
	d["Flash"] = r.URL.Query().Get("msg")
	// 系统信息
	var pageCount, pageSize int64
	_ = s.DB.QueryRow("PRAGMA page_count").Scan(&pageCount)
	_ = s.DB.QueryRow("PRAGMA page_size").Scan(&pageSize)
	d["DBSize"] = pageCount * pageSize
	d["UptimeH"] = int(time.Since(s.StartedAt).Hours())
	d["GoVersion"] = runtime.Version()
	s.render(w, "dash_admin_settings", http.StatusOK, d)
}

func (s *Server) handleAdminSettingsSave(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	keys := []string{"site_name", "site_title", "announcement", "kefu", "fee_rate", "order_timeout",
		"money_offset", "register_open", "min_settle", "pay_notice", "invite_open", "footer_text", "poll_interval",
		"site_url", "smtp_host", "smtp_port", "smtp_user", "smtp_pass", "smtp_tls", "smtp_from",
		"alert_email", "device_alert", "merchant_notice"}
	for _, k := range keys {
		v := strings.TrimSpace(r.FormValue(k))
		if r.Form[k] == nil {
			continue
		}
		if k == "money_offset" || k == "register_open" || k == "invite_open" {
			if v == "on" || v == "1" {
				v = "1"
			} else {
				v = "0"
			}
		}
		_ = s.DB.SetSetting(k, v)
	}
	s.audit(r, "settings.save", "更新系统设置")
	http.Redirect(w, r, "/admin/settings?msg="+urlEscape("设置已保存"), http.StatusFound)
}

func (s *Server) handleAdminPayLogs(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT p.id,p.device_id,p.type,p.money,p.content,p.order_id,p.created_at,d.name
		FROM pay_logs p LEFT JOIN devices d ON d.id=p.device_id ORDER BY p.id DESC LIMIT 100`)
	var logs []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, deviceID, money, orderID, createdAt int64
			var typ, content, devName string
			if rows.Scan(&id, &deviceID, &typ, &money, &content, &orderID, &createdAt, &devName) == nil {
				logs = append(logs, map[string]any{
					"ID": id, "DeviceID": deviceID, "DeviceName": devName, "Type": typ,
					"TypeName": typeName(typ), "Money": money, "Content": content,
					"OrderID": orderID, "CreatedAt": createdAt,
				})
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "admin_paylogs"
	d["Title"] = "到账流水"
	d["Logs"] = logs
	s.render(w, "dash_admin_paylogs", http.StatusOK, d)
}

// handleAdminPayLogMatch 手动把一条未匹配的到账流水匹配到指定订单并发起通知
// handleAdminMailTest 发送测试邮件(收件人取表单)
func (s *Server) handleAdminMailTest(w http.ResponseWriter, r *http.Request) {
	to := strings.TrimSpace(r.FormValue("to"))
	if to == "" {
		http.Redirect(w, r, "/admin/settings?msg="+urlEscape("请填写测试收件邮箱"), http.StatusFound)
		return
	}
	if err := s.SendMail(to, "KunPay 邮件测试",
		mailHTML("SMTP 配置测试", []string{
			"这是一封测试邮件。",
			"收到本邮件说明平台邮件服务配置正确,结算提现结果将同步通知商户。",
			"时间: " + fdate(model.Now()),
		})); err != nil {
		http.Redirect(w, r, "/admin/settings?msg="+urlEscape("测试邮件发送失败: "+err.Error()), http.StatusFound)
		return
	}
	s.audit(r, "mail.test", to)
	http.Redirect(w, r, "/admin/settings?msg="+urlEscape("测试邮件已发送至 "+to+",请查收"), http.StatusFound)
}

// notifyMerchantSettle 结算结果邮件通知(异步,SMTP 未配置时静默跳过)
func (s *Server) notifyMerchantSettle(to, merchant string, approve bool, amount int64, note string) {
	if to == "" {
		return
	}
	conf := s.smtpConf()
	if !conf.ready() {
		return
	}
	amountStr := service.MoneyStr(amount)
	title := "提现已打款"
	body := []string{
		"商户: " + merchant,
		"提现金额: ¥" + amountStr,
		"状态: <b style='color:#1fa56f'>已完成打款</b>,请留意查收。",
	}
	if !approve {
		title = "提现申请被拒绝"
		body = []string{
			"商户: " + merchant,
			"提现金额: ¥" + amountStr,
			"状态: <b style='color:#e5484d'>已拒绝</b>,金额已退回商户余额。",
		}
		if note != "" {
			body = append(body, "备注: "+note)
		}
	}
	go func() {
		_ = s.SendMail(to, "【"+s.DB.MustSetting("site_name")+"】"+title, mailHTML(title, body))
	}()
}

func (s *Server) handleAdminPayLogMatch(w http.ResponseWriter, r *http.Request) {
	id := atoint64(r.FormValue("id"))
	tradeNo := strings.TrimSpace(r.FormValue("trade_no"))
	var logID, logMoney int64
	var logType string
	err := s.DB.QueryRow("SELECT id,money,type FROM pay_logs WHERE id=?", id).Scan(&logID, &logMoney, &logType)
	if err != nil {
		http.Redirect(w, r, "/admin/paylogs?msg="+urlEscape("流水不存在"), http.StatusFound)
		return
	}
	o := s.Store.OrderByTradeNo(tradeNo)
	if o == nil {
		http.Redirect(w, r, "/admin/paylogs?msg="+urlEscape("订单不存在: "+tradeNo), http.StatusFound)
		return
	}
	if o.Status == 1 || o.Status == 3 {
		http.Redirect(w, r, "/admin/paylogs?msg="+urlEscape("该订单已是成功状态"), http.StatusFound)
		return
	}
	if o.PayMoney != logMoney {
		http.Redirect(w, r, "/admin/paylogs?msg="+urlEscape(
			fmt.Sprintf("金额不一致: 流水¥%s vs 订单应付¥%s", service.MoneyStr(logMoney), service.MoneyStr(o.PayMoney))), http.StatusFound)
		return
	}
	if err := s.Store.CompleteOrder(o, "手动匹配"); err != nil {
		http.Redirect(w, r, "/admin/paylogs?msg="+urlEscape("入账失败: "+err.Error()), http.StatusFound)
		return
	}
	_, _ = s.DB.Exec("UPDATE pay_logs SET order_id=? WHERE id=?", o.ID, logID)
	s.audit(r, "paylog.match", o.TradeNo+" ¥"+service.MoneyStr(logMoney))
	http.Redirect(w, r, "/admin/paylogs?msg="+urlEscape("已匹配订单 "+o.TradeNo+" 并入账通知"), http.StatusFound)
}

// handleAdminOrderNotify 手动重发某订单的异步通知
func (s *Server) handleAdminOrderNotify(w http.ResponseWriter, r *http.Request) {
	id := atoint64(r.FormValue("id"))
	o := s.Store.OrderByID(id)
	if o == nil || (o.Status != 1 && o.Status != 3) {
		http.Redirect(w, r, "/admin/orders?msg="+urlEscape("订单不存在或未支付"), http.StatusFound)
		return
	}
	if o.NotifyURL == "" {
		http.Redirect(w, r, "/admin/orders?msg="+urlEscape("该订单没有异步通知地址"), http.StatusFound)
		return
	}
	_, _ = s.DB.Exec("UPDATE orders SET notify_count=0, next_notify_at=? WHERE id=?", model.Now(), o.ID)
	s.Store.TriggerNotify(o.ID)
	s.audit(r, "order.notify", o.TradeNo)
	http.Redirect(w, r, "/admin/orders?msg="+urlEscape("已重新发起异步通知"), http.StatusFound)
}

// lastOrderID 跟踪最新订单 ID(用于到账实时推送)
// handleOrderPoll 到账实时轮询: 管理后台/商户仪表盘每 5 秒检查一次新支付订单
// 参数 after=上次检查的最大订单ID,返回比它更新的已完成订单
func (s *Server) handleOrderPoll(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u == nil {
		writeJSON(w, map[string]any{"code": -1})
		return
	}
	after := atoint64(r.FormValue("after"))
	var pidFilter string
	if u.Role != "admin" {
		pidFilter = " AND pid=" + strconv.FormatInt(u.Pid, 10)
	}
	var id, money int64
	var tradeNo, buyer string
	err := s.DB.QueryRow(`SELECT id,trade_no,COALESCE(buyer,''),money FROM orders
		WHERE status IN(1,3)`+pidFilter+` AND id>? ORDER BY id DESC LIMIT 1`, after).
		Scan(&id, &tradeNo, &buyer, &money)
	out := map[string]any{"code": 0, "max_id": after}
	if err == nil {
		out["code"] = 1
		out["id"] = id
		out["trade_no"] = tradeNo
		out["money"] = service.MoneyStr(money)
		out["buyer"] = buyer
	}
	writeJSON(w, out)
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.Query(`SELECT id,admin_id,admin,action,detail,ip,created_at FROM audit_logs ORDER BY id DESC LIMIT 200`)
	var logs []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, adminID, createdAt int64
			var admin, action, detail, ip string
			if rows.Scan(&id, &adminID, &admin, &action, &detail, &ip, &createdAt) == nil {
				logs = append(logs, map[string]any{
					"ID": id, "Admin": admin, "Action": action, "Detail": detail,
					"IP": ip, "CreatedAt": createdAt,
				})
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "admin_audit"
	d["Title"] = "操作日志"
	d["Logs"] = logs
	s.render(w, "dash_admin_audit", http.StatusOK, d)
}

// handleAdminBackupCreate 创建数据库备份(SQLite VACUUM INTO,在线安全备份)
func (s *Server) handleAdminBackupCreate(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.DataDir, "backups")
	_ = os.MkdirAll(dir, 0o755)
	name := "backup-" + timeFormat(model.Now(), "20060102150405") + ".db"
	target := filepath.Join(dir, name)
	_ = os.Remove(target)
	if _, err := s.DB.Exec("VACUUM INTO ?", target); err != nil {
		http.Redirect(w, r, "/admin/settings?msg="+urlEscape("备份失败: "+err.Error()), http.StatusFound)
		return
	}
	s.audit(r, "backup.create", name)
	// 保留策略: 只保留最近 10 份,防止磁盘占满
	if entries, err := os.ReadDir(dir); err == nil {
		var backups []os.DirEntry
		for _, e := range entries {
			if backupNameRe.MatchString(e.Name()) {
				backups = append(backups, e)
			}
		}
		if len(backups) > 10 {
			// 按文件名(含时间戳)升序,删除最旧的
			for i := 0; i < len(backups)-10; i++ {
				_ = os.Remove(filepath.Join(dir, backups[i].Name()))
			}
		}
	}
	http.Redirect(w, r, "/admin/settings?msg="+urlEscape("备份已创建: "+name), http.StatusFound)
}

var backupNameRe = regexp.MustCompile(`^backup-\d{14}\.db$`)

// handleAdminBackupDownload 下载备份文件
func (s *Server) handleAdminBackupDownload(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.FormValue("file"))
	if !backupNameRe.MatchString(name) {
		http.Error(w, "文件名不合法", 400)
		return
	}
	f, err := os.Open(filepath.Join(s.DataDir, "backups", name))
	if err != nil {
		http.Error(w, "备份不存在", 404)
		return
	}
	defer f.Close()
	s.audit(r, "backup.download", name)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", "attachment; filename="+name)
	_, _ = io.Copy(w, f)
}

// handleAdminBackupList 设置页备份列表数据
func (s *Server) backupList() []map[string]any {
	dir := filepath.Join(s.DataDir, "backups")
	entries, err := os.ReadDir(dir)
	var out []map[string]any
	if err != nil {
		return out
	}
	for _, e := range entries {
		if backupNameRe.MatchString(e.Name()) {
			info, err := e.Info()
			if err == nil {
				out = append(out, map[string]any{
					"Name": e.Name(), "Size": info.Size(), "ModTime": info.ModTime().Unix(),
				})
			}
		}
	}
	return out
}

// handleAdminLogoUpload 上传收银台/站点 Logo(白标)
func (s *Server) handleAdminLogoUpload(w http.ResponseWriter, r *http.Request) {
	file, header, err := r.FormFile("logo")
	if err != nil {
		http.Redirect(w, r, "/admin/settings?msg="+urlEscape("请选择图片文件"), http.StatusFound)
		return
	}
	defer file.Close()
	if header.Size > 2*1024*1024 {
		http.Redirect(w, r, "/admin/settings?msg="+urlEscape("Logo 不能超过 2MB"), http.StatusFound)
		return
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".webp" {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("仅支持 png/jpg/webp(SVG因安全原因不支持)"), http.StatusFound)
		return
	}
	if !validImageMagic(file, ext) {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("图片内容与格式不符"), http.StatusFound)
		return
	}
	_, _ = file.Seek(0, 0)
	fn := "site_logo_" + db.RandKey(6) + ext
	dst, err := os.Create(filepath.Join(s.DataDir, fn))
	if err != nil {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("保存失败"), http.StatusFound)
		return
	}
	defer dst.Close()
	if _, err := dst.ReadFrom(file); err != nil {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("写入失败"), http.StatusFound)
		return
	}
	_ = s.DB.SetSetting("site_logo", "/uploads/"+fn)
	s.audit(r, "logo.upload", fn)
	http.Redirect(w, r, "/admin/settings?msg="+urlEscape("Logo 已更新,收银台与站点导航将展示您的品牌"), http.StatusFound)
}

// handleAdminLogoClear 清除自定义 Logo
func (s *Server) handleAdminLogoClear(w http.ResponseWriter, r *http.Request) {
	_ = s.DB.SetSetting("site_logo", "")
	s.audit(r, "logo.clear", "")
	http.Redirect(w, r, "/admin/settings?msg="+urlEscape("已恢复默认 Logo"), http.StatusFound)
}

// handleAdminOrderDetail 订单详情: 完整信息 + 异步通知投递日志 + 关联到账流水
func (s *Server) handleAdminOrderDetail(w http.ResponseWriter, r *http.Request) {
	id := atoint64(r.PathValue("id"))
	o := s.Store.OrderByID(id)
	if o == nil {
		http.Redirect(w, r, "/admin/orders?msg="+urlEscape("订单不存在"), http.StatusFound)
		return
	}
	merchant, _ := s.Store.UserByPid(o.Pid)

	type notifyView struct {
		ID        int64
		URL       string
		Response  string
		OK        bool
		CreatedAt int64
	}
	var notifies []notifyView
	nRows, err := s.DB.Query("SELECT id,url,response,ok,created_at FROM notify_logs WHERE order_id=? ORDER BY id ASC", o.ID)
	if err == nil {
		defer nRows.Close()
		for nRows.Next() {
			var n notifyView
			var ok int
			if nRows.Scan(&n.ID, &n.URL, &n.Response, &ok, &n.CreatedAt) == nil {
				n.OK = ok == 1
				notifies = append(notifies, n)
			}
		}
	}

	type payView struct {
		ID        int64
		Device    string
		Type      string
		Money     int64
		Content   string
		OK        bool
		CreatedAt int64
	}
	var pays []payView
	pRows, err := s.DB.Query(`SELECT p.id,COALESCE(d.name,''),p.type,p.money,p.content,p.order_id,p.created_at
		FROM pay_logs p LEFT JOIN devices d ON d.id=p.device_id WHERE p.order_id=? ORDER BY p.id ASC`, o.ID)
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var p payView
			var okID int64
			if pRows.Scan(&p.ID, &p.Device, &p.Type, &p.Money, &p.Content, &okID, &p.CreatedAt) == nil {
				p.OK = okID > 0
				pays = append(pays, p)
			}
		}
	}

	d := s.pageData(r)
	d["Page"] = "admin_orderdetail"
	d["Title"] = "订单详情"
	d["Order"] = o
	d["Merchant"] = merchant
	d["Notifies"] = notifies
	d["Pays"] = pays
	d["TypeName"] = typeName(o.Type)
	s.render(w, "dash_admin_orderdetail", http.StatusOK, d)
}

// ---------- 数据清理 ----------

// cleanupTarget 清理分类定义
type cleanupTarget struct {
	Key   string
	Name  string
	Desc  string
	Count int64
	Timed bool // 是否按保留天数清理
}

func (s *Server) cleanupTargets(cut int64) []cleanupTarget {
	t := func(key, name, desc string, count int64, timed bool) cleanupTarget {
		return cleanupTarget{Key: key, Name: name, Desc: desc, Count: count, Timed: timed}
	}
	var c1, c2, c3, c4, c5, c6, c7 int64
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM orders WHERE status=2 AND expire_at<?`, cut).Scan(&c1)
	_ = s.DB.QueryRow(`SELECT COUNT(*) FROM orders WHERE status=2 AND expire_at<?`, cut).Scan(&c1)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM pay_logs WHERE created_at<?", cut).Scan(&c2)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM notify_logs WHERE created_at<?", cut).Scan(&c3)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM pay_logs WHERE order_id=0 AND created_at<?", cut).Scan(&c4)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM sessions WHERE expires_at<?", model.Now()).Scan(&c5)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM pwd_resets WHERE expires_at<?", model.Now()).Scan(&c6)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM balance_logs WHERE created_at<?", cut).Scan(&c7)
	return []cleanupTarget{
		t("expired_orders", "过期订单", "已超时未支付的作废订单(含其流水)", c1, true),
		t("pay_logs", "到账流水", "监控上报流水记录", c2, true),
		t("notify_logs", "通知投递日志", "异步通知尝试记录", c3, true),
		t("unmatched", "未匹配到账流水", "未匹配到订单的上报(谨慎:回溯匹配依赖近期流水)", c4, true),
		t("sessions", "已过期会话", "失效的登录会话(无保留意义)", c5, false),
		t("pwd_resets", "过期密码重置令牌", "已过期的一次性重置链接", c6, false),
		t("balance_logs", "资金明细", "商户资金变动记录(财务凭证,谨慎清理)", c7, true),
	}
}

func (s *Server) handleAdminCleanup(w http.ResponseWriter, r *http.Request) {
	days := atoint64Or(r.FormValue("days"), 90)
	if days < 1 {
		days = 90
	}
	cut := model.Now() - days*86400
	d := s.pageData(r)
	d["Page"] = "admin_cleanup"
	d["Title"] = "数据清理"
	d["Targets"] = s.cleanupTargets(cut)
	d["Days"] = days
	s.render(w, "dash_admin_cleanup", http.StatusOK, d)
}

func (s *Server) handleAdminCleanupRun(w http.ResponseWriter, r *http.Request) {
	days := atoint64Or(r.FormValue("days"), 90)
	if days < 1 {
		days = 90
	}
	cut := model.Now() - days*86400
	var actions []string
	var total int64
	for _, key := range r.Form["targets"] {
		var n int64
		switch key {
		case "expired_orders":
			res, _ := s.DB.Exec(`DELETE FROM pay_logs WHERE order_id IN (SELECT id FROM orders WHERE status=2 AND expire_at<?)`, cut)
			n, _ = res.RowsAffected()
			res, _ = s.DB.Exec(`DELETE FROM notify_logs WHERE order_id IN (SELECT id FROM orders WHERE status=2 AND expire_at<?)`, cut)
			m, _ := res.RowsAffected()
			n += m
			res, _ = s.DB.Exec("DELETE FROM orders WHERE status=2 AND expire_at<?", cut)
			n2, _ := res.RowsAffected()
			n += n2
		case "pay_logs":
			res, _ := s.DB.Exec("DELETE FROM pay_logs WHERE created_at<?", cut)
			n, _ = res.RowsAffected()
		case "notify_logs":
			res, _ := s.DB.Exec("DELETE FROM notify_logs WHERE created_at<?", cut)
			n, _ = res.RowsAffected()
		case "unmatched":
			res, _ := s.DB.Exec("DELETE FROM pay_logs WHERE order_id=0 AND created_at<?", cut)
			n, _ = res.RowsAffected()
		case "sessions":
			res, _ := s.DB.Exec("DELETE FROM sessions WHERE expires_at<?", model.Now())
			n, _ = res.RowsAffected()
		case "pwd_resets":
			res, _ := s.DB.Exec("DELETE FROM pwd_resets WHERE expires_at<?", model.Now())
			n, _ = res.RowsAffected()
		case "balance_logs":
			res, _ := s.DB.Exec("DELETE FROM balance_logs WHERE created_at<?", cut)
			n, _ = res.RowsAffected()
		default:
			continue
		}
		total += n
		actions = append(actions, key)
	}
	s.audit(r, "data.cleanup", fmt.Sprintf("保留%d天 分类[%s] 清理%d条", days, strings.Join(actions, ","), total))
	http.Redirect(w, r, "/admin/cleanup?days="+strconv.FormatInt(days, 10)+
		"&msg="+urlEscape(fmt.Sprintf("清理完成,共删除 %d 条数据", total)), http.StatusFound)
}

// csvCell CSV单元格: 引号转义 + 公式注入防护(=/+/-/@/Tab开头前置单引号)
func csvCell(f string) string {
	if strings.HasPrefix(f, "=") || strings.HasPrefix(f, "+") ||
		strings.HasPrefix(f, "-") || strings.HasPrefix(f, "@") ||
		strings.HasPrefix(f, "\t") || strings.HasPrefix(f, "\r") {
		f = "'" + f
	}
	return `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
}

// handleAdminChannelStats 渠道成功率统计
func (s *Server) handleAdminChannelStats(w http.ResponseWriter, r *http.Request) {
	type chanStat struct {
		Type     string
		Name     string
		Enabled  bool
		Total    int64
		Paid     int64
		Expired  int64
		Pending  int64
		Rate     float64 // 成功率%
	}
	var stats []chanStat
	rows, err := s.DB.Query(`SELECT c.type, c.name, c.enabled,
		COUNT(o.id) as total,
		SUM(CASE WHEN o.status IN(1,3) THEN 1 ELSE 0 END) as paid,
		SUM(CASE WHEN o.status=2 THEN 1 ELSE 0 END) as expired,
		SUM(CASE WHEN o.status=0 THEN 1 ELSE 0 END) as pending
		FROM channels c LEFT JOIN orders o ON o.type=c.type AND o.addtime>strftime('%s','now')-7*86400
		GROUP BY c.type, c.name, c.enabled ORDER BY c.id`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var cs chanStat
			var typ, name string
			var enabled int
			if rows.Scan(&typ, &name, &enabled, &cs.Total, &cs.Paid, &cs.Expired, &cs.Pending) == nil {
				cs.Type = typ; cs.Name = name; cs.Enabled = enabled == 1
				if cs.Total > 0 {
					cs.Rate = float64(cs.Paid) / float64(cs.Total) * 100
				}
				stats = append(stats, cs)
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "admin_chanstats"
	d["Title"] = "渠道成功率"
	d["Stats"] = stats
	s.render(w, "dash_admin_chanstats", http.StatusOK, d)
}
