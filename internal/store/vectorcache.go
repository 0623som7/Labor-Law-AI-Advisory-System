package store

import (
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"time"

	"github.com/redis/go-redis/v9"
)

// defaultVectorCacheTTL 是查询向量的存活时间。
//
// 24 小时足够覆盖一次彩排到正式演示的全过程，又不至于让改过词表后的旧向量
// 长期留在库里；键里带了模型名，换模型不需要手工清。
const defaultVectorCacheTTL = 24 * time.Hour

// RedisVectorCache 用 Redis 存查询向量。
//
// 为什么不 import internal/retrieval：那样 store 与 retrieval 会互相依赖
// （retrieval 已经在用 store 的 StatuteStore），Go 不允许包级循环引用。
// 两边各声明一个结构相同的小接口，靠鸭子类型对上即可。
type RedisVectorCache struct {
	client *redis.Client
	log    *slog.Logger
	ttl    time.Duration
}

// NewVectorCache 构造查询向量缓存。
func NewVectorCache(client *redis.Client, log *slog.Logger) *RedisVectorCache {
	if log == nil {
		log = slog.Default()
	}
	return &RedisVectorCache{client: client, log: log, ttl: defaultVectorCacheTTL}
}

// WithTTL 覆盖存活时间，测试用毫秒级值验证过期。
func (c *RedisVectorCache) WithTTL(ttl time.Duration) *RedisVectorCache {
	if ttl > 0 {
		c.ttl = ttl
	}
	return c
}

// GetVector 读一个查询向量。没 Redis、没这个键、格式不对、Redis 报错，一律当成没命中。
func (c *RedisVectorCache) GetVector(ctx context.Context, key string) ([]float32, bool) {
	if c.client == nil {
		return nil, false
	}
	raw, err := c.client.Get(ctx, key).Bytes()
	if err != nil {
		// redis.Nil 是正常未命中，不该刷日志；其它错误记一条就够，检索照常走公网。
		if err != redis.Nil {
			c.log.Warn("读取查询向量缓存失败，按未命中处理", "err", err.Error())
		}
		return nil, false
	}
	vec, ok := decodeVector(raw)
	if !ok {
		c.log.Warn("查询向量缓存格式不对，按未命中处理", "len", len(raw))
		return nil, false
	}
	return vec, true
}

// SetVector 写一个查询向量。任何失败都只记日志：缓存是优化，不能反过来影响检索。
func (c *RedisVectorCache) SetVector(ctx context.Context, key string, vec []float32) {
	if c.client == nil || len(vec) == 0 {
		return
	}
	if err := c.client.Set(ctx, key, encodeVector(vec), c.ttl).Err(); err != nil {
		c.log.Warn("写入查询向量缓存失败", "err", err.Error())
	}
}

// encodeVector 用 little-endian float32 定长编码：1024 维 = 4KB，JSON 要 12KB 左右。
//
// 不用 gob/json：这是一次性写入、高频读取的热键，解码开销与体积都值得省。
func encodeVector(vec []float32) []byte {
	buf := make([]byte, 4*len(vec))
	for i, v := range vec {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(v))
	}
	return buf
}

// decodeVector 是 encodeVector 的逆操作。长度不是 4 的倍数说明不是我们写的。
func decodeVector(raw []byte) ([]float32, bool) {
	if len(raw) == 0 || len(raw)%4 != 0 {
		return nil, false
	}
	vec := make([]float32, len(raw)/4)
	for i := range vec {
		vec[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return vec, true
}
