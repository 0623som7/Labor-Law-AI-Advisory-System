package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/id"
	"laborlaw/internal/llm"
	"laborlaw/internal/port"
)

const (
	// chatScene 是咨询场景名，必须与 llm.SceneConsult 一致。
	chatScene = "consult"

	// maxChatInputRunes 与 contract/openapi.yaml 的 maxLength: 4000 对齐。
	// 按 rune 而不是 byte 计：一个汉字 3 字节，按字节限制会把中文用户卡在 1333 字。
	maxChatInputRunes = 4000

	// lastEventIDHeader 是 SSE 断线续传的标准请求头。
	lastEventIDHeader = "Last-Event-ID"

	// chatSessionTitleRunes 是新建会话时从用户原话截取的标题长度。
	// 侧边栏一行大约放得下 12 个汉字，20 是留够余量又不至于撑爆列表的折中。
	chatSessionTitleRunes = 20

	// chatPersistTimeout 是落库的独立超时。
	// 不能复用请求 ctx：客户端在最后一帧之后立刻断开是常态，
	// 而此时答案已经生成，理应留下来，不该跟着 ctx 一起被取消。
	chatPersistTimeout = 5 * time.Second

	// chatRateLimit 是单用户在 chatRateWindow 内允许的提问次数。
	//
	// 20 次/10 分钟：正常问法一两分钟一轮，连追问带补充也撞不到；
	// 而拿着脚本狂刷的人几十秒就能顶穿。取这个数量级是为了让"真人无感、
	// 压测有感"，不是在精算成本。
	chatRateLimit = 20

	// chatRateWindow 是固定窗口的长度。
	//
	// 用固定窗口而不是滑动窗口：滑动窗口要么存时间戳列表（O(n) 内存且要
	// 每次清理），要么上 Redis Lua 做 ZSET 分片计数——为一条防刷规则多维护
	// 一段脚本和它的测试不划算。固定窗口的代价是窗口边界上最多放过 2 倍流量
	// （第 10 分钟末尾刷 20 次、第 11 分钟开头再刷 20 次），对话框场景可以接受。
	chatRateWindow = 10 * time.Minute

	// chatRateKeyPrefix 是对话计数的键前缀，与登录限流的 auth:login: 分开。
	// 两处限流的窗口与语义都不同，混用同一个键会让登录失败计数去挤占提问配额。
	chatRateKeyPrefix = "chat:rl:"
)

// chatRateLimited 判断该用户是否已刷爆对话配额。
//
// 三个设计点，改之前先看：
//  1. 必须在 Attach 之前调用。响应头一写出，状态码就只能是 200，
//     "提问过于频繁"就只能变成流内 error 帧，前端拿不到 429 也就没法提示
//     "稍后再试"，只能显示一段奇怪的失败回答。
//  2. 缓存故障一律放行。限流是减速带，不是可用性前置条件：
//     Redis 抖动时把所有人挡在门外，等于用限流造成一次自己发起的故障。
//  3. 不清零。对话是纯消耗，不像登录成功可以把失败计数清掉——
//     这里没有"做对一次就抵消之前的消耗"这回事，20 次用完就得等窗口过去。
func (a *API) chatRateLimited(ctx context.Context, c *gin.Context) bool {
	if a.cache == nil {
		// 没装配 Redis 是合法的降级形态（单机调试、无缓存部署），此时不限流。
		return false
	}

	key := chatRateKeyPrefix + UserID(c)
	n, err := a.cache.incr(ctx, key)
	if err != nil {
		loggerFrom(c).Warn("对话限流不可用，本次放行", "trace_id", TraceID(c), "err", err.Error())
		return false
	}
	if n == 1 {
		if err := a.cache.expire(ctx, key, chatRateWindow); err != nil {
			// 计数键没有过期时间会变成永久封禁，用户再也问不了话。
			// 删键重来比锁死账号好：宁可这一轮不计数，也不能让人永远 429。
			loggerFrom(c).Warn("对话限流窗口设置失败，重置计数",
				"trace_id", TraceID(c), "err", err.Error())
			_ = a.cache.del(ctx, key)
			return false
		}
	}
	return n > int64(chatRateLimit)
}

