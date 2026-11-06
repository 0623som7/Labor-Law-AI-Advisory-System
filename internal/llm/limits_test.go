package llm

import (
	"context"
	"testing"
	"time"

	"laborlaw/internal/port"
)

func TestTokenBucketAllowsInitialBurst(t *testing.T) {
	b := newTokenBucket(2) // 容量 2

	for i := 0; i < 2; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		err := b.Wait(ctx)
		cancel()
		if err != nil {
			t.Fatalf("第 %d 个令牌应当立刻拿到: %v", i+1, err)
		}
	}
}

func TestTokenBucketBlocksWhenEmpty(t *testing.T) {
	b := newTokenBucket(1)
	if err := b.Wait(context.Background()); err != nil {
		t.Fatalf("首个令牌应当立刻拿到: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()

	err := b.Wait(ctx)
	if err == nil {
		t.Fatal("令牌耗尽后应当阻塞到 ctx 结束")
	}
	if code := port.CodeOf(err); code != port.CodeRateLimited {
		t.Fatalf("错误码 = %v, want 42901", code)
	}
}

func TestTokenBucketReplenishes(t *testing.T) {
	b := newTokenBucket(20) // 每 50ms 回一个
	if err := b.Wait(context.Background()); err != nil {
		t.Fatalf("首个令牌: %v", err)
	}

	time.Sleep(70 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if err := b.Wait(ctx); err != nil {
		t.Fatalf("令牌应当已补充: %v", err)
	}
}

func TestBudgetAccumulatesAndExceeds(t *testing.T) {
	b := newBudget(1.0)
	fixed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed }
	b.day = b.dayKey(fixed)

	b.add(0.6)
	if spent, limit := b.snapshot(); spent != 0.6 || limit != 1.0 {
		t.Fatalf("snapshot = (%v, %v), want (0.6, 1)", spent, limit)
	}
	if b.exceeded() {
		t.Fatal("0.6 < 1 不该判定超限")
	}

	b.add(0.5)
	if !b.exceeded() {
		t.Fatal("累计 1.1 > 1 应当判定超限")
	}
}

func TestBudgetResetsOnNewDay(t *testing.T) {
	b := newBudget(1.0)
	day1 := time.Date(2026, 11, 6, 23, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return day1 }
	b.day = b.dayKey(day1)
	b.add(5)

	if !b.exceeded() {
		t.Fatal("当天已超额")
	}

	day2 := day1.Add(2 * time.Hour) // 跨到 UTC 次日
	b.now = func() time.Time { return day2 }

	if b.exceeded() {
		t.Fatal("跨天后应当重置")
	}
	if spent, _ := b.snapshot(); spent != 0 {
		t.Fatalf("跨天后 spent = %v, want 0", spent)
	}
}

func TestBudgetIgnoresNonPositiveCost(t *testing.T) {
	b := newBudget(1)
	b.add(0)
	b.add(-5)
	if spent, _ := b.snapshot(); spent != 0 {
		t.Fatalf("spent = %v, want 0", spent)
	}
}
