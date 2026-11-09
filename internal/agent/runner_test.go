package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"laborlaw/internal/port"
	"laborlaw/internal/prompt"
)

// recordingClient 记录收到的 ChatRequest。
//
// 不用 llm.FakeClient 的原因：它不暴露入参，而编排层的正确性恰恰体现在
// "发出了什么请求"——系统提示词有没有带条文、历史顺序对不对、场景名是否稳定。
type recordingClient struct {
	mu     sync.Mutex
	got    port.ChatRequest
	calls  int
	chunks []string
	usage  port.Usage
	err    error

	// started 非 nil 时，每次进入 ChatStream 都会向它发送一次，用于并发测试对齐。
	started chan struct{}
	// release 非 nil 时，ChatStream 会阻塞到它关闭，用于观察并发行为。
	release chan struct{}
}

func (c *recordingClient) Chat(_ context.Context, req port.ChatRequest) (port.ChatResponse, error) {
	c.mu.Lock()
	c.got, c.calls = req, c.calls+1
	c.mu.Unlock()
	if c.err != nil {
		return port.ChatResponse{}, c.err
	}
	return port.ChatResponse{Content: strings.Join(c.chunks, ""), Usage: c.usage}, nil
}

func (c *recordingClient) ChatStream(ctx context.Context, req port.ChatRequest, onDelta func(string) error) (port.ChatResponse, error) {
	c.mu.Lock()
	c.got, c.calls = req, c.calls+1
	c.mu.Unlock()

	if c.started != nil {
		c.started <- struct{}{}
	}
	if c.release != nil {
		select {
		case <-c.release:
		case <-ctx.Done():
			return port.ChatResponse{}, port.Wrap(port.CodeUpstreamTimeout, ctx.Err(), "已取消")
		}
	}
	if c.err != nil {
		return port.ChatResponse{}, c.err
	}

	var built strings.Builder
	for _, chunk := range c.chunks {
		if err := onDelta(chunk); err != nil {
			return port.ChatResponse{}, err
		}
		built.WriteString(chunk)
	}
	return port.ChatResponse{Content: built.String(), Usage: c.usage}, nil
}

// collector 收集事件，并可配置在第 N 个事件后失败。
type collector struct {
	events    []port.Event
	failAfter int
	err       error
}

func (c *collector) emit(ev port.Event) error {
	if c.failAfter > 0 && len(c.events) >= c.failAfter {
		return c.err
	}
	c.events = append(c.events, ev)
	return nil
}

func (c *collector) deltaText() string {
	var b strings.Builder
	for _, ev := range c.events {
		if ev.Type == port.AgentEventDelta {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func (c *collector) types() []port.AgentEventType {
	out := make([]port.AgentEventType, 0, len(c.events))
	for _, ev := range c.events {
		out = append(out, ev.Type)
	}
	return out
}

func (c *collector) count(typ port.AgentEventType) int {
	n := 0
	for _, ev := range c.events {
		if ev.Type == typ {
			n++
		}
	}
	return n
}

type fakeSearcher struct {
	hits     []port.StatuteHit
	err      error
	gotQuery string
	gotTopK  int
}

func (f *fakeSearcher) Search(_ context.Context, input string, topK int) ([]port.StatuteHit, error) {
	f.gotQuery, f.gotTopK = input, topK
	if f.err != nil {
		return nil, f.err
	}
	return f.hits, nil
}

type fakeHistory struct {
	records    []port.ChatMessage
	err        error
	gotSession string
	gotLimit   int
}

func (f *fakeHistory) ListBySession(_ context.Context, sessionID string, limit int) ([]port.ChatMessage, error) {
	f.gotSession, f.gotLimit = sessionID, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.records, nil
}

func TestConsultStreamsDeltas(t *testing.T) {
	client := &recordingClient{
		chunks: []string{"可以解除", "劳动合同。"},
		usage:  port.Usage{PromptTokens: 10, CompletionTokens: 6, TotalTokens: 16},
	}
	r := New(client)
	var c collector

	res, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "被欠薪怎么办"}, c.emit)
	if err != nil {
		t.Fatalf("Run 返回错误: %v", err)
	}
	if got := c.deltaText(); got != "可以解除劳动合同。" {
		t.Errorf("delta 拼接 = %q", got)
	}
	if res.Content != "可以解除劳动合同。" {
		t.Errorf("Result.Content = %q", res.Content)
	}
	if res.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", res.FinishReason)
	}
	if res.Usage.TotalTokens != 16 {
		t.Errorf("Usage = %+v", res.Usage)
	}
	// done 由 A 补发，Runner 自己发一个就会让前端结束两次。
	if c.count(port.AgentEventDone) != 0 {
		t.Error("Runner 不应自发 done 事件")
	}
	if client.got.Scene != SceneConsult {
		t.Errorf("场景 = %q", client.got.Scene)
	}
	if client.got.Messages[0].Role != port.RoleSystem {
		t.Errorf("首条消息角色 = %q", client.got.Messages[0].Role)
	}
	last := client.got.Messages[len(client.got.Messages)-1]
	if last.Role != port.RoleUser || last.Content != "被欠薪怎么办" {
		t.Errorf("末条消息 = %+v", last)
	}
}

