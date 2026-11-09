package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func budgetTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// 每天的账必须在 Redis 里累加，而不是被后一笔覆盖。
func TestRedisBudgetAccumulates(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	day := "test-budget-" + time.Now().UTC().Format("20060102.150405.000000000")
	key := budgetKey(day)
	rdb.Del(ctx, key)
	t.Cleanup(func() { rdb.Del(ctx, key) })

	s := NewBudgetStore(rdb, budgetTestLogger())

	if total, err := s.AddCost(ctx, day, 0.25); err != nil || total != 0.25 {
		t.Fatalf("第一笔 AddCost = (%v, %v), want (0.25, nil)", total, err)
	}
	if total, err := s.AddCost(ctx, day, 0.75); err != nil || total != 1.0 {
		t.Fatalf("第二笔 AddCost = (%v, %v), want (1, nil)", total, err)
	}
	if got, err := s.LoadCost(ctx, day); err != nil || got != 1.0 {
		t.Fatalf("LoadCost = (%v, %v), want (1, nil)", got, err)
	}
}

// TTL 必须在 (0, 8 天] 之间：没有 TTL 会让键永远躺着，太短又查不到昨天的账。
func TestRedisBudgetTTLSet(t *testing.T) {
	rdb := testRedis(t)
	ctx := context.Background()
	day := "test-budget-ttl-" + time.Now().UTC().Format("20060102.150405.000000000")
	key := budgetKey(day)
	rdb.Del(ctx, key)
	t.Cleanup(func() { rdb.Del(ctx, key) })

	s := NewBudgetStore(rdb, budgetTestLogger())
	if _, err := s.AddCost(ctx, day, 1); err != nil {
		t.Fatalf("AddCost 失败: %v", err)
	}
	ttl, err := rdb.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("TTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > defaultBudgetTTL {
		t.Fatalf("TTL = %v, 期望落在 (0, %v]", ttl, defaultBudgetTTL)
	}
}

// 没有 Redis 时不能报错：调用方据此退化成进程内计数，而不是拒绝服务。
func TestRedisBudgetNilClient(t *testing.T) {
	s := NewBudgetStore(nil, budgetTestLogger())
	ctx := context.Background()

	got, err := s.LoadCost(ctx, "2026-11-09")
	if err != nil || got != 0 {
		t.Fatalf("LoadCost = (%v, %v), want (0, nil)", got, err)
	}
	total, err := s.AddCost(ctx, "2026-11-09", 0.7)
	if err != nil || total != 0.7 {
		t.Fatalf("AddCost = (%v, %v), want (0.7, nil)", total, err)
	}
}
