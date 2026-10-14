package service

import "testing"

func TestIsInternalURLStrictVariants(t *testing.T) {
	blocked := []string{
		"http://0177.0.0.1:8799/x", "http://127.1:8799/x", "http://0x7f.1:8799/x",
		"http://2130706433:8799/x", "http://0x7f000001/x", "http://127.0.0.1/x",
		"http://[::1]/x", "http://10.0.0.1/x", "http://169.254.169.254/x",
		"http://172.16.0.9/x", "http://192.168.9/x", "http://0177.1/x",
	}
	for _, u := range blocked {
		if !IsInternalURLStrict(u) {
			t.Errorf("应当拦截: %s", u)
		}
	}
	allowed := []string{
		"https://merchant.example/notify", "http://0a.example.com/x",
	}
	for _, u := range allowed {
		if IsInternalURLStrict(u) {
			t.Errorf("不应误伤: %s", u)
		}
	}
}

func TestIsInternalURLStrictNumericInvalid(t *testing.T) {
	blocked := []string{
		"http://127.0.0.256/x", "http://999.999.999.999/x", "http://4294967296/x",
		"http://0177.0.0.1:8799/x", "http://127.1/x", "http://0x7f.1/x",
	}
	for _, u := range blocked {
		if !IsInternalURLStrict(u) {
			t.Errorf("应当拦截: %s", u)
		}
	}
	allowed := []string{
		"http://177.0.0.1/x", "https://merchant.example/n", "http://0a.example.com/x",
	}
	for _, u := range allowed {
		if IsInternalURLStrict(u) {
			t.Errorf("不应误伤: %s", u)
		}
	}
}
