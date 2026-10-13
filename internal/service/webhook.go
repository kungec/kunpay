package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrInvalidWebhook = errors.New("无效的 webhook 地址,仅支持 http(s)")

// SendWebhookAlert 发送 IM 机器人告警,按 URL 自动识别机器人类型:
// 飞书 / 钉钉 / 企业微信 / 通用格式({"text": msg})。
func SendWebhookAlert(rawURL, siteName, msg string) error {
	rawURL = strings.TrimSpace(rawURL)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return ErrInvalidWebhook
	}
	host := strings.ToLower(u.Host)
	payload := func() any {
		switch {
		case strings.Contains(host, "open.feishu.cn") || strings.Contains(host, "open.larksuite.com"):
			return map[string]any{"msg_type": "text", "content": map[string]string{"text": msg}}
		case strings.Contains(host, "dingtalk.com"):
			return map[string]any{"msgtype": "text", "text": map[string]string{"content": msg}}
		case strings.Contains(host, "qyapi.weixin.qq.com"):
			return map[string]any{"msgtype": "text", "text": map[string]string{"content": msg}}
		default:
			return map[string]any{"text": msg}
		}
	}()
	body, _ := json.Marshal(payload)
	client := &http.Client{Timeout: 6 * time.Second}
	resp, err := client.Post(rawURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		log.Printf("[webhook] 机器人告警响应异常: %s %s", resp.Status, u.Host)
	}
	return nil
}