// TestQASceneUsesConsultFlow 钉住 qa 是 consult 的别名。
//
// 场景名必须原样透传：模型路由与用量报表都按 scene 分组，若在 Runner 里
// 改写成 consult，qa 的调用会从自己的统计里消失。
func TestQASceneUsesConsultFlow(t *testing.T) {
	client := &recordingClient{chunks: []string{"最长六个月。"}}
	r := New(client)
	var c collector

	res, err := r.Run(context.Background(), port.Request{Scene: SceneQA, Input: "试用期最长多久"}, c.emit)
	if err != nil {
		t.Fatalf("qa 场景不应报错: %v", err)
	}
	if res.Content != "最长六个月。" {
		t.Errorf("Content = %q", res.Content)
	}
	if client.got.Scene != SceneQA {
		t.Errorf("传给模型的场景 = %q, want %q（不得改写成 consult）", client.got.Scene, SceneQA)
	}
	if client.calls != 1 {
		t.Errorf("模型调用次数 = %d, want 1", client.calls)
	}
}

func TestConsultRejectsInvalidRequests(t *testing.T) {
	cases := []struct {
		name string
		req  port.Request
		want port.ErrorCode
	}{
		{"空输入", port.Request{Scene: SceneConsult, Input: "   "}, port.CodeInvalidArgument},
		{"未知场景", port.Request{Scene: "fortune-telling", Input: "x"}, port.CodeInvalidArgument},
		{"审查未接入", port.Request{Scene: SceneReview, Input: "x"}, port.CodeInvalidArgument},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := New(&recordingClient{})
			var c collector
			_, err := r.Run(context.Background(), tc.req, c.emit)
			if !errors.Is(err, &port.AppError{Code: tc.want}) {
				t.Errorf("错误 = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestConsultWithoutClient(t *testing.T) {
	r := New(nil)
	var c collector
	_, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "x"}, c.emit)
	if !errors.Is(err, &port.AppError{Code: port.CodeInternal}) {
		t.Errorf("错误 = %v, want 50001", err)
	}
}

func TestConsultNilEmit(t *testing.T) {
	r := New(&recordingClient{})
	_, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "x"}, nil)
	if !errors.Is(err, &port.AppError{Code: port.CodeInvalidArgument}) {
		t.Errorf("错误 = %v, want 40001", err)
	}
}

// TestConsultEmitErrorStopsImmediately 守住 port.Runner 的硬约束：
// emit 报错必须立刻停止，否则客户端断开后还在继续烧 token。
func TestConsultEmitErrorStopsImmediately(t *testing.T) {
	client := &recordingClient{chunks: []string{"一", "二", "三", "四"}}
	r := New(client)
	boom := errors.New("客户端已断开")
	c := &collector{failAfter: 2, err: boom}

	_, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "x"}, c.emit)
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want 原样的 emit 错误", err)
	}
	if got := c.deltaText(); got != "一二" {
		t.Errorf("已发出的 delta = %q, 应只有前两段", got)
	}
	if len(c.events) != 2 {
		t.Errorf("事件数 = %d, 第三段不应再发出", len(c.events))
	}
}

