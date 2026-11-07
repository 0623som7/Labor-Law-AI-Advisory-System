package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// isDuplicate 判断错误是不是唯一键冲突。
//
// 用驱动给的错误号而不是匹配文案：文案随版本与语言变化，
// 一旦变了，冲突会静默退化成 50001。
func isDuplicate(err error) bool {
	var myErr *mysql.MySQLError
	return errors.As(err, &myErr) && myErr.Number == mysqlErrDupEntry
}

// sessionColumns 是 sessions 表的读取列序，与 scanSession 一一对应。
const sessionColumns = `id, user_id, title, scene, created_at, updated_at`

// messageColumns 是 messages 表的读取列序，与 scanMessage 一一对应。
const messageColumns = `id, session_id, role, content, prompt_tokens, completion_tokens, trace_id, created_at`

// 会话列表的分页默认值与上限。
//
// 上限必须存在：侧边栏是一次拉完再本地筛选的，不设上限时一个重度用户
// 能把几千条会话连同标题一起拖进一次响应。
const (
	defaultSessionPageSize = 20
	maxSessionPageSize     = 100
)

// defaultSessionTitle 是没有传标题时的兜底。
//
// 空字符串标题会在侧边栏留下一行看不见的内容，比"未命名会话"更难排查。
const defaultSessionTitle = "新会话"

// Sessions 是 port.SessionStore 的 MySQL 实现。
type Sessions struct{ db querier }

// NewSessions 构造会话域 DAL。
func NewSessions(db *sql.DB) *Sessions { return &Sessions{db: db} }

// newSessionsOn 构造绑定在任意连接（连接池或事务）上的会话域 DAL。
func newSessionsOn(q querier) *Sessions { return &Sessions{db: q} }

var _ port.SessionStore = (*Sessions)(nil)

// Create 插入一条会话，ID 为空时生成 ULID。
//
// 不在这里校验"用户是否存在"：那是调用方的事，而多查一次 users
// 只会让每个会话创建多一次往返，却挡不住任何真实攻击。
func (s *Sessions) Create(ctx context.Context, sn *port.Session) error {
	if sn == nil || sn.UserID == "" {
		return port.New(port.CodeInvalidArgument, "会话缺少 user_id")
	}
	if sn.ID == "" {
		sn.ID = id.New()
	}
	if sn.Scene == "" {
		sn.Scene = "consult"
	}
	title := strings.TrimSpace(sn.Title)
	if title == "" {
		title = defaultSessionTitle
	}
	// 标题列是 VARCHAR(100)。按 rune 截而不是按字节：一个汉字 3 字节，
	// 按字节截会把标题切成半个字符，MySQL 严格模式下直接报错。
	if r := []rune(title); len(r) > 100 {
		title = string(r[:100])
	}

	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (id, user_id, title, scene) VALUES (?, ?, ?, ?)`,
		sn.ID, sn.UserID, title, sn.Scene)
	if err != nil {
		if isDuplicate(err) {
			return port.Wrap(port.CodeConflict, err, "会话已存在")
		}
		return port.Wrap(port.CodeInternal, err, "创建会话失败")
	}
	sn.Title = title
	return nil
}

// Get 按 ID 查会话，同时按 userID 过滤。
//
// 归属写进 WHERE 而不是查完再比：少写一次条件就是一次越权，
// 而越权查询在日志里和正常查询长得一模一样。
func (s *Sessions) Get(ctx context.Context, userID, sessionID string) (*port.Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, sessionID, userID)
	return scanSession(row)
}

// ListByUser 按最后活跃时间倒序返回会话列表。
func (s *Sessions) ListByUser(ctx context.Context, userID string, limit, offset int) ([]port.Session, error) {
	limit = normalizePage(limit, defaultSessionPageSize, maxSessionPageSize)
	if offset < 0 {
		offset = 0
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions
		 WHERE user_id = ? AND deleted_at IS NULL
		 ORDER BY updated_at DESC, id DESC
		 LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "查询会话列表失败")
	}
	defer rows.Close()

	// 空结果返回长度为 0 的切片而不是 nil：前端会把 null 序列化成 null，
	// 再对它 .map() 就是一个白屏。
	out := make([]port.Session, 0, limit)
	for rows.Next() {
		var (
			sn        port.Session
			createdAt time.Time
			updatedAt time.Time
		)
		if err := rows.Scan(&sn.ID, &sn.UserID, &sn.Title, &sn.Scene, &createdAt, &updatedAt); err != nil {
			return nil, port.Wrap(port.CodeInternal, err, "读取会话行失败")
		}
		sn.CreatedAt = createdAt
		sn.UpdatedAt = updatedAt
		out = append(out, sn)
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历会话行失败")
	}
	return out, nil
}

// Touch 更新最后活跃时间。
//
// 影响行数为 0 说明会话不存在、不属于该用户或已删除，三种情况对外都是 40401：
// 说清"存在但不是你的"等于把别人的会话 ID 是否存在告诉了他。
func (s *Sessions) Touch(ctx context.Context, userID, sessionID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = CURRENT_TIMESTAMP(3)
		 WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, sessionID, userID)
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "更新会话活跃时间失败")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "读取更新行数失败")
	}
	if affected == 0 {
		return port.New(port.CodeNotFound, "会话不存在")
	}
	return nil
}

// Delete 软删会话。消息不级联删除，保留审计能力。
//
// 软删而不是 DELETE：用户投诉"我的对话记录没了"时，硬删之后没有任何东西可查。
func (s *Sessions) Delete(ctx context.Context, userID, sessionID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET deleted_at = CURRENT_TIMESTAMP(3)
		 WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, sessionID, userID)
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "删除会话失败")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "读取删除行数失败")
	}
	if affected == 0 {
		return port.New(port.CodeNotFound, "会话不存在")
	}
	return nil
}

// Messages 是 port.MessageStore 的 MySQL 实现。
type Messages struct{ db querier }

// NewMessages 构造消息域 DAL。
func NewMessages(db *sql.DB) *Messages { return &Messages{db: db} }

// newMessagesOn 构造绑定在任意连接（连接池或事务）上的消息域 DAL。
func newMessagesOn(q querier) *Messages { return &Messages{db: q} }

var _ port.MessageStore = (*Messages)(nil)

// Append 追加一条消息。
//
// 不校验会话归属：调用方必须先 Session().Get 过一遍，这里的签名也拿不到 userID。
func (m *Messages) Append(ctx context.Context, msg *port.ChatMessage) error {
	if msg == nil || msg.SessionID == "" || msg.Role == "" {
		return port.New(port.CodeInvalidArgument, "消息缺少 session_id 或 role")
	}
	if msg.ID == "" {
		msg.ID = id.New()
	}
	_, err := m.db.ExecContext(ctx, `INSERT INTO messages
		(id, session_id, role, content, prompt_tokens, completion_tokens, trace_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		msg.ID, msg.SessionID, string(msg.Role), msg.Content,
		msg.PromptTokens, msg.CompletionTokens, msg.TraceID)
	if err != nil {
		if isDuplicate(err) {
			return port.Wrap(port.CodeConflict, err, "消息已存在")
		}
		return port.Wrap(port.CodeInternal, err, "写入消息失败")
	}
	return nil
}