// chatRequest 是 /chat/completions 的请求体。
type chatRequest struct {
	// SessionID 为空表示新建会话；非空时本轮要挂到该会话的历史上下文上。
	SessionID string `json:"session_id"`
	// Input 是用户原话，后端不做清洗与改写（改写是 B 的编排职责）。
	Input string `json:"input"`
	// Options 是透传业务选项，A 不解释含义。
	Options map[string]any `json:"options"`
}

// chatCompletions 是劳动法咨询的 SSE 入口。
//
// 处理顺序有三条硬约束，顺序错了就修不回来：
//  1. 校验必须在 Attach 之前。一旦写下 SSE 响应头，状态码就只能是 200，
//     再想返回 40001 只能靠"发一帧 error 然后断开"，前端拿到的不是 HTTP 错误。
//  2. meta 帧必须在 Run 之前发。前端靠它建占位气泡，晚一帧就会出现"空白等三秒"。
//  3. done 帧由 A 补发。B 的 Run 返回后才知道 finish_reason 与 usage，
//     而 done 是前端结束 loading 的唯一信号。
func (a *API) chatCompletions(c *gin.Context) {
	if a.runner == nil || a.hub == nil {
		FailCode(c, port.CodeInternal, "对话接口尚未接入（缺少 agent 或 stream）")
		return
	}
	if UserID(c) == "" {
		FailCode(c, port.CodeUnauthorized, "请先登录")
		return
	}

	// 限流必须落在读 body 之前：先花时间解一个 4KB 的 JSON 再回一句"你太频繁"，
	// 等于把限流想省下的成本又付了一遍。这也是它能挡在 Attach 之前的前提。
	if a.chatRateLimited(c.Request.Context(), c) {
		FailCode(c, port.CodeRateLimited, "提问过于频繁，请稍后再试")
		return
	}

	// 限制请求体大小：JSON 接口同样可能被塞一个大 body 打满内存。
	// 只在配置为正数时启用：MaxBytesReader(0) 会把所有请求都截成 0 字节，
	// 在零值 Config 的测试里表现为"所有 body 都不是合法 JSON"。
	if a.cfg.HTTP.MaxBodyBytes > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, a.cfg.HTTP.MaxBodyBytes)
	}

	// UTF-8 校验必须落在原始字节上，不能等解码之后再查 req.Input。
	// encoding/json 遇到字符串里的非法字节不会报错，而是静默替换成 U+FFFD
	// （见 encoding/json 文档 "invalid UTF-8 ... replaced by the Unicode
	// replacement character"）：等到 req.Input 里已经是替换字符时，
	// ValidString 永远为真，用户只会看到一段像乱码的"正常回答"，
	// 而不是一条明确的 40001。所以这里整体读入、先验字节、再交回解码。
	var raw []byte
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		FailCode(c, port.CodeInvalidArgument, "请求体读取失败")
		return
	}
	if !utf8.Valid(raw) {
		FailCode(c, port.CodeInvalidArgument, "请求体不是合法的 UTF-8")
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	var req chatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		FailCode(c, port.CodeInvalidArgument, "请求体不是合法 JSON")
		return
	}

	req.Input = strings.TrimSpace(req.Input)
	if !utf8.ValidString(req.Input) {
		FailCode(c, port.CodeInvalidArgument, "请求体不是合法的 UTF-8")
		return
	}
	if req.Input == "" {
		FailCode(c, port.CodeInvalidArgument, "input 不能为空")
		return
	}
	if utf8.RuneCountInString(req.Input) > maxChatInputRunes {
		FailCode(c, port.CodeInvalidArgument, "input 超过 %d 字", maxChatInputRunes)
		return
	}

	ctx := c.Request.Context()

	messageID := id.New()
	// 会话解析必须发生在 Attach 之前：一旦写了 SSE 响应头，
	// "会话不存在"就只能变成流内 error 帧，前端拿到的不是 404。
	sessionID, err := a.resolveSession(c, strings.TrimSpace(req.SessionID), req.Input)
	if err != nil {
		Fail(c, err)
		return
	}

	writer, err := a.hub.Attach(ctx, messageID, parseLastEventID(c.GetHeader(lastEventIDHeader)), c.Writer)
	if err != nil {
		Fail(c, err)
		return
	}
	defer func() {
		if closeErr := writer.Close(); closeErr != nil {
			// 客户端断开是常态，只记 debug，避免把正常断线打成错误淹没真故障。
			a.log.Debug("close sse writer", "message_id", messageID, "err", closeErr.Error())
		}
	}()

	if _, err := a.hub.PublishMeta(messageID, messageID, sessionID, a.cfg.LLM.ModelConsult); err != nil {
		a.log.Error("publish meta failed", "message_id", messageID, "err", err.Error())
		return
	}

	sawDone := false
	// port.Result 里没有引用，只能在推流时就地收集：
	// citation 帧发出去之后就没有第二次机会拿到这几条法条了，而刷新页面要靠它重建卡片。
	var citations []port.Citation
	emit := func(ev port.Event) error {
		if ev.Type == port.AgentEventDone {
			// 记下来避免下面再补一个 done：两个 done 会让前端重复结束。
			sawDone = true
		}
		if ev.Type == port.AgentEventCitation && ev.Citation != nil {
			citations = append(citations, *ev.Citation)
		}
		if _, perr := a.hub.Publish(messageID, ev); perr != nil {
			return perr
		}
		return nil
	}

	// 计量归属：llm 层按 ctx 上的 user_id 记账，不从 port.ChatRequest 取。
	// 放在 Run 之前，重试与流式都会继承同一个 ctx。
	ctx = llm.WithUserID(ctx, UserID(c))

	result, runErr := a.runner.Run(ctx, port.Request{
		Scene:     chatScene,
		SessionID: sessionID,
		Input:     req.Input,
		Options:   req.Options,
	}, emit)
	if runErr != nil {
		a.publishRunError(messageID, runErr)
		// 提问必须留下来：会话标题就是这句话生成的，只丢历史会让侧边栏
		// 有一条"标题有、正文没有"的会话，刷新一次就自相矛盾。
		a.persistUserTurn(ctx, UserID(c), sessionID, req.Input)
		return
	}

	if !sawDone {
		if _, err := a.hub.PublishDone(messageID, messageID, result.FinishReason, result.Usage); err != nil {
			a.log.Warn("publish done failed", "message_id", messageID, "err", err.Error())
		}
	}

	// 落库放在最后：它不影响用户看到答案的时机（done 已经发出），
	// 而放在 Run 之前会让本轮提问被 loadHistory 读成"上一轮"，
	// 模型就会看到两条一模一样的用户消息。
	a.persistTurn(ctx, UserID(c), sessionID, messageID, req.Input, result, citations)
}

