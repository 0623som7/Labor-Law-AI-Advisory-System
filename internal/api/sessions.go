package api

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// 消息历史的分页上限与 openapi 的 /sessions/{sessionId}/messages 对齐。
// 会话列表用 parsePageParams 的 1..100，这里必须单独一套：两个接口的
// 契约上限本来就不同，硬套一个常量会让其中一方偏离 openapi。
const (
	defaultMessageLimit = 50
	maxMessageLimit     = 200
	// maxSessionTitleRunes 是 openapi Session.title 的 maxLength。
	maxSessionTitleRunes = 100
)

// sessionScenes 是 openapi Scene 的 enum。
//
// 用白名单而不是"非空即可"：scene 决定后续走哪套编排与提示词，
// 存进一个拼错的场景名，用户会发现咨询页永远返回空回答，而且日志里看不出来。
var sessionScenes = map[string]struct{}{
	"consult": {},
	"qa":      {},
}

// createSessionRequest 对应 openapi 的 SessionCreateRequest。
type createSessionRequest struct {
	// Scene 必填，取值 consult / qa。
	Scene string `json:"scene"`
	// Title 可选，缺省由存储层填"新会话"。
	Title string `json:"title"`
}

// listSessions 处理 GET /api/v1/sessions。
func (a *API) listSessions(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("会话列表")(c)
		return
	}
	limit, offset, ok := parsePageParams(c)
	if !ok {
		return
	}
	items, err := a.store.Session().ListByUser(ctx, UserID(c), limit, offset)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, toSessionDTOs(items))
}

// createSession 处理 POST /api/v1/sessions。
func (a *API) createSession(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("创建会话")(c)
		return
	}

	var req createSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		FailCode(c, port.CodeInvalidArgument, "请求体不是合法的 JSON")
		return
	}

	scene := strings.TrimSpace(req.Scene)
	if scene == "" {
		FailCode(c, port.CodeInvalidArgument, "scene 必填")
		return
	}
	if _, ok := sessionScenes[scene]; !ok {
		FailCode(c, port.CodeInvalidArgument, "scene 只能是 consult 或 qa")
		return
	}

	title := strings.TrimSpace(req.Title)
	if utf8.RuneCountInString(title) > maxSessionTitleRunes {
		FailCode(c, port.CodeInvalidArgument, "会话标题不能超过 100 个字符")
		return
	}

	sn := &port.Session{UserID: UserID(c), Title: title, Scene: scene}
	if err := a.store.Session().Create(ctx, sn); err != nil {
		Fail(c, err)
		return
	}

	// Create 只回填 ID 与标题（created_at / updated_at 由 MySQL 默认值生成），
	// 而契约要求响应里有这两列。回读一次把成本换成正确性：
	// 建会话是低频操作，而返回零值时间会让前端立刻显示 1970。
	if full, err := a.store.Session().Get(ctx, UserID(c), sn.ID); err == nil {
		sn = full
	}
	OK(c, toSessionDTO(sn))
}

// getSession 处理 GET /api/v1/sessions/{sessionId}。
func (a *API) getSession(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("会话详情")(c)
		return
	}
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	if sessionID == "" {
		FailCode(c, port.CodeInvalidArgument, "会话 ID 不能为空")
		return
	}
	s, err := a.store.Session().Get(ctx, UserID(c), sessionID)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, toSessionDTO(s))
}

// deleteSession 处理 DELETE /api/v1/sessions/{sessionId}。
//
// 先 Get 再 Delete 是有意的重复查询：Get 明确区分"不存在/不属于我"（40401），
// 而 Delete 的影响行数为 0 同时包含这两种情况与"已经删过"。
// 想给前端一句准确的报错，就只能先读一次。
func (a *API) deleteSession(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("删除会话")(c)
		return
	}
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	if sessionID == "" {
		FailCode(c, port.CodeInvalidArgument, "会话 ID 不能为空")
		return
	}
	if _, err := a.store.Session().Get(ctx, UserID(c), sessionID); err != nil {
		Fail(c, err)
		return
	}
	if err := a.store.Session().Delete(ctx, UserID(c), sessionID); err != nil {
		Fail(c, err)
		return
	}
	OK(c, nil)
}

// listMessages 处理 GET /api/v1/sessions/{sessionId}/messages。
//
// 先校验会话归属再读消息：messages 表只有 session_id，不校验就等于
// 任何人拿到一个 session ID 就能读到别人的对话。
func (a *API) listMessages(c *gin.Context) {
	ctx := c.Request.Context()
	if a.store == nil {
		a.notImplemented("消息历史")(c)
		return
	}
	sessionID := strings.TrimSpace(c.Param("sessionId"))
	if sessionID == "" {
		FailCode(c, port.CodeInvalidArgument, "会话 ID 不能为空")
		return
	}
	if _, err := a.store.Session().Get(ctx, UserID(c), sessionID); err != nil {
		Fail(c, err)
		return
	}

	limit, ok := parseLimitParam(c, defaultMessageLimit, maxMessageLimit)
	if !ok {
		return
	}
	items, err := a.store.Message().ListBySession(ctx, sessionID, limit)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, toChatMessageDTOs(items))
}

// parseLimitParam 解析可以单独定上限的 limit 参数。
//
// 与 parsePageParams 分开放：那个函数把 100 写死在函数体里，
// 改它会影响所有分页接口；消息历史的 200 是 openapi 单独给的额度。
func parseLimitParam(c *gin.Context, def, max int) (int, bool) {
	raw := strings.TrimSpace(c.Query("limit"))
	if raw == "" {
		return def, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 || v > max {
		FailCode(c, port.CodeInvalidArgument, "limit 必须是 1 到 %d 的整数", max)
		return 0, false
	}
	return v, true
}
