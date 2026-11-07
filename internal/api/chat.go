package api

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/id"
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
)

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
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		// 会话落库要等 store 的 Session 域，这里先生成一个可用的 ID 返回给前端，
		// 保证"同一轮问答的 message 与 session 是一对稳定标识"。
		sessionID = id.New()
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
	emit := func(ev port.Event) error {
		if ev.Type == port.AgentEventDone {
			// 记下来避免下面再补一个 done：两个 done 会让前端重复结束。
			sawDone = true
		}
		if _, perr := a.hub.Publish(messageID, ev); perr != nil {
			return perr
		}
		return nil
	}

	result, runErr := a.runner.Run(ctx, port.Request{
		Scene:     chatScene,
		SessionID: sessionID,
		Input:     req.Input,
		Options:   req.Options,
	}, emit)
	if runErr != nil {
		a.publishRunError(messageID, runErr)
		return
	}

	if !sawDone {
		if _, err := a.hub.PublishDone(messageID, messageID, result.FinishReason, result.Usage); err != nil {
			a.log.Warn("publish done failed", "message_id", messageID, "err", err.Error())
		}
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
