package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/go-sql-driver/mysql"

	"laborlaw/internal/auth"
	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// userColumns 是 users 表的读取列序，与 scanUser 一一对应。
//
// 不含 phone_hash：它只作为检索条件出现，读回来没有任何用途，
// 读出来反而多一个"不小心序列化进响应"的机会。
const userColumns = `id, phone_masked, password_hash, nickname, created_at`

// mysqlErrDupEntry 是 MySQL 唯一键冲突的错误号。
//
// 用驱动给的错误号而不是 strings.Contains(err.Error(), "Duplicate entry")：
// 文案会随版本与语言变化，一旦变了，冲突就会静默退化成 50001，
// 表现为"重复注册偶发 500"。
const mysqlErrDupEntry = 1062

// Users 是 port.UserStore 的 MySQL 实现。
//
// 手机号在库里从不以明文出现：检索走 phone_hash（HMAC-SHA256），展示走 phone_masked。
// 因此 DAL 必须持有哈希密钥——它不接受"调用方已经算好哈希"的入参，那样
// 一旦某条调用路径忘了算，就会写出一个永远查不到的账号。
type Users struct {
	db querier
	// secret 是手机号 HMAC 的密钥，与 JWT 签名共用（见 auth.Hash 的注释）。
	secret string
}

// NewUsers 构造账号域 DAL。
func NewUsers(db *sql.DB, secret string) *Users {
	return &Users{db: db, secret: secret}
}

// newUsersOn 构造绑定在任意连接（连接池或事务）上的账号域 DAL。
func newUsersOn(q querier, secret string) *Users {
	return &Users{db: q, secret: secret}
}

// 编译期断言：实现必须与 B 依赖的接口一致。
var _ port.UserStore = (*Users)(nil)

// Create 插入用户。
//
// 入参 u.Phone 是用户输入的明文号码，出参被就地改写成脱敏形态：
// 这样调用方拿到的结构体可以直接进响应体，不需要再记得脱敏一次。
// 忘记脱敏的那一次就会把明文手机号写进日志或响应——把安全性做进数据流，
// 比把安全性写成一条口头约定可靠。
func (u *Users) Create(ctx context.Context, user *port.User) error {
	if user == nil {
		return port.New(port.CodeInvalidArgument, "用户为空")
	}

	phone, err := auth.Normalize(user.Phone)
	if err != nil {
		return port.Wrap(port.CodeInvalidArgument, err, "手机号格式不正确")
	}
	phoneHash, err := auth.Hash(u.secret, phone)
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "手机号哈希失败")
	}
	if user.PasswordHash == "" {
		return port.New(port.CodeInvalidArgument, "密码哈希为空")
	}
	if user.ID == "" {
		user.ID = id.New()
	}

	_, err = u.db.ExecContext(ctx,
		`INSERT INTO users (id, phone_masked, phone_hash, password_hash, nickname)
		 VALUES (?, ?, ?, ?, ?)`,
		user.ID, auth.Mask(phone), phoneHash, user.PasswordHash, user.Nickname,
	)
	if err != nil {
		var myErr *mysql.MySQLError
		if errors.As(err, &myErr) && myErr.Number == mysqlErrDupEntry {
			return port.Wrap(port.CodeConflict, err, "该手机号已注册")
		}
		return port.Wrap(port.CodeInternal, err, "写入用户失败")
	}

	// 回读 created_at：列默认值由数据库生成，应用层自己造一个"差不多的 now"
	// 会在跨时区部署时与库里的值差上整小时，而前端拿它做"注册于"展示。
	var createdAt time.Time
	if err := u.db.QueryRowContext(ctx,
		`SELECT created_at FROM users WHERE id = ?`, user.ID).Scan(&createdAt); err != nil {
		// 回读失败不返回错误：插入已经成功，此时回 500 会让用户重试并撞上 40901，
		// 比"创建时间显示得粗略"糟糕得多。
		user.CreatedAt = time.Now().UTC()
	} else {
		user.CreatedAt = createdAt
	}

	user.Phone = auth.Mask(phone)
	return nil
}

// GetByPhone 按明文手机号查用户，登录用。
//
// 未命中返回 40401 而不是 40101：DAL 只负责"有没有"，把"有没有"折成"一律失败"
// 是 API 层的职责——DAL 若擅自改成 40101，将来做"账号解封"之类的后台功能
// 就会需要一条能区分存在性的查询，而那时无从判断差异从何而来。
func (u *Users) GetByPhone(ctx context.Context, phone string) (*port.User, error) {
	norm, err := auth.Normalize(phone)
	if err != nil {
		return nil, port.Wrap(port.CodeInvalidArgument, err, "手机号格式不正确")
	}
	hash, err := auth.Hash(u.secret, norm)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "手机号哈希失败")
	}

	row := u.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE phone_hash = ? AND deleted_at IS NULL`, hash)

	user, err := scanUser(row)
	if err != nil {
		return nil, notFound(err, "用户不存在")
	}
	return user, nil
}

// Get 按 ID 查用户，鉴权与 token 校验用。
func (u *Users) Get(ctx context.Context, userID string) (*port.User, error) {
	if userID == "" {
		return nil, port.New(port.CodeInvalidArgument, "用户 ID 为空")
	}
	row := u.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ? AND deleted_at IS NULL`, userID)

	user, err := scanUser(row)
	if err != nil {
		return nil, notFound(err, "用户不存在")
	}
	return user, nil
}

// scanUser 把一行扫进 port.User。
func scanUser(row *sql.Row) (*port.User, error) {
	var user port.User
	if err := row.Scan(&user.ID, &user.Phone, &user.PasswordHash, &user.Nickname, &user.CreatedAt); err != nil {
		return nil, err
	}
	return &user, nil
}

// unimplementedUsers 是 Users() 在"没有数据库或没给密钥"时的兜底。
//
// 与 unimplemented.go 里六个域的占位同一理由：返回 nil 接口会让调用方
// 在第一个方法调用上 panic，而 panic 在 recovery 之后只剩一条 500 日志。
type unimplementedUsers struct{}

var _ port.UserStore = unimplementedUsers{}

func (unimplementedUsers) Create(context.Context, *port.User) error {
	return errNotImplemented("账号")
}

func (unimplementedUsers) GetByPhone(context.Context, string) (*port.User, error) {
	return nil, errNotImplemented("账号")
}

func (unimplementedUsers) Get(context.Context, string) (*port.User, error) {
	return nil, errNotImplemented("账号")
}
