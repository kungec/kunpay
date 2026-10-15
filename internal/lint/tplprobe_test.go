package lint

import (
	"html/template"
	"os"
	"strings"
	"testing"
)

func TestDocTemplateWebhookBranch(t *testing.T) {
	src, err := os.ReadFile("../../web/templates/pages/pub/doc.html")
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := template.New("t").Parse(string(src))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	for page, marker := range map[string]string{
		"webhook": "order.paid", "refund": "act=refund", "monitor": "心跳地址", "rsa": "RSA-SHA256",
	} {
		var sb strings.Builder
		if err := tpl.ExecuteTemplate(&sb, "content", map[string]any{"DocPage": page}); err != nil {
			t.Fatalf("%s 执行失败: %v", page, err)
		}
		if !strings.Contains(sb.String(), marker) {
			t.Errorf("%s 分支未命中(输出 %d 字节)", page, sb.Len())
		}
	}
}
