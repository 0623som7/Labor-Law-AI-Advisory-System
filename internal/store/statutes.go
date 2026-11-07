package store

import (
	"context"
	"database/sql"
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

// Statutes 是 port.StatuteStore 的法条部分实现。
//
// 判例与向量的四个方法仍返回"尚未实现"：判例没有权威数据来源、向量没有选定
// embedding 模型，现在写出实现也无法验证，留明确错误比留一段没被跑过的 SQL 强。
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

// SaveEmbeddings 向量域尚未落地：embedding 模型未定，向量维度都无从确定。
func (s *Statutes) SaveEmbeddings(context.Context, []port.Embedding) error {
	return errNotImplemented("向量")
}

// LoadEmbeddings 向量域尚未落地。
func (s *Statutes) LoadEmbeddings(context.Context, string, string) ([]port.Embedding, error) {
	return nil, errNotImplemented("向量")
}
