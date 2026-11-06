package port

import "time"

// 领域实体集中放在本文件。
//
// 为什么不让各 interface 文件各自定义：B 加字段时要评审的表结构只有一个来源，
// 而且 store 子接口、api DTO、DAL 扫描目标都指向同一份结构体，
// 分散定义必然出现"两处字段名不一致、tag 不同"这类编译不过又看不出原因的故障。

// Session 是一次问答会话。
type Session struct {
	// ID 是 CHAR(26) ULID，带时间前缀，便于按时间排序而不建额外索引。
	ID string
	// UserID 是归属人，资源级鉴权只认这个字段。
	UserID string
	// Title 是会话标题，首条消息截断生成，用户可改。
	Title string
	// Scene 是会话场景，同一会话内固定，避免历史和当前场景错配。
	Scene string
	// CreatedAt 是创建时间，由 DB 默认值填充。
	CreatedAt time.Time
	// UpdatedAt 是最后活跃时间，列表按它倒序。
	UpdatedAt time.Time
}

// ChatMessage 是一条历史消息。与 llm.Message 的区别：这里带计量与 trace_id，落库用。
type ChatMessage struct {
	// ID 是 CHAR(26) ULID。
	ID string
	// SessionID 是所属会话。
	SessionID string
	// Role 是说话人，取值与 llm.Role 一致。
	Role Role
	// Content 是正文。这里会存合同审查结论，属于敏感数据，出库时不得写进日志。
	Content string
	// PromptTokens 是本条关联调用的输入 token。
	PromptTokens int
	// CompletionTokens 是本条关联调用的输出 token。
	CompletionTokens int
	// TraceID 关联请求链路，便于拿用户投诉的时间点反查。
	TraceID string
	// CreatedAt 是写入时间。
	CreatedAt time.Time
}

// FileRecord 是上传文件元数据。正文不进这张表，也不进任何 TEXT 字段。
type FileRecord struct {
	// ID 是 CHAR(26) ULID，同时是 storage 下的目录名。
	ID string
	// UserID 是归属人。跨用户访问返回 CodeForbidden 而不是 NotFound。
	UserID string
	// Filename 是用户上传时的原文件名，只用于展示。拼接磁盘路径时禁止使用它，
	// 否则 "../" 型文件名会造成路径穿越。
	Filename string
	// Ext 是小写扩展名，不含点。
	Ext string
	// SizeBytes 是文件字节数。
	SizeBytes int64
	// SHA256 是内容哈希，用于同一用户重复上传时命中缓存、避免重复解析。
	SHA256 string
	// PageCount 是页数，解析完成后回填。
	PageCount int
	// StoragePath 是相对于 config.storage.path 的相对路径。
	StoragePath string
	// OCRUsed 表示走了 OCR 兜底。
	OCRUsed bool
	// Confidence 是解析置信度。
	Confidence float64
	// CreatedAt 是上传时间。
	CreatedAt time.Time
	// UpdatedAt 是状态变更时间。
	UpdatedAt time.Time
}

// Severity 是风险等级。
type Severity string

// 风险等级枚举。用三档而不是百分比，是因为规则引擎只能给出定性判断，
// 假精度会让用户在答辩时被追问"42 分怎么算的"。
const (
	// SeverityHigh 高风险：违法条款，可能导致条款无效或赔偿。
	SeverityHigh Severity = "high"
	// SeverityMedium 中风险：合法但不利，建议谈判修改。
	SeverityMedium Severity = "medium"
	// SeverityLow 低风险：表述瑕疵，可改可不改。
	SeverityLow Severity = "low"
)

// Review 是一次合同审查的汇总结果。
type Review struct {
	// ID 是 CHAR(26) ULID。
	ID string
	// FileID 是被审查文件。
	FileID string
	// UserID 是归属人。
	UserID string
	// Status 是审查进度状态，取值与 TaskStatus 一致。
	Status TaskStatus
	// RiskLevel 是整体风险等级，取 findings 里最高的那一档。
	RiskLevel Severity
	// Score 是 0-100 的合规分，由 B 的规则引擎给出，A 只存不算。
	Score int
	// ClausePassRate 是条款通过率，0-1。
	ClausePassRate float64
	// Counts 是各等级的问题条数。
	Counts ReviewCounts
	// TaskID 关联异步任务，同步审查时为空串。
	TaskID string
	// CreatedAt 是创建时间。
	CreatedAt time.Time
	// UpdatedAt 是最近一次状态变更时间。
	UpdatedAt time.Time
}

