package port

import (
	"context"
	"encoding/json"
)

// Handler 是 B 注册给 A 的任务处理函数。
//
// 提供方：A（定义）。消费方：B（注册）。
// 并发安全：同一个 Handler 会被池中多个 worker 并发调用，实现必须自带隔离，
// 不许用包级变量存单次任务的状态。
// 错误：返回 *AppError 时 A 按 Code.Retryable 决定是否重试（最多 2 次）；
// 返回普通 error 时一律不重试，直接标 failed——未分类错误大概是确定性 bug，
// 重试只会把一个缺陷变成三次资源浪费。
// 变更：签名几乎不变。需要"取消原因"之类的透出时，走 progress 的 stage 字段。
//
// 为什么不让 B 自己起 goroutine：并发上限、超时、重试、进度回写、优雅关闭这五件事
// 必须集中在一处，否则并发数会随 B 加节点而失控，压测时才发现跑出了 200 个 goroutine。
type Handler func(ctx context.Context, payload json.RawMessage, progress func(percent int, stage string)) error