// resolveSession 解析本轮要挂载的会话。
//
// 三种情况：store 未装配（单测/无库部署）→ 不落库，只在内存里用调用方给的 ID；
// 带 session_id → 必须属于当前用户，否则 40401；不带 → 建一个新会话。
//
// 校验归属放在这里而不是让存储层兜底，是因为越权访问要报 404 而不是"查不到消息"：
// 前者告诉用户这个会话不属于你，后者会让前端以为会话刚被删了。
func (a *API) resolveSession(c *gin.Context, sessionID, input string) (string, error) {
	if a.store == nil {
		if sessionID == "" {
			return id.New(), nil
		}
		return sessionID, nil
	}

	ctx := c.Request.Context()
	userID := UserID(c)
	if sessionID != "" {
		if _, err := a.store.Session().Get(ctx, userID, sessionID); err != nil {
			return "", err
		}
		return sessionID, nil
	}

	sn := &port.Session{UserID: userID, Scene: chatScene, Title: sessionTitleFromInput(input)}
	if err := a.store.Session().Create(ctx, sn); err != nil {
		return "", err
	}
	return sn.ID, nil
}

// sessionTitleFromInput 把用户第一句话压成会话标题。
//
// 按 rune 截断再而不是按字节：汉字 3 字节，按字节切会切出半个字符，
// MySQL 严格模式下直接报 1406。
func sessionTitleFromInput(input string) string {
	// 换行会把侧边栏撑成两行，统一压成空格。
	flat := strings.Join(strings.Fields(input), " ")
	r := []rune(flat)
	if len(r) > chatSessionTitleRunes {
		return string(r[:chatSessionTitleRunes]) + "…"
	}
	return string(r)
}