// ReviewCounts 是问题条数统计。前端图表直接消费，不让它自己聚合。
type ReviewCounts struct {
	// High 高风险条数。
	High int
	// Medium 中风险条数。
	Medium int
	// Low 低风险条数。
	Low int
}

// ClauseRef 指向合同里的具体位置，是标注层的数据源。
type ClauseRef struct {
	// Section 是条款编号，如 "第二条"。
	Section string
	// Page 是页号，0 起。
	Page int
	// BBox 是坐标，协议同 TextBlock.BBox：pt、原点左下、y 轴向上。
	BBox [4]float64
}

// LegalBasis 是一条法律依据。正文必须来自库，不许运行时生成。
type LegalBasis struct {
	// Law 是法律名称。
	Law string
	// Article 是条号。
	Article string
	// Text 是条文原文。
	Text string
}

// Suggestion 是一条修改建议。
type Suggestion struct {
	// Type 是建议类型："rewrite"（改写）/ "add"（补充）/ "delete"（删除）。
	Type string
	// Original 是原文片段。
	Original string
	// Revised 是建议改成的内容。
	Revised string
	// Reason 是理由，面向用户，必须是人话。
	Reason string
}

// Finding 是一条审查发现。一条 finding 对应一条命中规则。
type Finding struct {
	// ID 是 finding 标识，形如 "F-001"，在单次审查内唯一。
	ID string
	// ReviewID 是所属审查。
	ReviewID string
	// RuleID 是命中的规则 ID，如 "R-PROBATION-MATCH"。
	RuleID string
	// Severity 是风险等级。
	Severity Severity
	// Title 是一句话结论。
	Title string
	// ClauseRef 指向合同位置。
	ClauseRef ClauseRef
	// Evidence 是合同里的原文证据。
	Evidence string
	// LegalBasis 是法律依据，可能多条。
	LegalBasis []LegalBasis
	// Suggestion 是修改建议，无建议时为零值。
	Suggestion Suggestion
	// EconomicImpact 是经济影响估算，P2 功能未启用时为 nil。
	EconomicImpact *float64
}

// Statute 是一条法律法规条文。
//
// 这张表是只读数据：只允许全量重建（DEL 前缀后重灌），不允许单条 UPDATE。
// 原因是缓存里躺着 24h TTL 的副本，允许单条改却不失效缓存，就会出现"库改了页面上还是旧条文"。
type Statute struct {
	// ID 是业务 ID，如 "LCL-38"，跨库稳定，不用自增。
	ID string
	// Law 是法律名称。
	Law string
	// Article 是条号。
	Article string
	// Title 是条文标题（部分来源没有，可空）。
	Title string
	// Text 是条文原文。
	Text string
	// URL 是官方来源链接。
	URL string
	// EffectiveAt 是生效日期，用于判断新旧法适用。
	EffectiveAt time.Time
}

// StatuteHit 是一条检索命中。
type StatuteHit struct {
	// Statute 是命中的条文。
	Statute Statute
	// Score 是相关度，越大越相关，具体量纲由检索实现决定，B 只做排序。
	Score float64
}

// LegalCase 是一条判例。
type LegalCase struct {
	// ID 是业务 ID，如 "CASE-GD-0001"。
	ID string
	// Title 是案号或标题。
	Title string
	// Region 是省级行政区，用于地方性差异。
	Region string
	// Summary 是裁判要点。
	Summary string
	// Outcome 是裁判结果。
	Outcome string
	// WinRate 是同类案件劳动者胜诉率，0-1，无统计时为 nil。
	WinRate *float64
	// DecidedAt 是裁判日期。
	DecidedAt time.Time
}

// CaseHit 是一条判例检索命中。
type CaseHit struct {
	// Case 是命中的判例。
	Case LegalCase
	// Score 是相关度。
	Score float64
}

