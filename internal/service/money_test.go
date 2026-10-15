package service

import "testing"

func TestParseMoney(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1.00", 100, false}, {"0.01", 1, false}, {"10", 1000, false},
		{"1234.50", 123450, false}, {"0.1", 10, false}, {"0.5", 50, false},
		{"1,234.50", 0, true}, {"1.999", 0, true}, {"1e3", 0, true},
		{"12ab", 0, true}, {"-5.00", -500, false}, {"-0.01", -1, false},
		{"", 0, true}, {"  ", 0, true}, {"99999999.99", 9999999999, false},
		{"1.234", 0, true}, {"abc", 0, true}, {"1.2.3", 0, true},
	}
	for _, c := range cases {
		got, err := ParseMoney(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseMoney(%q) 应报错,却得到 %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMoney(%q) 意外报错: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseMoney(%q) = %d, 期望 %d", c.in, got, c.want)
		}
	}
}

func TestMoneyStr(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{100, "1.00"}, {1, "0.01"}, {0, "0.00"}, {123450, "1234.50"},
		{-500, "-5.00"}, {1, "0.01"},
	}
	for _, c := range cases {
		if got := MoneyStr(c.in); got != c.want {
			t.Errorf("MoneyStr(%d) = %s, 期望 %s", c.in, got, c.want)
		}
	}
}

func TestSignAndVerify(t *testing.T) {
	params := map[string]string{
		"pid": "1001", "type": "alipay", "out_trade_no": "T1",
		"name": "商品", "money": "1.00", "notify_url": "", "sign": "junk", "sign_type": "MD5",
	}
	key := "testkey"
	sig := Sign(params, key)
	if len(sig) != 32 {
		t.Fatalf("签名长度异常: %s", sig)
	}
	params["sign"] = sig
	if !VerifySign(params, key) {
		t.Error("正确签名应通过")
	}
	params["money"] = "2.00"
	if VerifySign(params, key) {
		t.Error("金额被篡改后不应通过")
	}
	if VerifySign(map[string]string{"sign": sig}, key) {
		t.Error("缺参重放不应通过")
	}
}