// TestConsultEmitFailureDuringRetrieveStops 守住"检索阶段断线也不能再调模型"。
// 少这一条，客户端断开后仍会白烧一次完整的 token。
func TestConsultEmitFailureDuringRetrieveStops(t *testing.T) {
	searcher := &fakeSearcher{hits: []port.StatuteHit{{Statute: port.Statute{ID: "LCL-38", Text: "条文"}}}}
	client := &recordingClient{chunks: []string{"不该被生成"}}
	r := New(client, WithStatutes(searcher))
	boom := errors.New("连接断开")
	c := &collector{failAfter: 1, err: boom}

	_, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "x"}, c.emit)
	if !errors.Is(err, boom) {
		t.Fatalf("错误 = %v, want 原样的 emit 错误", err)
	}
	if client.calls != 0 {
		t.Errorf("断线后仍调用了模型 %d 次", client.calls)
	}
}

func TestConsultUpstreamErrorPassesThrough(t *testing.T) {
	client := &recordingClient{err: port.New(port.CodeUpstream, "上游不可用")}
	r := New(client)
	var c collector
	_, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "x"}, c.emit)
	if !errors.Is(err, &port.AppError{Code: port.CodeUpstream}) {
		t.Errorf("错误 = %v, want 50201", err)
	}
}

func TestConsultCitationsComeFromStore(t *testing.T) {
	const statuteText = "用人单位未及时足额支付劳动报酬的，劳动者可以解除劳动合同。"
	searcher := &fakeSearcher{hits: []port.StatuteHit{{
		Statute: port.Statute{ID: "LCL-38", Law: "劳动合同法", Article: "第三十八条", Text: statuteText, URL: "https://flk.npc.gov.cn/"},
		Score:   1,
	}}}
	client := &recordingClient{chunks: []string{"可以解除。"}}
	r := New(client, WithStatutes(searcher))
	var c collector

	if _, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "欠薪能离职吗"}, c.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if searcher.gotQuery != "欠薪能离职吗" {
		t.Errorf("检索词 = %q", searcher.gotQuery)
	}
	if searcher.gotTopK != 3 {
		t.Errorf("topK = %d", searcher.gotTopK)
	}
	if c.count(port.AgentEventCitation) != 1 {
		t.Fatalf("citation 帧数 = %d", c.count(port.AgentEventCitation))
	}
	for _, ev := range c.events {
		if ev.Type == port.AgentEventCitation && ev.Citation.Text != statuteText {
			t.Errorf("citation 原文被改动: %q", ev.Citation.Text)
		}
	}
	// 系统提示词必须真的带上条文，否则模型只能凭记忆回答。
	if !strings.Contains(client.got.Messages[0].Content, statuteText) {
		t.Error("系统提示词未包含检索到的条文")
	}
	// 工具状态要有始有终，否则前端一直显示"正在检索"。
	statuses := map[port.ToolRunStatus]int{}
	for _, ev := range c.events {
		if ev.Type == port.AgentEventToolCall {
			statuses[ev.Tool.Status]++
			if strings.Contains(string(ev.Tool.Args), "欠薪") {
				t.Error("ToolStatus.Args 泄漏了用户原话")
			}
		}
	}
	if statuses[port.ToolRunning] != 1 || statuses[port.ToolOK] != 1 {
		t.Errorf("工具状态 = %+v", statuses)
	}
}

func TestConsultSearchFailureStillAnswers(t *testing.T) {
	searcher := &fakeSearcher{err: errors.New("mysql 连接断开")}
	client := &recordingClient{chunks: []string{"建议先保留证据。"}}
	r := New(client, WithStatutes(searcher))
	var c collector

	res, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "x"}, c.emit)
	if err != nil {
		t.Fatalf("检索失败不应导致回答失败: %v", err)
	}
	if res.Content == "" {
		t.Error("回答为空")
	}
	if c.count(port.AgentEventCitation) != 0 {
		t.Error("检索失败时不应发出 citation")
	}
	failed := 0
	for _, ev := range c.events {
		if ev.Type == port.AgentEventToolCall && ev.Tool.Status == port.ToolFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Errorf("failed 状态帧 = %d, want 1", failed)
	}
	// 没有检索结果时不能出现条文注入区块，否则模型会以为查过但没命中而自行补一条。
	if strings.Contains(client.got.Messages[0].Content, "以下来自法条库") {
		t.Error("检索失败时系统提示词不应含条文注入区块")
	}
	if !strings.Contains(client.got.Messages[0].Content, "没有检索到") {
		t.Error("检索失败时应显式告知模型没有可引用的原文")
	}
}

