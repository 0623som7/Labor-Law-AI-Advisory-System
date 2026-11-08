package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/auth"
	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// 账号域对外文案。
//
// 抽成常量是因为三条要求逐字依赖它们：登录失败文案必须"一字不差"，
// 否则"账号不存在"和"密码错"一旦被写成两句话，就等于把账号枚举送给了攻击者。
const (
	msgBadCredentials = "手机号或密码错误"
	msgUnauthorized   = "登录状态已失效，请重新登录"
	msgNeedLogin      = "请先登录"
	msgUserService    = "账号服务未就绪"
	msgTokenService   = "令牌服务暂不可用"
	msgRefreshService = "续期服务暂不可用"
	msgRateLimited    = "登录尝试过于频繁，请稍后再试"
	msgRegRateLimited = "注册尝试过于频繁，请稍后再试"
	msgBadBody        = "请求体格式不正确"
	msgBodyTooLarge   = "请求体过大"
)

// 注册请求的字段边界，与 contract/openapi.yaml 的 RegisterRequest 对齐。
const (
	passwordMinLen = 8
	passwordMaxLen = 64
	nicknameMaxLen = 32
	// loginWindow 是登录限流窗口。每 IP 每分钟 N 次，窗口固定而非滑动：
	// 实现只有一次 INCR + 一次 EXPIRE，而滑动窗口要 ZSET，MVP 阶段收益不值这个复杂度。
	loginWindow = time.Minute
	// loginIPRateLimit 是单 IP 每分钟的登录尝试上限。
	//
	// 取 20 而不是 AUTH_LOGIN_RATE_LIMIT（默认 5）：一个教室、一间公司、一个
	// 运营商 NAT 后面常常有几十个人共用一个出口 IP。按 5 次算，评委里第一个人
	// 连错几次，后面所有人都会被 429 挡在门外——限流不该由陌生人替自己买单。
	// 挡单机撞库 20 次/分钟足够：对手要试 10^10 个号码，这个速率毫无意义。
	loginIPRateLimit = 20
	// rateLimitRetryAfter 是 429 响应里 Retry-After 的秒数，与 loginWindow 对齐。
	// 前端不读它，但 curl 调试和演示时能直接说明"等多久再来"。
	rateLimitRetryAfter = 60
	// registerRateLimit 是注册接口每 IP 每分钟的上限。与登录同量级，但更值钱：
	// 注册没有"猜密码"可言，每放行一次就实打实烧掉约 250ms 的 bcrypt CPU。
	registerRateLimit = 5
)

// registerRequest 对应 components.schemas.RegisterRequest。
//
// 这里写 json tag 是允许的：契约规定的对外形状必须由 api 层声明，
// 受限的是 port 结构体——它们不许带 transport 细节。
type registerRequest struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
	Nickname string `json:"nickname"`
}

// loginRequest 对应 components.schemas.LoginRequest。
type loginRequest struct {
	Phone    string `json:"phone"`
	Password string `json:"password"`
}

