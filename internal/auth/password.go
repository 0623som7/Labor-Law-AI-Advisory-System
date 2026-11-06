package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost 是哈希成本。
//
// cost 12 在本机实测约 200~300ms：对登录接口是"可接受的慢"，
// 对离线撞库是"每百万候选要跑几十小时"。调低到默认 10 会让破解成本降到 1/4，
// 调高到 14 会让并发登录把 CPU 打满——12 是这两个约束的交点。
const bcryptCost = 12

// dummyHash 是一份固定的、格式合法的 bcrypt 哈希，明文是随机串。
//
// 它的唯一用途：用户不存在时也执行一次等价耗时的比对，让"手机号未注册"与
// "密码错误"在响应时间上不可区分。没有它，攻击者可以用一台秒表按 250ms / 0.1ms
// 的差异批量枚举哪些手机号在系统里。
const dummyHash = "$2a$12$C6UzMDM.H6dfI/f/IKcEe.j4nJkvJHqjT3kbd1SxKgQkAJ1rVfLZC"

// ErrPasswordMismatch 是密码校验失败的唯一错误。
//
// 不复用 bcrypt 的原始错误：bcrypt.ErrMismatchedHashAndPassword 与
// "哈希格式非法" 是两种成因，混在一起会让上层误以为可以据此区分"账号坏"。
var ErrPasswordMismatch = errors.New("密码不匹配")

// bcryptMaxBytes 是 bcrypt 实际参与运算的明文长度。
//
// bcrypt 的输入按 72 字节截断——这是算法本身的定义（Blowfish 的 key schedule
// 只吃 72 字节），不是库的脾气。Go 版本选择报错而不是静默截断，于是同一件事
// 在调用方看起来像个"输入不合法"，其实只是一个上限。
const bcryptMaxBytes = 72

// truncateForBcrypt 把明文截到 bcrypt 的长度上限。
//
// 截断必须发生在哈希与校验两侧，且规则一致：只对字节前缀敏感是 bcrypt 的固有
// 语义，前端按字符数限制密码（8-64 字符），一个 40 字的中文密码就是 120 字节，
// 不截断的话这种完全合法的密码会在建号时直接失败。
func truncateForBcrypt(plain string) string {
	if len(plain) <= bcryptMaxBytes {
		return plain
	}
	return plain[:bcryptMaxBytes]
}

// HashPassword 生成 bcrypt 哈希。
//
// 不做长度以外的强度校验：密码策略属于参数校验层（见 API 的 RegisterRequest），
// 放在这里会让"改策略"需要改 DAL。
func HashPassword(plain string) (string, error) {
	if plain == "" {
		return "", errors.New("密码为空")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(truncateForBcrypt(plain)), bcryptCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// VerifyPassword 校验密码，成功返回 nil。
//
// 传空哈希直接返回失败而不是 panic：数据库里出现空哈希说明写入流程有缺陷，
// 但那不该让登录接口 500——501 与 401 的差别会泄漏"这个账号存在但坏了"。
func VerifyPassword(hash, plain string) error {
	if hash == "" || plain == "" {
		return ErrPasswordMismatch
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(truncateForBcrypt(plain))) != nil {
		return ErrPasswordMismatch
	}
	return nil
}

// VerifyPasswordAlways 永远跑一次 bcrypt 比对，用于登录路径。
//
// hash 为空（用户不存在）时用 dummyHash 顶上，使失败耗时不随"账号是否存在"变化。
// 返回值恒为 ErrPasswordMismatch 或 nil，但调用方在用户不存在时应当直接按失败处理，
// 不要把这个返回值当判据——它只能告诉你"密码对不对"，不能告诉你"用户有没有"。
func VerifyPasswordAlways(hash, plain string) error {
	if hash == "" {
		// 参数顺序是 CompareHashAndPassword(hash, plain)，这里刻意把 dummyHash 放第一位。
		_ = bcrypt.CompareHashAndPassword([]byte(dummyHash), []byte(truncateForBcrypt(plain)))
		return ErrPasswordMismatch
	}
	return VerifyPassword(hash, plain)
}
