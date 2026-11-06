package llm

import (
	"context"

	"laborlaw/internal/port"
)

// UsageRecorder 持久化一次调用的计量。
//
// 用接口而不是直接依赖 store：llm 包不应该认识 SQL，测试用内存实现即可。
// 实现方必须真的落库并在失败时返回 error——静默丢弃会让日成本闸在无人察觉的
// 情况下被击穿，而这类问题通常要到账单出来才发现。
type UsageRecorder interface {
	Record(ctx context.Context, rec port.UsageRecord) error
}
