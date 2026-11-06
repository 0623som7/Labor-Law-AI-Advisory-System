package auth

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testSecret = "0123456789abcdef0123456789abcdef"

// tokenWith 构造一个篡改后的令牌：保留合法签名段，替换头部或载荷。
func tokenWith(t *testing.T, secret string, head jwtHeader, c Claims) string {
	t.Helper()
	h, err := json.Marshal(head)
	if err != nil {
		t.Fatalf("序列化头失败: %v", err)
	}
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("序列化载荷失败: %v", err)
	}
	signing := b64.EncodeToString(h) + "." + b64.EncodeToString(b)
	return signing + "." + b64.EncodeToString(mac(secret, signing))
}

func TestSignParseRoundTrip(t *testing.T) {
	tok, err := Sign(testSecret, "laborlaw", Claims{Sub: "u1", Typ: TypAccess, Jti: "j1"}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	c, err := Parse(testSecret, "laborlaw", tok, TypAccess)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if c.Sub != "u1" || c.Jti != "j1" || c.Typ != TypAccess || c.Iss != "laborlaw" {
		t.Fatalf("载荷不符: %+v", c)
	}
	if c.Exp <= c.Iat {
		t.Fatalf("exp 应当晚于 iat: iat=%d exp=%d", c.Iat, c.Exp)
	}
}

func TestParseRejectsWrongSecret(t *testing.T) {
	tok, err := Sign(testSecret, "laborlaw", Claims{Sub: "u1", Typ: TypAccess}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := Parse("ffffffffffffffffffffffffffffffff", "laborlaw", tok, TypAccess); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("换密钥应当报签名错误，实得 %v", err)
	}
}

func TestParseRejectsTamperedPayload(t *testing.T) {
	tok, err := Sign(testSecret, "laborlaw", Claims{Sub: "u1", Typ: TypAccess}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	parts := strings.Split(tok, ".")
	body, err := b64.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("解码载荷失败: %v", err)
	}
	var c Claims
	if err := json.Unmarshal(body, &c); err != nil {
		t.Fatalf("反序列化载荷失败: %v", err)
	}
	c.Sub = "attacker"
	patched, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	tampered := parts[0] + "." + b64.EncodeToString(patched) + "." + parts[2]

	if _, err := Parse(testSecret, "laborlaw", tampered, TypAccess); !errors.Is(err, ErrTokenSignature) {
		t.Fatalf("篡改载荷应当报签名错误，实得 %v", err)
	}
}

func TestParseRejectsAlgNone(t *testing.T) {
	// 经典 alg=none 构造：头部声明 none，签名段留空。
	head := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	body := b64.EncodeToString([]byte(`{"sub":"attacker","typ":"access","iss":"laborlaw","exp":9999999999}`))
	token := head + "." + body + "."

	if _, err := Parse(testSecret, "laborlaw", token, TypAccess); !errors.Is(err, ErrTokenAlg) {
		t.Fatalf("alg=none 应当被拒绝，实得 %v", err)
	}
}

func TestParseRejectsAlgHS512(t *testing.T) {
	tok := tokenWith(t, testSecret, jwtHeader{Alg: "HS512", Typ: "JWT"}, Claims{
		Sub: "u1", Typ: TypAccess, Iss: "laborlaw", Exp: time.Now().Add(time.Minute).Unix(),
	})
	if _, err := Parse(testSecret, "laborlaw", tok, TypAccess); !errors.Is(err, ErrTokenAlg) {
		t.Fatalf("HS512 应当被拒绝，实得 %v", err)
	}
}

func TestParseRejectsExpired(t *testing.T) {
	// 过期 61 秒：超出 30 秒偏移窗口。
	c := Claims{Sub: "u1", Typ: TypAccess, Iss: "laborlaw", Exp: time.Now().Add(-61 * time.Second).Unix()}
	tok := tokenWith(t, testSecret, jwtHeader{Alg: "HS256", Typ: "JWT"}, c)
	if _, err := Parse(testSecret, "laborlaw", tok, TypAccess); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("过期令牌应当被拒绝，实得 %v", err)
	}
}

func TestParseToleratesClockSkew(t *testing.T) {
	// 刚过期 10 秒：在 30 秒窗口内，应当放行。
	c := Claims{Sub: "u1", Typ: TypAccess, Iss: "laborlaw", Exp: time.Now().Add(-10 * time.Second).Unix()}
	tok := tokenWith(t, testSecret, jwtHeader{Alg: "HS256", Typ: "JWT"}, c)
	if _, err := Parse(testSecret, "laborlaw", tok, TypAccess); err != nil {
		t.Fatalf("10 秒偏移应当被容忍，实得 %v", err)
	}
}

func TestParseRejectsTypeMismatch(t *testing.T) {
	tok, err := Sign(testSecret, "laborlaw", Claims{Sub: "u1", Typ: TypRefresh}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := Parse(testSecret, "laborlaw", tok, TypAccess); !errors.Is(err, ErrTokenType) {
		t.Fatalf("refresh 当 access 用应当被拒绝，实得 %v", err)
	}
	// 反向同样要拒。
	acc, err := Sign(testSecret, "laborlaw", Claims{Sub: "u1", Typ: TypAccess}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := Parse(testSecret, "laborlaw", acc, TypRefresh); !errors.Is(err, ErrTokenType) {
		t.Fatalf("access 当 refresh 用应当被拒绝，实得 %v", err)
	}
}

func TestParseRejectsEmptyWantTyp(t *testing.T) {
	tok, err := Sign(testSecret, "laborlaw", Claims{Sub: "u1", Typ: TypAccess}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := Parse(testSecret, "laborlaw", tok, ""); !errors.Is(err, ErrTokenType) {
		t.Fatalf("未指定期望类型应当被拒绝，实得 %v", err)
	}
}

func TestParseRejectsIssuerMismatch(t *testing.T) {
	tok, err := Sign(testSecret, "other", Claims{Sub: "u1", Typ: TypAccess}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := Parse(testSecret, "laborlaw", tok, TypAccess); !errors.Is(err, ErrTokenIssuer) {
		t.Fatalf("iss 不符应当被拒绝，实得 %v", err)
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"abc",
		"a.b",
		"a.b.c.d",
		"!!!.@@@.###",
		"eyJhbGciOiJIUzI1NiJ9", // 只有一个合法段
	}
	for _, token := range cases {
		if _, err := Parse(testSecret, "laborlaw", token, TypAccess); err == nil {
			t.Fatalf("%q 不应当解析成功", token)
		}
	}
}

func TestParseRequiresSecret(t *testing.T) {
	tok, err := Sign(testSecret, "laborlaw", Claims{Sub: "u1", Typ: TypAccess}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := Parse("", "laborlaw", tok, TypAccess); !errors.Is(err, ErrTokenSecret) {
		t.Fatalf("空密钥应当报错，实得 %v", err)
	}
}

func TestSignUsesRawURLEncoding(t *testing.T) {
	tok, err := Sign(testSecret, "laborlaw", Claims{Sub: "u1", Typ: TypAccess, Jti: "j"}, time.Minute)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	// 标准 base64 的 + / = 在 URL 里要转义，JWT 规定用 RawURLEncoding。
	if strings.ContainsAny(tok, "+/=") {
		t.Fatalf("令牌含非 URL 安全字符: %s", tok)
	}
	if _, err := base64.RawURLEncoding.DecodeString(strings.Split(tok, ".")[0]); err != nil {
		t.Fatalf("头部不是 RawURLEncoding: %v", err)
	}
}
