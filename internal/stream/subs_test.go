package stream

import (
	"testing"
	"time"

	"laborlaw/internal/port"
)

// TestSubscribersCountsAttachAndDetach 钉住最基础的一对加减：
// 挂两个订阅者读 2，关掉一个读 1，再关掉读 0。
func TestSubscribersCountsAttachAndDetach(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: time.Hour})

	if got := h.Subscribers(); got != 0 {
		t.Fatalf("新建 Hub 的 Subscribers = %d, want 0", got)
	}

	w1, _ := attach(t, h, "m1", 0)
	w2, _ := attach(t, h, "m2", 0)
	if got := h.Subscribers(); got != 2 {
		t.Fatalf("挂两条后 Subscribers = %d, want 2", got)
	}

	if err := w1.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}
	if got := h.Subscribers(); got != 1 {
		t.Fatalf("关一条后 Subscribers = %d, want 1", got)
	}

	if err := w2.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}
	if got := h.Subscribers(); got != 0 {
		t.Fatalf("全关掉后 Subscribers = %d, want 0", got)
	}
}

// TestSubscribersGuardsDoubleDetach 钉住重复摘除不会把计数减成负数。
//
// 真实路径里 Writer.Close 有 closeOnce 兜底，但 detach 是包内函数，
// 将来若有人从别处再调一次，负数会让 /metrics 出现 -1。
func TestSubscribersGuardsDoubleDetach(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: time.Hour})
	w, _ := attach(t, h, "m1", 0)
	defer func() { _ = w.Close() }()

	if got := h.Subscribers(); got != 1 {
		t.Fatalf("挂上后 Subscribers = %d, want 1", got)
	}

	h.detach("m1", w)
	if got := h.Subscribers(); got != 0 {
		t.Fatalf("第一次 detach 后 Subscribers = %d, want 0", got)
	}

	h.detach("m1", w)
	if got := h.Subscribers(); got != 0 {
		t.Fatalf("重复 detach 后 Subscribers = %d, want 0（不能变负数）", got)
	}
}

// TestSubscribersAfterStreamEvicted 钉住"流被 TTL 回收后订阅者数不受影响"：
// 订阅者计数只跟 Writer 有关，回收的是帧缓冲，两者不能互相干扰。
func TestSubscribersAfterStreamEvicted(t *testing.T) {
	h := NewHub(Options{HeartbeatInterval: time.Hour}, time.Minute, discardLogger())
	base := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return base }

	w, _ := attach(t, h, "m1", 0)
	if got := h.Subscribers(); got != 1 {
		t.Fatalf("挂上后 Subscribers = %d, want 1", got)
	}
	if got := h.Streams(); got != 1 {
		t.Fatalf("Streams = %d, want 1", got)
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}
	// 关掉之后流还在（TTL 内保留帧，供断线续传），但订阅者已经是 0。
	if got := h.Subscribers(); got != 0 {
		t.Fatalf("关闭后 Subscribers = %d, want 0", got)
	}
	if got := h.Streams(); got != 1 {
		t.Fatalf("TTL 内 Streams = %d, want 1（空闲流保留帧）", got)
	}

	// 时间推过 TTL，再挂一条新流触发 evictLocked。
	h.now = func() time.Time { return base.Add(2 * time.Minute) }
	w2, _ := attach(t, h, "m2", 0)
	defer func() { _ = w2.Close() }()

	if got := h.Streams(); got != 1 {
		t.Fatalf("回收后 Streams = %d, want 1（m1 应已过期）", got)
	}
	if got := h.Subscribers(); got != 1 {
		t.Fatalf("回收后 Subscribers = %d, want 1（只算新挂的这条）", got)
	}
	if _, ok := h.streams["m1"]; ok {
		t.Fatal("m1 应当已被回收")
	}
}

// TestSubscribersAfterPublishEvicts 覆盖另一条回收触发路径（publish）：
// 空闲流被 publish 顺手清掉时，订阅者计数不能跟着掉。
func TestSubscribersAfterPublishEvicts(t *testing.T) {
	h := NewHub(Options{HeartbeatInterval: time.Hour}, time.Minute, discardLogger())
	base := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return base }

	w, _ := attach(t, h, "m1", 0)
	if err := w.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}
	if got := h.Subscribers(); got != 0 {
		t.Fatalf("关闭后 Subscribers = %d, want 0", got)
	}

	h.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := h.Publish("m2", port.Event{Type: port.AgentEventDelta}); err != nil {
		t.Fatalf("Publish 返回错误: %v", err)
	}

	if got := h.Subscribers(); got != 0 {
		t.Fatalf("Publish 不是订阅，Subscribers = %d, want 0", got)
	}
	if got := h.Streams(); got != 1 {
		t.Fatalf("Streams = %d, want 1（只剩 m2）", got)
	}
}