// Embedding 是一条向量。
//
// 向量存在 MySQL 的 JSON/BLOB 列里，先不引向量库：数据量在千级，
// 引 Milvus 会多一个必须跟着起的容器，答辩现场挂一个就全挂。
type Embedding struct {
	// OwnerType 是宿主类型："statute" / "case" / "block"。
	OwnerType string
	// OwnerID 是宿主 ID。
	OwnerID string
	// Model 是产生该向量的模型名，换模型必须整体重建，不能混用。
	Model string
	// Vector 是向量本体，float32 以省内存。
	Vector []float32
}

// TaskStatus 是任务状态机的状态值。
type TaskStatus string

// 任务状态枚举。合法转移见 TaskStatus.CanTransition。
const (
	// TaskPending 已入库，尚未被 worker 领取。
	TaskPending TaskStatus = "pending"
	// TaskRunning 已被 worker 领取并在执行。
	TaskRunning TaskStatus = "running"
	// TaskSucceeded 执行成功。
	TaskSucceeded TaskStatus = "succeeded"
	// TaskFailed 执行失败（含超时与进程崩溃后的对账结果）。
	TaskFailed TaskStatus = "failed"
	// TaskCancelled 被取消。
	TaskCancelled TaskStatus = "cancelled"
)

// Terminal 判断是否终态。终态任务不再被领取，也不再接受进度回写。
func (s TaskStatus) Terminal() bool {
	return s == TaskSucceeded || s == TaskFailed || s == TaskCancelled
}

// CanTransition 判断状态机是否允许 s → to。
//
// 非法转移返回 CodeConflict（40901），而不是静默改或忽略：
// 静默改会让"任务卡在 running"这种故障永远查不到根因。
// 允许 pending → failed 是为了覆盖"入库成功但派发失败"的场景（队列满、池已关闭）。
func (s TaskStatus) CanTransition(to TaskStatus) bool {
	switch s {
	case TaskPending:
		return to == TaskRunning || to == TaskFailed || to == TaskCancelled
	case TaskRunning:
		return to == TaskSucceeded || to == TaskFailed || to == TaskCancelled
	default:
		// 终态不能再去任何地方，重复完成是并发缺陷的症状，必须报出来。
		return false
	}
}

// Task 是一条异步任务。worker 池与 api 层的轮询接口共用。
type Task struct {
	// ID 是 CHAR(26) ULID，返回给前端做轮询。
	ID string
	// Type 是任务类型，同时也是 Handler 的注册名，如 "review.run"。
	Type string
	// Status 是当前状态。
	Status TaskStatus
	// Payload 是任务入参 JSON 原文，由 B 的 Handler 自行解码。
	Payload []byte
	// Progress 是 0-100 的进度。写库有 ≥500ms 节流，读到的值可能落后半秒。
	Progress int
	// Stage 是当前阶段的人话描述，直接展示给用户。
	Stage string
	// Error 是失败原因，只放 AppError.Message，不放 cause，防敏感信息泄漏。
	Error string
	// Attempt 是已执行次数，从 1 开始。
	Attempt int
	// CreatedAt 是入队时间。
	CreatedAt time.Time
	// UpdatedAt 是最近一次进度或状态变更时间。
	UpdatedAt time.Time
}

// UsageRecord 是一次模型调用的计量记录，用于成本核算与压测证据。
//
// 只写不读的业务路径不需要它，但 A3 的日成本闸和答辩时的"token 消耗按 scene 分组"
// 都必须靠这张表，落日志会导致数据随容器重建丢失。
type UsageRecord struct {
	// TraceID 关联请求链路。
	TraceID string
	// Scene 是调用场景。
	Scene string
	// Model 是实际路由到的模型名。
	Model string
	// PromptTokens 是输入 token。
	PromptTokens int
	// CompletionTokens 是输出 token。
	CompletionTokens int
	// LatencyMS 是上游耗时。
	LatencyMS int
	// CostEst 是估算成本，单位元。
	CostEst float64
	// CreatedAt 是记录时间。
	CreatedAt time.Time
}
