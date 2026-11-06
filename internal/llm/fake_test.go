package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"laborlaw/internal/port"
)

func TestFakeClientChat(t *testing.T) {
	f := &FakeClient{Reply: "你可以申请劳动仲裁。"}

	resp, err := f.Chat(context.Background(), port.ChatRequest{Scene: SceneConsult})
	if err != nil {
		t.Fatalf("Chat() 返回错误: %v", err)
	}
	if resp.Content != "你可以申请劳动仲裁。" {
		t.Fatalf("Content = %q", resp.Content)
	}
}

func TestFakeClientChatError(t *testing.T) {
	sentinel := errors.New("fake failure")
	f := &FakeClient{Err: sentinel}

	if _, err := f.Chat(context.Background(), port.ChatRequest{Scene: SceneConsult}); !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
	if _, err := f.ChatStream(context.Background(), port.ChatRequest{Scene: SceneConsult}, func(string) error { return nil }); !errors.Is(err, sentinel) {
		t.Fatalf("stream err = %v, want sentinel", err)
	}
}

// TestFakeClientStreamKeepsRunesIntact 保证按 rune 切分：
// 按字节切会把一个汉字劈成两半，前端拼出来就是乱码。
func TestFakeClientStreamKeepsRunesIntact(t *testing.T) {
	text := "根据《劳动合同法》第三十八条，你可以解除劳动合同并要求经济补偿。"
	f := &FakeClient{Reply: text}

	var got []string
	resp, err := f.ChatStream(context.Background(), port.ChatRequest{Scene: SceneConsult}, func(d string) error {
		if !utf8.ValidString(d) {
			t.Fatalf("片段不是合法 UTF-8: %q", d)
		}
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() 返回错误: %v", err)
	}
	if strings.Join(got, "") != text {
		t.Fatalf("拼接结果 = %q", strings.Join(got, ""))
	}
	if resp.Content != text {
		t.Fatalf("Content = %q", resp.Content)
	}
	if len(got) < 2 {
		t.Fatalf("应当切成多片，实际 %d 片", len(got))
	}
}

func TestFakeClientStreamExplicitChunks(t *testing.T) {
	f := &FakeClient{Chunks: []string{"第一段", "第二段"}, Usage: port.Usage{PromptTokens: 3, CompletionTokens: 6}}

	var got []string
	resp, err := f.ChatStream(context.Background(), port.ChatRequest{Scene: SceneConsult}, func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() 返回错误: %v", err)
	}
	if len(got) != 2 || got[0] != "第一段" || got[1] != "第二段" {
		t.Fatalf("增量 = %v", got)
	}
	if resp.Usage.CompletionTokens != 6 {
		t.Fatalf("Usage 没有被带回来: %+v", resp.Usage)
	}
}

func TestFakeClientStreamStopsOnCancel(t *testing.T) {
	f := &FakeClient{Chunks: []string{"a", "b", "c", "d"}, Delay: 50 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	done := make(chan error, 1)
	go func() {
		_, err := f.ChatStream(ctx, port.ChatRequest{Scene: SceneConsult}, func(string) error {
			calls++
			cancel() // 第一片之后立刻取消
			return nil
		})
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("取消后应当返回错误")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后仍在吐数据，goroutine 没有被回收")
	}
	if calls != 1 {
		t.Fatalf("回调次数 = %d, want 1", calls)
	}
}

func TestFakeClientStreamPropagatesDeltaError(t *testing.T) {
	sentinel := errors.New("停")
	f := &FakeClient{Chunks: []string{"x", "y"}}

	_, err := f.ChatStream(context.Background(), port.ChatRequest{Scene: SceneConsult}, func(string) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}
