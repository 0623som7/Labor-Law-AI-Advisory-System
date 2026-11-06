package auth

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	ok := []struct{ in, want string }{
		{"13800138000", "13800138000"},
		{" 13800138000 ", "13800138000"},
		{"138-0013-8000", "13800138000"},
		{"138 0013 8000", "13800138000"},
		{"19912345678", "19912345678"},
	}
	for _, c := range ok {
		got, err := Normalize(c.in)
		if err != nil {
			t.Fatalf("%q 应当合法: %v", c.in, err)
		}
		if got != c.want {
			t.Fatalf("%q 规整为 %q，期望 %q", c.in, got, c.want)
		}
	}

	bad := []string{
		"",
		"1380013800",   // 10 位
		"138001380000", // 12 位
		"12800138000",  // 第二位不合法
		"23800138000",  // 不是 1 开头
		"+8613800138000",
		"1380013800a",
		"１３８００１３８０００", // 全角数字
	}
	for _, in := range bad {
		if _, err := Normalize(in); !errors.Is(err, ErrPhoneFormat) {
			t.Fatalf("%q 应当被判非法，实得 %v", in, err)
		}
	}
}

func TestMask(t *testing.T) {
	cases := []struct{ in, want string }{
		{"13800138000", "138****8000"},
		{"19912345678", "199****5678"},
		{"", ""},
		{"123", "***"},
	}
	for _, c := range cases {
		if got := Mask(c.in); got != c.want {
			t.Fatalf("Mask(%q)=%q，期望 %q", c.in, got, c.want)
		}
	}
	// 脱敏结果里不能残留中间四位。
	if strings.Contains(Mask("13800138000"), "0013") {
		t.Fatal("脱敏后仍含号码中段")
	}
}

func TestHashPhoneIsStableAndSecretBound(t *testing.T) {
	a, err := Hash(testSecret, "13800138000")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	b, err := Hash(testSecret, "13800138000")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if a != b {
		t.Fatal("同一密钥同一号码应当得到相同哈希（登录检索依赖它）")
	}
	if len(a) != 64 {
		t.Fatalf("哈希应当是 64 位 hex，实得 %d", len(a))
	}
	other, err := Hash("ffffffffffffffffffffffffffffffff", "13800138000")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if a == other {
		t.Fatal("换密钥后哈希必须变化（防止拖库后离线枚举）")
	}
}

func TestHashPhoneRequiresSecret(t *testing.T) {
	if _, err := Hash("", "13800138000"); !errors.Is(err, ErrTokenSecret) {
		t.Fatalf("空密钥应当报错，实得 %v", err)
	}
}
