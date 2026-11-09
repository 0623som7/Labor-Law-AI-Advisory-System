package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

func testConfig(baseURL string) config.LLMConfig {
	return config.LLMConfig{
		BaseURL:          baseURL,
		APIKey:           "sk-test-key-do-not-leak",
		ModelConsult:     "model-consult",
		ModelExtract:     "model-extract",
		ModelAdvise:      "model-advise",
		Timeout:          2 * time.Second,
		StreamTimeout:    5 * time.Second,
		MaxRetries:       2,
		RetryBackoff:     0,
		PriceInputPer1K:  0.001,
		PriceOutputPer1K: 0.002,
		QPS:              1000,
		DailyBudgetYuan:  100,
	}
}

func simpleRequest(scene string) port.ChatRequest {
	return port.ChatRequest{
		Scene:    scene,
		Messages: []port.Message{{Role: port.RoleUser, Content: "我被辞退了怎么办"}},
	}
}

type memRecorder struct {
	mu   sync.Mutex
	recs []port.UsageRecord
	err  error
}

func (m *memRecorder) Record(_ context.Context, rec port.UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.recs = append(m.recs, rec)
	return nil
}

func (m *memRecorder) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.recs)
}

func TestChatSuccess(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("路径 = %q, want /chat/completions", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test-key-do-not-leak" {
			t.Errorf("Authorization = %q", got)
		}
		var body chatCompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("请求体不是合法 JSON: %v", err)
		}
		gotModel = body.Model
		if body.Stream {
			t.Error("非流式调用不该设置 stream=true")
		}

		_, _ = w.Write([]byte(`{"model":"model-consult","choices":[{"message":{"role":"assistant","content":"你可以主张经济补偿。"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":50,"total_tokens":150}}`))
	}))
	defer srv.Close()

	rec := &memRecorder{}
	c := New(testConfig(srv.URL), WithUsageRecorder(rec))

	resp, err := c.Chat(context.Background(), simpleRequest(SceneConsult))
	if err != nil {
		t.Fatalf("Chat() 返回错误: %v", err)
	}
	if resp.Content != "你可以主张经济补偿。" {
		t.Fatalf("Content = %q", resp.Content)
	}
	if gotModel != "model-consult" {
		t.Fatalf("路由到 %q, want model-consult", gotModel)
	}
	if resp.Usage.TotalTokens != 150 {
		t.Fatalf("TotalTokens = %d, want 150", resp.Usage.TotalTokens)
	}

	// 成本 = 100/1000*0.001 + 50/1000*0.002 = 0.0002
	wantCost := 0.0002
	if got := c.Cost(resp.Usage); got != wantCost {
		t.Fatalf("Cost = %v, want %v", got, wantCost)
	}
	if _, limit := c.Budget(); limit != 100 {
		t.Fatalf("预算上限 = %v, want 100", limit)
	}
	if rec.count() != 1 {
		t.Fatalf("计量落库次数 = %d, want 1", rec.count())
	}
}

func TestChatRetriesOnServerError(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	resp, err := c.Chat(context.Background(), simpleRequest(SceneConsult))
	if err != nil {
		t.Fatalf("Chat() 返回错误: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("Content = %q", resp.Content)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("调用次数 = %d, want 3（1 次 + 2 次重试）", got)
	}
}

func TestChatDoesNotRetryOnBadRequest(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad payload"}}`))
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	_, err := c.Chat(context.Background(), simpleRequest(SceneConsult))

	if err == nil {
		t.Fatal("期望报错，实际成功")
	}
	if code := port.CodeOf(err); code != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %v, want 40001", code)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("调用次数 = %d, want 1（参数错不该重试）", got)
	}
}

func TestChatTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"too late"}}]}`))
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Timeout = 50 * time.Millisecond
	c := New(cfg)

	_, err := c.Chat(context.Background(), simpleRequest(SceneConsult))
	if err == nil {
		t.Fatal("期望超时，实际成功")
	}
	if code := port.CodeOf(err); code != port.CodeUpstreamTimeout {
		t.Fatalf("错误码 = %v, want 50301", code)
	}
}

func TestChatStreamDeltas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body chatCompletionRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !body.Stream {
			t.Error("流式调用必须设置 stream=true")
		}
		if body.StreamOptions == nil || !body.StreamOptions.IncludeUsage {
			t.Error("流式调用必须要求上游带 usage")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("测试服务器不支持 Flush")
			return
		}
		frames := []string{
			`{"choices":[{"delta":{"content":"根据"}}]}`,
			`{"choices":[{"delta":{"content":"劳动合同法"}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":8,"total_tokens":28}}`,
		}
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))

	var got []string
	resp, err := c.ChatStream(context.Background(), simpleRequest(SceneConsult), func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream() 返回错误: %v", err)
	}
	if strings.Join(got, "") != "根据劳动合同法" {
		t.Fatalf("增量 = %v", got)
	}
	if resp.Content != "根据劳动合同法" {
		t.Fatalf("Content = %q", resp.Content)
	}
	if resp.Usage.CompletionTokens != 8 {
		t.Fatalf("CompletionTokens = %d, want 8", resp.Usage.CompletionTokens)
	}
}

// TestChatStreamPropagatesOnDeltaError 守住 port 里写死的约定：
// onDelta 的错误必须原样返回，不许被换码，否则 B 的"掐断生成"逻辑认不出来。
func TestChatStreamPropagatesOnDeltaError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"无源引用\"}}]}\n\n")
		w.(http.Flusher).Flush()
		// 后续内容不应再被读到。
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"不该出现\"}}]}\n\n")
	}))
	defer srv.Close()

	sentinel := errors.New("引用无源，掐断")
	c := New(testConfig(srv.URL))

	_, err := c.ChatStream(context.Background(), simpleRequest(SceneConsult), func(string) error {
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want 原样返回 sentinel", err)
	}
}

// TestChatStreamNoRetryAfterEmit 是"重复输出"的防线：
// 一旦吐过字，再重试就会把前半段推两遍，而日志里一切正常。
func TestChatStreamNoRetryAfterEmit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"前半段\"}}]}\n\n")
		w.(http.Flusher).Flush()
		// 直接断开：客户端读流时会拿到 unexpected EOF。
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("测试服务器不支持 Hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL))
	var got []string
	_, err := c.ChatStream(context.Background(), simpleRequest(SceneConsult), func(d string) error {
		got = append(got, d)
		return nil
	})

	if err == nil {
		t.Fatal("期望读到断流错误")
	}
	if len(got) != 1 {
		t.Fatalf("增量 = %v, want 只有第一次", got)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("调用次数 = %d, want 1（已吐字后不许重试）", n)
	}
}