// ListBySession 按时间正序返回最近 limit 条消息。
//
// 实现上是"先倒序取 limit 条，再翻转"：直接正序 LIMIT 拿到的是最早的一批，
// 而对话历史要的永远是最新的那几轮。
func (m *Messages) ListBySession(ctx context.Context, sessionID string, limit int) ([]port.ChatMessage, error) {
	if limit <= 0 {
		limit = defaultSessionPageSize
	}
	if limit > maxSessionPageSize {
		limit = maxSessionPageSize
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT `+messageColumns+` FROM messages
		 WHERE session_id = ?
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "查询消息失败")
	}
	defer rows.Close()

	out := make([]port.ChatMessage, 0, limit)
	for rows.Next() {
		var (
			msg       port.ChatMessage
			role      string
			createdAt time.Time
		)
		if err := rows.Scan(&msg.ID, &msg.SessionID, &role, &msg.Content,
			&msg.PromptTokens, &msg.CompletionTokens, &msg.TraceID, &createdAt); err != nil {
			return nil, port.Wrap(port.CodeInternal, err, "读取消息行失败")
		}
		msg.Role = port.Role(role)
		msg.CreatedAt = createdAt
		out = append(out, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历消息行失败")
	}

	// 翻成时间正序：上下游都按"从旧到新"拼接上下文。
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// scanSession 把一个会话行扫进 port.Session。
func scanSession(row interface{ Scan(...any) error }) (*port.Session, error) {
	var (
		sn        port.Session
		createdAt time.Time
		updatedAt time.Time
	)
	if err := row.Scan(&sn.ID, &sn.UserID, &sn.Title, &sn.Scene, &createdAt, &updatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, port.New(port.CodeNotFound, "会话不存在")
		}
		return nil, port.Wrap(port.CodeInternal, err, "读取会话失败")
	}
	sn.CreatedAt = createdAt
	sn.UpdatedAt = updatedAt
	return &sn, nil
}

// normalizePage 把分页参数收敛到有效区间，避免负数或超大 limit 直接进 SQL。
func normalizePage(limit, def, max int) int {
	if limit <= 0 {
		limit = def
	}
	if limit > max {
		limit = max
	}
	return limit
}