// refreshRequest 对应 /auth/refresh 的请求体。
type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// register 处理 POST /api/v1/auth/register。
//
// 契约写明注册成功后直接返回令牌（省一次登录往返），所以这里建号后立刻签发，
// 不做"注册归注册、登录归登录"的两次握手。
func (a *API) register(c *gin.Context) {
	var req registerRequest
	if !bindAuthBody(c, &req) {
		return
	}

	// 限流排在哈希之前：要在花掉 bcrypt 的 CPU 之前就把流量挡住。
	if a.registerRateLimited(c.Request.Context(), c) {
		FailCode(c, port.CodeRateLimited, msgRegRateLimited)
		return
	}

	phone, err := auth.Normalize(req.Phone)
	if err != nil {
		Fail(c, port.New(port.CodeInvalidArgument, "参数校验失败").
			WithField("phone", "手机号格式不正确"))
		return
	}
	// 按字符数而不是字节数校验：len("密码") 是 6，用户以为自己在填 2 个字符，
	// 界面却提示"需为 8-64 位"。契约里的 minLength/maxLength 也是字符数。
	if n := utf8.RuneCountInString(req.Password); n < passwordMinLen || n > passwordMaxLen {
		Fail(c, port.New(port.CodeInvalidArgument, "参数校验失败").
			WithField("password", "密码长度需为 8-64 位"))
		return
	}
	nickname := strings.TrimSpace(req.Nickname)
	if utf8.RuneCountInString(nickname) > nicknameMaxLen {
		Fail(c, port.New(port.CodeInvalidArgument, "参数校验失败").
			WithField("nickname", "昵称最长 32 个字符"))
		return
	}

	if a.users == nil {
		// 无存储模式（dev 连不上 MySQL）：明确说"未就绪"，不要退化成 500 的一团雾。
		FailCode(c, port.CodeInternal, msgUserService)
		return
	}

	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		// 不把 err 挂到响应上：bcrypt 的错误信息可能带输入长度，属于内部细节。
		Fail(c, port.Wrap(port.CodeInternal, err, "密码加密失败"))
		return
	}

	u := &port.User{
		ID:           id.New(),
		Phone:        phone,
		Nickname:     nickname,
		PasswordHash: hash,
		CreatedAt:    time.Now().UTC(),
	}
	if err := a.users.Create(c.Request.Context(), u); err != nil {
		if errors.Is(err, &port.AppError{Code: port.CodeConflict}) {
			FailCode(c, port.CodeConflict, "该手机号已注册")
			return
		}
		Fail(c, err)
		return
	}

	res, err := a.issueTokens(c.Request.Context(), u, loggerFrom(c))
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, res)
}

// login 处理 POST /api/v1/auth/login。
//
// 两条硬约束：
//  1. 先限流后校验——顺序反了就是"每次撞库请求都要跑一次 bcrypt"，CPU 先被打满。
//  2. 任何失败都是同一个 40101 和同一句文案，且都花掉一次 bcrypt 的时间。
func (a *API) login(c *gin.Context) {
	var req loginRequest
	if !bindAuthBody(c, &req) {
		return
	}

	ctx := c.Request.Context()
	log := loggerFrom(c)

	if a.loginIPRateLimited(ctx, c) {
		failRateLimited(c)
		return
	}

	phone, err := auth.Normalize(req.Phone)
	if err != nil {
		// 格式错的手机号也走完整的一次 bcrypt，否则"格式非法"会比"密码错"快得多。
		_ = auth.VerifyPasswordAlways("", req.Password)
		failBadCredentials(c)
		return
	}

	// 账号桶必须在 GetByPhone 与 bcrypt 之前：它的意义就是"猜某个人密码"时
	// 连 CPU 都不给对方烧。放在 bcrypt 之后，撞库照样每次跑满 250ms。
	if a.loginAccountRateLimited(ctx, c, phone) {
		failRateLimited(c)
		return
	}

	if a.users == nil {
		FailCode(c, port.CodeInternal, msgUserService)
		return
	}

	u, err := a.users.GetByPhone(ctx, phone)
	if err != nil {
		if !errors.Is(err, &port.AppError{Code: port.CodeNotFound}) {
			// 库挂了不能也报 401：那是把故障说成"密码错"，运维永远查不到。
			// 对外的文案仍旧不透露账号是否存在，但错误码要能区分故障。
			log.Error("登录查询用户失败", "trace_id", TraceID(c), "err", err.Error())
			Fail(c, port.New(port.CodeInternal, "登录服务暂不可用"))
			return
		}
		_ = auth.VerifyPasswordAlways("", req.Password)
		failBadCredentials(c)
		return
	}

	if err := auth.VerifyPasswordAlways(u.PasswordHash, req.Password); err != nil {
		failBadCredentials(c)
		return
	}

	// 成功即清零：两个桶都要清。IP 桶不清，同一出口的其他人在本窗口内会
	// 替这次输错继续背账；账号桶不清，本人下一次登录可能被判超限。
	// 计数器只减不增的话，一天里正常登录几次就会攒到上限，把自己锁进 429。
	if a.cache != nil {
		keys := []string{keyLoginIPAttempts + c.ClientIP()}
		if key, ok := a.loginAccountKey(c, phone); ok {
			keys = append(keys, key)
		}
		if err := a.cache.del(ctx, keys...); err != nil {
			log.Warn("登录成功但限流计数未清零", "trace_id", TraceID(c), "err", err.Error())
		}
	}

	res, err := a.issueTokens(ctx, u, log)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, res)
}

