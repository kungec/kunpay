package web

import (
	"bufio"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"kunzhifu/internal/db"
	"kunzhifu/internal/model"
	"kunzhifu/internal/service"
)

func i64(n int64) string { return strconv.FormatInt(n, 10) }

func (s *Server) handleUserDashboard(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	dayStart := todayStart()
	var todayOrders, todayOk, yesterdayOk int64
	var todayMoney, yesterdayMoney int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE pid=?", u.Pid).Scan(&todayOrders)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=?", u.Pid, dayStart).Scan(&todayOk)
	_ = s.DB.QueryRow("SELECT COALESCE(SUM(money),0) FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=?", u.Pid, dayStart).Scan(&todayMoney)
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE pid=? AND status IN(1,3) AND addtime<? AND addtime>=?", u.Pid, dayStart, dayStart-86400).Scan(&yesterdayOk)
	_ = s.DB.QueryRow("SELECT COALESCE(SUM(money),0) FROM orders WHERE pid=? AND status IN(1,3) AND addtime<? AND addtime>=?", u.Pid, dayStart, dayStart-86400).Scan(&yesterdayMoney)

	// 近7日趋势
	var days []StatDay
	for i := 6; i >= 0; i-- {
		start := dayStart - int64(i)*86400
		var c, m int64
		_ = s.DB.QueryRow("SELECT COUNT(*),COALESCE(SUM(money),0) FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=? AND addtime<?",
			u.Pid, start, start+86400).Scan(&c, &m)
		days = append(days, StatDay{Date: timeFormat(start, "01-02"), Count: c, Money: m})
	}
	// 最近订单
	rows, err := s.DB.Query(`SELECT id,trade_no,out_trade_no,type,name,money,pay_money,status,addtime,endtime
		FROM orders WHERE pid=? ORDER BY id DESC LIMIT 8`, u.Pid)
	var recent []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, money, payMoney, addtime, endtime int64
			var tradeNo, outTradeNo, typ, name string
			var status int
			if rows.Scan(&id, &tradeNo, &outTradeNo, &typ, &name, &money, &payMoney, &status, &addtime, &endtime) == nil {
				recent = append(recent, map[string]any{
					"TradeNo": tradeNo, "OutTradeNo": outTradeNo, "Type": typ, "Name": name,
					"Money": money, "PayMoney": payMoney, "Status": status, "Addtime": addtime,
				})
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "user_home"
	d["Title"] = "商户首页"
	d["BaseURL"] = s.absoluteURL(r)
	d["Stats"] = map[string]any{
		"TodayOrders": todayOrders, "TodayOk": todayOk, "TodayMoney": todayMoney,
		"YesterdayOk": yesterdayOk, "YesterdayMoney": yesterdayMoney,
		"Balance": u.Balance,
		"Rate":    u.Rate,
	}
	d["Days"] = days
	d["Recent"] = recent
	d["Notice"] = s.DB.MustSetting("merchant_notice")
	s.render(w, "dash_user_home", http.StatusOK, d)
}

func (s *Server) handleUserMerchant(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	d := s.pageData(r)
	d["Page"] = "user_merchant"
	d["Title"] = "商户信息"
	d["BaseURL"] = s.absoluteURL(r)
	d["Merchant"] = u
	d["Flash"] = r.URL.Query().Get("msg")
	s.render(w, "dash_user_merchant", http.StatusOK, d)
}

func (s *Server) handleUserMerchantRegen(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_, _ = s.DB.Exec("UPDATE users SET skey=? WHERE id=?", db.RandKey(32), u.ID)
	http.Redirect(w, r, "/user/merchant?msg=密钥已重置,请及时更新到您的业务系统", http.StatusFound)
}

// ---------- 我的通道(商户自有收款码,同类型可多个,支持轮询) ----------

type chanItemView struct {
	ID      int64
	Name    string
	Qrcode  string
	Enabled bool
	Count   int64
	Amount  int64
}

