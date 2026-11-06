package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
)

// phonePattern 是中国大陆手机号：1 开头、第二位 3-9、共 11 位。
//
// 用正则而不是逐位判断：号码规则将来放宽（如新增号段）时只改这一行，
// 且这条规则与 contract/openapi.yaml 里 RegisterRequest.phone.pattern 逐字一致。
var phonePattern = regexp.MustCompile(`^1[3-9]\d{9}$`)

// ErrPhoneFormat 表示手机号格式不合法。
//
// 只暴露一个错误值：调用方需要区分"格式错"与"密钥没配"，后者走 ErrTokenSecret。
var ErrPhoneFormat = errors.New("手机号格式不正确")

// Normalize 规整并校验手机号，返回 11 位纯数字。
//
// 去掉空格与连字符是为人服务的：用户从通讯录粘贴过来常带 "138-0013-8000"。
// 但去完必须严格匹配，不做"自动补 0"之类的猜测——猜错就是把 A 的账号认成 B。
func Normalize(phone string) (string, error) {
	p := strings.TrimSpace(phone)
	p = strings.NewReplacer(" ", "", "-", "", "\u00a0", "").Replace(p)
	if !phonePattern.MatchString(p) {
		return "", ErrPhoneFormat
	}
	return p, nil
}

// Mask 把手机号脱敏成 前3****后4 的形态。
//
// 长度不足 7 时整串打码而不是切出怪形状：脱敏函数的输出会进日志与响应体，
// 半个号码比全打码更容易让人误以为"就差一点"。
func Mask(phone string) string {
	if len(phone) < 7 {
		if phone == "" {
			return ""
		}
		return strings.Repeat("*", len(phone))
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

// Hash 计算手机号的检索哈希（HMAC-SHA256，hex 编码 64 字符）。
//
// 为什么用 HMAC 而不是裸 SHA256：手机号空间只有约 10^10，裸哈希用一张彩虹表
// 就能反查全部号码。加一个只有服务端知道的密钥做 HMAC，库被拖走后攻击者
// 也拿不到密钥，无法离线建表。
//
// 密钥复用 JWT_SECRET 而不是新增配置项：两者都是"进程级秘密"，
// 多一个键就多一处部署时忘记配置的机会；而它已经满足 minSecretLen=32 的强度要求。
func Hash(secret, phone string) (string, error) {
	if secret == "" {
		return "", ErrTokenSecret
	}
	h := hmac.New(sha256.New, []byte(secret))
	h.Write([]byte(phone))
	return hex.EncodeToString(h.Sum(nil)), nil
}
