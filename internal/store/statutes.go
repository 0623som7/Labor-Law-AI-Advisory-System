package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"laborlaw/internal/port"
)

// statuteColumns 是 statutes 表的读取列序，与 scanStatute 一一对应。
const statuteColumns = `id, law, article, title, text, url, effective_at`

// defaultStatuteTopK 与 maxStatuteTopK 限定单次检索的返回条数。
//
// 上限必须存在：检索结果会被逐条拼进系统提示词，不设上限的话一次"劳动法"
// 这种泛词查询就能把整部法律塞进上下文，token 费用和时延一起失控。
const (
	defaultStatuteTopK = 5
	maxStatuteTopK     = 50
)

// insertBatchSize 是全量重建时每批 INSERT 的行数。
//
// 分批不是性能调优，是为了让单条 SQL 的长度有界：205 条法条一次拼完
// 有上千个占位符，出问题时日志里那一条语句本身就没法读。
const insertBatchSize = 50

// Statutes 是 port.StatuteStore 的实现：法条读写、检索与向量。
//
// 判例的两个方法仍返回"尚未实现"：没有权威判例来源，写出来的 SQL 无从验证，
// 留明确错误比留一段没被跑过的查询强。
type Statutes struct {
	db querier
}

// NewStatutes 构造法条域 DAL。
func NewStatutes(db *sql.DB) *Statutes { return &Statutes{db: db} }

// newStatutesOn 构造绑定在任意连接（连接池或事务）上的法条域 DAL。
func newStatutesOn(q querier) *Statutes { return &Statutes{db: q} }

// 编译期断言：实现必须与 B 依赖的接口一致。
var _ port.StatuteStore = (*Statutes)(nil)

// Get 按业务 ID 查法条，未命中返回 40401。
func (s *Statutes) Get(ctx context.Context, statuteID string) (*port.Statute, error) {
	if strings.TrimSpace(statuteID) == "" {
		return nil, port.New(port.CodeInvalidArgument, "法条 ID 为空")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+statuteColumns+` FROM statutes WHERE id = ?`, statuteID)
	return scanStatute(row)
}

// Count 返回法条行数，导入验收与启动自检用。
func (s *Statutes) Count(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM statutes`).Scan(&n); err != nil {
		return 0, port.Wrap(port.CodeInternal, err, "统计法条数量失败")
	}
	return n, nil
}

// ReplaceAll 全量重建法条表。
//
// 空列表直接拒绝：这个方法的语义是"用新版本替换旧版本"，传空等于清库，
// 而清库只可能来自导入脚本读到了空文件——那是 bug，不该让它执行成功。
func (s *Statutes) ReplaceAll(ctx context.Context, items []port.Statute) error {
	if len(items) == 0 {
		return port.New(port.CodeInvalidArgument, "法条列表为空，拒绝清空法条表")
	}
	for i := range items {
		if items[i].ID == "" || items[i].Law == "" || items[i].Article == "" {
			return port.New(port.CodeInvalidArgument, "第 %d 条法条缺少 ID、法律名称或条号", i+1)
		}
		if strings.TrimSpace(items[i].Text) == "" {
			return port.New(port.CodeInvalidArgument, "%s 正文为空，拒绝入库", items[i].ID)
		}
	}

	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM statutes`); err != nil {
			return port.Wrap(port.CodeInternal, err, "清空法条表失败")
		}
		for start := 0; start < len(items); start += insertBatchSize {
			end := start + insertBatchSize
			if end > len(items) {
				end = len(items)
			}
			if err := insertStatutes(ctx, tx, items[start:end]); err != nil {
				return err
			}
		}
		return nil
	})
}

// insertStatutes 批量插入一批法条。
func insertStatutes(ctx context.Context, tx *sql.Tx, batch []port.Statute) error {
	var sb strings.Builder
	sb.WriteString(`INSERT INTO statutes (id, law, article, title, text, url, effective_at) VALUES `)
	args := make([]any, 0, len(batch)*7)
	for i, item := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`(?, ?, ?, ?, ?, ?, ?)`)
		var effective any
		if !item.EffectiveAt.IsZero() {
			effective = item.EffectiveAt.Format("2006-01-02")
		}
		args = append(args, item.ID, item.Law, item.Article, item.Title, item.Text, item.URL, effective)
	}
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return port.Wrap(port.CodeInternal, err, "写入法条失败（起始 ID %s）", batch[0].ID)
	}
	return nil
}

