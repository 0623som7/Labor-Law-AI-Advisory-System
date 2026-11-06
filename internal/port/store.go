package port

import (
	"context"
	"time"
)

// Store 是 B 访问持久层的唯一入口，按域分组而不是三十个方法平铺。
//
// 提供方：A。消费方：B。
// 并发安全：所有方法可并发调用；Tx 内拿到的实例绑定同一连接，不可跨 goroutine 传递。
// 错误：CodeNotFound（查无此资源）、CodeForbidden（归属不符）、CodeConflict（状态机非法转移）、
// CodeInternal（驱动错误）。驱动错误一律经 A 包装，B 不会看到 mysql 的裸错误。
// 变更：高频。加表就加方法，但"只加不改"：删除或改签名必须先出变更单。
//
// 为什么按域分组：B 读代码时只想看"我要的那三类查询"，三十个方法平铺在一屏之外，
// 他会自己另写 SQL，那 DAL 就形同虚设。
type Store interface {
	// Session 返回会话域。
	Session() SessionStore
	// Message 返回消息域。
	Message() MessageStore
	// File 返回文件域。
	File() FileStore
	// Review 返回审查域。
	Review() ReviewStore
	// Statute 返回法条/判例/向量域。
	Statute() StatuteStore
	// Task 返回任务域。
	Task() TaskStore

	// Tx 在一个数据库事务里执行 fn，fn 拿到的 Store 所有操作落在同一事务。
	//
	// fn 返回 error 时回滚；返回 nil 时提交。fn 内不得：
	//   - 起 goroutine 用这个 Store（连接会被归还，跨 goroutine 使用是未定义行为）；
	//   - 再调 Tx（MySQL 不支持真正的嵌套事务，本实现会直接返回 CodeInternal）；
	//   - 执行超过 config.db.tx_timeout 的操作（ctx 到点会中断，回滚）。
	Tx(ctx context.Context, fn func(Store) error) error
}

// SessionStore 是会话域。
type SessionStore interface {
	// Create 插入一条会话，ID 为空时由实现生成 ULID。
	Create(ctx context.Context, s *Session) error
	// Get 按 ID 查会话，同时按 userID 过滤：归属必须是查询条件而不是查完再判断，
	// 少写一次 WHERE 就是一次越权。不匹配返回 CodeNotFound。
	Get(ctx context.Context, userID, id string) (*Session, error)
	// ListByUser 按最后活跃时间倒序返回会话列表。
	// 空结果必须返回长度为 0 的切片，不能返回 nil——前端会把它序列化成 null。
	ListByUser(ctx context.Context, userID string, limit, offset int) ([]Session, error)
	// Touch 更新最后活跃时间，供每次对话复用会话时调用。
	Touch(ctx context.Context, userID, id string) error
	// Delete 软删会话（deleted_at），消息不级联删除，保留审计能力。
	Delete(ctx context.Context, userID, id string) error
}

// MessageStore 是消息域。
type MessageStore interface {
	// Append 追加一条消息。归属校验由调用方先 Session().Get 完成。
	Append(ctx context.Context, m *ChatMessage) error
	// ListBySession 按时间正序返回最近 limit 条消息。
	// 空结果返回空切片而不是 nil。
	ListBySession(ctx context.Context, sessionID string, limit int) ([]ChatMessage, error)
}

// FileStore 是文件与解析结果域。
type FileStore interface {
	// Create 插入文件元数据。
	Create(ctx context.Context, f *FileRecord) error
	// Get 按 ID 查文件，同时按 userID 过滤，不匹配返回 CodeNotFound。
	Get(ctx context.Context, userID, id string) (*FileRecord, error)
	// GetByDigest 按内容哈希查同用户已解析过的同文件，用于跳过重复解析。
	// 未命中返回 CodeNotFound，调用方据此走完整解析流程。
	GetByDigest(ctx context.Context, userID, sha256 string) (*FileRecord, error)
	// Update 覆盖文件的可变字段（页数、OCRUsed、Confidence、StoragePath、UpdatedAt）。
	Update(ctx context.Context, f *FileRecord) error
	// SavePages 写入解析结果（file_pages + text_blocks），同一 fileID 先删后插，保持幂等。
	SavePages(ctx context.Context, fileID string, pages []ParsedPage) error
	// LoadPages 读回解析结果，用于二次审查复用。未解析过返回 CodeNotFound。
	// 空结果返回空切片而不是 nil。
	LoadPages(ctx context.Context, fileID string) ([]ParsedPage, error)
}

