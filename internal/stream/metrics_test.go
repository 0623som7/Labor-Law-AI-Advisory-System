package stream

import (
	"net/http/httptest"
	"testing"
	"time"

	"laborlaw/internal/port"
)

func TestDroppedTotalCountsAcrossWriters(t *testing.T) {
	before := DroppedTotal()

	// 每个 Writer 队列长度为 1 且先塞满一帧：后续可丢帧只能落进 default 分支。
	// 不用真起写协程——卡死的客户端正是要测的那种情况。
	opts := Options{HeartbeatInterval: time.Hour, QueueSize: 1, CloseTimeout: 20 * time.Millisecond}

	for i := 0; i < 3; i++ {
		w, err := newWriter("m-pkg-metrics", httptest.NewRecorder(), opts, discardLogger(), func() {})
		if err != nil {
			t.Fatalf("newWriter() 返回错误: %v", err)
		}
		w.ch <- frame{typ: port.AgentEventMeta}
		if ok := w.enqueue(frame{typ: port.AgentEventToolCall}); !ok {
			t.Fatalf("tool_call 应当可丢，enqueue 却返回 false")
		}
		// 单个 Writer 的计数各自为 1，包级计数应当是三者的累计。
		if got := w.Dropped(); got != 1 {
			t.Fatalf("单个 Writer 的 dropped = %d, want 1", got)
		}
	}

	if got, want := DroppedTotal()-before, int64(3); got != want {
		t.Fatalf("DroppedTotal 增量 = %d, want %d（包级计数必须跨 Writer 累计）", got, want)
	}
}

// TestDroppedTotalIgnoresCriticalFrames 确保关键帧不会虚增计数器：
// delta/done 被拒时 Hub 是"踢订阅者"而不是"丢帧"，没丢就不能记。
func TestDroppedTotalIgnoresCriticalFrames(t *testing.T) {
	before := DroppedTotal()

	w, err := newWriter("m-pkg-metrics-2", httptest.NewRecorder(),
		Options{HeartbeatInterval: time.Hour, QueueSize: 1, CloseTimeout: 20 * time.Millisecond}, discardLogger(), func() {})
	if err != nil {
		t.Fatalf("newWriter() 返回错误: %v", err)
	}
	w.ch <- frame{typ: port.AgentEventMeta}

	if ok := w.enqueue(frame{typ: port.AgentEventDelta}); ok {
		t.Fatalf("delta 是关键帧，队列满时 enqueue 应返回 false")
	}
	if got := DroppedTotal() - before; got != 0 {
		t.Fatalf("DroppedTotal 增量 = %d, want 0（关键帧被拒不算丢帧）", got)
	}
}