// refresh 处理 POST /api/v1/auth/refresh。
//
// 轮换语义：旧 jti 从白名单里原子取出并删除，再签新的一对。只有取到值的那一个
// 请求能继续——取不到或取到别人的就按失效处理，所以同一枚 refresh 只能用一次。
func (a *API) refresh(c *gin.Context) {
	var req refreshRequest
	if !bindAuthBody(c, &req) {
		return
	}
	token := strings.TrimSpace(req.RefreshToken)
	if token == "" {
		failUnauthorized(c)
		return
	}

	ctx := c.Request.Context()
	log := loggerFrom(c)

	claims, err := auth.Parse(a.cfg.Auth.Secret, a.cfg.Auth.Issuer, token, auth.TypRefresh)
	if err != nil {
		// 令牌内容绝不进日志：它是有效期 30 天的凭据。
		log.Warn("续期令牌校验失败", "trace_id", TraceID(c), "err", err.Error())
		failUnauthorized(c)
		return
	}
	if claims.Jti == "" {
		failUnauthorized(c)
		return
	}

	if a.cache == nil || a.users == nil {
		log.Warn("续期所需依赖缺失", "trace_id", TraceID(c))
		FailCode(c, port.CodeInternal, msgTokenService)
		return
	}

	// 取值与删除合并成一次 GETDEL：分两步做的话，两个并发请求可以都 get 命中、
	// 都 del 成功，同一枚 refresh 于是换出两条续期链，轮换形同虚设。
	owner, found, err := a.cache.getdel(ctx, keyRefreshToken+claims.Jti)
	if err != nil {
		log.Warn("续期白名单不可用", "trace_id", TraceID(c), "err", err.Error())
		FailCode(c, port.CodeInternal, msgTokenService)
		return
	}
	if !found || owner != claims.Sub {
		// found=false：已被用过一次（轮换生效）或已登出。
		// owner 不匹配：令牌与白名单记录对不上，按伪造处理。
		log.Warn("续期令牌不在白名单", "trace_id", TraceID(c), "found", found)
		failUnauthorized(c)
		return
	}

	u, err := a.users.Get(ctx, claims.Sub)
	if err != nil {
		if errors.Is(err, &port.AppError{Code: port.CodeNotFound}) {
			// 用户已被删除：这就是"凭证失效"，403 语义，不必让前端看见故障码。
			failUnauthorized(c)
			return
		}
		// 库抖动不能报成"登录失效"。此刻旧 jti 已经删了，若混进 40101，
		// 用户看到的是"重新登录"，而日志把它记成一次正常过期，故障就被藏起来了。
		log.Error("续期查询用户失败", "trace_id", TraceID(c), "err", err.Error())
		Fail(c, port.New(port.CodeInternal, msgRefreshService))
		return
	}

	res, err := a.issueTokens(ctx, u, log)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, res)
}

// logout 处理 POST /api/v1/auth/logout。
//
// 只吊销续期能力，不动 access token：access 已经无状态地流出去了，收不回来；
// 它的 TTL 是 2h，真正需要长期吊销的是 refresh。吊销失败要明确报错，
// 因为"登出成功"却仍能用 30 天的 refresh 续期，比登出失败更糟。
func (a *API) logout(c *gin.Context) {
	uid := UserID(c)
	log := loggerFrom(c)
	if a.cache == nil {
		// 没有缓存就吊销不了续期令牌。但登出必须成功：access token 本来就收不回来，
		// 回 50001 只会把前端卡在"点了登出还是登不进"的死循环里。
		log.Warn("无缓存，登出仅作废本端会话", "trace_id", TraceID(c))
		OK(c, nil)
		return
	}

	ctx := c.Request.Context()

	jti, found, err := a.cache.get(ctx, keyUserRefresh+uid)
	if err != nil {
		log.Warn("登出反查失败", "trace_id", TraceID(c), "err", err.Error())
		FailCode(c, port.CodeInternal, msgTokenService)
		return
	}
	keys := []string{keyUserRefresh + uid}
	if found {
		keys = append(keys, keyRefreshToken+jti)
	}
	if err := a.cache.del(ctx, keys...); err != nil {
		log.Warn("登出清理失败", "trace_id", TraceID(c), "err", err.Error())
		FailCode(c, port.CodeInternal, msgTokenService)
		return
	}
	OK(c, nil)
}

