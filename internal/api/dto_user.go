package api

import (
	"time"

	"laborlaw/internal/port"
)

// userDTO 是 components.schemas.User 的 HTTP 形状。
//
// 只放四个字段：id / phone_masked / nickname / created_at。手机号明文、密码哈希、
// 删除标记一律不出现在这里——DTO 的字段集合就是"对外可见面"的定义，
// 把 port.User 直接序列化会把新增的内部字段自动泄漏出去。
type userDTO struct {
	ID          string    `json:"id"`
	PhoneMasked string    `json:"phone_masked"`
	Nickname    string    `json:"nickname"`
	CreatedAt   time.Time `json:"created_at"`
}

// toUserDTO 映射用户。nil 返回零值而不是 panic：handler 已判过 nil，
// 这里再兜一层只为让"某天忘了判"退化成一条空响应而不是 500。
func toUserDTO(u *port.User) userDTO {
	if u == nil {
		return userDTO{}
	}
	return userDTO{
		ID:          u.ID,
		PhoneMasked: u.Phone,
		Nickname:    u.Nickname,
		CreatedAt:   u.CreatedAt,
	}
}

// authResultDTO 是 components.schemas.AuthResult 的 HTTP 形状。
//
// 注册与登录都返回它：契约写明注册直接发令牌，省掉一次登录往返。
type authResultDTO struct {
	User         userDTO `json:"user"`
	AccessToken  string  `json:"access_token"`
	RefreshToken string  `json:"refresh_token"`
	// ExpiresIn 是 access token 剩余秒数，前端据此决定何时静默续期。
	ExpiresIn int64 `json:"expires_in"`
}