func TestConsultWithoutSearcherEmitsNoToolCall(t *testing.T) {
	r := New(&recordingClient{chunks: []string{"答"}})
	var c collector
	if _, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "x"}, c.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if c.count(port.AgentEventToolCall) != 0 {
		t.Error("未装配检索能力时不应发出工具状态")
	}
}

func TestConsultHistoryLoadedAndFiltered(t *testing.T) {
	history := &fakeHistory{records: []port.ChatMessage{
		{Role: port.RoleSystem, Content: "旧版提示词"},
		{Role: port.RoleUser, Content: "试用期多久合法"},
		{Role: port.RoleAssistant, Content: "看合同期限"},
		{Role: port.RoleTool, Content: "工具结果"},
		{Role: port.RoleUser, Content: "   "},
		{Role: port.RoleUser, Content: "那能约定一年吗"},
	}}
	client := &recordingClient{chunks: []string{"不能。"}}
	r := New(client, WithHistory(history))
	var c collector

	if _, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, SessionID: "S1", Input: "为什么"}, c.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// 上限默认 40：它只做粗筛，真正的闸门是按 token 裁。
	if history.gotSession != "S1" || history.gotLimit != 40 {
		t.Errorf("历史请求 = (%q, %d)", history.gotSession, history.gotLimit)
	}

	msgs := client.got.Messages
	// system + 3 条有效历史 + 当前输入
	if len(msgs) != 5 {
		t.Fatalf("消息数 = %d, want 5", len(msgs))
	}
	if msgs[1].Content != "试用期多久合法" || msgs[2].Content != "看合同期限" || msgs[3].Content != "那能约定一年吗" {
		t.Errorf("历史顺序或内容错误: %+v", msgs)
	}
	for _, m := range msgs[1:] {
		if m.Role == port.RoleSystem || m.Role == port.RoleTool {
			t.Errorf("历史里混入了 %q 角色", m.Role)
		}
	}
}

func TestConsultHistoryFailureDegradesToStateless(t *testing.T) {
	history := &fakeHistory{err: errors.New("存储抖动")}
	client := &recordingClient{chunks: []string{"答"}}
	r := New(client, WithHistory(history))
	var c collector

	if _, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, SessionID: "S1", Input: "x"}, c.emit); err != nil {
		t.Fatalf("历史读取失败不应中断回答: %v", err)
	}
	if len(client.got.Messages) != 2 {
		t.Errorf("降级后消息数 = %d, want 2（system + user）", len(client.got.Messages))
	}
}

// longHistory 造 n 条各 runes 个汉字的问答，用于把预算顶满。
func longHistory(n, runes int) []port.ChatMessage {
	body := ""
	for i := 0; i < runes; i++ {
		body += "字"
	}
	out := make([]port.ChatMessage, 0, n)
	for i := 0; i < n; i++ {
		role := port.RoleUser
		if i%2 == 1 {
			role = port.RoleAssistant
		}
		out = append(out, port.ChatMessage{Role: role, Content: body})
	}
	return out
}