// ReviewStore 是审查域。
type ReviewStore interface {
	// Create 插入审查主记录。
	Create(ctx context.Context, r *Review) error
	// Get 按 ID 查审查，同时按 userID 过滤。
	Get(ctx context.Context, userID, id string) (*Review, error)
	// GetByFile 按文件查最近一次审查，用于"同一合同不重复审"。
	GetByFile(ctx context.Context, userID, fileID string) (*Review, error)
	// Update 覆盖审查的可变字段（Status、RiskLevel、Score、ClausePassRate、Counts、UpdatedAt）。
	Update(ctx context.Context, r *Review) error
	// SaveFindings 写入发现列表，同一 reviewID 先删后插，保持幂等。
	SaveFindings(ctx context.Context, reviewID string, findings []Finding) error
	// ListFindings 按严重度倒序返回发现列表。空结果返回空切片而不是 nil。
	ListFindings(ctx context.Context, reviewID string) ([]Finding, error)
	// ListByUser 按创建时间倒序返回用户的审查历史。空结果返回空切片而不是 nil。
	ListByUser(ctx context.Context, userID string, limit, offset int) ([]Review, error)
}

// StatuteStore 是法条、判例与向量域。
//
// 这张域的数据是只读的：只允许全量重建（ReplaceXxx），不允许单条更新。
// 允许单条改却不失效 Redis 里的 24h 副本，就会出现"库里改了、页面上还是旧条文"。
type StatuteStore interface {
	// Get 按业务 ID 查法条。未命中返回 CodeNotFound。
	Get(ctx context.Context, id string) (*Statute, error)
	// ReplaceAll 全量重建法条表（事务内清表后批量插入），重建完成后调用方负责清缓存前缀。
	ReplaceAll(ctx context.Context, statutes []Statute) error
	// Count 返回法条行数，用于数据导入后的验收断言。
	Count(ctx context.Context) (int, error)
	// SearchText 按全文关键词检索法条，law 为空表示不限法律。topK<=0 时由实现取默认 5。
	// 空结果返回空切片而不是 nil。
	SearchText(ctx context.Context, query, law string, topK int) ([]StatuteHit, error)
	// ReplaceCases 全量重建判例表。
	ReplaceCases(ctx context.Context, cases []LegalCase) error
	// SearchCases 按关键词检索判例，region 为空表示不限地域。空结果返回空切片而不是 nil。
	SearchCases(ctx context.Context, query, region string, topK int) ([]CaseHit, error)
	// SaveEmbeddings 批量写向量，同一 (owner_type, owner_id, model) 覆盖旧值。
	SaveEmbeddings(ctx context.Context, items []Embedding) error
	// LoadEmbeddings 按宿主类型与模型名读回全部向量，供 B 在内存里建索引。
	// 换模型必须整体重建，混用会算出无意义的相似度。空结果返回空切片而不是 nil。
	LoadEmbeddings(ctx context.Context, ownerType, model string) ([]Embedding, error)
}

// TaskStore 是任务域。
type TaskStore interface {
	// Create 插入任务，初始状态必须是 TaskPending，否则返回 CodeConflict。
	Create(ctx context.Context, t *Task) error
	// Get 按 ID 查任务。
	Get(ctx context.Context, id string) (*Task, error)
	// UpdateProgress 回写进度。percent 会被夹到 0-100；写入方需自行节流（≥500ms 一次）。
	UpdateProgress(ctx context.Context, id string, percent int, stage string) error
	// Finish 把任务置为终态并写入失败原因（只放可展示文案）。
	// 非法转移返回 CodeConflict。
	Finish(ctx context.Context, id string, status TaskStatus, message string) error
	// ListByStatus 按状态取任务，用于启动对账与运维盘查。空结果返回空切片而不是 nil。
	ListByStatus(ctx context.Context, status TaskStatus, limit int) ([]Task, error)
	// FailRunning 把库里所有 running 任务标为 failed，返回受影响行数。
	// 只在 worker 启动时调用一次：进程崩溃后 running 是脏状态，不清掉就永远没人接手。
	FailRunning(ctx context.Context, reason string) (int64, error)
}

// User 是账号实体。
//
// 密码只存 bcrypt 哈希，不存明文也不存可逆密文；手机号与身份证属于敏感字段，
// 出库时必须已脱敏（id_masked 形态），本结构体不提供原文读写。
type User struct {
	// ID 是 CHAR(26) ULID。
	ID string
	// Phone 是登录账号，存前 3 后 4 中间脱敏的形态，检索用独立哈希列（由 DAL 负责）。
	Phone string
	// Nickname 是展示名。
	Nickname string
	// PasswordHash 是 bcrypt cost 12 的哈希。
	PasswordHash string
	// CreatedAt 是注册时间。
	CreatedAt time.Time
}

// UserStore 是账号域。
//
// TODO(确认): 方案 §3.3 的 Store 只有六个域，没有 User。A10 鉴权需要它，
// 但不能擅自往已定稿的 interface 里加方法。这里先给出草案，等 B 回话后再挂进 Store。
type UserStore interface {
	// Create 插入用户，手机号重复返回 CodeConflict。
	Create(ctx context.Context, u *User) error
	// GetByPhone 按手机号查用户，登录用。未命中返回 CodeNotFound，
	// 但 API 层必须把它折成 CodeUnauthorized，不许暴露"用户不存在"。
	GetByPhone(ctx context.Context, phone string) (*User, error)
	// Get 按 ID 查用户，鉴权中间件与 token 校验用。
	Get(ctx context.Context, id string) (*User, error)
}
