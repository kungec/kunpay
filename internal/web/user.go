package web

import (
	"bufio"
	"database/sql"
	"encoding/pem"
	"fmt"
	"github.com/skip2/go-qrcode"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	d["FlashErr"] = r.URL.Query().Get("err")
	s.render(w, "dash_user_merchant", http.StatusOK, d)
}

func (s *Server) handleUserMerchantRegen(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_, _ = s.DB.Exec("UPDATE users SET skey=? WHERE id=?", db.RandKey(32), u.ID)
	http.Redirect(w, r, "/user/merchant?msg=密钥已重置,请及时更新到您的业务系统", http.StatusFound)
}

// handleUserApiIPs 保存商户 API IP 白名单
func (s *Server) handleUserApiIPs(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	raw := strings.TrimSpace(r.FormValue("api_ips"))
	if len(raw) > 500 {
		http.Redirect(w, r, "/user/merchant?err="+urlEscape("白名单过长(最多500字符)"), http.StatusFound)
		return
	}
	// 逐项校验格式,防手误把自己锁死
	if raw != "" {
		for _, entry := range strings.Split(raw, ",") {
			entry = strings.TrimSpace(entry)
			if entry == "" {
				continue
			}
			if strings.Contains(entry, "/") {
				if _, _, err := net.ParseCIDR(entry); err != nil {
					http.Redirect(w, r, "/user/merchant?err="+urlEscape("CIDR 格式不正确: "+entry), http.StatusFound)
					return
				}
				continue
			}
			if strings.HasSuffix(entry, "*") {
				continue
			}
			if net.ParseIP(entry) == nil {
				http.Redirect(w, r, "/user/merchant?err="+urlEscape("IP 格式不正确: "+entry), http.StatusFound)
				return
			}
		}
	}
	if _, err := s.DB.Exec("UPDATE users SET api_ips=? WHERE id=?", raw, u.ID); err != nil {
		http.Redirect(w, r, "/user/merchant?err="+urlEscape("保存失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/merchant?msg="+urlEscape("API IP 白名单已保存"), http.StatusFound)
}

// ---------- 我的通道(商户自有收款码,同类型可多个,支持轮询) ----------

type chanItemView struct {
	ID      int64
	Name    string
	Qrcode  string
	Enabled bool
	Weight  int64
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
				Weight: it.Weight, Count: cnt, Amount: amt,
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
	if err := s.Store.AddMerchantItem(u.ID, typ, name, "/uploads/"+fn, atoint64(r.FormValue("weight"))); err != nil {
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

func (s *Server) handleUserChannelsItemWeight(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	id := atoint64(r.FormValue("id"))
	if err := s.Store.SetMerchantItemWeight(u.ID, id, atoint64(r.FormValue("weight"))); err != nil {
		http.Redirect(w, r, "/user/channels?err="+urlEscape("操作失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/channels?msg="+urlEscape("权重已更新,轮询将按新比例分配订单"), http.StatusFound)
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

// handleUserMerchantRSA 保存商户 RSA 公钥(空=回落 MD5)
func (s *Server) handleUserMerchantRSA(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	pub := strings.TrimSpace(r.FormValue("rsa_public"))
	if len(pub) > 4000 {
		http.Redirect(w, r, "/user/merchant?err="+urlEscape("公钥内容过长"), http.StatusFound)
		return
	}
	if pub != "" && !service.VerifyRSA(pub, "probe", "invalid") {
		// VerifyRSA 对无效签名必返回 false,这里仅校验公钥本身可解析
		if block, _ := pem.Decode([]byte(pub)); block == nil {
			http.Redirect(w, r, "/user/merchant?err="+urlEscape("公钥格式不正确,请粘贴 PEM 格式公钥"), http.StatusFound)
			return
		}
	}
	if err := s.Store.SetUserRSAPublic(u.ID, pub); err != nil {
		http.Redirect(w, r, "/user/merchant?err="+urlEscape("保存失败"), http.StatusFound)
		return
	}
	if pub == "" {
		http.Redirect(w, r, "/user/merchant?msg="+urlEscape("已清除 RSA 公钥,接口回落 MD5 签名"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/merchant?msg="+urlEscape("RSA 公钥已保存,请求 sign_type=RSA 即可使用 RSA 签名"), http.StatusFound)
}

// handleUserPayTypes 保存商户可用支付方式
func (s *Server) handleUserPayTypes(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	var types []string
	for _, t := range []string{"alipay", "wxpay", "qqpay"} {
		if r.FormValue(t) == "1" {
			types = append(types, t)
		}
	}
	if err := s.Store.SetUserPayTypes(u.ID, types); err != nil {
		http.Redirect(w, r, "/user/merchant?err="+urlEscape("保存失败"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/merchant?msg="+urlEscape("可用支付方式已更新"), http.StatusFound)
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
	notifyEmail := r.FormValue("notify_email") == "1"
	// 邮箱为空时强制关闭邮件通知
	if email == "" {
		notifyEmail = false
	}
	_, err := s.DB.Exec(`UPDATE users SET email=?, qq=?, settle_type=?, settle_account=?, settle_name=?, notify_email_on=? WHERE id=?`,
		email, qq, settleType, settleAccount, settleName, boolToInt(notifyEmail), u.ID)
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
	lt := time.Unix(dayStart, 0)
	d["Year"], d["Month"] = lt.Year(), int(lt.Month())
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
			bw.WriteString(csvCell(f))
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
			bw.WriteString(csvCell(f))
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
	if !validImageMagic(file, ext) {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("图片内容与格式不符"), http.StatusFound)
		return
	}
	_, _ = file.Seek(0, 0)
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

// handleUserDebugNotify 商户回调测试: 向商户 notify_url 发送一条带签名的模拟异步通知
func (s *Server) handleUserDebugNotify(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_ = r.ParseForm()
	notifyURL := strings.TrimSpace(r.FormValue("notify_url"))
	out := map[string]any{}
	if notifyURL == "" {
		out["code"] = -1
		out["msg"] = "请填写 notify_url"
		writeJSON(w, out)
		return
	}
	lu := strings.ToLower(notifyURL)
	if !strings.HasPrefix(lu, "http://") && !strings.HasPrefix(lu, "https://") {
		out["code"] = -1
		out["msg"] = "仅支持 http/https 协议"
		writeJSON(w, out)
		return
	}

	// 构造与真实异步通知一致的签名参数
	params := map[string]string{
		"pid":          strconv.FormatInt(u.Pid, 10),
		"trade_no":     timeFormat(model.Now(), "20060102150405") + db.RandDigits(5),
		"out_trade_no": "TEST" + db.RandDigits(6),
		"type":         "alipay",
		"name":         "回调测试",
		"money":        "1.00",
		"trade_status": "TRADE_SUCCESS",
	}
	params["sign"] = service.Sign(params, u.Skey)
	params["sign_type"] = "MD5"

	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	target := notifyURL
	if strings.Contains(target, "?") {
		target += "&" + q.Encode()
	} else {
		target += "?" + q.Encode()
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(target)
	rr := ""
	if err != nil {
		rr = "请求失败: " + err.Error()
		out["ok"] = false
	} else {
		defer resp.Body.Close()
		buf := make([]byte, 512)
		n, _ := resp.Body.Read(buf)
		rr = fmt.Sprintf("HTTP %d, 响应: %s", resp.StatusCode, string(buf[:n]))
		if strings.EqualFold(strings.TrimSpace(rr[strings.Index(rr, "响应: ")+len("响应: "):]), "success") {
			out["ok"] = true
		}
	}
	if out["ok"] == true {
		out["msg"] = "✓ 回调送达,商户响应 success — 异步通知配置正确"
	} else {
		out["msg"] = "✗ 商户响应非 success(应原样输出 success): " + rr
	}
	out["response"] = rr
	writeJSON(w, out)
}

// handleUserOrderDetail 商户订单详情(仅本人订单): 信息 + 通知投递记录 + 到账流水
func (s *Server) handleUserOrderDetail(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	id := atoint64(r.PathValue("id"))
	o := s.Store.OrderByID(id)
	if o == nil || o.Pid != u.Pid {
		renderMsg(s, w, r, "订单不存在", "订单不存在或无权查看", http.StatusNotFound)
		return
	}
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
		Device    string
		Type      string
		Money     int64
		Content   string
		CreatedAt int64
	}
	var pays []payView
	pRows, err := s.DB.Query(`SELECT COALESCE(d.name,''),p.type,p.money,p.content,p.created_at
		FROM pay_logs p LEFT JOIN devices d ON d.id=p.device_id WHERE p.order_id=? ORDER BY p.id ASC`, o.ID)
	if err == nil {
		defer pRows.Close()
		for pRows.Next() {
			var p payView
			if pRows.Scan(&p.Device, &p.Type, &p.Money, &p.Content, &p.CreatedAt) == nil {
				pays = append(pays, p)
			}
		}
	}
	d := s.pageData(r)
	d["Page"] = "user_orderdetail"
	d["Title"] = "订单详情"
	d["Order"] = o
	d["Notifies"] = notifies
	d["Pays"] = pays
	d["TypeName"] = typeName(o.Type)
	s.render(w, "dash_user_orderdetail", http.StatusOK, d)
}

// ---------- 支付链接生成器 ----------

// handleUserPayLink 创建可分享的收款链接
func (s *Server) handleUserPayLink(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_ = r.ParseForm()
	typ := r.FormValue("type")
	if !allowTypes[typ] {
		typ = "alipay"
	}
	money := strings.TrimSpace(r.FormValue("money"))
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" {
		name = "商品" + timeFormat(model.Now(), "01021504")
	}
	out := map[string]any{}
	// 使用 API 调试同逻辑创建订单
	testReq := &service.OrderReq{
		Type: typ, OutTradeNo: "LINK" + timeFormat(model.Now(), "0102150405") + db.RandDigits(4),
		NotifyURL: "", Name: name, Money: money, Sitename: u.Username, Device: "mobile",
	}
	o, err := s.Store.CreateOrder(u, testReq, clientIP(r))
	if err != nil {
		out["code"] = -1
		out["msg"] = err.Error()
		writeJSON(w, out)
		return
	}
	out["code"] = 1
	out["pay_url"] = "/pay/" + o.TradeNo
	out["trade_no"] = o.TradeNo
	out["full_url"] = s.absoluteURL(r) + "/pay/" + o.TradeNo
	out["name"] = name
	out["money"] = service.MoneyStr(o.PayMoney)
	writeJSON(w, out)
}

// handleUserOrderRefund 商户提交退款申请(状态4)
func (s *Server) handleUserOrderRefund(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	_ = r.ParseForm()
	orderID := atoint64(r.FormValue("id"))
	o := s.Store.OrderByID(orderID)
	if o == nil || o.Pid != u.Pid {
		http.Redirect(w, r, "/user/orders?err="+urlEscape("订单不存在"), http.StatusFound)
		return
	}
	if o.Status != 1 && o.Status != 3 {
		http.Redirect(w, r, "/user/orders?err="+urlEscape("仅已支付订单可退款"), http.StatusFound)
		return
	}
	_, _ = s.DB.Exec("UPDATE orders SET status=4, buyer=buyer||' [退款]' WHERE id=? AND status IN(1,3)", o.ID)
	// 扣回商户余额
	refund := o.Money - o.Fee
	if refund > 0 {
		_, _ = s.DB.Exec("UPDATE users SET balance=balance-? WHERE id=?", refund, u.ID)
		var bal int64
		s.DB.QueryRow("SELECT balance FROM users WHERE id=?", u.ID).Scan(&bal)
		s.DB.Exec(`INSERT INTO balance_logs(user_id,type,amount,before,after,memo,created_at)
			VALUES(?,?,?,?,?,?,?)`, u.ID, "refund", -refund, bal+refund, bal,
			"订单退款 "+o.TradeNo, model.Now())
	}
	http.Redirect(w, r, "/user/orders?msg="+urlEscape("订单已退款,余额已扣减"), http.StatusFound)
}

// handleUserInvite 邀请返佣页: 邀请链接 + 统计 + 返佣明细
func (s *Server) handleUserInvite(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	var invitedN, totalRebate int64
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM users WHERE invited_by=?", u.ID).Scan(&invitedN)
	_ = s.DB.QueryRow("SELECT COALESCE(SUM(amount),0) FROM balance_logs WHERE user_id=? AND type='invite'", u.ID).Scan(&totalRebate)
	var logs []map[string]any
	rows, err := s.DB.Query(`SELECT amount,memo,created_at FROM balance_logs
		WHERE user_id=? AND type='invite' ORDER BY id DESC LIMIT 50`, u.ID)
	if err == nil {
		for rows.Next() {
			var amount, at int64
			var memo string
			if rows.Scan(&amount, &memo, &at) == nil {
				logs = append(logs, map[string]any{"Amount": amount, "Memo": memo, "CreatedAt": at})
			}
		}
		rows.Close()
	}
	d := s.pageData(r)
	d["Page"] = "user_invite"
	d["Title"] = "邀请返佣"
	d["InviteURL"] = s.baseURL(r) + "/register?invite=" + u.InviteCode
	d["InvitedN"] = invitedN
	d["TotalRebate"] = totalRebate
	d["Logs"] = logs
	d["Rate"] = s.DB.MustSetting("invite_rate")
	d["Open"] = s.DB.MustSetting("invite_open") == "1"
	s.render(w, "dash_user_invite", http.StatusOK, d)
}

// handleUserTOTPQR 输出 otpauth:// 绑定二维码(login 后方可取)
func (s *Server) handleUserTOTPQR(w http.ResponseWriter, r *http.Request) {
	secret := r.URL.Query().Get("s")
	if secret == "" || len(secret) > 64 || !reTOTPSecret.MatchString(secret) {
		http.Error(w, "bad secret", 400)
		return
	}
	u := s.currentUser(r)
	uri := service.OTPAuthURL(secret, u.Username, s.DB.MustSetting("site_name"))
	data, err := qrcode.Encode(uri, qrcode.Medium, 260)
	if err != nil {
		http.Error(w, "qrcode", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// handleUserTOTPSetup 两步验证绑定页
func (s *Server) handleUserTOTPSetup(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if u.TotpSecret != "" {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("两步验证已开启,如需更换请先关闭"), http.StatusFound)
		return
	}
	secret := service.GenerateTOTPSecret()
	d := s.pageData(r)
	d["Page"] = "user_settings"
	d["Title"] = "开启两步验证"
	d["Secret"] = secret
	d["QRSrc"] = "/user/totp/qr?s=" + secret
	d["OTPURL"] = service.OTPAuthURL(secret, u.Username, s.DB.MustSetting("site_name"))
	s.render(w, "dash_totp_setup", http.StatusOK, d)
}

// handleUserTOTPEnable 确认开启(校验动态码后写库)
func (s *Server) handleUserTOTPEnable(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	secret := strings.ToUpper(strings.TrimSpace(r.FormValue("secret")))
	code := strings.TrimSpace(r.FormValue("code"))
	if !reTOTPSecret.MatchString(secret) {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("密钥格式不正确"), http.StatusFound)
		return
	}
	if !service.VerifyTOTP(secret, code) {
		http.Redirect(w, r, "/user/settings/totp/setup?err="+urlEscape("动态码不正确,请重试(注意手机时间需准确)"), http.StatusFound)
		return
	}
	_ = s.Store.SetUserTOTP(u.ID, secret)
	http.Redirect(w, r, "/user/settings?msg="+urlEscape("两步验证已开启,下次登录需输入动态码"), http.StatusFound)
}

// handleUserTOTPDisable 关闭两步验证(需密码 + 动态码)
func (s *Server) handleUserTOTPDisable(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	pwd := r.FormValue("password")
	code := strings.TrimSpace(r.FormValue("code"))
	if u.TotpSecret == "" {
		http.Redirect(w, r, "/user/settings", http.StatusFound)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(pwd)) != nil {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("密码错误,两步验证未关闭"), http.StatusFound)
		return
	}
	if !service.VerifyTOTP(u.TotpSecret, code) {
		http.Redirect(w, r, "/user/settings?err="+urlEscape("动态码不正确,两步验证未关闭"), http.StatusFound)
		return
	}
	_ = s.Store.SetUserTOTP(u.ID, "")
	http.Redirect(w, r, "/user/settings?msg="+urlEscape("两步验证已关闭"), http.StatusFound)
}

// handleUserNotifyWebhook 保存收款成功 IM 推送地址
func (s *Server) handleUserNotifyWebhook(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	hook := strings.TrimSpace(r.FormValue("notify_webhook"))
	if hook != "" {
		if err := service.SendWebhookAlert(hook, s.DB.MustSetting("site_name"), "✅ 坤支付推送测试:配置成功,收款到账后将推送到这里。"); err != nil {
			http.Redirect(w, r, "/user/settings?err="+urlEscape("测试推送失败,请检查地址(支持飞书/钉钉/企业微信机器人)"), http.StatusFound)
			return
		}
	}
	if err := s.Store.SetUserNotifyWebhook(u.ID, hook); err != nil {
		http.Redirect(w, r, "/user/settings?err="+urlEscape(err.Error()), http.StatusFound)
		return
	}
	if hook == "" {
		http.Redirect(w, r, "/user/settings?msg="+urlEscape("IM 推送已关闭"), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/settings?msg="+urlEscape("保存成功,测试消息已发送到您的机器人"), http.StatusFound)
}

// handleUserOrderNotify 商户手动重发异步通知(仅本人已支付订单)
func (s *Server) handleUserOrderNotify(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	o := s.Store.OrderByID(atoint64(r.FormValue("id")))
	if o == nil || o.Pid != u.Pid {
		http.Redirect(w, r, "/user/orders?err="+urlEscape("订单不存在"), http.StatusFound)
		return
	}
	if o.Status != 1 && o.Status != 3 {
		http.Redirect(w, r, "/user/orders/"+strconv.FormatInt(o.ID, 10)+"?err="+urlEscape("仅已支付订单可重发通知"), http.StatusFound)
		return
	}
	s.Store.TriggerNotify(o.ID)
	http.Redirect(w, r, "/user/orders/"+strconv.FormatInt(o.ID, 10)+"?msg="+urlEscape("已发起重新通知,可在下方投递记录查看结果"), http.StatusFound)
}

// handleUserStatementExport 月度对账单导出(按日汇总 CSV)
func (s *Server) handleUserStatementExport(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	month := strings.TrimSpace(r.FormValue("month"))
	t, err := time.Parse("2006-01", month)
	if err != nil {
		now := time.Unix(model.Now(), 0)
		t = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	}
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).Unix()
	next := t.AddDate(0, 1, 0)
	end := time.Date(next.Year(), next.Month(), 1, 0, 0, 0, 0, next.Location()).Unix()

	type dayRow struct {
		day                           string
		total, ok, money, fee, income int64
	}
	var rows []dayRow
	qRows, qerr := s.DB.Query(`SELECT (addtime/86400)*86400 AS d, COUNT(*),
			SUM(CASE WHEN status IN(1,3) THEN 1 ELSE 0 END),
			COALESCE(SUM(CASE WHEN status IN(1,3) THEN money ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status IN(1,3) THEN fee ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN status IN(1,3) THEN money-fee ELSE 0 END),0)
		FROM orders WHERE pid=? AND addtime>=? AND addtime<? GROUP BY d ORDER BY d ASC`,
		u.Pid, start, end)
	if qerr == nil {
		for qRows.Next() {
			var ts int64
			var dr dayRow
			if qRows.Scan(&ts, &dr.total, &dr.ok, &dr.money, &dr.fee, &dr.income) == nil {
				dr.day = time.Unix(ts, 0).Format("2006-01-02")
				rows = append(rows, dr)
			}
		}
		qRows.Close()
	}

	var sumMoney, sumFee, sumIncome, sumOk, sumTotal int64
	for _, dr := range rows {
		sumMoney += dr.money
		sumFee += dr.fee
		sumIncome += dr.income
		sumOk += dr.ok
		sumTotal += dr.total
	}

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename=statement_"+month+".csv")
	// BOM 便于 Excel 识别中文
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF})
	bw := bufio.NewWriter(w)
	fmt.Fprintln(bw, "日期,订单数,成功笔数,成交额(元),手续费(元),净入账(元)")
	for _, dr := range rows {
		fmt.Fprintf(bw, "%s,%d,%d,%s,%s,%s\n", dr.day, dr.total, dr.ok,
			service.MoneyStr(dr.money), service.MoneyStr(dr.fee), service.MoneyStr(dr.income))
	}
	fmt.Fprintf(bw, "合计,%d,%d,%s,%s,%s\n", sumTotal, sumOk,
		service.MoneyStr(sumMoney), service.MoneyStr(sumFee), service.MoneyStr(sumIncome))
	_ = bw.Flush()
}

// handleDownload 公开下载中心: 列出数据目录 downloads/ 下的文件
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.DataDir, "downloads")
	_ = os.MkdirAll(dir, 0o755)
	type finfo struct {
		Name     string
		SizeText string
		ModTime  int64
	}
	var files []finfo
	if ents, err := os.ReadDir(dir); err == nil {
		for _, e := range ents {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			info, err := e.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				continue
			}
			size := info.Size()
			sizeText := fmt.Sprintf("%.1f KB", float64(size)/1024)
			if size >= 1024*1024 {
				sizeText = fmt.Sprintf("%.1f MB", float64(size)/1024/1024)
			}
			files = append(files, finfo{Name: e.Name(), SizeText: sizeText, ModTime: info.ModTime().Unix()})
		}
	}
	d := s.pageData(r)
	d["Page"] = "download"
	d["Title"] = "下载中心"
	d["Files"] = files
	s.render(w, "pub_download", http.StatusOK, d)
}

var downloadExtRe = regexp.MustCompile(`(?i)\.(apk|exe|dmg|pkg|zip|tar\.gz|tgz|msi|deb|rpm)$`)

// handleAdminDownloadsUpload 管理员上传文件到下载中心(限安装包格式,≤200MB)
func (s *Server) handleAdminDownloadsUpload(w http.ResponseWriter, r *http.Request) {
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("请选择文件"), http.StatusFound)
		return
	}
	defer file.Close()
	name := filepath.Base(header.Filename)
	if !downloadExtRe.MatchString(name) || len(name) > 120 ||
		strings.ContainsAny(name, "\\/") || strings.HasPrefix(name, ".") {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("仅支持 apk/exe/dmg/pkg/zip/tar.gz/tgz/msi/deb/rpm 安装包文件名"), http.StatusFound)
		return
	}
	if header.Size > 200<<20 {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("文件不能超过 200MB"), http.StatusFound)
		return
	}
	dir := filepath.Join(s.DataDir, "downloads")
	_ = os.MkdirAll(dir, 0o755)
	dst, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("保存文件失败"), http.StatusFound)
		return
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("写入文件失败"), http.StatusFound)
		return
	}
	s.audit(r, "download.upload", name)
	http.Redirect(w, r, "/admin/settings?msg="+urlEscape("文件已上传到下载中心: "+name), http.StatusFound)
}

// handleAdminDownloadsDelete 管理员删除下载中心文件
func (s *Server) handleAdminDownloadsDelete(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.FormValue("name"))
	if !downloadExtRe.MatchString(name) || strings.HasPrefix(name, ".") {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("文件名不合法"), http.StatusFound)
		return
	}
	if err := os.Remove(filepath.Join(s.DataDir, "downloads", name)); err != nil {
		http.Redirect(w, r, "/admin/settings?err="+urlEscape("删除失败(文件不存在?)"), http.StatusFound)
		return
	}
	s.audit(r, "download.delete", name)
	http.Redirect(w, r, "/admin/settings?msg="+urlEscape("已删除: "+name), http.StatusFound)
}

