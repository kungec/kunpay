package web

import "testing"

// 全回路测试: 生成 → 正确码验证通过 → 验证后销毁(一次性) → 错误码拒绝
// (e2e 回归以 -captcha=false 运行,验证码回路仅由此单测覆盖)
func TestCaptchaRoundTrip(t *testing.T) {
	id, png := NewCaptchaForTest()
	if len(png) < 100 {
		t.Fatalf("验证码图片异常: %d 字节", len(png))
	}
	code := LastCodeForTest()
	if len(code) != 4 {
		t.Fatalf("验证码长度异常: %q", code)
	}
	if !VerifyCaptchaForTest(id, code) {
		t.Error("正确验证码应通过")
	}
	if VerifyCaptchaForTest(id, code) {
		t.Error("验证码应一次性销毁,二次验证应失败")
	}

	// 大小写不敏感 + 前后空格容忍(输入习惯)
	id2, _ := NewCaptchaForTest()
	code2 := LastCodeForTest()
	if !VerifyCaptchaForTest(id2, " "+strings_ToUpper(code2)+" ") {
		t.Errorf("大小写/空格变体应通过: %q", code2)
	}
	// 错误码拒绝
	if VerifyCaptchaForTest(id2, "aaaa") && code2 != "aaaa" {
		t.Error("错误验证码不应通过")
	}
	// 未知 id 拒绝
	if VerifyCaptchaForTest("nonexistent", code2) {
		t.Error("未知 id 不应通过")
	}
}

// 全角字符归一化测试(中文输入法全角是"验证码错误"的高发原因,Windows 尤甚)
func TestCaptchaFullWidthNormalize(t *testing.T) {
	id, _ := NewCaptchaForTest()
	code := LastCodeForTest()
	// 构造全角版本
	var fw []rune
	for _, r := range code {
		if r >= 0x21 && r <= 0x7e {
			fw = append(fw, r+0xfee0)
		} else {
			fw = append(fw, r)
		}
	}
	if !VerifyCaptchaForTest(id, string(fw)) {
		t.Errorf("全角输入应归一化后通过: code=%q 输入=%q", code, string(fw))
	}
}

func strings_ToUpper(s string) string {
	b := []rune(s)
	for i, r := range b {
		if r >= 'a' && r <= 'z' {
			b[i] = r - 32
		}
	}
	return string(b)
}