// SearchText 按关键词检索法条，按正文命中次数排序。
//
// 用 LIKE 而不是表上已有的 ngram 全文索引，是权衡之后的决定：
//   - 数据只有几百条，全表扫描是毫秒级，索引省不下可观的时间；
//   - ngram 自然语言模式会把"二倍工资"拆成 bigram 分别计分，召回大量只沾一个字的条文，
//     而咨询场景要的是"这一条就是讲这件事"；
//   - 同义词归一、多路召回与排序策略都在 retrieval 包里，DAL 保持可预测更有利。
//
// 命中次数作为 Score：一条反复讲"经济补偿"的条文，通常比只提到一次的更切题。
func (s *Statutes) SearchText(ctx context.Context, query, law string, topK int) ([]port.StatuteHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return []port.StatuteHit{}, nil
	}
	if topK <= 0 {
		topK = defaultStatuteTopK
	}
	if topK > maxStatuteTopK {
		topK = maxStatuteTopK
	}

	sqlText := `SELECT ` + statuteColumns + `,
       (LENGTH(text) - LENGTH(REPLACE(text, ?, ''))) / LENGTH(?) AS hits
       FROM statutes WHERE text LIKE ? ESCAPE '\\'`
	args := []any{query, query, "%" + escapeLike(query) + "%"}
	if law = strings.TrimSpace(law); law != "" {
		sqlText += ` AND law = ?`
		args = append(args, law)
	}
	sqlText += ` ORDER BY hits DESC, id ASC LIMIT ?`
	args = append(args, topK)

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "检索法条失败")
	}
	defer rows.Close()

	hits := make([]port.StatuteHit, 0, topK)
	for rows.Next() {
		var (
			st        port.Statute
			effective sql.NullTime
			score     float64
		)
		if err := rows.Scan(&st.ID, &st.Law, &st.Article, &st.Title, &st.Text, &st.URL, &effective, &score); err != nil {
			return nil, port.Wrap(port.CodeInternal, err, "读取法条行失败")
		}
		if effective.Valid {
			st.EffectiveAt = effective.Time
		}
		hits = append(hits, port.StatuteHit{Statute: st, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历法条结果失败")
	}
	return hits, nil
}

// escapeLike 转义 LIKE 模式里的特殊字符。
//
// 用户问"扣 50% 工资合法吗"，归一后的关键词若带 % 而不转义，
// 一次检索就会把整张表匹配出来，再把几十条无关条文塞进提示词。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// scanStatute 把一行扫进 port.Statute。
func scanStatute(row *sql.Row) (*port.Statute, error) {
	var (
		st        port.Statute
		effective sql.NullTime
	)
	if err := row.Scan(&st.ID, &st.Law, &st.Article, &st.Title, &st.Text, &st.URL, &effective); err != nil {
		return nil, notFound(err, "法条不存在")
	}
	if effective.Valid {
		st.EffectiveAt = effective.Time
	}
	return &st, nil
}

// ReplaceCases 判例域尚未落地：没有权威判例来源之前不写无法验证的 SQL。
func (s *Statutes) ReplaceCases(context.Context, []port.LegalCase) error {
	return errNotImplemented("判例")
}

// SearchCases 判例域尚未落地。
func (s *Statutes) SearchCases(context.Context, string, string, int) ([]port.CaseHit, error) {
	return nil, errNotImplemented("判例")
}

// SaveEmbeddings 批量写向量，同一 (owner_type, owner_id, model) 覆盖旧值。
//
// 先全量校验再写：半截写入是最坏的失败形态——一部分法条有向量、一部分没有，
// 混合检索时缺向量的那些只能靠词表那一路，而分数上看不出差别，只会觉得
// "召回有点飘"。
func (s *Statutes) SaveEmbeddings(ctx context.Context, items []port.Embedding) error {
	if len(items) == 0 {
		return nil
	}
	if err := validateEmbeddings(items); err != nil {
		return err
	}

	return withTx(ctx, s.db, func(tx *sql.Tx) error {
		for start := 0; start < len(items); start += insertBatchSize {
			end := start + insertBatchSize
			if end > len(items) {
				end = len(items)
			}
			if err := insertEmbeddings(ctx, tx, items[start:end]); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadEmbeddings 按宿主类型与模型名读回全部向量。
func (s *Statutes) LoadEmbeddings(ctx context.Context, ownerType, model string) ([]port.Embedding, error) {
	if strings.TrimSpace(ownerType) == "" {
		return nil, port.New(port.CodeInvalidArgument, "向量宿主类型为空")
	}
	if strings.TrimSpace(model) == "" {
		return nil, port.New(port.CodeInvalidArgument, "向量模型名为空")
	}

	// ORDER BY owner_id 不能省：不排序时返回顺序由执行计划决定，同一份数据
	// 两次加载顺序可能不同，检索结果就不可复现。
	rows, err := s.db.QueryContext(ctx,
		`SELECT owner_id, vector FROM embeddings
		 WHERE owner_type = ? AND model = ?
		 ORDER BY owner_id`, ownerType, model)
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "读取向量失败")
	}
	defer func() { _ = rows.Close() }()

	out := make([]port.Embedding, 0, 256)
	for rows.Next() {
		var (
			ownerID string
			raw     []byte
		)
		if err := rows.Scan(&ownerID, &raw); err != nil {
			return nil, port.Wrap(port.CodeInternal, err, "扫描向量行失败")
		}
		var vec []float32
		if err := json.Unmarshal(raw, &vec); err != nil {
			// 带上 owner_id：205 条里哪条坏的要能直接定位，不能靠猜。
			return nil, port.Wrap(port.CodeInternal, err, "向量不是合法 JSON：owner_id=%s", ownerID)
		}
		out = append(out, port.Embedding{
			OwnerType: ownerType,
			OwnerID:   ownerID,
			Model:     model,
			Vector:    vec,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "遍历向量失败")
	}
	return out, nil
}

// validateEmbeddings 校验整批，任一条不合格就整体拒绝。
func validateEmbeddings(items []port.Embedding) error {
	dim := 0
	for i, item := range items {
		switch {
		case item.OwnerType == "" || len(item.OwnerType) > 16:
			return port.New(port.CodeInvalidArgument,
				"第 %d 条向量宿主类型非法（空或超过 16 字节）：owner_id=%s", i+1, item.OwnerID)
		case item.OwnerID == "" || len(item.OwnerID) > 32:
			return port.New(port.CodeInvalidArgument,
				"第 %d 条向量宿主 ID 非法（空或超过 32 字节）：%q", i+1, item.OwnerID)
		case item.Model == "" || len(item.Model) > 64:
			return port.New(port.CodeInvalidArgument,
				"第 %d 条向量模型名非法（空或超过 64 字节）：owner_id=%s", i+1, item.OwnerID)
		case len(item.Vector) == 0:
			return port.New(port.CodeInvalidArgument, "第 %d 条向量为空：owner_id=%s", i+1, item.OwnerID)
		}
		if dim == 0 {
			dim = len(item.Vector)
			continue
		}
		if len(item.Vector) != dim {
			return port.New(port.CodeInvalidArgument,
				"第 %d 条向量维度 %d 与首条 %d 不一致：owner_id=%s", i+1, len(item.Vector), dim, item.OwnerID)
		}
	}
	return nil
}

// insertEmbeddings 写一批（≤ insertBatchSize 条）。
func insertEmbeddings(ctx context.Context, q querier, items []port.Embedding) error {
	var b strings.Builder
	b.WriteString(`INSERT INTO embeddings (owner_type, owner_id, model, vector) VALUES `)
	args := make([]any, 0, len(items)*4)
	for i, item := range items {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString("(?,?,?,?)")
		// json.Marshal 对 float32 输出最短可逆表示，自己拼字符串会丢精度。
		raw, err := json.Marshal(item.Vector)
		if err != nil {
			return port.Wrap(port.CodeInvalidArgument, err, "向量无法序列化：owner_id=%s", item.OwnerID)
		}
		args = append(args, item.OwnerType, item.OwnerID, item.Model, raw)
	}
	b.WriteString(` ON DUPLICATE KEY UPDATE vector = VALUES(vector)`)

	if _, err := q.ExecContext(ctx, b.String(), args...); err != nil {
		return port.Wrap(port.CodeInternal, err, "写入向量失败")
	}
	return nil
}
