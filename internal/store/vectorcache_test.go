package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedis 连一台真 Redis，连不上就跳过。
//
// 缓存是本轮新增的唯一外部依赖，用一个 FakeCache 测等于只测了自己的 map；
// 编码、TTL、redis.Nil 这三件事只有真 Redis 能验。
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	client := redis.NewClient(&redis.Options{Addr: addr})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("没有可用的 Redis（%s）：%v", addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestRedisVectorCacheRoundTrip(t *testing.T) {
	client := testRedis(t)
	cache := NewVectorCache(client, nil)
	ctx := context.Background()
	key := "retrieval:qvec:test-roundtrip"

	t.Cleanup(func() { client.Del(context.Background(), key) })

	vec := []float32{0.125, -1.5, 3.25, 0, 1e-7}
	cache.SetVector(ctx, key, vec)

	got, ok := cache.GetVector(ctx, key)
	if !ok {
		t.Fatal("刚写进去的向量读不到")
	}
	if len(got) != len(vec) {
		t.Fatalf("长度 = %d, want %d", len(got), len(vec))
	}
	for i := range vec {
		if got[i] != vec[i] {
			t.Fatalf("第 %d 位 = %v, want %v（逐位相等，不能有精度损失）", i, got[i], vec[i])
		}
	}

	if ttl := client.TTL(ctx, key).Val(); ttl <= 0 {
		t.Fatalf("键应当带过期时间，实际 TTL=%v", ttl)
	}
}

func TestRedisVectorCacheExpires(t *testing.T) {
	client := testRedis(t)
	cache := NewVectorCache(client, nil).WithTTL(time.Millisecond)
	ctx := context.Background()
	key := "retrieval:qvec:test-expire"

	t.Cleanup(func() { client.Del(context.Background(), key) })

	cache.SetVector(ctx, key, []float32{1, 2, 3})
	time.Sleep(20 * time.Millisecond)

	if _, ok := cache.GetVector(ctx, key); ok {
		t.Fatal("过了 TTL 还读到向量，说明过期时间没写进去")
	}
}

// TestRedisVectorCacheWithoutClient 钉住"没 Redis 就是不缓存，不是错误"。
func TestRedisVectorCacheWithoutClient(t *testing.T) {
	cache := NewVectorCache(nil, nil)
	ctx := context.Background()
	cache.SetVector(ctx, "retrieval:qvec:none", []float32{1})
	if _, ok := cache.GetVector(ctx, "retrieval:qvec:none"); ok {
		t.Fatal("没有 Redis 时不该命中")
	}
}

// TestDecodeVectorRejectsGarbage 钉住格式校验：长度不是 4 的倍数就当未命中，
// 而不是把半截字节解成一堆垃圾浮点喂进点积。
func TestDecodeVectorRejectsGarbage(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, {1}, {1, 2, 3, 4, 5}} {
		if _, ok := decodeVector(raw); ok {
			t.Fatalf("长度 %d 的字节不该被当成向量", len(raw))
		}
	}
}
