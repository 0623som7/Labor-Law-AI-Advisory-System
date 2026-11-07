package api

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/port"
)

// 查询边界与 contract/openapi.yaml 的 /statutes 参数声明逐项对齐。
const (
	// statuteQueryMaxRunes 对应 q 的 maxLength: 100。按 rune 数而不是字节数：
	// 契约的 minLength/maxLength 在 JSON Schema 里数的是字符，中文一个字三字节，
	// 按字节算会让 34 个汉字就被判超长。
	statuteQueryMaxRunes = 100
	// statuteTopKDefault / Max 对应 top_k 的 default: 5 与 maximum: 20。
	statuteTopKDefault = 5
	statuteTopKMax     = 20
)

// listStatutes 处理 GET /api/v1/statutes。
//
// 检索必须走 a.statutes（retrieval.Retriever）而不是 store.SearchText：
// 用户口语与法条用语不是一套词（"加班费" vs "延长工作时间"），归一那一步在这里。
func (a *API) listStatutes(c *gin.Context) {
	if a.statutes == nil {
		// 无存储模式：路由在、实现没接。与其它未实现接口保持同一种失败方式。
		a.notImplemented("法条检索")(c)
		return
	}

	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		FailCode(c, port.CodeInvalidArgument, "q 不能为空")
		return
	}
	if utf8.RuneCountInString(q) > statuteQueryMaxRunes {
		FailCode(c, port.CodeInvalidArgument, "q 最长 %d 个字", statuteQueryMaxRunes)
		return
	}

	topK, ok := parseStatuteTopK(c.Query("top_k"))
	if !ok {
		FailCode(c, port.CodeInvalidArgument, "top_k 必须是 1 到 %d 的整数", statuteTopKMax)
		return
	}

	// law 原样透传：契约只声明它是字符串，这里不维护"合法法律名"白名单——
	// 那样的白名单得跟着种子数据一起改，而检索本来就只会命中库里真有的法律。
	law := strings.TrimSpace(c.Query("law"))
	hits, err := a.statutes.SearchIn(c.Request.Context(), q, law, topK)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, toStatuteHitDTOs(hits))
}

// parseStatuteTopK 解析 top_k：缺省取契约的 default，越界或非数字一律 40001。
//
// 不静默截断：前端传 100 是以为能拿到 100 条，悄悄减到 20 只会让它以为库里只有 20 条。
func parseStatuteTopK(raw string) (int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return statuteTopKDefault, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > statuteTopKMax {
		return 0, false
	}
	return n, true
}
