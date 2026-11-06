package api

import (
	"strings"
	"testing"
)

func TestNewIDShape(t *testing.T) {
	id := newID()

	if len(id) != 26 {
		t.Fatalf("长度 = %d, want 26（CHAR(26) 是库里主键的定义）", len(id))
	}
	for _, r := range id {
		if !strings.ContainsRune(crockford, r) {
			t.Fatalf("ID 含非法字符 %q: %s", r, id)
		}
	}
}

func TestNewIDUnique(t *testing.T) {
	seen := make(map[string]struct{}, 1000)
	for i := 0; i < 1000; i++ {
		id := newID()
		if _, dup := seen[id]; dup {
			t.Fatalf("生成了重复 ID: %s", id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewIDTimeOrdered 守住 ULID 的核心承诺：按字典序排就是时间序。
// 时间戳占前 10 位，同一毫秒内前缀相同，跨毫秒必须单调不减。
func TestNewIDTimeOrdered(t *testing.T) {
	prev := newID()
	for i := 0; i < 200; i++ {
		cur := newID()
		if cur[:10] < prev[:10] {
			t.Fatalf("时间前缀倒退: %s -> %s", prev, cur)
		}
		prev = cur
	}
}