// me 处理 GET /api/v1/auth/me。
//
// 返回值只经 userDTO 出口：Phone 在库里已是脱敏形态，但"库里是什么"和"接口给什么"
// 必须由 DTO 显式声明——否则某天有人把明文塞回 port.User，接口层会一声不响地跟着泄漏。
func (a *API) me(c *gin.Context) {
	if a.users == nil {
		FailCode(c, port.CodeInternal, msgUserService)
		return
	}
	u, err := a.users.Get(c.Request.Context(), UserID(c))
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, toUserDTO(u))
}

// issueTokens 是签发令牌的唯一出口，登录、注册、续期都走这里。
//
// 白名单写入是尽力而为：写失败不阻断签发，代价只是这个 refresh 之后换不到新令牌
// （refresh 查不到白名单项会拒绝）。反过来如果写失败就回滚登录，Redis 一抖
// 全站都登不进来——两害相权，选"少部分人 2 小时后要重新登录"。
func (a *API) issueTokens(ctx context.Context, u *port.User, log *slog.Logger) (authResultDTO, error) {
	now := time.Now().UTC()

	access, err := auth.Sign(a.cfg.Auth.Secret, a.cfg.Auth.Issuer,
		auth.Claims{Sub: u.ID, Typ: auth.TypAccess, Jti: id.New(), Iat: now.Unix()},
		a.cfg.Auth.AccessTTL)
	if err != nil {
		return authResultDTO{}, port.Wrap(port.CodeInternal, err, "签发访问令牌失败")
	}

	refreshJti := id.New()
	refresh, err := auth.Sign(a.cfg.Auth.Secret, a.cfg.Auth.Issuer,
		auth.Claims{Sub: u.ID, Typ: auth.TypRefresh, Jti: refreshJti, Iat: now.Unix()},
		a.cfg.Auth.RefreshTTL)
	if err != nil {
		return authResultDTO{}, port.Wrap(port.CodeInternal, err, "签发续期令牌失败")
	}

	if a.cache != nil {
		// 先注销旧链，再写新链。顺序反过来会留出一小段两条链同时有效的时间窗，
		// 而"同一用户只有一条续期链"正是靠这里保证的：不注销的话，上一枚 refresh
		// 会活满 30 天，logout 也吊销不到它（反查键已经被新 jti 覆盖）。
		if old, ok, err := a.cache.get(ctx, keyUserRefresh+u.ID); err != nil {
			log.Warn("查询旧续期链失败，旧链将留到自然过期", "err", err.Error())
		} else if ok && old != "" && old != refreshJti {
			if err := a.cache.del(ctx, keyRefreshToken+old); err != nil {
				log.Warn("旧续期链作废失败，该令牌将留到自然过期", "err", err.Error())
			}
		}
		if err := a.cache.set(ctx, keyRefreshToken+refreshJti, u.ID, a.cfg.Auth.RefreshTTL); err != nil {
			log.Warn("续期令牌入白名单失败，该令牌将无法续期", "err", err.Error())
		} else if err := a.cache.set(ctx, keyUserRefresh+u.ID, refreshJti, a.cfg.Auth.RefreshTTL); err != nil {
			log.Warn("续期令牌反查索引写入失败，登出无法吊销该令牌", "err", err.Error())
		}
	} else {
		log.Warn("未注入缓存，续期能力不可用")
	}

	return authResultDTO{
		User:         toUserDTO(u),
		AccessToken:  access,
		RefreshToken: refresh,
		ExpiresIn:    int64(a.cfg.Auth.AccessTTL / time.Second),
	}, nil
}

// loginIPRateLimited 判断该 IP 是否已超过登录尝试上限（防单机撞库）。
func (a *API) loginIPRateLimited(ctx context.Context, c *gin.Context) bool {
	return a.incrWindow(ctx, c, keyLoginIPAttempts+c.ClientIP(), loginIPRateLimit)
}

