package web

import (
	"html/template"
	"net/http"
	"strings"

	"kunzhifu/internal/db"
	"kunzhifu/internal/model"
	"kunzhifu/internal/service"
)

// handleHome 商户平台首页(参考 xpay 风格)
func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	d := s.pageData(r)
	d["Page"] = "home"
	d["Title"] = s.DB.MustSetting("site_title")
	s.render(w, "pub_home", http.StatusOK, d)
}

type docSection struct {
	Slug  string
	Title string
	HTML  template.HTML
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request) {
	page := r.PathValue("page")
	if page == "" {
		page = "submit"
	}
	d := s.pageData(r)
	d["Page"] = "doc"
	d["Title"] = "开发文档"
	d["DocPage"] = page
	s.render(w, "pub_doc", http.StatusOK, d)
}

// handleDemoPage 演示收银台(平台内置演示商户)
func (s *Server) handleDemoPage(w http.ResponseWriter, r *http.Request) {
	d := s.pageData(r)
	d["Page"] = "demo"
	d["Title"] = "支付测试"
	d["DemoTradeNo"] = genDemoNo()
	chs, _ := s.Store.AllChannels()
	var enabled []model.Channel
	for _, c := range chs {
		if c.Enabled == 1 {
			enabled = append(enabled, c)
		}
	}
	d["Channels"] = enabled
	s.render(w, "pub_demo", http.StatusOK, d)
}

func genDemoNo() string {
	return timeFormat(model.Now(), "20060102150405") + db.RandDigits(6)
}

// handleDemoCreate POST /demo/create 由演示页发起下单(内部演示商户,免签名)
func (s *Server) handleDemoCreate(w http.ResponseWriter, r *http.Request) {
	p := paramsOfForm(r)
	out := map[string]any{}
	typ := p["type"]
	if !allowTypes[typ] {
		typ = "alipay"
	}
	demoPid := atoint64(s.DB.MustSetting("demo_pid"))
	merchant, err := s.Store.UserByPid(demoPid)
	if err != nil || merchant == nil {
		out["code"] = -1
		out["msg"] = "演示商户不存在,请检查系统设置"
		writeJSON(w, out)
		return
	}
	req := &service.OrderReq{
		Type:       typ,
		OutTradeNo: "DEMO" + genDemoNo(),
		Name:       "支付测试",
		Money:      strings.TrimSpace(p["money"]),
		Sitename:   "支付测试",
		Device:     "pc",
		ReturnURL:  s.absoluteURL(r) + "/demo/done",
	}
	if req.Money == "" {
		req.Money = "1.00"
	}
	o, err := s.Store.CreateOrder(merchant, req, clientIP(r))
	if err != nil {
		out["code"] = -1
		out["msg"] = err.Error()
		writeJSON(w, out)
		return
	}
	out["code"] = 1
	out["pay_url"] = "/pay/" + o.TradeNo
	writeJSON(w, out)
}

func (s *Server) handleDemoDone(w http.ResponseWriter, r *http.Request) {
	d := s.pageData(r)
	d["Page"] = "demo_done"
	d["Title"] = "支付测试完成"
	s.render(w, "pub_demo_done", http.StatusOK, d)
}
