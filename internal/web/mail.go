package web

import (
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
)

// SMTPConf 邮件服务器配置(存于系统设置)
type SMTPConf struct {
	Host string
	Port int
	User string
	Pass string
	TLS  string // ssl(465直连) / starttls(587) / none(25)
	From string // 发件人,缺省用 User
}

func (s *Server) smtpConf() SMTPConf {
	c := SMTPConf{
		Host: s.DB.MustSetting("smtp_host"),
		User: s.DB.MustSetting("smtp_user"),
		Pass: s.DB.MustSetting("smtp_pass"),
		TLS:  s.DB.MustSetting("smtp_tls"),
	}
	c.Port = atoiDefault2(s.DB.MustSetting("smtp_port"), 465)
	if c.TLS == "" {
		c.TLS = "ssl"
	}
	c.From = s.DB.MustSetting("smtp_from")
	if c.From == "" {
		c.From = c.User
	}
	return c
}

func (c SMTPConf) ready() bool {
	return c.Host != "" && c.Port > 0 && c.From != ""
}

func atoiDefault2(s string, def int) int {
	v, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || v <= 0 || v > 65535 {
		return def
	}
	return v
}

// SendMail 发送简单 HTML 邮件(主题按 RFC2047 编码,支持中文)
func (s *Server) SendMail(to, subject, htmlBody string) error {
	c := s.smtpConf()
	if !c.ready() {
		return fmt.Errorf("SMTP 未配置")
	}
	addr := c.Host + ":" + strconv.Itoa(c.Port)
	subject = "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(subject)) + "?="
	from := c.From
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/html; charset=UTF-8",
		"",
		htmlBody,
	}, "\r\n")

	var conn net.Conn
	var err error
	dial := func() (net.Conn, error) { return net.Dial("tcp", addr) }
	tlsDial := func() (net.Conn, error) {
		return tls.Dial("tcp", addr, &tls.Config{ServerName: c.Host})
	}
	switch c.TLS {
	case "ssl":
		conn, err = tlsDial()
	default:
		conn, err = dial()
	}
	if err != nil {
		return fmt.Errorf("连接邮件服务器失败: %w", err)
	}
	client, err := smtp.NewClient(conn, c.Host)
	if err != nil {
		conn.Close()
		return fmt.Errorf("SMTP 握手失败: %w", err)
	}
	defer client.Close()

	if c.TLS == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err = client.StartTLS(&tls.Config{ServerName: c.Host}); err != nil {
				return fmt.Errorf("STARTTLS 失败: %w", err)
			}
		}
	}
	if c.User != "" && c.Pass != "" {
		auth := smtp.PlainAuth("", c.User, c.Pass, c.Host)
		if err = client.Auth(auth); err != nil {
			return fmt.Errorf("SMTP 认证失败: %w", err)
		}
	}
	if err = client.Mail(from); err != nil {
		return err
	}
	if err = client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = w.Write([]byte(msg)); err != nil {
		return err
	}
	if err = w.Close(); err != nil {
		return err
	}
	return client.Quit()
}

// 邮件 HTML 模板(简单封装,视觉与平台一致)
func mailHTML(title string, lines []string) string {
	var b strings.Builder
	b.WriteString(`<div style="max-width:560px;margin:0 auto;font-family:-apple-system,'PingFang SC','Microsoft YaHei',sans-serif;">`)
	b.WriteString(`<div style="background:linear-gradient(120deg,#2e63f6,#5b8bff);border-radius:14px 14px 0 0;padding:22px 26px;color:#fff;font-size:18px;font-weight:700;">KunPay 码支付</div>`)
	b.WriteString(`<div style="border:1px solid #eef1f7;border-top:0;border-radius:0 0 14px 14px;padding:24px 26px;color:#1f2329;">`)
	b.WriteString(`<div style="font-size:16px;font-weight:700;margin-bottom:14px;">` + title + `</div>`)
	for _, l := range lines {
		b.WriteString(`<div style="padding:5px 0;color:#4e5969;font-size:14px;">` + l + `</div>`)
	}
	b.WriteString(`<div style="margin-top:18px;padding-top:14px;border-top:1px dashed #e5e6eb;color:#86909c;font-size:12px;">本邮件由系统自动发送,请勿回复。</div>`)
	b.WriteString(`</div></div>`)
	return b.String()
}
