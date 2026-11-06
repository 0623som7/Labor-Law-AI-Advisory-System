package stream

import (
	"strings"
	"testing"

	"laborlaw/internal/port"
)

func TestEncodeFrame(t *testing.T) {
	ev := port.Event{Type: port.AgentEventDelta, Seq: 7, Text: "你好"}

	f, err := encodeFrame(ev.Type, toWire(ev))
	if err != nil {
		t.Fatalf("encodeFrame() 返回错误: %v", err)
	}

	got := string(f.data)
	wantPrefix := "id: 7\nevent: delta\ndata: {"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("帧前缀 = %q, want %q", got, wantPrefix)
	}
	if !strings.HasSuffix(got, "}\n\n") {
		t.Fatalf("帧必须以空行结尾，got %q", got)
	}
	if strings.Count(got, "\n\n") != 1 {
		t.Fatalf("一帧只能有一个空行分隔，got %q", got)
	}
	if !strings.Contains(got, `"text":"你好"`) {
		t.Fatalf("data 里缺正文: %q", got)
	}
	if !strings.Contains(got, `"seq":7`) {
		t.Fatalf("data 里缺 seq: %q", got)
	}
}

// TestEncodeFrameIsSingleLine 守住 SSE 最脆的约定：
// data 只能是单行，多行必须每行加前缀，否则浏览器会把后半段当新字段。
func TestEncodeFrameIsSingleLine(t *testing.T) {
	ev := port.Event{
		Type: port.AgentEventDelta,
		Seq:  1,
		Text: "第一行\n第二行", // 模型输出里真的有换行
	}

	f, err := encodeFrame(ev.Type, toWire(ev))
	if err != nil {
		t.Fatalf("encodeFrame() 返回错误: %v", err)
	}
	// 去掉结尾的空行后，帧体内部不应再有裸换行（\n 只出现在 "id:/event:/data:" 之后）。
	body := strings.TrimSuffix(string(f.data), "\n\n")
	for _, line := range strings.Split(body, "\n") {
		if line != "" && !strings.HasPrefix(line, "id: ") && !strings.HasPrefix(line, "event: ") && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("帧体里出现了不属于任何字段的行: %q（完整帧 %q）", line, body)
		}
	}
}

func TestDroppable(t *testing.T) {
	tests := []struct {
		typ  port.AgentEventType
		want bool
	}{
		{typ: port.AgentEventMeta, want: false},
		{typ: port.AgentEventDelta, want: false},
		{typ: port.AgentEventCitation, want: false},
		{typ: port.AgentEventDone, want: false},
		{typ: port.AgentEventError, want: false},
		{typ: port.AgentEventToolCall, want: true},
	}

	for _, tc := range tests {
		if got := droppable(tc.typ); got != tc.want {
			t.Fatalf("droppable(%s) = %v, want %v", tc.typ, got, tc.want)
		}
	}
}

func TestFrameBufferLimit(t *testing.T) {
	b := newFrameBuffer(100)

	for i := 1; i <= 10; i++ {
		payload := make([]byte, 30)
		b.add(frame{seq: i, data: payload})
	}

	if b.size > 100 {
		t.Fatalf("缓冲字节数 = %d, 超过上限 100", b.size)
	}
	if last := b.lastSeq(); last != 10 {
		t.Fatalf("lastSeq = %d, want 10", last)
	}
	// 老帧应当被挤掉：只剩最近的三四帧。
	if frames := b.from(0); len(frames) >= 10 {
		t.Fatalf("老帧没有被回收，剩 %d 帧", len(frames))
	}
}

func TestFrameBufferFrom(t *testing.T) {
	b := newFrameBuffer(1 << 20)
	for i := 1; i <= 5; i++ {
		b.add(frame{seq: i, data: []byte{byte(i)}})
	}

	got := b.from(2)
	if len(got) != 3 {
		t.Fatalf("from(2) 返回 %d 帧, want 3", len(got))
	}
	for i, f := range got {
		if f.seq != i+3 {
			t.Fatalf("第 %d 帧 seq = %d, want %d", i, f.seq, i+3)
		}
	}

	if got := b.from(99); len(got) != 0 {
		t.Fatalf("from(99) 应当为空, got %d 帧", len(got))
	}
}

func TestFrameBufferEmpty(t *testing.T) {
	b := newFrameBuffer(0) // 走默认值
	if b.lastSeq() != 0 {
		t.Fatalf("空缓冲 lastSeq 应为 0")
	}
	if got := b.from(0); len(got) != 0 {
		t.Fatalf("空缓冲 from(0) 应为空")
	}
}
