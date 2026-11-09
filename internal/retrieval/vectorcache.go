package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// queryVectorPrefix 是缓存键前缀。
//
// 带前缀是为了在 Redis 里一眼看出是谁写的，且能用 --scan --pattern 单独清：
// 与 auth:*、task:* 这些键混在一堆时，运维只能靠 key 名认领。
const queryVectorPrefix = "retrieval:qvec:"

// VectorCache 缓存"一句话 → 一个查询向量"。
//
// 只做性能优化，任何失败都必须表现成"没命中"，不许让检索失败：缓存挂了顶多
// 多一次公网往返，而检索失败等于这次回答没有法条依据。
//
// 方法不带 error 返回是故意的——有 error 的接口会诱使调用方把它当成失败分支，
// 而这层本来就没有失败这个概念；Redis 的错误在实现里自己吞掉并记 Warn。
type VectorCache interface {
	GetVector(ctx context.Context, key string) ([]float32, bool)
	SetVector(ctx context.Context, key string, vec []float32)
}

// QueryVectorKey 是缓存键。scope 标识"哪一家的哪个模型"，不能只用模型名：
// 同名同维不同空间的向量混在一起时，余弦仍然算得出数，只是名次没有意义。
//
// 只带模型名时换平台换不掉缓存：两个平台都叫 text-embedding-v3、都返 1024 维，
// 维度校验挡不住，于是旧缓存被当成命中，检索看着正常、引用全错、日志一条都没有。
// scope 由装配点拼成"模型@host"，换服务商必然换键。
//
// 只 TrimSpace，不 lowercase、不去标点：中文改一个标点就是另一句话，归一化是
// 检索层的事（keywords.go 的 aliases），缓存不掺和，否则关键词词表一调，
// 缓存里躺着的就是旧口径的向量。
func QueryVectorKey(scope, input string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + strings.TrimSpace(input)))
	return queryVectorPrefix + hex.EncodeToString(sum[:])
}