// TestChatStreamUsesStreamTimeout 守住"流式超时覆盖整段生成"。
//
// 修复前 doChatStream 用的是 cfg.Timeout（单次往返口径）。这里的服务端每 30ms
// 吐一段、共五段（约 150ms），Timeout 设 50ms：用 Timeout 当上限时读到第三段
// 就被 ctx 掐断；用 StreamTimeout 才能读完整段回答。
func TestChatStreamUsesStreamTimeout(t *testing.T) {
	parts := []string{"一", "二", "三", "四", "五"}
	nl := string(rune(10)) + string(rune(10)) // SSE 帧分隔，避免源码里的转义
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, part := range parts {
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%q}}]}`+nl, part)
			w.(http.Flusher).Flush()
			time.Sleep(30 * time.Millisecond)
		}
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+nl)
		fmt.Fprint(w, `data: [DONE]`+nl)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Timeout = 50 * time.Millisecond
	cfg.StreamTimeout = 2 * time.Second
	c := New(cfg)

	var got []string
	resp, err := c.ChatStream(context.Background(), simpleRequest(SceneConsult), func(d string) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("流式调用被提前切断: %v（读到 %v）", err, got)
	}
	if want := strings.Join(parts, ""); resp.Content != want {
		t.Fatalf("Content = %q, want %q", resp.Content, want)
	}
	if len(got) != len(parts) {
		t.Fatalf("增量 %v，期望 %d 段", got, len(parts))
	}
}

// TestChatStreamDisabledFallsBackToTimeout 守住兼容：StreamTimeout 未配置
// （手工构造的配置）时退回 Timeout，行为与改前一致。
func TestChatStreamDisabledFallsBackToTimeout(t *testing.T) {
	nl := string(rune(10)) + string(rune(10))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"迟"}}]}`+nl)
		w.(http.Flusher).Flush()
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"到"}}]}`+nl)
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.Timeout = 50 * time.Millisecond
	cfg.StreamTimeout = 0
	if _, err := New(cfg).ChatStream(context.Background(), simpleRequest(SceneConsult), func(string) error { return nil }); err == nil {
		t.Fatal("StreamTimeout 未配置时应退回 Timeout（本次应超时）")
	}
}

func TestValidateRequest(t *testing.T) {
	c := New(testConfig("http://127.0.0.1:1"))

	tests := []struct {
		name string
		req  port.ChatRequest
	}{
		{name: "缺少 messages", req: port.ChatRequest{Scene: SceneConsult}},
		{name: "缺少 scene", req: port.ChatRequest{Messages: []port.Message{{Role: port.RoleUser, Content: "x"}}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Chat(context.Background(), tc.req); port.CodeOf(err) != port.CodeInvalidArgument {
				t.Fatalf("错误码 = %v, want 40001", port.CodeOf(err))
			}
		})
	}

	if _, err := c.ChatStream(context.Background(), simpleRequest(SceneConsult), nil); port.CodeOf(err) != port.CodeInvalidArgument {
		t.Fatalf("onDelta 为 nil 时应返回 40001，实际 %v", port.CodeOf(err))
	}
}

func TestRoute(t *testing.T) {
	tests := []struct {
		name      string
		scene     string
		explicit  string
		overSpend bool
		want      string
	}{
		{name: "consult 用强模型", scene: SceneConsult, want: "model-consult"},
		{name: "qa 用强模型", scene: SceneQA, want: "model-consult"},
		{name: "抽取用便宜模型", scene: SceneReviewExtract, want: "model-extract"},
		{name: "建议用强模型", scene: SceneReviewAdvise, want: "model-advise"},
		{name: "未知场景兜底", scene: "unknown", want: "model-consult"},
		{name: "显式指定优先", scene: SceneConsult, explicit: "model-debug", want: "model-debug"},
		{name: "超预算时 consult 降级", scene: SceneConsult, overSpend: true, want: "model-extract"},
		{name: "超预算时 advise 降级", scene: SceneReviewAdvise, overSpend: true, want: "model-extract"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fresh := New(testConfig("http://127.0.0.1:1"))
			if tc.overSpend {
				fresh.budget.add(context.Background(), 1000)
			}
			if got := fresh.route(tc.scene, tc.explicit); got != tc.want {
				t.Fatalf("route(%q) = %q, want %q", tc.scene, got, tc.want)
			}
		})
	}
}

// TestChatErrorDoesNotLeakAPIKey 是"key 不进错误信息"的闸门。
func TestChatErrorDoesNotLeakAPIKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"Authorization: Bearer sk-test-key-do-not-leak is invalid"}}`))
	}))
	defer srv.Close()

	cfg := testConfig(srv.URL)
	cfg.MaxRetries = 0
	c := New(cfg)

	_, err := c.Chat(context.Background(), simpleRequest(SceneConsult))
	if err == nil {
		t.Fatal("期望报错")
	}
	if strings.Contains(port.AsAppError(err).Error(), "sk-test-key-do-not-leak") {
		t.Fatalf("错误链里泄漏了 API key: %v", port.AsAppError(err).Error())
	}
}

// TestRecorderFailureDoesNotFailCall 说明记账失败不响应用户。
func TestRecorderFailureDoesNotFailCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	c := New(testConfig(srv.URL), WithUsageRecorder(&memRecorder{err: errors.New("db down")}))
	if _, err := c.Chat(context.Background(), simpleRequest(SceneConsult)); err != nil {
		t.Fatalf("落库失败不该让调用失败: %v", err)
	}
}
