package stream

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"laborlaw/internal/port"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestHub(opts Options) *Hub {
	return NewHub(opts, time.Minute, discardLogger())
}

// attach 建连接并返回 writer 与 recorder。调用方必须 Close 之后再读 body，
// 否则会与写协程并发读同一个 buffer（-race 会红）。
func attach(t *testing.T, h *Hub, key string, lastEventID int) (*Writer, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	w, err := h.Attach(context.Background(), key, lastEventID, rec)
	if err != nil {
		t.Fatalf("Attach() 返回错误: %v", err)
	}
	return w, rec
}

func TestAttachSetsSSEHeaders(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: time.Hour})
	w, rec := attach(t, h, "m1", 0)
	defer func() { _ = w.Close() }()

	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-cache") {
		t.Fatalf("Cache-Control = %q", got)
	}
	if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}
}

func TestPublishDeliversFrames(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: time.Hour})
	w, rec := attach(t, h, "m1", 0)

	if _, err := h.Publish("m1", port.Event{Type: port.AgentEventMeta}); err != nil {
		t.Fatalf("Publish(meta) 返回错误: %v", err)
	}
	if _, err := h.Publish("m1", port.Event{Type: port.AgentEventDelta, Text: "你好"}); err != nil {
		t.Fatalf("Publish(delta) 返回错误: %v", err)
	}
	if seq, err := h.Publish("m1", port.Event{Type: port.AgentEventDone}); err != nil || seq != 3 {
		t.Fatalf("Publish(done) = (%d, %v), want (3, nil)", seq, err)
	}

	// Close 会先摘订阅再关队列；写协程会把队列里剩下的帧写完才退出，
	// 所以 Close 返回之后读 body 既完整、也不会有并发读写。
	if err := w.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}

	body := rec.Body.String()
	for _, want := range []string{"id: 1\nevent: meta", "id: 2\nevent: delta", `"text":"你好"`, "id: 3\nevent: done"} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应体缺少 %q\n---\n%s", want, body)
		}
	}
}

func TestPublishBeforeAttachReplaysFromStart(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: time.Hour})

	// Agent 先跑，浏览器后连：晚到的客户端应当看到完整回答。
	if _, err := h.Publish("m1", port.Event{Type: port.AgentEventDelta, Text: "先到的内容"}); err != nil {
		t.Fatalf("Publish 返回错误: %v", err)
	}

	w, rec := attach(t, h, "m1", 0)
	if err := w.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}

	if !strings.Contains(rec.Body.String(), "先到的内容") {
		t.Fatalf("晚连接没有补发历史帧:\n%s", rec.Body.String())
	}
}

func TestResumeFromLastEventID(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: time.Hour})

	for i := 1; i <= 3; i++ {
		if _, err := h.Publish("m1", port.Event{Type: port.AgentEventDelta, Text: "片段"}); err != nil {
			t.Fatalf("Publish 返回错误: %v", err)
		}
	}

	w, rec := attach(t, h, "m1", 1) // 客户端说"我收到第 1 帧"
	if err := w.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}

	body := rec.Body.String()
	if strings.Contains(body, "id: 1\n") {
		t.Fatalf("不该重发第 1 帧:\n%s", body)
	}
	if !strings.Contains(body, "id: 2\n") || !strings.Contains(body, "id: 3\n") {
		t.Fatalf("应当补发第 2、3 帧:\n%s", body)
	}
}

func TestHeartbeat(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: 20 * time.Millisecond})
	w, rec := attach(t, h, "m1", 0)

	time.Sleep(70 * time.Millisecond)
	if err := w.Close(); err != nil {
		t.Fatalf("Close() 返回错误: %v", err)
	}

	if !strings.Contains(rec.Body.String(), ": ping") {
		t.Fatalf("没有发出心跳注释帧:\n%q", rec.Body.String())
	}
}

func TestMultipleSubscribersShareSeq(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: time.Hour})
	w1, rec1 := attach(t, h, "m1", 0)
	w2, rec2 := attach(t, h, "m1", 0)

	if _, err := h.Publish("m1", port.Event{Type: port.AgentEventDelta, Text: "广播"}); err != nil {
		t.Fatalf("Publish 返回错误: %v", err)
	}

	_ = w1.Close()
	_ = w2.Close()

	if !strings.Contains(rec1.Body.String(), "id: 1") || !strings.Contains(rec2.Body.String(), "id: 1") {
		t.Fatalf("两个订阅者应当收到同一 seq")
	}
}

func TestSlowSubscriberIsDetached(t *testing.T) {
	h := newTestHub(Options{HeartbeatInterval: time.Hour, QueueSize: 1, CloseTimeout: 20 * time.Millisecond})

	rec := httptest.NewRecorder()
	// 模拟卡死的客户端：不给它起写协程，队列会一直满。
	var w *Writer
	var err error
	w, err = newWriter("m1", rec, Options{HeartbeatInterval: time.Hour, QueueSize: 1, CloseTimeout: 20 * time.Millisecond}, discardLogger(), func() {
		h.detach("m1", w)
	})
	if err != nil {
		t.Fatalf("newWriter() 返回错误: %v", err)
	}
	w.ch <- frame{typ: port.AgentEventMeta}

	h.mu.Lock()
	s := h.streamLocked("m1")
	s.subs[w] = struct{}{}
	h.mu.Unlock()

	// tool_call 可以丢，不踢人。
	if _, err := h.Publish("m1", port.Event{Type: port.AgentEventToolCall}); err != nil {
		t.Fatalf("Publish(tool_call) 返回错误: %v", err)
	}
	if got := w.Dropped(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}

	// delta 不能丢：队列满时必须踢掉这个订阅者，而不是拖住整个流。
	if _, err := h.Publish("m1", port.Event{Type: port.AgentEventDelta, Text: "关键"}); err != nil {
		t.Fatalf("Publish(delta) 返回错误: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		_, stillThere := h.streams["m1"].subs[w]
		h.mu.Unlock()
		if !stillThere {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("慢订阅者没有被摘除")
}

func TestStreamTTLEviction(t *testing.T) {
	h := NewHub(Options{HeartbeatInterval: time.Hour}, time.Minute, discardLogger())
	base := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return base }

	// 建一条没人订阅的流。
	if _, err := h.Publish("m1", port.Event{Type: port.AgentEventDelta}); err != nil {
		t.Fatalf("Publish 返回错误: %v", err)
	}
	if got := h.Streams(); got != 1 {
		t.Fatalf("Streams = %d, want 1", got)
	}

	// 时间前进 2 分钟，再触发一次 evict。
	h.now = func() time.Time { return base.Add(2 * time.Minute) }
	if _, err := h.Publish("m2", port.Event{Type: port.AgentEventDelta}); err != nil {
		t.Fatalf("Publish 返回错误: %v", err)
	}

	if got := h.Streams(); got != 1 {
		t.Fatalf("过期流没有被回收，Streams = %d, want 1", got)
	}
	if _, ok := h.streams["m1"]; ok {
		t.Fatal("m1 应当已被回收")
	}
}