// TestConsultTrimsHistoryByBudget 钉住"历史按 token 预算装，不是按条数装"。
//
// 6 条 300 字的问答共 1800 token；system（无检索时约 800 字）+ 本轮输入 +
// 输出预留 1000 之后，3000 的上下文只装得下其中一部分，所以最终消息数必须
// 少于 8（system + 6 历史 + input）。
func TestConsultTrimsHistoryByBudget(t *testing.T) {
	history := &fakeHistory{records: longHistory(6, 300)}
	client := &recordingClient{chunks: []string{"答"}}
	r := New(client, WithHistory(history), WithContextTokens(3000), WithMaxTokens(1000))
	var c collector

	if _, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, SessionID: "S1", Input: "为什么"}, c.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}

	msgs := client.got.Messages
	if len(msgs) >= 8 {
		t.Fatalf("消息数 = %d，预算 3000 装不下 6 条 300 字的历史，应当小于 8", len(msgs))
	}
	if len(msgs) <= 2 {
		t.Fatalf("消息数 = %d，预算够装一部分历史时不该全丢", len(msgs))
	}
	if msgs[0].Role != port.RoleSystem {
		t.Errorf("首条角色 = %q，期望 system", msgs[0].Role)
	}
	if first := msgs[1]; first.Role != port.RoleUser {
		t.Errorf("历史首条角色 = %q，期望 user（不能以 assistant 开场）", first.Role)
	}
	last := msgs[len(msgs)-1]
	if last.Role != port.RoleUser || last.Content != "为什么" {
		t.Errorf("末条消息 = %+v，期望本轮输入", last)
	}
}

// TestConsultWithoutBudgetStillAnswers 钉住"预算不足是降级不是报错"：
// 用户问一句短问题不该因为带不下历史就 500。
func TestConsultWithoutBudgetStillAnswers(t *testing.T) {
	history := &fakeHistory{records: longHistory(4, 100)}
	client := &recordingClient{chunks: []string{"答"}}
	r := New(client, WithHistory(history), WithContextTokens(1), WithMaxTokens(1200))
	var c collector

	res, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, SessionID: "S1", Input: "在吗"}, c.emit)
	if err != nil {
		t.Fatalf("预算不足不应报错: %v", err)
	}
	if res.Content != "答" {
		t.Errorf("Content = %q", res.Content)
	}
	if client.calls != 1 {
		t.Errorf("客户端调用次数 = %d，期望 1", client.calls)
	}
	if len(client.got.Messages) != 2 {
		t.Errorf("消息数 = %d，期望 2（system + 本轮输入）", len(client.got.Messages))
	}
}

func TestConsultNoSessionSkipsHistory(t *testing.T) {
	history := &fakeHistory{records: []port.ChatMessage{{Role: port.RoleUser, Content: "不该被读到"}}}
	client := &recordingClient{chunks: []string{"答"}}
	r := New(client, WithHistory(history))
	var c collector

	if _, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: "x"}, c.emit); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if history.gotSession != "" {
		t.Error("无 SessionID 时不应查询历史")
	}
}

func TestConsultContextCancel(t *testing.T) {
	client := &recordingClient{release: make(chan struct{})}
	r := New(client)
	var c collector

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := r.Run(ctx, port.Request{Scene: SceneConsult, Input: "x"}, c.emit)
	if !errors.Is(err, &port.AppError{Code: port.CodeUpstreamTimeout}) {
		t.Errorf("错误 = %v, want 50301", err)
	}
}

// TestSameSessionSerialized 验证 port.Runner 要求的"同 SessionID 并发 Run 由 B 串行化"。
func TestSameSessionSerialized(t *testing.T) {
	client := &recordingClient{
		chunks:  []string{"答"},
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	r := New(client)

	var wg sync.WaitGroup
	run := func(session string) {
		defer wg.Done()
		var c collector
		_, _ = r.Run(context.Background(), port.Request{Scene: SceneConsult, SessionID: session, Input: "x"}, c.emit)
	}

	wg.Add(2)
	go run("SAME")
	<-client.started
	go run("SAME")

	select {
	case <-client.started:
		t.Fatal("同一会话的第二次 Run 在第一次结束前就开始了")
	case <-time.After(60 * time.Millisecond):
	}

	close(client.release)
	<-client.started
	wg.Wait()
}

func TestDifferentSessionsRunConcurrently(t *testing.T) {
	client := &recordingClient{
		chunks:  []string{"答"},
		started: make(chan struct{}, 2),
		release: make(chan struct{}),
	}
	r := New(client)

	var wg sync.WaitGroup
	wg.Add(2)
	for _, session := range []string{"S-A", "S-B"} {
		go func(s string) {
			defer wg.Done()
			var c collector
			_, _ = r.Run(context.Background(), port.Request{Scene: SceneConsult, SessionID: s, Input: "x"}, c.emit)
		}(session)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-client.started:
		case <-time.After(2 * time.Second):
			t.Fatal("不同会话没有并发执行")
		}
	}
	close(client.release)
	wg.Wait()
}

