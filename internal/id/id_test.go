package id

import (
	"strings"
	"testing"
)

func TestNewShape(t *testing.T) {
	got := New()

	if len(got) != Len {
		t.Fatalf("长度 = %d, want %d", len(got), Len)
	}
	for _, r := range got {
		if !strings.ContainsRune(crockford, r) {
			t.Fatalf("ID 含非法字符 %q: %s", r, got)
		}
	}
}

func TestNewUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		v := New()
		if _, dup := seen[v]; dup {
			t.Fatalf("生成了重复 ID: %s", v)
		}
		seen[v] = struct{}{}
	}
}

// TestNewTimeOrdered 守住 ULID 的核心承诺：按字典序排就是时间序。
func TestNewTimeOrdered(t *testing.T) {
	prev := New()
	for i := 0; i < 200; i++ {
		cur := New()
		if cur[:10] < prev[:10] {
			t.Fatalf("时间前缀倒退: %s -> %s", prev, cur)
		}
		prev = cur
	}
}
