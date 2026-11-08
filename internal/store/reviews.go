package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// reviewColumns 是 reviews 表的读取列序，与 scanReview 一一对应。
const reviewColumns = `id, file_id, user_id, status, risk_level, score,
	clause_pass_rate, counts_high, counts_medium, counts_low, task_id, created_at, updated_at`

// findingColumns 是 findings 表的读取列序，与 scanFinding 一一对应。
const findingColumns = `id, review_id, rule_id, severity, title, section, page_no,
	bbox_x0, bbox_y0, bbox_x1, bbox_y1, evidence, legal_basis, suggestion, economic_impact`

// 列表分页的默认值与上限。
//
// 上限必须存在：审查历史页是前端一次拉完再本地筛选的，不设上限时
// 一个重度用户能把几千条审查连同 findings 的关联查询一起拖慢。
const (
	defaultReviewPageSize = 20
	maxReviewPageSize     = 100
)

// Reviews 是 port.ReviewStore 的 MySQL 实现。
type Reviews struct{ db querier }

// NewReviews 构造审查域 DAL。
func NewReviews(db *sql.DB) *Reviews { return &Reviews{db: db} }

// newReviewsOn 构造绑定在任意连接（连接池或事务）上的审查域 DAL。
func newReviewsOn(q querier) *Reviews { return &Reviews{db: q} }

var _ port.ReviewStore = (*Reviews)(nil)

// Create 插入审查主记录。
//
// 状态由调用方给定；缺省按 pending 处理。这里不校验"必须从 pending 开始"，
// 因为 Create 与 Update 是两条路径，把状态机放在 Create 里会让"补录历史审查"
// 这种运维操作变得不可能。
func (r *Reviews) Create(ctx context.Context, rv *port.Review) error {
	if rv == nil || rv.ID == "" || rv.FileID == "" || rv.UserID == "" {
		return port.New(port.CodeInvalidArgument, "审查记录缺少 id、file_id 或 user_id")
	}
	status := rv.Status
	if status == "" {
		status = port.TaskPending
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO reviews
		(id, file_id, user_id, status, risk_level, score, clause_pass_rate,
		 counts_high, counts_medium, counts_low, task_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rv.ID, rv.FileID, rv.UserID, string(status), string(rv.RiskLevel), rv.Score,
		rv.ClausePassRate, rv.Counts.High, rv.Counts.Medium, rv.Counts.Low, rv.TaskID,
	)
	if err != nil {
		// 1062 在这里有两种来源：主键重复（几乎不会发生，ULID）与
		// uk_reviews_active_file（同一份合同已有一条 pending/running）。
		// 后者是并发审查的唯一拦截点——api 层的 GetByFile 与 Create 不在同一事务里，
		// 只能靠唯一键兜底。文案要让前端能直接用。
		if isDuplicate(err) {
			return port.Wrap(port.CodeConflict, err, "该合同正在审查中，请等待结果")
		}
		return port.Wrap(port.CodeInternal, err, "写入审查记录失败")
	}
	return nil
}

// Get 按 ID 查审查，同时按 userID 过滤。
func (r *Reviews) Get(ctx context.Context, userID, reviewID string) (*port.Review, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+reviewColumns+` FROM reviews WHERE id = ? AND user_id = ?`, reviewID, userID)
	return scanReview(row)
}

// GetByFile 返回该文件最近一次审查，供"同一合同不重复审"复用。
func (r *Reviews) GetByFile(ctx context.Context, userID, fileID string) (*port.Review, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+reviewColumns+` FROM reviews
		 WHERE user_id = ? AND file_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT 1`, userID, fileID)
	return scanReview(row)
}