func TestPromptStatuteInjection(t *testing.T) {
	if got := prompt.ConsultWithStatutes(nil); got == prompt.ConsultSystem() {
		t.Error("无命中时应追加未检索到说明")
	}
	hits := make([]port.StatuteHit, 0, 12)
	for i := 0; i < 12; i++ {
		hits = append(hits, port.StatuteHit{Statute: port.Statute{Law: "劳动合同法", Article: "第一条", Text: "条文"}})
	}
	got := prompt.ConsultWithStatutes(hits)
	if n := strings.Count(got, "【劳动合同法"); n != 8 {
		t.Errorf("注入条数 = %d, want 8（超出只会稀释注意力）", n)
	}
}

// TestConsultInputLengthLimit 守 contract/openapi.yaml 里 ChatRequest.input 的 maxLength=4000。
//
// 用汉字构造而不是单个 ASCII 字符：一个汉字 3 字节，len("欠"*4000) = 12000，
// 这条用例能同时证伪"按字节数校验"的错误实现。
func TestConsultInputLengthLimit(t *testing.T) {
	const cjk = "欠"

	t.Run("正好4000字通过", func(t *testing.T) {
		r := New(&recordingClient{chunks: []string{"ok"}})
		var c collector
		if _, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: strings.Repeat(cjk, maxConsultInputRunes)}, c.emit); err != nil {
			t.Fatalf("4000 字应通过，得到错误: %v", err)
		}
	})

	t.Run("4001字被拒", func(t *testing.T) {
		r := New(&recordingClient{chunks: []string{"ok"}})
		var c collector
		_, err := r.Run(context.Background(), port.Request{Scene: SceneConsult, Input: strings.Repeat(cjk, maxConsultInputRunes+1)}, c.emit)
		if !errors.Is(err, &port.AppError{Code: port.CodeInvalidArgument}) {
			t.Errorf("错误 = %v, want %v", err, port.CodeInvalidArgument)
		}
	})
}

// cancelAfterFirstDelta 模拟"用户点了停止"：吐一个字之后 ctx 被取消，
// 上游随即返回可重试的超时错误（真实 ChatStream 在 ctx 取消时就是这样）。
type cancelAfterFirstDelta struct {
	cancel context.CancelFunc
}

func (c *cancelAfterFirstDelta) Chat(context.Context, port.ChatRequest) (port.ChatResponse, error) {
	return port.ChatResponse{}, port.Wrap(port.CodeUpstreamTimeout, context.Canceled, "测试不覆盖非流式")
}

func (c *cancelAfterFirstDelta) ChatStream(ctx context.Context, _ port.ChatRequest, onDelta func(string) error) (port.ChatResponse, error) {
	if err := onDelta("用人单位应当"); err != nil {
		return port.ChatResponse{}, err
	}
	c.cancel()
	return port.ChatResponse{}, port.Wrap(port.CodeUpstreamTimeout, context.Canceled, "已取消")
}

// TestConsultCancelledFinishReason 守住"被中断的一轮要标明 cancelled"。
//
// 缺陷形态：FinishReason 写死 "stop"，前端无法区分"正常说完"和"被掐断"。
func TestConsultCancelledFinishReason(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := New(&cancelAfterFirstDelta{cancel: cancel})
	var c collector

	res, err := r.Run(ctx, port.Request{Scene: SceneConsult, Input: "公司不给我交社保"}, c.emit)
	if err != nil {
		t.Fatalf("客户端主动停止不该当成失败: %v", err)
	}
	if res.FinishReason != "cancelled" {
		t.Fatalf("FinishReason = %q, want cancelled", res.FinishReason)
	}
	if res.Content != "用人单位应当" {
		t.Fatalf("半截回答必须留下来，实际 %q", res.Content)
	}
}