// persistTurn 把一轮问答写进库：user 消息 + assistant 消息 + 会话时间戳。
//
// 三条消息必须在一个事务里：只写进 user 而不写 assistant 会留下一条
// 永远没有回答的提问，用户刷新页面就会看到一个卡住的对话。
// 落库失败只记 Warn 不报错：答案是用户真正要的东西，写库失败不该把它吞掉。
func (a *API) persistTurn(parent context.Context, userID, sessionID, messageID, input string, result port.Result, citations []port.Citation) {
	if a.store == nil || sessionID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), chatPersistTimeout)
	defer cancel()

	answer := strings.TrimSpace(result.Content)

	fn := func(st port.Store) error {
		if err := st.Message().Append(ctx, &port.ChatMessage{
			SessionID: sessionID,
			Role:      port.RoleUser,
			Content:   input,
		}); err != nil {
			return err
		}
		if answer == "" {
			return nil
		}
		// assistant 消息的 ID 就是 meta 帧里的 message_id，
		// 前端刷新历史后可以据此把占位气泡替换成真正的那条。
		if err := st.Message().Append(ctx, &port.ChatMessage{
			ID:               messageID,
			SessionID:        sessionID,
			Role:             port.RoleAssistant,
			Content:          answer,
			PromptTokens:     result.Usage.PromptTokens,
			CompletionTokens: result.Usage.CompletionTokens,
			Citations:        citations,
		}); err != nil {
			return err
		}
		return st.Session().Touch(ctx, userID, sessionID)
	}

	if err := a.store.Tx(ctx, fn); err != nil {
		a.log.Warn("persist chat turn failed",
			"session_id", sessionID,
			"message_id", messageID,
			"err", err.Error(),
		)
	}
}

// persistUserTurn 在生成失败时补写用户提问。
//
// 不写 assistant 消息：失败时没有答案，占位内容会被下一轮的 loadHistory
// 当成"模型说过这句话"回放给上游，污染上下文。
func (a *API) persistUserTurn(parent context.Context, userID, sessionID, input string) {
	if a.store == nil || sessionID == "" {
		return
	}

	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), chatPersistTimeout)
	defer cancel()

	fn := func(st port.Store) error {
		if err := st.Message().Append(ctx, &port.ChatMessage{
			SessionID: sessionID,
			Role:      port.RoleUser,
			Content:   input,
		}); err != nil {
			return err
		}
		// 时间戳照样要推进：用户刚问过，列表按最近活跃排序就该排在前面。
		return st.Session().Touch(ctx, userID, sessionID)
	}

	if err := a.store.Tx(ctx, fn); err != nil {
		a.log.Warn("persist user turn failed",
			"session_id", sessionID,
			"err", err.Error(),
		)
	}
}

// publishRunError 把编排错误写成 error 帧。
//
// 此时响应头已经发出、状态码只能是 200，所以错误必须走 SSE 事件而不是 JSON 包络——
// 前端要同时处理"HTTP 层错误"和"流内错误"两种形态，这是 SSE 的固有代价。
func (a *API) publishRunError(messageID string, runErr error) {
	// 客户端主动断开：连接已经没了，再写只会多一条无意义的写失败日志。
	if errors.Is(runErr, context.Canceled) {
		return
	}

	if _, err := a.hub.PublishError(messageID, port.AsAppError(runErr)); err != nil {
		a.log.Warn("publish error frame failed", "message_id", messageID, "err", err.Error())
	}
}

// parseLastEventID 解析断线续传头。
//
// 非法值一律当 0（从头补发）：宁可重复几帧让前端看到重复内容，
// 也不能因为一个脏 header 直接 500——那样用户刷新页面就再也接不上了。
func parseLastEventID(raw string) int {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v < 0 {
		return 0
	}
	return v
}
