package service

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Sign 按易支付标准签名规则计算签名:
// 1. 过滤空值、sign、sign_type
// 2. 参数名 ASCII 升序排序
// 3. 拼接为 a=b&c=d 形式(值不做 URL 编码)
// 4. 末尾拼接商户密钥后取 MD5,输出小写
func Sign(params map[string]string, key string) string {
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
	raw := strings.Join(parts, "&") + key
	sum := md5.Sum([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// VerifySign 校验签名(不变异原 map)
func VerifySign(params map[string]string, key string) bool {
	got := strings.ToLower(params["sign"])
	if got == "" {
		return false
	}
	want := Sign(params, key)
	return got == want
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

// ParseMoney 元字符串 -> 分;最多两位小数,非法返回 error
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
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if fracPart == "" {
		fracPart = "0"
	}
	if len(fracPart) > 2 {
		// 超过两位小数:截断到分
		fracPart = fracPart[:2]
	}
	if intPart == "" {
		intPart = "0"
	}
	var ip, fp int64
	if _, err := fmt.Sscanf(intPart, "%d", &ip); err != nil {
		return 0, fmt.Errorf("金额格式错误")
	}
	// 补齐两位解析小数
	for len(fracPart) < 2 {
		fracPart += "0"
	}
	if _, err := fmt.Sscanf(fracPart, "%d", &fp); err != nil {
		return 0, fmt.Errorf("金额格式错误")
	}
	total := ip*100 + fp
	if neg {
		total = -total
	}
	return total, nil
}