// handleDownloadFile 提供下载(严格防目录穿越)
func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.URL.Query().Get("name"))
	if name == "" || name == "." || name == "/" || strings.ContainsAny(name, "\\/") ||
		!regexp.MustCompile(`^[A-Za-z0-9._\-]+$`).MatchString(name) || strings.HasPrefix(name, ".") {
		http.Error(w, "invalid file name", 400)
		return
	}
	path := filepath.Join(s.DataDir, "downloads", name)
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	_, _ = io.Copy(w, f)
}

// ---------- 工单支持 ----------

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// handleUserTickets 商户工单列表 + 新建
func (s *Server) handleUserTickets(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	tickets, _ := s.Store.MerchantTickets(u.ID, 50)
	d := s.pageData(r)
	d["Page"] = "user_tickets"
	d["Title"] = "工单支持"
	d["Tickets"] = tickets
	d["Flash"] = r.URL.Query().Get("msg")
	d["FlashErr"] = r.URL.Query().Get("err")
	s.render(w, "dash_user_tickets", http.StatusOK, d)
}

func (s *Server) handleUserTicketCreate(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	id, err := s.Store.CreateTicket(u.ID, r.FormValue("subject"), r.FormValue("content"))
	if err != nil {
		http.Redirect(w, r, "/user/tickets?err="+urlEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/tickets/"+strconv.FormatInt(id, 10)+"?msg="+urlEscape("工单已提交,平台会尽快处理"), http.StatusFound)
}

func (s *Server) handleUserTicketView(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	id := atoint64(r.PathValue("id"))
	subject, status, _, err := s.Store.TicketOf(id, u.ID)
	if err != nil {
		renderMsg(s, w, r, "工单不存在", "工单不存在或无权查看", http.StatusNotFound)
		return
	}
	msgs, _ := s.Store.TicketMsgs(id)
	d := s.pageData(r)
	d["Page"] = "user_tickets"
	d["Title"] = "工单详情"
	d["TID"] = id
	d["Subject"] = subject
	d["Status"] = status
	d["Msgs"] = msgs
	d["Flash"] = r.URL.Query().Get("msg")
	d["FlashErr"] = r.URL.Query().Get("err")
	s.render(w, "dash_user_ticketdetail", http.StatusOK, d)
}

func (s *Server) handleUserTicketReply(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	id := atoint64(r.FormValue("id"))
	if _, _, owner, err := s.Store.TicketOf(id, u.ID); err != nil {
		http.Redirect(w, r, "/user/tickets?err="+urlEscape("工单不存在"), http.StatusFound)
		return
	} else if owner != u.ID {
		http.Redirect(w, r, "/user/tickets?err="+urlEscape("无权操作"), http.StatusFound)
		return
	}
	if err := s.Store.AddTicketMsg(id, false, r.FormValue("content")); err != nil {
		http.Redirect(w, r, "/user/tickets/"+strconv.FormatInt(id, 10)+"?err="+urlEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/tickets/"+strconv.FormatInt(id, 10)+"?msg="+urlEscape("回复已发送"), http.StatusFound)
}

func (s *Server) handleUserTicketClose(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	id := atoint64(r.FormValue("id"))
	if err := s.Store.CloseTicket(id, u.ID); err != nil {
		http.Redirect(w, r, "/user/tickets?err="+urlEscape(err.Error()), http.StatusFound)
		return
	}
	http.Redirect(w, r, "/user/tickets?msg="+urlEscape("工单已关闭"), http.StatusFound)
}
