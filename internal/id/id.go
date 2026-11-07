// Package id 生成 26 位 ULID。
//
// 为什么单独成包：api 要它（message_id / session_id）、store 要它（file_id / page_id），
// 两处各写一份必然漂移——一处用 Crockford 字母表、另一处用 hex，落库后长度都对不上。
package id

import (
	"crypto/rand"
	"sync"
	"time"
)

// crockford 是 ULID 规定的 Base32 字母表：去掉了 I / L / O / U，
// 避免人工转录时把 1 和 I、0 和 O 看混。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Len 是 ULID 的字符数，与库里 CHAR(26) 的主键定义一致。
const Len = 26

// 单调性状态：同一毫秒内连续生成的 ID 必须严格递增。
//
// 没有它的时候，"按字典序排就是时间序"只在跨毫秒成立：一次请求里 user 与 assistant
// 两条消息几乎总落在同一毫秒，后 16 位是纯随机的，于是 ORDER BY created_at DESC, id DESC
// 会把两条消息的先后顺序随机翻转——对话历史被倒着喂给模型，日志里看不出任何异常。
var (
	mu       sync.Mutex
	lastMS   uint64
	lastTail [16]byte // 上一枚 ID 的后 16 个 Base32 字符
	seeded   bool
)

// New 生成一个 ULID：前 10 位是毫秒时间戳，后 16 位是 80 位随机数（同毫秒内递增）。
//
// 用它而不是自增/UUID 的原因：按字典序排就是时间序（含同毫秒），列表页可以直接
// ORDER BY id；同时它不暴露"系统里一共有多少条记录"。
func New() string {
	mu.Lock()
	defer mu.Unlock()

	ms := uint64(time.Now().UnixMilli())
	switch {
	case !seeded:
		fillRandom(&lastTail)
		seeded = true
	case ms > lastMS:
		fillRandom(&lastTail)
	case incrTail(&lastTail):
		// 同一毫秒（或时钟回拨）：沿用上一条的后缀并 +1，保证严格递增。
		ms = lastMS
	default:
		// 80 位全部进位（一毫秒内 2^80 枚 ID，实践中不会发生）：借一毫秒重新随机，
		// 而不是回绕成比上一条更小的值。
		ms = lastMS + 1
		fillRandom(&lastTail)
	}
	lastMS = ms

	out := build(ms, lastTail)
	return string(out[:])
}

// build 把时间戳与后 16 位拼成完整 ID。
func build(ms uint64, tail [16]byte) [Len]byte {
	var out [Len]byte
	encodeTime(&out, ms)
	copy(out[10:], tail[:])
	return out
}

// encodeTime 把毫秒时间戳写成前 10 个 Base32 字符。
func encodeTime(out *[Len]byte, ms uint64) {
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&0x1F]
		ms >>= 5
	}
}

// fillRandom 用 80 位随机数填满后 16 个字符。
func fillRandom(tail *[16]byte) {
	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		// crypto/rand 失败意味着内核熵池异常。库代码禁止 panic，
		// 退化用纳秒时间戳填满后 16 位：可预测但唯一，宁可弱一点也不能让请求 500。
		nanos := uint64(time.Now().UnixNano())
		for i := range tail {
			tail[i] = crockford[(nanos>>uint((i%8)*5))&0x1F]
			nanos++
		}
		return
	}

	// 80 位随机数正好切成 16 个 5 位组。
	var bitBuf uint32
	bits := uint(0)
	idx := 0
	for _, b := range rnd {
		bitBuf = bitBuf<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			tail[idx] = crockford[(bitBuf>>bits)&0x1F]
			idx++
		}
	}
}

// incrTail 把 16 位 Base32 后缀当作 80 位大整数加一；全 Z 时返回 false。
func incrTail(tail *[16]byte) bool {
	for i := len(tail) - 1; i >= 0; i-- {
		if tail[i] == 'Z' {
			tail[i] = '0'
			continue
		}
		tail[i] = crockford[indexOf(tail[i])+1]
		return true
	}
	return false
}

// indexOf 返回字符在 Crockford 字母表中的下标（不区分大小写）。
func indexOf(c byte) int {
	if c >= 'a' && c <= 'z' {
		c -= 'a' - 'A'
	}
	for i := 0; i < len(crockford); i++ {
		if crockford[i] == c {
			return i
		}
	}
	return 0
}
