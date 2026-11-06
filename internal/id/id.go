// Package id 生成 26 位 ULID。
//
// 为什么单独成包：api 要它（message_id / session_id）、store 要它（file_id / page_id），
// 两处各写一份必然漂移——一处用 Crockford 字母表、另一处用 hex，落库后长度都对不上。
package id

import (
	"crypto/rand"
	"time"
)

// crockford 是 ULID 规定的 Base32 字母表：去掉了 I / L / O / U，
// 避免人工转录时把 1 和 I、0 和 O 看混。
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// Len 是 ULID 的字符数，与库里 CHAR(26) 的主键定义一致。
const Len = 26

// New 生成一个 ULID：前 10 位是毫秒时间戳，后 16 位是 80 位随机数。
//
// 用它而不是自增/UUID 的原因：按字典序排就是时间序，列表页可以直接 ORDER BY id；
// 同时它不暴露"系统里一共有多少条记录"。
func New() string {
	var out [Len]byte

	ms := uint64(time.Now().UnixMilli())
	for i := 9; i >= 0; i-- {
		out[i] = crockford[ms&0x1F]
		ms >>= 5
	}

	var rnd [10]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		// crypto/rand 失败意味着内核熵池异常。库代码禁止 panic，
		// 退化用纳秒时间戳填满后 16 位：可预测但唯一，宁可弱一点也不能让请求 500。
		nanos := uint64(time.Now().UnixNano())
		for i := 10; i < Len; i++ {
			out[i] = crockford[(nanos>>uint((i%8)*5))&0x1F]
			nanos++
		}
		return string(out[:])
	}

	// 80 位随机数正好切成 16 个 5 位组。
	var bitBuf uint32
	bits := uint(0)
	idx := 10
	for _, b := range rnd {
		bitBuf = bitBuf<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out[idx] = crockford[(bitBuf>>bits)&0x1F]
			idx++
		}
	}

	return string(out[:])
}
