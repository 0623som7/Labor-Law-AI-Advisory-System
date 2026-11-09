package llm

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// 这三个函数是"上游错误 → 业务码"的唯一映射点。
//
// 为什么要从 Client 的方法里提出来：向量化客户端也要用同一套映射，而 Client 的
// 方法绑在 *Client 上带不走。复制一份的话，改错误码时就得记得改两处，迟早漏。

// redactKey 把文本里可能出现的 API key 抹掉。
func redactKey(cfg config.LLMConfig, s string) string {
	if cfg.APIKey == "" {
		return s
	}
	return strings.ReplaceAll(s, cfg.APIKey, "***")
}

// mapStatusError 把非 200 映射成业务码。
//
// 上游的错误正文只作为 cause 进日志，不进 Message：它经常把请求体回显出来，
// 而请求体里可能已经有合同原文。回显 Authorization 头的网关也真实存在，
// 所以这里先做一次 key 脱敏再进错误链。
func mapStatusError(cfg config.LLMConfig, status int, raw []byte) error {
	detail := errors.New(truncate(redactKey(cfg, strings.TrimSpace(string(raw))), 300))
	switch {
	case status == http.StatusTooManyRequests:
		return port.Wrap(port.CodeRateLimited, detail, "模型服务限流")
	case status >= 500:
		return port.Wrap(port.CodeUpstream, detail, "模型服务错误（HTTP %d）", status)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		// 鉴权失败不会因为重试而恢复，因此用不可重试的码，只把原因留在 cause 里。
		return port.Wrap(port.CodeInvalidArgument, detail, "模型鉴权失败，请检查 LLM_API_KEY")
	default:
		return port.Wrap(port.CodeInvalidArgument, detail, "模型请求被拒绝（HTTP %d）", status)
	}
}

// mapTransportError 把网络层错误翻译成业务码。
//
// 超时有三条来源，必须全部认出来，否则同一个 50ms 超时在不同 Go 版本上会
// 落到不同业务码：
//  1. ctx 到点 → context.DeadlineExceeded；
//  2. 调用方取消 → context.Canceled；
//  3. transport 的 ResponseHeaderTimeout 先到 → net.Error.Timeout() == true，
//     它既不是 context 错误，文本也不稳定（"timeout awaiting response headers"）。
func mapTransportError(cfg config.LLMConfig, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || isTimeout(err) {
		return port.Wrap(port.CodeUpstreamTimeout, errors.New(redactKey(cfg, err.Error())), "模型调用超时")
	}
	return port.Wrap(port.CodeUpstream, errors.New(redactKey(cfg, err.Error())), "模型调用失败")
}
