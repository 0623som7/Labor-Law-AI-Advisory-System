package auth

import (
	"strings"
	"testing"
)

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("password123")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if hash == "password123" {
		t.Fatal("哈希不能等于明文")
	}
	// cost 12 的 bcrypt 前缀形如 $2a$12$。
	if !strings.HasPrefix(hash, "$2a$12$") {
		t.Fatalf("cost 不是 12: %s", hash[:7])
	}
	if err := VerifyPassword(hash, "password123"); err != nil {
		t.Fatalf("正确密码应当通过: %v", err)
	}
	if err := VerifyPassword(hash, "password124"); err == nil {
		t.Fatal("错误密码不应当通过")
	}
}

func TestHashPasswordSalted(t *testing.T) {
	a, err := HashPassword("password123")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	b, err := HashPassword("password123")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if a == b {
		t.Fatal("同一明文两次哈希应当不同（盐生效）")
	}
}

func TestHashPasswordRejectsBadInput(t *testing.T) {
	if _, err := HashPassword(""); err == nil {
		t.Fatal("空密码应当报错")
	}
	if _, err := HashPassword(strings.Repeat("a", 73)); err == nil {
		t.Fatal("超过 72 字节应当报错")
	}
}

func TestVerifyPasswordEmptyHash(t *testing.T) {
	if err := VerifyPassword("", "password123"); err == nil {
		t.Fatal("空哈希不应当通过")
	}
	if err := VerifyPassword("not-a-bcrypt-hash", "password123"); err == nil {
		t.Fatal("非法哈希不应当通过")
	}
}

func TestVerifyPasswordAlways(t *testing.T) {
	// 存在用户且密码正确。
	hash, err := HashPassword("password123")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if err := VerifyPasswordAlways(hash, "password123"); err != nil {
		t.Fatalf("正确密码应当通过: %v", err)
	}
	if err := VerifyPasswordAlways(hash, "nope"); err == nil {
		t.Fatal("错误密码不应当通过")
	}

	// 用户不存在（空哈希）：无论密码是什么都必须失败，且仍然执行一次 bcrypt。
	if err := VerifyPasswordAlways("", "anything"); err == nil {
		t.Fatal("空哈希必须失败")
	}
	if err := VerifyPasswordAlways("", ""); err == nil {
		t.Fatal("空哈希 + 空密码必须失败")
	}
}

func TestDummyHashIsValidBcrypt(t *testing.T) {
	// dummyHash 必须能被 bcrypt 解析，否则"用户不存在"路径会走一条完全不同的
	// 快速失败分支，计时差异依然存在。
	if err := VerifyPassword(dummyHash, "definitely-not-the-password"); err == nil {
		t.Fatal("dummyHash 不应当匹配任意密码")
	}
}