type chanView struct {
	Type       string
	Name       string
	Platform   string
	PlatformOn bool
	Items      []chanItemView
	EnabledN   int
	Rotate     bool
	CanRotate  bool
}

func (s *Server) handleUserChannels(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	chs, _ := s.Store.AllChannels()
	var views []chanView
	for _, c := range chs {
		v := chanView{Type: c.Type, Name: c.Name, Platform: c.Qrcode, PlatformOn: c.Enabled == 1}
		items, _ := s.Store.MerchantItems(u.ID, c.Type)
		for _, it := range items {
			if it.Enabled == 1 {
				v.EnabledN++
			}
			var cnt int64
			var amt int64
			_ = s.DB.QueryRow("SELECT COUNT(*),COALESCE(SUM(money-fee),0) FROM orders WHERE m_item_id=? AND status IN(1,3)", it.ID).
				Scan(&cnt, &amt)
			v.Items = append(v.Items, chanItemView{
				ID: it.ID, Name: it.Name, Qrcode: it.Qrcode, Enabled: it.Enabled == 1,
				Count: cnt, Amount: amt,
			})
		}
		v.Rotate = s.Store.MerchantRotate(u.ID, c.Type)
		v.CanRotate = v.EnabledN >= 2
		views = append(views, v)
	}
	d := s.pageData(r)
	d["Page"] = "user_channels"
	d["Title"] = "我的通道"
	d["Channels"] = views
	d["Flash"] = r.URL.Query().Get("msg")
	d["FlashErr"] = r.URL.Query().Get("err")
	s.render(w, "dash_user_channels", http.StatusOK, d)
}

func (s *Server) handleUserChannelsUpload(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	typ := r.FormValue("type")
	if !allowTypes[typ] {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("通道类型不合法"), http.StatusFound)
		return
	}
	ch, _ := s.Store.ChannelByType(typ)
	if ch == nil || ch.Enabled != 1 {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("该渠道平台未开放,请联系管理员"), http.StatusFound)
		return
	}
	file, header, err := r.FormFile("qrcode")
	if err != nil {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("请选择图片文件"), http.StatusFound)
		return
	}
	defer file.Close()
	if header.Size > 4*1024*1024 {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("图片不能超过 4MB"), http.StatusFound)
		return
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".gif" && ext != ".webp" {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("仅支持 png/jpg/gif/webp 图片"), http.StatusFound)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "收款码" + strconv.Itoa(int(model.Now())%1000)
	}
	fn := fmt.Sprintf("user%d_%s_%s%s", u.Pid, typ, db.RandKey(6), ext)
	dst, err := os.Create(filepath.Join(s.DataDir, fn))
	if err != nil {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("保存文件失败"), http.StatusFound)
		return
	}
	defer dst.Close()
	if _, err := dst.ReadFrom(file); err != nil {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("写入文件失败"), http.StatusFound)
		return
	}
	if err := s.Store.AddMerchantItem(u.ID, typ, name, "/uploads/"+fn); err != nil {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("保存失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/channels?msg="+urlEscape("收款码已添加"), http.StatusFound)
}