// loginAccountRateLimited 判断该手机号是否已超过登录尝试上限（防猜某个人密码）。
//
// phone 必须是 auth.Normalize 之后的 11 位号码：拿原始输入做键的话，
// "138 0013 8000" 和 "13800138000" 会各算一个桶，攻击者换个写法就重置了计数。
func (a *API) loginAccountRateLimited(ctx context.Context, c *gin.Context, phone string) bool {
	key, ok := a.loginAccountKey(c, phone)
	if !ok {
		return false
	}
	return a.incrWindow(ctx, c, key, a.cfg.Auth.LoginRateLimit)
}

// loginAccountKey 拼账号桶的 Redis 键，后缀是手机号的 HMAC。
//
// 哈希失败（密钥太短/没配）时限流失效并 Warn：宁可少一道减速带，
// 也不能让带 "-" 的号码因为 Key 拼不出来而绕过限流，更不能 panic 掉登录。
func (a *API) loginAccountKey(c *gin.Context, phone string) (string, bool) {
	sum, err := auth.Hash(a.cfg.Auth.Secret, phone)
	if err != nil || sum == "" {
		loggerFrom(c).Warn("手机号哈希不可用，账号维度限流放行", "trace_id", TraceID(c), "err", err)
		return "", false
	}
	return keyLoginAccountAttempts + sum, true
}

// registerRateLimited 是注册接口的限流：只有 IP 单桶。
//
// 注册防的是"批量注册"，与账号无关；同一个人注册第二个号的正常需求也不该被锁。
func (a *API) registerRateLimited(ctx context.Context, c *gin.Context) bool {
	return a.incrWindow(ctx, c, keyRegisterAttempts+c.ClientIP(), registerRateLimit)
}

// incrWindow 是限流原语：对给定键在固定窗口内计数，返回是否超限。
//
// 失败一律放行并 Warn：限流是防撞库的减速带，不是可用性的前置条件。
// Redis 挂掉时把所有人挡在门外，等于用限流造成一次自己发起的故障。
func (a *API) incrWindow(ctx context.Context, c *gin.Context, key string, limit int) bool {
	if limit <= 0 || a.cache == nil {
		return false
	}
	n, err := a.cache.incr(ctx, key)
	if err != nil {
		loggerFrom(c).Warn("限流不可用，本次放行", "trace_id", TraceID(c), "err", err.Error())
		return false
	}
	if n == 1 {
		if err := a.cache.expire(ctx, key, loginWindow); err != nil {
			// 计数器没有过期时间会变成永久封禁：宁可删键重来，也不能锁死这个键。
			loggerFrom(c).Warn("限流窗口设置失败，重置计数", "trace_id", TraceID(c), "err", err.Error())
			_ = a.cache.del(ctx, key)
			return false
		}
	}
	return n > int64(limit)
}

// failRateLimited 统一超限响应：42901 + Retry-After。
//
// 带上秒数是为了让调用方知道"等一分钟"，而不是把 429 当成服务坏了。
func failRateLimited(c *gin.Context) {
	c.Header("Retry-After", strconv.Itoa(rateLimitRetryAfter))
	FailCode(c, port.CodeRateLimited, msgRateLimited)
}

// bindAuthBody 解析账号域请求体，并把"超限"与"格式错"分成两个错误码。
//
// 请求体上限由 bodyLimit 中间件用 http.MaxBytesReader 施加，越界时那个
// 错误会原样从 ShouldBindJSON 里带出来。两者混成一个 40001 的话，
// 前端只看得到"请求体格式不正确"，永远查不出真正撞的是哪堵墙。
func bindAuthBody(c *gin.Context, dst any) bool {
	if err := c.ShouldBindJSON(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			FailCode(c, port.CodeFileTooLarge, msgBodyTooLarge)
			return false
		}
		FailCode(c, port.CodeInvalidArgument, msgBadBody)
		return false
	}
	return true
}

// failBadCredentials 是登录失败的唯一出口，文案与错误码都不可分叉。
func failBadCredentials(c *gin.Context) {
	FailCode(c, port.CodeUnauthorized, msgBadCredentials)
}

// failUnauthorized 用于令牌类失败（续期、鉴权中间件），与密码错误分开：
// 前者是"凭证过期"，后者是"凭证不对"，文案不同不会泄漏账号是否存在。
func failUnauthorized(c *gin.Context) {
	FailCode(c, port.CodeUnauthorized, msgUnauthorized)
}
