package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/auth"
	"laborlaw/internal/port"
	"laborlaw/internal/stream"
)

// decodeEnvelope 解析 JSON 包络；非 JSON 响应直接判定失败。
func decodeEnvelope(t *testing.T, body []byte) Envelope {
	t.Helper()

	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("响应不是 JSON: %v (body=%q)", err, string(body))
	}
	return env
}

// fakeRunner 是测试用的 port.Runner，行为可控且不依赖网络。
type fakeRunner struct {
	chunks   []string
	runErr   error
	sendDone bool
	got      port.Request
}

func (f *fakeRunner) Run(ctx context.Context, req port.Request, emit func(port.Event) error) (port.Result, error) {
	f.got = req

	var built strings.Builder
	for _, chunk := range f.chunks {
		if err := emit(port.Event{Type: port.AgentEventDelta, Text: chunk}); err != nil {
			return port.Result{}, err
		}
		built.WriteString(chunk)
	}
	if f.runErr != nil {
		return port.Result{}, f.runErr
	}
	if f.sendDone {
		if err := emit(port.Event{Type: port.AgentEventDone}); err != nil {
			return port.Result{}, err
		}
	}
	return port.Result{
		Content:      built.String(),
		FinishReason: "stop",
		Usage:        port.Usage{PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30},
	}, nil
}

func newChatHandler(t *testing.T, r port.Runner) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	return New(testConfig("dev-user-0001"), discardLogger(),
		WithRunner(r),
		WithHub(stream.NewHub(stream.Options{HeartbeatInterval: time.Hour}, time.Minute, discardLogger())),
	).Handler()
}

func postChat(t *testing.T, h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// sseSeqs 从响应体里抽出所有 `id: N` 的数字。
func sseSeqs(body string) []int {
	var out []int
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "id: ")))
		if err == nil {
			out = append(out, v)
		}
	}
	return out
}