func (s *Server) handleUserChannelsItemToggle(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	id := atoint64(r.FormValue("id"))
	enabled := 0
	if r.FormValue("enabled") == "1" {
		enabled = 1
	}
	if err := s.Store.SetMerchantItemState(u.ID, id, enabled); err != nil {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("操作失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/channels?msg="+urlEscape("收款码状态已更新"), http.StatusFound)
}

func (s *Server) handleUserChannelsItemDel(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	id := atoint64(r.FormValue("id"))
	_ = s.Store.DeleteMerchantItem(u.ID, id)
	http.Redirect(w, r, "/user/channels?msg="+urlEscape("收款码已删除"), http.StatusFound)
}

func (s *Server) handleUserChannelsRotate(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	typ := r.FormValue("type")
	if !allowTypes[typ] {
		http.Redirect(w, r, "/user/channels", http.StatusFound)
		return
	}
	on := r.FormValue("rotate") == "1"
	if err := s.Store.SetMerchantRotate(u.ID, typ, on); err != nil {
		http.Redirect(w, r, "/user/channels?err="+urlEscape(err.Error()), http.StatusFound)
		return
	}
	if on {
		http.Redirect(w, r, "/user/channels?msg="+urlEscape("轮询已开启,订单将轮流使用您的各个收款码"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/channels?msg="+urlEscape("轮询已关闭"), http.StatusFound)
}

func (s *Server) handleUserOrders(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_ = r.ParseForm()
	page := atoint64Or(r.FormValue("p"), 1)
	if page < 1 {
		page = 1
	}
	const pageSize = 15
	tradeNo := strings.TrimSpace(r.FormValue("trade_no"))
	status := strings.TrimSpace(r.FormValue("status"))
	typ := strings.TrimSpace(r.FormValue("type"))
	dateFrom := strings.TrimSpace(r.FormValue("date_from"))
	dateTo := strings.TrimSpace(r.FormValue("date_to"))

	where := "pid=?"
	args := []any{u.Pid}
	if tradeNo != "" {
		where += " AND (trade_no=? OR out_trade_no=?)"
		args = append(args, tradeNo, tradeNo)
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
	rows, err := s.DB.Query(`SELECT id,trade_no,out_trade_no,type,name,money,pay_money,status,notify_ok,addtime,endtime
		FROM orders WHERE `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, pageSize, (page-1)*pageSize)...)
	var orders []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, money, payMoney, addtime, endtime int64
			var tradeNo2, outTradeNo2, typ2, name string
			var status2, notifyOK int
			if rows.Scan(&id, &tradeNo2, &outTradeNo2, &typ2, &name, &money, &payMoney, &status2, &notifyOK, &addtime, &endtime) == nil {
				orders = append(orders, map[string]any{
					"ID": id, "TradeNo": tradeNo2, "OutTradeNo": outTradeNo2, "Type": typ2, "TypeName": typeName(typ2),
					"Name": name, "Money": money, "PayMoney": payMoney, "Status": status2,
					"NotifyOK": notifyOK, "Addtime": addtime, "Endtime": endtime,
				})
			}
		}
	}
	totalPages := (total + pageSize - 1) / pageSize
	d := s.pageData(r)
	d["Page"] = "user_orders"
	d["Title"] = "订单列表"
	d["Orders"] = orders
	d["Total"] = total
	d["PageNo"] = page
	d["TotalPages"] = totalPages
	d["QTradeNo"] = tradeNo
	d["QStatus"] = status
	d["QType"] = typ
	d["DateFrom"] = dateFrom
	d["DateTo"] = dateTo
	s.render(w, "dash_user_orders", http.StatusOK, d)
}

func (s *Server) handleUserSettle(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	var minSettle int64 = 1000
	if v := atoint64(s.DB.MustSetting("min_settle")); v > 0 {
		minSettle = v
	}
	rows, err := s.DB.Query(`SELECT id,amount,settle_type,account,name,status,note,created_at,done_at
		FROM settle_logs WHERE user_id=? ORDER BY id DESC LIMIT 50`, u.ID)
	var logs []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, amount, settleType, createdAt, doneAt int64
			var account, name, note string
			var status int
			if rows.Scan(&id, &amount, &settleType, &account, &name, &status, &note, &createdAt, &doneAt) == nil {
				logs = append(logs, map[string]any{
					"ID": id, "Amount": amount, "SettleType": settleType, "Account": account,
					"Name": name, "Status": status, "Note": note, "CreatedAt": createdAt, "DoneAt": doneAt,
				})
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "user_settle"
	d["Title"] = "结算提现"
	d["MinSettle"] = minSettle
	d["Logs"] = logs
	d["Flash"] = r.URL.Query().Get("msg")
	d["FlashErr"] = r.URL.Query().Get("err")
	s.render(w, "dash_user_settle", http.StatusOK, d)
}

func (s *Server) handleUserSettleApply(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_ = r.ParseForm()
	amountStr := strings.TrimSpace(r.FormValue("amount"))
	amount, err := service.ParseMoney(amountStr)
	if err != nil || amount <= 0 {
		http.Redirect(w, r, "/user/settle?err="+urlEscape("金额格式错误"), http.StatusFound)
		return
	}
	var minSettle int64 = 1000
	if v := atoint64(s.DB.MustSetting("min_settle")); v > 0 {
		minSettle = v
	}
	if amount < minSettle {
		http.Redirect(w, r, "/user/settle?err="+urlEscape("最低提现金额为 ¥"+service.MoneyStr(minSettle)), http.StatusFound)
		return
	}
	if u.SettleAccount == "" || u.SettleName == "" {
		http.Redirect(w, r, "/user/settle?err="+urlEscape("请先在个人信息中设置结算账号与姓名"), http.StatusFound)
		return
	}
	// 原子扣减余额
	res, err := s.DB.Exec("UPDATE users SET balance=balance-? WHERE id=? AND balance>=?", amount, u.ID, amount)
	if err != nil || rowsAffected(res) == 0 {
		http.Redirect(w, r, "/user/settle?err="+urlEscape("余额不足"), http.StatusFound)
		return
	}
	var balance int64
	_ = s.DB.QueryRow("SELECT balance FROM users WHERE id=?", u.ID).Scan(&balance)
	_, _ = s.DB.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
		VALUES(?,?,?,?,?,?,?)`,
		u.ID, "settle", -amount, balance+amount, balance, "申请提现", model.Now())
	_, _ = s.DB.Exec(`INSERT INTO settle_logs(user_id,amount,settle_type,account,name,status,created_at)
		VALUES(?,?,?,?,?,0,?)`, u.ID, amount, u.SettleType, u.SettleAccount, u.SettleName, model.Now())
	http.Redirect(w, r, "/user/settle?msg="+urlEscape("提现申请已提交,请等待管理员处理"), http.StatusFound)
}

func (s *Server) handleUserSettings(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	d := s.pageData(r)
	d["Page"] = "user_settings"
	d["Title"] = "个人设置"
	d["Merchant"] = u
	d["Flash"] = r.URL.Query().Get("msg")
	d["FlashErr"] = r.URL.Query().Get("err")
	s.render(w, "dash_user_settings", http.StatusOK, d)
}

func (s *Server) handleUserSettingsProfile(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_ = r.ParseForm()
	email := strings.TrimSpace(r.FormValue("email"))
	qq := strings.TrimSpace(r.FormValue("qq"))
	settleType := atoint64Or(r.FormValue("settle_type"), 1)
	if settleType < 1 || settleType > 4 {
		settleType = 1
	}
	settleAccount := strings.TrimSpace(r.FormValue("settle_account"))
	settleName := strings.TrimSpace(r.FormValue("settle_name"))
	if email != "" && !reEmail.MatchString(email) {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("邮箱格式不正确"), http.StatusFound)
		return
	}
	_, err := s.DB.Exec(`UPDATE users SET email=?, qq=?, settle_type=?, settle_account=?, settle_name=? WHERE id=?`,
		email, qq, settleType, settleAccount, settleName, u.ID)
	if err != nil {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("保存失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/settings?msg="+urlEscape("个人信息已保存"), http.StatusFound)
}

func (s *Server) handleUserSettingsPassword(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_ = r.ParseForm()
	old := r.FormValue("old_password")
	newPw := r.FormValue("new_password")
	newPw2 := r.FormValue("new_password2")
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(old)) != nil {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("原密码错误"), http.StatusFound)
		return
	}
	if len(newPw) < 6 {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("新密码长度至少 6 位"), http.StatusFound)
		return
	}
	if newPw != newPw2 {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("两次输入的新密码不一致"), http.StatusFound)
		return
	}
	_, _ = s.DB.Exec("UPDATE users SET password_hash=? WHERE id=?", hashPassword(newPw), u.ID)
	http.Redirect(w, r, "/user/settings?msg="+urlEscape("密码修改成功"), http.StatusFound)
}

func (s *Server) handleUserBalance(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	page := atoint64Or(r.FormValue("p"), 1)
	if page < 1 {
		page = 1
	}
	const pageSize = 20
	var total int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM balance_logs WHERE user_id=?", u.ID).Scan(&total)
	rows, err := s.DB.Query(`SELECT id,type,amount,before,after,memo,created_at FROM balance_logs
		WHERE user_id=? ORDER BY id DESC LIMIT ? OFFSET ?`, u.ID, pageSize, (page-1)*pageSize)
	var logs []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, amount, before, after, createdAt int64
			var typ, memo string
			if rows.Scan(&id, &typ, &amount, &before, &after, &memo, &createdAt) == nil {
				logs = append(logs, map[string]any{
					"ID": id, "Type": typ, "Amount": amount, "Before": before, "After": after,
					"Memo": memo, "CreatedAt": createdAt,
				})
			}
		}
	}
	totalPages := (total + pageSize - 1) / pageSize
	d := s.pageData(r)
	d["Page"] = "user_balance"
	d["Title"] = "资金明细"
	d["Logs"] = logs
	d["Total"] = total
	d["PageNo"] = page
	d["TotalPages"] = totalPages
	d["Balance"] = u.Balance
	s.render(w, "dash_user_balance", http.StatusOK, d)
}

func rowsAffected(res sql.Result) int64 {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return n
}

func urlEscape(s string) string { return url.QueryEscape(s) }

// ---------- 每日对账单 ----------

func (s *Server) handleUserReport(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	dateStr := strings.TrimSpace(r.FormValue("date"))
	dayStart := todayStart()
	if t, err := time.Parse("2006-01-02", dateStr); err == nil {
		t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
		if t.Unix() < model.Now()-86400*366 {
			t = time.Unix(dayStart, 0)
		}
		dayStart = t.Unix()
	}
	dayEnd := dayStart + 86400

	var totalOrders, okOrders, totalMoney, okMoney, feeSum int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM orders WHERE pid=? AND addtime>=? AND addtime<?", u.Pid, dayStart, dayEnd).Scan(&totalOrders)
	_ = s.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(money),0),COALESCE(SUM(fee),0),COALESCE(SUM(money-fee),0)
		FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=? AND addtime<?`,
		u.Pid, dayStart, dayEnd).Scan(&okOrders, &okMoney, &feeSum, &totalMoney)

	rows, err := s.DB.Query(`SELECT trade_no,out_trade_no,type,name,money,pay_money,fee,status,addtime,endtime
		FROM orders WHERE pid=? AND addtime>=? AND addtime<? ORDER BY id DESC LIMIT 500`,
		u.Pid, dayStart, dayEnd)
	var orders []map[string]any
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var tradeNo, outTradeNo, typ, name string
			var money, payMoney, fee, addtime, endtime int64
			var status int
			if rows.Scan(&tradeNo, &outTradeNo, &typ, &name, &money, &payMoney, &fee, &status, &addtime, &endtime) == nil {
				orders = append(orders, map[string]any{
					"TradeNo": tradeNo, "OutTradeNo": outTradeNo, "Type": typ, "TypeName": typeName(typ),
					"Name": name, "Money": money, "PayMoney": payMoney, "Fee": fee,
					"Status": status, "Addtime": addtime, "Endtime": endtime,
				})
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "user_report"
	d["Title"] = "对账单"
	d["Date"] = timeFormat(dayStart, "2006-01-02")
	d["Stats"] = map[string]any{
		"TotalOrders": totalOrders, "OkOrders": okOrders,
		"OkMoney": okMoney, "FeeSum": feeSum, "Income": okMoney - feeSum,
	}
	d["Orders"] = orders
	s.render(w, "dash_user_report", http.StatusOK, d)
}

// handleUserReportExport 对账单当日成功订单 CSV
func (s *Server) handleUserReportExport(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	dateStr := strings.TrimSpace(r.FormValue("date"))
	dayStart := todayStart()
	if t, err := time.Parse("2006-01-02", dateStr); err == nil {
		dayStart = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Unix()
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=report_"+timeFormat(dayStart, "20060102")+".csv")
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	bw.WriteString("平台订单号,商户订单号,支付方式,商品名称,金额,手续费,净入账,完成时间\n")
	rows, err := s.DB.Query(`SELECT trade_no,out_trade_no,type,name,money,fee,endtime
		FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=? AND addtime<? ORDER BY id ASC`,
		u.Pid, dayStart, dayStart+86400)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var tradeNo, outTradeNo, typ, name string
		var money, fee, endtime int64
		if rows.Scan(&tradeNo, &outTradeNo, &typ, &name, &money, &fee, &endtime) != nil {
			continue
		}
		rec := []string{tradeNo, outTradeNo, typeName(typ), name,
			yuan(money), yuan(fee), yuan(money - fee), fdate(endtime)}
		for i, f := range rec {
			if i > 0 {
				bw.WriteString(",")
			}
			bw.WriteString(`"` + strings.ReplaceAll(f, `"`, `""`) + `"`)
		}
		bw.WriteString("\n")
	}
}

// handleUserReportMail 将某日对账汇总发送到商户邮箱
func (s *Server) handleUserReportMail(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u.Email == "" {
		http.Redirect(w, r, "/user/report?err="+urlEscape("请先在个人设置中填写邮箱"), http.StatusFound)
		return
	}
	dateStr := strings.TrimSpace(r.FormValue("date"))
	dayStart := todayStart()
	if t, err := time.Parse("2006-01-02", dateStr); err == nil {
		dayStart = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Unix()
	}
	var okOrders int64
	var okMoney, feeSum int64
	_ = s.DB.QueryRow(`SELECT COUNT(*),COALESCE(SUM(money),0),COALESCE(SUM(fee),0)
		FROM orders WHERE pid=? AND status IN(1,3) AND addtime>=? AND addtime<?`,
		u.Pid, dayStart, dayStart+86400).Scan(&okOrders, &okMoney, &feeSum)
	dateCn := timeFormat(dayStart, "2006年01月02日")
	body := mailHTML(dateCn+" 对账单", []string{
		"商户: " + u.Username + " (PID " + strconv.FormatInt(u.Pid, 10) + ")",
		"成功订单: <b>" + strconv.FormatInt(okOrders, 10) + "</b> 笔",
		"成功交易额: <b>¥" + yuan(okMoney) + "</b>",
		"手续费: ¥" + yuan(feeSum),
		"净入账: <b style='color:#1fa56f'>¥" + yuan(okMoney-feeSum) + "</b>",
	})
	if err := s.SendMail(u.Email, "【"+s.DB.MustSetting("site_name")+"】"+dateCn+" 对账单", body); err != nil {
		http.Redirect(w, r, "/user/report?err="+urlEscape("对账单发送失败: "+err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/report?msg="+urlEscape("对账单已发送至 "+u.Email), http.StatusFound)
}

// handleUserDebug API 在线调试: 用商户自己的密钥生成签名请求并创建真实订单
func (s *Server) handleUserDebug(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_ = r.ParseForm()
	out := map[string]any{}
	typ := r.FormValue("type")
	if !allowTypes[typ] {
		typ = "alipay"
	}
	money := strings.TrimSpace(r.FormValue("money"))
	if money == "" {
		money = "1.00"
	}
	outTradeNo := strings.TrimSpace(r.FormValue("out_trade_no"))
	if outTradeNo == "" {
		outTradeNo = "DEBUG" + timeFormat(model.Now(), "0102150405") + db.RandDigits(4)
	}
	notifyURL := strings.TrimSpace(r.FormValue("notify_url"))

	testReq := &service.OrderReq{
		Type: typ, OutTradeNo: outTradeNo,
		NotifyURL: notifyURL, Name: "API调试测试",
		Money: money, Sitename: "API调试", Device: "pc",
	}
	o, err := s.Store.CreateOrder(u, testReq, clientIP(r))
	if err != nil {
		out["code"] = -1
		out["msg"] = err.Error()
		writeJSON(w, out)
		return
	}
	params := map[string]string{
		"pid": strconv.FormatInt(u.Pid, 10), "type": typ,
		"out_trade_no": outTradeNo, "notify_url": notifyURL,
		"name": "API调试测试", "money": money, "sitename": "API调试",
	}
	params["sign"] = service.Sign(params, u.Skey)
	params["sign_type"] = "MD5"
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	fullURL := s.absoluteURL(r) + "/submit.php?" + q.Encode()
	out["code"] = 1
	out["url"] = fullURL
	out["pay_url"] = "/pay/" + o.TradeNo
	out["trade_no"] = o.TradeNo
	out["sign"] = params["sign"]
	out["params"] = params
	writeJSON(w, out)
}

// handleUserSettleExport 结算记录 CSV 导出
func (s *Server) handleUserSettleExport(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=settles_"+u.Username+".csv")
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	bw.WriteString("编号,提现金额,收款方式,收款账号,收款人,状态,备注,申请时间,处理时间\n")
	rows, err := s.DB.Query(`SELECT id,amount,settle_type,account,name,status,note,created_at,done_at
		FROM settle_logs WHERE user_id=? ORDER BY id DESC`, u.ID)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id, amount, settleType, createdAt, doneAt int64
		var account, name, note string
		var status int
		if rows.Scan(&id, &amount, &settleType, &account, &name, &status, &note, &createdAt, &doneAt) != nil {
			continue
		}
		st := "待处理"
		if status == 1 {
			st = "已打款"
		} else if status == 2 {
			st = "已拒绝"
		}
		rec := []string{strconv.FormatInt(id, 10), yuan(amount), settleTypeName(settleType),
			account, name, st, note, fdate(createdAt), fdate(doneAt)}
		for i, f := range rec {
			if i > 0 {
				bw.WriteString(",")
			}
			bw.WriteString(`"` + strings.ReplaceAll(f, `"`, `""`) + `"`)
		}
		bw.WriteString("\n")
	}
}

// ---------- 用户头像 ----------

// handleUserAvatarUpload 上传自定义头像
func (s *Server) handleUserAvatarUpload(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	file, header, err := r.FormFile("avatar")
	if err != nil {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("请选择图片文件"), http.StatusFound)
		return
	}
	defer file.Close()
	if header.Size > 2*1024*1024 {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("头像不能超过 2MB"), http.StatusFound)
		return
	}
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".gif" && ext != ".webp" {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("仅支持 png/jpg/gif/webp 图片"), http.StatusFound)
		return
	}
	// 清理旧头像文件
	if old := u.Avatar; strings.HasPrefix(old, "/uploads/") {
		_ = os.Remove(filepath.Join(s.DataDir, filepath.Base(old)))
	}
	fn := fmt.Sprintf("avatar%d_%s%s", u.ID, db.RandKey(4), ext)
	dst, err := os.Create(filepath.Join(s.DataDir, fn))
	if err != nil {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("保存失败"), http.StatusFound)
		return
	}
	defer dst.Close()
	if _, err := dst.ReadFrom(file); err != nil {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("写入失败"), http.StatusFound)
		return
	}
	_, _ = s.DB.Exec("UPDATE users SET avatar=? WHERE id=?", "/uploads/"+fn, u.ID)
	http.Redirect(w, r, "/user/settings?msg="+urlEscape("头像已更新"), http.StatusFound)
}

// handleUserAvatarClear 恢复默认头像
func (s *Server) handleUserAvatarClear(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if old := u.Avatar; strings.HasPrefix(old, "/uploads/") {
		_ = os.Remove(filepath.Join(s.DataDir, filepath.Base(old)))
	}
	_, _ = s.DB.Exec("UPDATE users SET avatar='' WHERE id=?", u.ID)
	http.Redirect(w, r, "/user/settings?msg="+urlEscape("已恢复默认头像"), http.StatusFound)
}
