package service

import (
	"crypto"
	"crypto/md5" // #nosec G501 -- 易支付协议签名规定使用 MD5(兼容性要求,非安全哈希用途)
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// moneyRe 金额格式: 1-9 位整数 + 最多两位小数
var moneyRe = regexp.MustCompile(`^\d{1,9}(\.\d{1,2})?$`)

// SortedRaw 按易支付规则构造排序串(过滤空值与 sign/sign_type,ASCII 升序,a=b&c=d)
func SortedRaw(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v == "" || k == "sign" || k == "sign_type" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+params[k])
	}
	return strings.Join(parts, "&")
}

// Sign 按易支付标准签名规则计算签名:
// 1. 过滤空值、sign、sign_type
// 2. 参数名 ASCII 升序排序
// 3. 拼接为 a=b&c=d 形式(值不做 URL 编码)
// 4. 末尾拼接商户密钥后取 MD5,输出小写
func Sign(params map[string]string, key string) string {
	raw := SortedRaw(params) + key
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// GenerateRSAKeypair 生成 2048 位 RSA 密钥对(PEM)
func GenerateRSAKeypair() (pubPEM, privPEM string, err error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}
	privPEM = string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	pubBytes, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return "", "", err
	}
	pubPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubBytes}))
	return pubPEM, privPEM, nil
}

// SignRSA RSA-SHA256 签名,输出 Base64(平台私钥,用于通知签名)
func SignRSA(privPEM, data string) (string, error) {
	block, _ := pem.Decode([]byte(privPEM))
	if block == nil {
		return "", fmt.Errorf("私钥格式错误")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		if k, e2 := x509.ParsePKCS8PrivateKey(block.Bytes); e2 == nil {
			if rk, ok := k.(*rsa.PrivateKey); ok {
				key = rk
			} else {
				return "", e2
			}
		} else {
			return "", err
		}
	}
	sum := sha256.Sum256([]byte(data))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// VerifyRSA 校验商户请求的 RSA-SHA256 签名(商户公钥)
func VerifyRSA(pubPEM, data, signB64 string) bool {
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		return false
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		if k, e2 := x509.ParsePKCS1PublicKey(block.Bytes); e2 == nil {
			pubAny = k
		} else {
			return false
		}
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return false
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(signB64))
	if err != nil || len(sig) == 0 {
		return false
	}
	sum := sha256.Sum256([]byte(data))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig) == nil
}

// IsRSAType 请求是否使用 RSA 签名
func IsRSAType(signType string) bool {
	t := strings.ToUpper(strings.TrimSpace(signType))
	return t == "RSA" || t == "RSA2"
}

// VerifySign 校验签名(不变异原 map)
func VerifySign(params map[string]string, key string) bool {
	got := strings.ToLower(params["sign"])
	if got == "" {
		return false
	}
	want := Sign(params, key)
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// MoneyStr 分 -> 元字符串(两位小数,支持负数)
func MoneyStr(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s%d.%02d", sign, cents/100, cents%100)
}

// ParseMoney 元字符串 -> 分;最多两位小数,非法返回 error(不截断、不接受多余字符)
func ParseMoney(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("金额为空")
	}
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if !moneyRe.MatchString(s) {
		return 0, fmt.Errorf("金额格式错误(最多两位小数)")
	}
	intPart, fracPart := s, "0"
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" {
		intPart = "0"
	}
	// 小数位按位补齐: "1" → "10"(0.1 元 = 10 分,缺补零会缩小 10 倍)
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	var ip, fp int64
	fmt.Sscanf(intPart, "%d", &ip)
	fmt.Sscanf(fracPart, "%d", &fp)
	total := ip*100 + fp
	if neg {
		total = -total
	}
	return total, nil
}
