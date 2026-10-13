package service

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
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

// GenerateRecoveryCodes 生成 8 个一次性恢复代码(展示用明文,形如 XXXX-XXXX)
func GenerateRecoveryCodes() []string {
	codes := make([]string, 8)
	for i := range codes {
		buf := make([]byte, 4)
		_, _ = rand.Read(buf)
		codes[i] = fmt.Sprintf("%04X-%04X", binary.BigEndian.Uint16(buf[:2]), binary.BigEndian.Uint16(buf[2:]))
	}
	return codes
}

// HashRecoveryCodes 明文恢复代码 → SHA256 十六进制 CSV(入库用)
func HashRecoveryCodes(codes []string) string {
	hashes := make([]string, 0, len(codes))
	for _, c := range codes {
		sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(c))))
		hashes = append(hashes, hex.EncodeToString(sum[:]))
	}
	return strings.Join(hashes, ",")
}

// ConsumeRecoveryCode 校验并消费一个恢复代码(命中即从库中移除,一次性)。
// 用 CAS(比较后交换)防止并发双用: 两个请求同时用同一码时只有一个能成功。
func (s *Store) ConsumeRecoveryCode(userID int64, code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return false
	}
	sum := sha256.Sum256([]byte(code))
	want := hex.EncodeToString(sum[:])
	for attempt := 0; attempt < 3; attempt++ {
		var raw string
		if err := s.DB.QueryRow("SELECT COALESCE(recovery_codes,'') FROM users WHERE id=?", userID).Scan(&raw); err != nil || raw == "" {
			return false
		}
		parts := strings.Split(raw, ",")
		consumed := false
		for i, h := range parts {
			if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
				parts = append(parts[:i], parts[i+1:]...)
				res, err := s.DB.Exec("UPDATE users SET recovery_codes=? WHERE id=? AND recovery_codes=?",
					strings.Join(parts, ","), userID, raw)
				if err != nil {
					return false
				}
				if n, _ := res.RowsAffected(); n == 1 {
					return true
				}
				consumed = false
				break // 期间被并发修改,重读重试
			}
		}
		if !consumed && attempt < 2 {
			continue
		}
	}
	return false
}

// RecoveryCodesRemaining 剩余可用恢复代码数量
func (s *Store) RecoveryCodesRemaining(userID int64) int {
	var raw string
	_ = s.DB.QueryRow("SELECT COALESCE(recovery_codes,'') FROM users WHERE id=?", userID).Scan(&raw)
	if raw == "" {
		return 0
	}
	return len(strings.Split(raw, ","))
}

// SetUserRecoveryCodes 写入恢复代码哈希
func (s *Store) SetUserRecoveryCodes(userID int64, hashedCSV string) error {
	_, err := s.DB.Exec("UPDATE users SET recovery_codes=? WHERE id=?", hashedCSV, userID)
	return err
}
