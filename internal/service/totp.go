package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const totpPeriod = 30

// GenerateTOTPSecret 生成新的 TOTP 密钥(base32, 无填充,20 字节随机)
func GenerateTOTPSecret() string {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand 失败极罕见,退化为时间混合熵
		now := uint64(time.Now().UnixNano())
		for i := range buf {
			buf[i] = byte(now >> (uint(i%8) * 8))
			now = now*6364136223846793005 + 1442695040888963407
		}
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf)
}

// totpCode 计算指定时间步的 6 位动态码
func totpCode(secretB32 string, counter uint64) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(
		strings.ToUpper(strings.ReplaceAll(secretB32, " ", "")))
	if err != nil || len(key) == 0 {
		return "", fmt.Errorf("密钥格式错误")
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	code := (binary.BigEndian.Uint32(sum[off:off+4]) & 0x7fffffff) % 1000000
	return fmt.Sprintf("%06d", code), nil
}

// VerifyTOTP 校验动态码,允许前后各一个周期(30s)的时钟偏差
func VerifyTOTP(secret, code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 6 {
		return false
	}
	counter := uint64(time.Now().Unix()) / totpPeriod
	for _, d := range []uint64{0, 1, ^uint64(0)} { // 当前/上一步/下一步
		want, err := totpCode(secret, counter+d)
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return true
		}
	}
	return false
}

// OTPAuthURL 生成验证器 APP 扫码用的 otpauth:// 链接
func OTPAuthURL(secret, account, issuer string) string {
	return "otpauth://totp/" + url.PathEscape(issuer+":"+account) +
		"?secret=" + secret + "&issuer=" + url.QueryEscape(issuer) +
		"&algorithm=SHA1&digits=6&period=" + fmt.Sprint(totpPeriod)
}