// Update 覆盖审查的可变字段。
//
// 不带 user_id 条件：Update 的调用方是 worker，它手里只有 review_id（来自任务载荷）。
// 归属校验必须在 api 层先做一次 Review().Get——这是刻意的分工，
// 如果这里再按 user_id 过滤，worker 的每次回写都会变成"没有记录被更新"。
func (r *Reviews) Update(ctx context.Context, rv *port.Review) error {
	if rv == nil || rv.ID == "" {
		return port.New(port.CodeInvalidArgument, "审查记录缺少 id")
	}
	res, err := r.db.ExecContext(ctx, `UPDATE reviews SET
			status = ?, risk_level = ?, score = ?, clause_pass_rate = ?,
			counts_high = ?, counts_medium = ?, counts_low = ?, updated_at = CURRENT_TIMESTAMP(3)
		WHERE id = ?`,
		string(rv.Status), string(rv.RiskLevel), rv.Score, rv.ClausePassRate,
		rv.Counts.High, rv.Counts.Medium, rv.Counts.Low, rv.ID,
	)
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "更新审查记录失败")
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return port.Wrap(port.CodeInternal, err, "读取更新结果失败")
	}
	if affected == 0 {
		// MySQL 对"新值与旧值完全相同"的 UPDATE 返回 0 行受影响，而 updated_at 用的是
		// CURRENT_TIMESTAMP(3)——同一毫秒内重写同一份结果就会命中这条路径。
		// 不加这层存在性检查，一次空操作的进度回写会被误报成 40401。
		var exists int
		if err := r.db.QueryRowContext(ctx, `SELECT 1 FROM reviews WHERE id = ?`, rv.ID).Scan(&exists); err != nil {
			return notFound(err, "审查记录不存在")
		}
	}
	return nil
}

// SaveFindings 覆盖写入风险清单：同一 reviewID 先删后插。
//
// 先删后插而不是逐条 upsert：findings 没有稳定的业务主键（每次审查生成的 ID 都不同），
// 重跑一次审查就应当得到"这一份清单"，而不是新旧两份的并集。
// 删除与插入必须在同一事务里，否则中途失败会留下一份空清单。
func (r *Reviews) SaveFindings(ctx context.Context, reviewID string, findings []port.Finding) error {
	if strings.TrimSpace(reviewID) == "" {
		return port.New(port.CodeInvalidArgument, "审查 ID 为空")
	}

	return withTx(ctx, r.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM findings WHERE review_id = ?`, reviewID); err != nil {
			return port.Wrap(port.CodeInternal, err, "清空旧的风险清单失败")
		}
		if len(findings) == 0 {
			return nil
		}
		for start := 0; start < len(findings); start += insertBatchSize {
			end := start + insertBatchSize
			if end > len(findings) {
				end = len(findings)
			}
			if err := insertFindings(ctx, tx, reviewID, findings[start:end]); err != nil {
				return err
			}
		}
		return nil
	})
}

// insertFindings 批量插入一批发现。
//
// legal_basis 与 suggestion 用 JSON 列存：这两个结构的字段会随规则演进
// （比如以后给 suggestion 加"参考条款"），拆成列就得每次加迁移；
// 而它们从不作为查询条件，只随清单整条取出。
func insertFindings(ctx context.Context, tx *sql.Tx, reviewID string, batch []port.Finding) error {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO findings
		(id, review_id, rule_id, severity, title, section, page_no,
		 bbox_x0, bbox_y0, bbox_x1, bbox_y1, evidence, legal_basis, suggestion, economic_impact)
		VALUES `)
	args := make([]any, 0, len(batch)*15)
	for i, f := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)

		legal := f.LegalBasis
		if legal == nil {
			legal = []port.LegalBasis{}
		}
		legalJSON, err := json.Marshal(legal)
		if err != nil {
			return port.Wrap(port.CodeInternal, err, "序列化法条依据失败")
		}
		suggestionJSON, err := json.Marshal(f.Suggestion)
		if err != nil {
			return port.Wrap(port.CodeInternal, err, "序列化修改建议失败")
		}

		findingID := f.ID
		if findingID == "" {
			findingID = id.New()
		}
		var impact any
		if f.EconomicImpact != nil {
			impact = *f.EconomicImpact
		}
		args = append(args, findingID, reviewID, f.RuleID, string(f.Severity), f.Title,
			f.ClauseRef.Section, f.ClauseRef.Page,
			f.ClauseRef.BBox[0], f.ClauseRef.BBox[1], f.ClauseRef.BBox[2], f.ClauseRef.BBox[3],
			f.Evidence, string(legalJSON), string(suggestionJSON), impact)
	}
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return port.Wrap(port.CodeInternal, err, "写入风险清单失败")
	}
	return nil
}

