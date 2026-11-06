// Package auth 实现账号域的密码、手机号与令牌原语。
//
// 为什么手写 HS256 而不是引第三方 JWT 库：全局只用一个算法、一份密钥，
// 需要的校验（alg 白名单、iss、typ、时钟偏移）都在十行以内。引一个通用库意味着
// 接受它支持的全部算法面与解析歧义，而那些正是历史上绕过校验的入口。
package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// 令牌类型。写进 claims 的 typ，用它在解析时区分 access 与 refresh。
const (
	// TypAccess 是访问令牌，随每个业务请求发出。
	TypAccess = "access"
	// TypRefresh 是续期令牌，只能在 /auth/refresh 使用一次。
	TypRefresh = "refresh"
)

// clockSkew 是 exp 校验的容忍窗口。
//
// 容忍是必要的：签发方与校验方可能不是同一台机器，且容器时钟同步存在滞后；
// 30 秒足以吸收 NTP 抖动而远小于 2h 的有效期，不会实质延长令牌寿命。
const clockSkew = 30 * time.Second

// 解析失败的分类。上层全部折成 40101，这里分开只为测试与日志能说清原因。
var (
	// ErrTokenMalformed 表示不是三段、或 base64 / JSON 解不开。
	ErrTokenMalformed = errors.New("令牌格式错误")
	// ErrTokenAlg 表示 alg 不是 HS256（含 "none"）。
	ErrTokenAlg = errors.New("令牌算法不被接受")
	// ErrTokenSignature 表示签名不匹配。
	ErrTokenSignature = errors.New("令牌签名不匹配")
	// ErrTokenIssuer 表示 iss 与配置不符。
	ErrTokenIssuer = errors.New("令牌签发方不符")
	// ErrTokenType 表示 typ 与期望不符，如把 refresh 当 access 用。
	ErrTokenType = errors.New("令牌类型不符")
	// ErrTokenExpired 表示已过 exp（不含偏移）。
	ErrTokenExpired = errors.New("令牌已过期")
	// ErrTokenNotYetValid 表示 iat 在未来超出偏移，时钟回拨或伪造。
	ErrTokenNotYetValid = errors.New("令牌尚未生效")
	// ErrTokenSecret 表示密钥为空，属于装配错误而不是请求错误。
	ErrTokenSecret = errors.New("令牌密钥未配置")
)

// Claims 是令牌载荷。
//
// 字段名按 RFC 7519 的短名：JWT 会被塞进 header，名字越短越好，
// 而且短名是各家校验器都认识的公共词汇，自造字段名会让网关的 JWT 插件失明。
type Claims struct {
	// Sub 是用户 ID（ULID）。
	Sub string `json:"sub"`
	// Iss 是签发方，与配置的 JWT_ISSUER 一致。
	Iss string `json:"iss"`
	// Typ 是令牌类型，access 或 refresh。
	Typ string `json:"typ"`
	// Jti 是令牌唯一 ID。refresh 用它做 Redis 白名单键，保证一次性。
	Jti string `json:"jti"`
	// Iat 是签发时刻（秒）。
	Iat int64 `json:"iat"`
	// Exp 是过期时刻（秒）。
	Exp int64 `json:"exp"`
}

// jwtHeader 是固定头部。
//
// 不提供可配置的 alg：一旦允许调用方指定算法，就等于允许我在这段代码里
// 重新实现一遍 "alg 协商漏洞"。alg 永远由签发方在这里写死，校验方也只认这个值。
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

var b64 = base64.RawURLEncoding

// Sign 用 HS256 签发一个令牌。
//
// ttl 由调用方给（access / refresh 各一份配置），这里不读配置：原语不依赖
// config 包，测试可以用 1 秒的令牌验证过期而不必等待两小时。
func Sign(secret, issuer string, c Claims, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", ErrTokenSecret
	}
	now := time.Now()
	c.Iss = issuer
	c.Iat = now.Unix()
	c.Exp = now.Add(ttl).Unix()

	head, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(c)
	if err != nil {
		return "", err
	}

	signing := b64.EncodeToString(head) + "." + b64.EncodeToString(body)
	return signing + "." + b64.EncodeToString(mac(secret, signing)), nil
}

// Parse 校验令牌并返回载荷。
//
// wantTyp 必须由调用方显式给出，不接受空串：给空串等于"两种令牌通用"，
// 那正是 refresh 令牌被当成 access 直接访问业务接口的路径。
//
// 校验顺序刻意把便宜且与密钥无关的检查（段数、alg）放前面，
// 签名比对放最后——签名之前的一律不可信，包括 iss 与 typ。
func Parse(secret, issuer, token, wantTyp string) (*Claims, error) {
	if secret == "" {
		return nil, ErrTokenSecret
	}
	if wantTyp == "" {
		return nil, ErrTokenType
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, ErrTokenMalformed
	}

	headBytes, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	var head jwtHeader
	if err := json.Unmarshal(headBytes, &head); err != nil {
		return nil, ErrTokenMalformed
	}
	// 先比算法再验签：alg=none 的令牌根本没有第三段签名可验，
	// 若先验签会得到一个"空签名不匹配"的误导性结论，掩盖真正的攻击面。
	if head.Alg != "HS256" {
		return nil, ErrTokenAlg
	}

	// 常量时间比较：普通 == 会在首个不同字节处返回，攻击者能用计时差逐字节猜签名。
	expect := mac(secret, parts[0]+"."+parts[1])
	got, err := b64.DecodeString(parts[2])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	if !hmac.Equal(expect, got) {
		return nil, ErrTokenSignature
	}

	bodyBytes, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrTokenMalformed
	}
	var c Claims
	if err := json.Unmarshal(bodyBytes, &c); err != nil {
		return nil, ErrTokenMalformed
	}

	if issuer != "" && c.Iss != issuer {
		return nil, ErrTokenIssuer
	}
	if c.Typ != wantTyp {
		return nil, ErrTokenType
	}

	now := time.Now()
	if c.Exp == 0 || now.After(time.Unix(c.Exp, 0).Add(clockSkew)) {
		return nil, ErrTokenExpired
	}
	if c.Iat != 0 && time.Unix(c.Iat, 0).After(now.Add(clockSkew)) {
		return nil, ErrTokenNotYetValid
	}
	return &c, nil
}

// mac 计算 HS256 签名。
func mac(secret, signing string) []byte {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(signing))
	return h.Sum(nil)
}
