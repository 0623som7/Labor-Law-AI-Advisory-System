package agent

import (
	"unicode/utf8"

	"laborlaw/internal/port"
)

// historyReserveTokens 是消息角色标记与分隔符的余量。
//
// 按条数留而不是按 token 算：没人会去给 "<|im_start|>user" 数 token，
// 一条消息的封装开销在 4~8 token 量级，200 对十几条历史足够宽松。
const historyReserveTokens = 200

// estimateTokens 估算一段文本的 token 数。
//
// 汉字按 1 token/字（qwen 分词器大致如此，取 rune 数当上界）；
// ASCII 会被高估 3~4 倍——方向是安全的：宁可少带两句历史，
// 也不让上游回 400 说 context length exceeded。
//
// 不用 tiktoken 之类的库：为一次粗估引一棵依赖树（含 CGO 或几 MB 词表）不值得，
// 而这里的误差方向已经被"高估"这个选择兜住了。
func estimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return utf8.RuneCountInString(s)
}

// trimHistory 从最新往回装历史，直到预算用完。
//
// 返回保留的消息（保持原有正序，老的在前面）和丢弃条数，后者供日志与测试。
// 截断后若首条是 assistant（留下一句没有提问的回答），从头丢掉直到首条是 user——
// 模型看到开场就是自己的发言会接错话。
// 历史里不可能出现两条连续 assistant，但用循环而不是 if，免得将来改落库时漏。
//
// 预算为零或为负时返回空切片而不是 nil：调用方会直接 append 结果，
// nil 与空在这里没有语义差别，但测试里 len==0 的断言会因为 nil 少一层信息。
func trimHistory(msgs []port.Message, budget int) (kept []port.Message, dropped int) {
	if budget <= 0 || len(msgs) == 0 {
		return make([]port.Message, 0), len(msgs)
	}

	used := 0
	start := len(msgs)
	for i := len(msgs) - 1; i >= 0; i-- {
		cost := estimateTokens(msgs[i].Content)
		if used+cost > budget {
			break
		}
		used += cost
		start = i
	}

	dropped = start
	kept = make([]port.Message, 0, len(msgs)-start)
	kept = append(kept, msgs[start:]...)

	// 丢掉"没有提问的回答"：裁剪点正好落在 assistant 上时，模型会以为
	// 上一句是它自己说的、用户还没开口，接着往下编。
	for len(kept) > 0 && kept[0].Role != port.RoleUser {
		kept = kept[1:]
		dropped++
	}
	return kept, dropped
}
