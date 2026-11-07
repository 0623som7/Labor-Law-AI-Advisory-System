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

// TestNewStrictlyIncreasing 守住同毫秒内的单调性。
//
// 一次请求里 user 与 assistant 两条消息几乎必然落在同一毫秒，若后 16 位纯随机，
// ORDER BY created_at DESC, id DESC 会把两条的先后随机翻转——对话历史被倒着
// 喂给模型。CI 上 sessions_test 的 TestMessageListReturnsLatest 就是这么红的。
func TestNewStrictlyIncreasing(t *testing.T) {
	const n = 5000

	ids := make([]string, n)
	for i := range ids {
		ids[i] = New()
	}
	for i := 1; i < n; i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("第 %d 枚没有严格递增: %s -> %s", i, ids[i-1], ids[i])
		}
	}
}

func TestIncrTail(t *testing.T) {
	tail := [16]byte{}
	copy(tail[:], "0000000000000000")
	if !incrTail(&tail) || string(tail[:]) != "0000000000000001" {
		t.Fatalf("加一结果 = %s", tail)
	}

	copy(tail[:], "0Z0000000000000Z")
	if !incrTail(&tail) || string(tail[:]) != "0Z00000000000010" {
		t.Fatalf("进位结果 = %s", tail)
	}

	for i := range tail {
		tail[i] = 'Z'
	}
	if incrTail(&tail) {
		t.Fatal("全 Z 应返回 false（表示 80 位溢出）")
	}
}