func TestChatStreamsFrames(t *testing.T) {
	runner := &fakeRunner{chunks: []string{"根据", "劳动合同法"}}
	h := newChatHandler(t, runner)

	rec := postChat(t, h, `{"input":"我被辞退了怎么办"}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（body=%s）", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	body := rec.Body.String()
	for _, want := range []string{
		"event: meta", `"message_id"`, `"session_id"`,
		"event: delta", `"text":"根据"`, `"text":"劳动合同法"`,
		"event: done", `"finish_reason":"stop"`, `"total_tokens":30`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应体缺少 %q\n---\n%s", want, body)
		}
	}

	// seq 必须从 1 开始严格递增：meta=1, delta=2, delta=3, done=4。
	seqs := sseSeqs(body)
	if len(seqs) != 4 {
		t.Fatalf("帧数 = %d, want 4（seqs=%v）\n%s", len(seqs), seqs, body)
	}
	for i, s := range seqs {
		if s != i+1 {
			t.Fatalf("第 %d 帧 seq = %d, want %d", i+1, s, i+1)
		}
	}

	if runner.got.Scene != chatScene {
		t.Fatalf("传给 Runner 的 Scene = %q, want %q", runner.got.Scene, chatScene)
	}
	if runner.got.Input != "我被辞退了怎么办" {
		t.Fatalf("传给 Runner 的 Input = %q", runner.got.Input)
	}
}

func TestChatEmptyInput(t *testing.T) {
	h := newChatHandler(t, &fakeRunner{})

	rec := postChat(t, h, `{"input":"   "}`, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != int(port.CodeInvalidArgument) {
		t.Fatalf("code = %d, want 40001", env.Code)
	}
}

func TestChatInputTooLong(t *testing.T) {
	h := newChatHandler(t, &fakeRunner{})

	body := `{"input":"` + strings.Repeat("法", maxChatInputRunes+1) + `"}`
	rec := postChat(t, h, body, nil)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if env := decodeEnvelope(t, rec.Body.Bytes()); env.Code != int(port.CodeInvalidArgument) {
		t.Fatalf("code = %d, want 40001", env.Code)
	}
}

// TestChatRejectsInvalidUTF8：非法字节必须在解码前就被拦掉。
//
// 这条测试盯的正是"解码后检查永远为真"这个坑：json 解码会把 \xff\xfe
// 换成两个 U+FFFD，之后 utf8.ValidString 全部通过。断言 40001 才说明
// 校验真的落在了原始字节上。
func TestChatRejectsInvalidUTF8(t *testing.T) {
	h := newChatHandler(t, &fakeRunner{})

	body := []byte("{\"input\":\"\xff\xfe\"}")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400（body=%s）", rec.Code, rec.Body.String())
	}
	if env := decodeEnvelope(t, rec.Body.Bytes()); env.Code != int(port.CodeInvalidArgument) {
		t.Fatalf("code = %d, want 40001（body=%s）", env.Code, rec.Body.String())
	}
}

func TestChatWithoutRunner(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := New(testConfig("dev-user-0001"), discardLogger()).Handler()

	rec := postChat(t, h, `{"input":"你好"}`, nil)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if env := decodeEnvelope(t, rec.Body.Bytes()); env.Code != int(port.CodeInternal) {
		t.Fatalf("code = %d, want 50001", env.Code)
	}
}

// TestChatRunnerErrorBecomesErrorFrame 守住"头部已发出后只能用帧报错"这条约束。
func TestChatRunnerErrorBecomesErrorFrame(t *testing.T) {
	runner := &fakeRunner{chunks: []string{"前半段"}, runErr: port.New(port.CodeUpstream, "模型服务不可用")}
	h := newChatHandler(t, runner)

	rec := postChat(t, h, `{"input":"你好"}`, nil)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200（SSE 已开始，不能再改状态码）", rec.Code)
	}
	if !strings.Contains(body, "event: error") {
		t.Fatalf("缺少 error 帧:\n%s", body)
	}
	if !strings.Contains(body, `"code":50201`) {
		t.Fatalf("error 帧里缺业务码:\n%s", body)
	}
	if strings.Contains(body, "event: done") {
		t.Fatalf("出错后不该再发 done:\n%s", body)
	}
}

// TestChatDoesNotDuplicateDone：B 自己发了 done 时，A 不许再补一个。
func TestChatDoesNotDuplicateDone(t *testing.T) {
	runner := &fakeRunner{chunks: []string{"回答"}, sendDone: true}
	h := newChatHandler(t, runner)

	rec := postChat(t, h, `{"input":"你好"}`, nil)
	if got := strings.Count(rec.Body.String(), "event: done"); got != 1 {
		t.Fatalf("done 帧数量 = %d, want 1\n%s", got, rec.Body.String())
	}
}

func TestChatAcceptsLastEventID(t *testing.T) {
	runner := &fakeRunner{chunks: []string{"续传内容"}}
	h := newChatHandler(t, runner)

	// 非法值不该 500，而是当成 0 处理。
	rec := postChat(t, h, `{"input":"你好"}`, map[string]string{"Last-Event-ID": "not-a-number"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "event: error") {
		t.Fatalf("非法 Last-Event-ID 不该产生错误帧:\n%s", rec.Body.String())
	}
}

// chatRateLimitHandler 组装一个挂了缓存的对话 handler。
// cache 传 nil 时走"未装配 Redis"的降级形态。
func chatRateLimitHandler(t *testing.T, r port.Runner, cache authCache) http.Handler {
	t.Helper()
	gin.SetMode(gin.TestMode)
	opts := []Option{
		WithRunner(r),
		WithHub(stream.NewHub(stream.Options{HeartbeatInterval: time.Hour}, time.Minute, discardLogger())),
	}
	if cache != nil {
		opts = append(opts, func(a *API) { a.cache = cache })
	}
	// LoginRateLimit 传 0：本组测试只关心对话配额，登录限流保持关闭。
	return New(authTestConfig("dev-user-0001", 0), discardLogger(), opts...).Handler()
}

// value 读计数器的原值，用来确认计数真的记在了预期的键上。
func (f *fakeCache) value(key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.kv[key]
}

// accessTokenFor 用测试密钥签一个 access 令牌，用来在同一个 handler 上扮演不同用户。
func accessTokenFor(t *testing.T, userID string) string {
	t.Helper()
	tok, err := auth.Sign(testSecret, "laborlaw-test",
		auth.Claims{Sub: userID, Typ: auth.TypAccess, Jti: "jti-" + userID}, time.Hour)
	if err != nil {
		t.Fatalf("签令牌失败: %v", err)
	}
	return tok
}

func TestChatRateLimitBlocksAfterQuota(t *testing.T) {
	cache := newFakeCache()
	h := chatRateLimitHandler(t, &fakeRunner{chunks: []string{"好"}}, cache)
	body := `{"input":"加班费怎么算"}`

	for i := 1; i <= chatRateLimit; i++ {
		rec := postChat(t, h, body, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d, want 200（body=%s）", i, rec.Code, rec.Body.String())
		}
	}

	rec := postChat(t, h, body, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("第 %d 次 status = %d, want 429（body=%s）", chatRateLimit+1, rec.Code, rec.Body.String())
	}
	env := decodeEnvelope(t, rec.Body.Bytes())
	if env.Code != int(port.CodeRateLimited) {
		t.Fatalf("code = %d, want %d", env.Code, port.CodeRateLimited)
	}
	if env.Message != "提问过于频繁，请稍后再试" {
		t.Fatalf("message = %q，与约定文案不一致", env.Message)
	}
	// 键里带的必须是用户 ID，按 IP 或全局计数都会误伤同一个出口后面的其他人。
	if got := cache.value(chatRateKeyPrefix + "dev-user-0001"); got != strconv.Itoa(chatRateLimit+1) {
		t.Fatalf("计数键值 = %q, want %d", got, chatRateLimit+1)
	}
}

func TestChatRateLimitIsolatesUsers(t *testing.T) {
	cache := newFakeCache()
	h := chatRateLimitHandler(t, &fakeRunner{chunks: []string{"好"}}, cache)
	body := `{"input":"被辞退了有补偿吗"}`

	alice := map[string]string{"Authorization": "Bearer " + accessTokenFor(t, "u-alice")}
	bob := map[string]string{"Authorization": "Bearer " + accessTokenFor(t, "u-bob")}

	for i := 1; i <= chatRateLimit; i++ {
		if rec := postChat(t, h, body, alice); rec.Code != http.StatusOK {
			t.Fatalf("alice 第 %d 次 status = %d, want 200（body=%s）", i, rec.Code, rec.Body.String())
		}
	}
	if rec := postChat(t, h, body, alice); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("alice 超限后 status = %d, want 429", rec.Code)
	}

	// 关键断言：同一时刻、同一进程、同一个缓存，另一个人不受影响。
	if rec := postChat(t, h, body, bob); rec.Code != http.StatusOK {
		t.Fatalf("bob status = %d, want 200（被 alice 的配额牵连了）", rec.Code)
	}
	if cache.value(chatRateKeyPrefix+"u-bob") != "1" {
		t.Fatalf("bob 的计数键不存在或不是 1：%q", cache.value(chatRateKeyPrefix+"u-bob"))
	}
}

func TestChatRateLimitSkipsWithoutCache(t *testing.T) {
	// 没装配缓存时不限流：这是合法的降级形态，不能因为少了 Redis 就断掉对话。
	h := chatRateLimitHandler(t, &fakeRunner{chunks: []string{"好"}}, nil)
	body := `{"input":"社保没交能直接走吗"}`

	for i := 1; i <= chatRateLimit+3; i++ {
		if rec := postChat(t, h, body, nil); rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d, want 200（body=%s）", i, rec.Code, rec.Body.String())
		}
	}
}

func TestChatRateLimitAllowsWhenCacheDown(t *testing.T) {
	// 缓存故障一律放行：限流是减速带，不该变成一次自己发起的故障。
	cache := newFakeCache()
	cache.down = true
	h := chatRateLimitHandler(t, &fakeRunner{chunks: []string{"好"}}, cache)
	body := `{"input":"试用期最长多久"}`

	for i := 1; i <= chatRateLimit+3; i++ {
		rec := postChat(t, h, body, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("第 %d 次 status = %d, want 200（缓存故障不该挡住对话）", i, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "event: error") {
			t.Fatalf("第 %d 次出现了错误帧:\n%s", i, rec.Body.String())
		}
	}
}

func TestParseLastEventID(t *testing.T) {
	tests := []struct {
		raw  string
		want int
	}{
		{raw: "", want: 0},
		{raw: "0", want: 0},
		{raw: "12", want: 12},
		{raw: " 7 ", want: 7},
		{raw: "-3", want: 0},
		{raw: "abc", want: 0},
		{raw: "1.5", want: 0},
	}
	for _, tc := range tests {
		if got := parseLastEventID(tc.raw); got != tc.want {
			t.Fatalf("parseLastEventID(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}
