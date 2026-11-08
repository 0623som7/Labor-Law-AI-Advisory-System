package llm

import "context"

// userIDKey 是 context 里的用户 ID 键。
//
// 用私有空结构体而不是字符串：其他包拿到同一个字符串就能覆写，
// 而私有类型只有本包能构造，杜绝"某个中间件顺手塞个 user_id"的意外。
type userIDKey struct{}

// WithUserID 把当前用户挂到 ctx 上，供计量落库归属。
//
// 走 context 而不是给 ChatRequest 加字段：ChatRequest 是 port 里冻结的契约，
// 加字段要 B 签字；而 user_id 只影响"记到谁账上"，不影响模型调用本身。
// 另一个好处是重试、流式与后台任务都自动继承，不用在每个调用点手工传。
func WithUserID(ctx context.Context, userID string) context.Context {
	if userID == "" {
		return ctx
	}
	return context.WithValue(ctx, userIDKey{}, userID)
}

// UserIDFrom 取回 ctx 上的用户；没有则返回空串（匿名路径）。
func UserIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(userIDKey{}).(string)
	return v
}
