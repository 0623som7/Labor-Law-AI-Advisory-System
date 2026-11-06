package llm

import (
	"context"
	"math"
	"sync"
	"time"

	"laborlaw/internal/port"
)

// tokenBucket 是全局出站限流器。
//
// 为什么自己写而不引 golang.org/x/time/rate：只需要"按 QPS 匀速放行、ctx 取消时放弃"
// 这一种行为，四十行就够；多一个依赖就多一份升级负担，而这个包会被 B 的每个 graph 调用。
//
// 容量取 QPS（至少 1）：允许一次突发，但连续打满时会被压回配置速率。
type tokenBucket struct {
	mu       sync.Mutex
	rate     float64 // 每秒补充的令牌数
	capacity float64
	tokens   float64
	last     time.Time
	now      func() time.Time
}

// newTokenBucket 构造限流器。qps <= 0 时按 1 处理，避免除零死循环。
func newTokenBucket(qps float64) *tokenBucket {
	if qps <= 0 {
		qps = 1
	}
	capacity := math.Max(1, qps)
	return &tokenBucket{
		rate:     qps,
		capacity: capacity,
		tokens:   capacity,
		last:     time.Now(),
		now:      time.Now,
	}
}

// Wait 阻塞到拿到一个令牌，或 ctx 结束。
//
// 返回 *port.AppError 而不是裸 error：调用方（Agent）要能按码判断"是限流还是超时"，
// 两者在前端的表现完全不同——一个该退避重试，一个该等用户重发。
func (b *tokenBucket) Wait(ctx context.Context) error {
	for {
		b.mu.Lock()
		now := b.now()
		if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
			b.tokens = math.Min(b.capacity, b.tokens+elapsed*b.rate)
			b.last = now
		}
		if b.tokens >= 1 {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		need := (1 - b.tokens) / b.rate
		b.mu.Unlock()

		wait := time.Duration(need * float64(time.Second))
		if wait < time.Millisecond {
			wait = time.Millisecond
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return port.Wrap(port.CodeRateLimited, ctx.Err(), "等待限流令牌时请求已取消")
		case <-timer.C:
		}
	}
}
