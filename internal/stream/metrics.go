package stream

import "sync/atomic"

// droppedTotal 是全进程累计因背压丢弃的帧数。
//
// Writer.Dropped() 只反映单个订阅者，而运维要回答的是"这台机器整体在丢帧吗"，
// 所以在这里额外维护一个包级计数，而不是去遍历 Hub 里所有存活的 Writer
// （遍历要拿 Hub 锁，还把"流已关闭但计数还在涨"的窗口漏掉）。
var droppedTotal atomic.Int64

// DroppedTotal 返回全进程累计因背压丢弃的帧数。
//
// 只增不减：它是 counter 而不是 gauge，进程重启归零正是 Prometheus 里
// counter 的语义（用 rate() 看趋势，不看绝对值）。
func DroppedTotal() int64 { return droppedTotal.Load() }