// ListFindings 按严重度倒序（high → medium → low）返回风险清单。
//
// 用 FIELD() 而不是按字符串排序：'high' < 'low' < 'medium' 的字典序
// 会把中危排到最后，前端"按严重度"的默认视图直接错位。
func (r *Reviews) ListFindings(ctx context.Context, reviewID string) ([]port.Finding, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+findingColumns+` FROM findings WHERE review_id = ?
		 ORDER BY FIELD(severity, 'high', 'medium', 'low'), id ASC`, reviewID)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "查询风险清单失败")
	}
	defer rows.Close()

	out := make([]port.Finding, 0, 8)
	for rows.Next() {
		f, err := scanFinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *f)
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历风险清单失败")
	}
	return out, nil
}

// ListByUser 按创建时间倒序返回用户的审查历史。
func (r *Reviews) ListByUser(ctx context.Context, userID string, limit, offset int) ([]port.Review, error) {
	if limit <= 0 {
		limit = defaultReviewPageSize
	}
	if limit > maxReviewPageSize {
		limit = maxReviewPageSize
	}
	if offset < 0 {
		offset = 0
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT `+reviewColumns+` FROM reviews WHERE user_id = ?
		 ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, userID, limit, offset)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "查询审查历史失败")
	}
	defer rows.Close()

	out := make([]port.Review, 0, limit)
	for rows.Next() {
		rv, err := scanReviewRows(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *rv)
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历审查历史失败")
	}
	return out, nil
}

// scanReview 扫一行审查记录（QueryRow 版本）。
func scanReview(row *sql.Row) (*port.Review, error) {
	return scanReviewFrom(row, "审查不存在")
}

// scanReviewRows 扫一行审查记录（Query 版本）。
func scanReviewRows(rows *sql.Rows) (*port.Review, error) {
	return scanReviewFrom(rows, "读取审查记录失败")
}

// scanner 是 *sql.Row 与 *sql.Rows 的公共子集。
type scanner interface {
	Scan(dest ...any) error
}

// scanReviewFrom 把一行扫进 port.Review。
func scanReviewFrom(s scanner, missing string) (*port.Review, error) {
	var (
		rv     port.Review
		status string
		risk   string
	)
	if err := s.Scan(&rv.ID, &rv.FileID, &rv.UserID, &status, &risk, &rv.Score,
		&rv.ClausePassRate, &rv.Counts.High, &rv.Counts.Medium, &rv.Counts.Low,
		&rv.TaskID, &rv.CreatedAt, &rv.UpdatedAt); err != nil {
		return nil, notFound(err, "%s", missing)
	}
	rv.Status = port.TaskStatus(status)
	rv.RiskLevel = port.Severity(risk)
	return &rv, nil
}

// scanFinding 把一行扫进 port.Finding。
func scanFinding(rows *sql.Rows) (*port.Finding, error) {
	var (
		f          port.Finding
		severity   string
		legal      []byte
		suggestion []byte
		impact     sql.NullFloat64
	)
	if err := rows.Scan(&f.ID, &f.ReviewID, &f.RuleID, &severity, &f.Title,
		&f.ClauseRef.Section, &f.ClauseRef.Page,
		&f.ClauseRef.BBox[0], &f.ClauseRef.BBox[1], &f.ClauseRef.BBox[2], &f.ClauseRef.BBox[3],
		&f.Evidence, &legal, &suggestion, &impact); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "读取风险条目失败")
	}
	f.Severity = port.Severity(severity)
	if len(legal) > 0 {
		if err := json.Unmarshal(legal, &f.LegalBasis); err != nil {
			return nil, port.Wrap(port.CodeInternal, err, "解析法条依据失败")
		}
	}
	if f.LegalBasis == nil {
		f.LegalBasis = []port.LegalBasis{}
	}
	if len(suggestion) > 0 {
		if err := json.Unmarshal(suggestion, &f.Suggestion); err != nil {
			return nil, port.Wrap(port.CodeInternal, err, "解析修改建议失败")
		}
	}
	if impact.Valid {
		v := impact.Float64
		f.EconomicImpact = &v
	}
	return &f, nil
}
