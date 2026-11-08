package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"laborlaw/internal/extract"
	"laborlaw/internal/port"
	"laborlaw/internal/rules"
)

// 审查阶段文案与进度节点。
//
// 进度值不是拍脑袋：抽取是最慢的一步（一次模型调用，几秒到几十秒），
// 把它压在 30% 之前，用户看到进度条卡住时至少知道卡在抽取，
// 而不是以为后端挂了。落库只是两条 SQL，给到 100 前留给它 10%。
const (
	stageReadFile  = "正在读取合同"
	stageExtract   = "正在抽取合同要素"
	stageExtracted = "要素抽取完成"
	stageRules     = "正在核对风险规则"
	stageStatutes  = "正在读取法条依据"
	stageSave      = "正在保存审查结果"
	stageDone      = "审查完成"
)

// ReviewPayload 是审查任务的载荷。
//
// 比任务书多一个 user_id：port.FileStore.Get 与 port.ReviewStore.Get 都按
// (userID, id) 查，worker 里没有 HTTP 上下文，拿不到当前用户。
// 这个字段由 A 侧创建任务时写入，B 侧只读，不放任何可展示的个人信息。
type ReviewPayload struct {
	ReviewID string `json:"review_id"`
	FileID   string `json:"file_id"`
	UserID   string `json:"user_id"`
	// RuleScope 为空表示跑全部规则；前端勾选部分规则时按 ID 过滤。
	RuleScope []string `json:"rule_scope"`
}

// ReviewDeps 是审查编排的依赖集合。
//
// 全部用接口而不是 *store.DB：这条链路必须能用内存 fake 跑完整流程，
// 否则每次改一条规则都要先往 MySQL 插一个真实 PDF 才能验证。
type ReviewDeps struct {
	Files     port.FileStore
	Reviews   port.ReviewStore
	Statutes  port.StatuteStore
	Extractor *extract.Extractor
	Logger    *slog.Logger
	// Rules 为空时使用 rules.All()，测试可注入子集。
	Rules []rules.Rule
}

// NewReviewHandler 构造审查任务处理器。
//
// 返回 port.Handler 而不是 struct 方法：worker 只认识这个函数签名，
// 将来换执行器（独立进程、外部队列）不用动编排代码。
func NewReviewHandler(deps ReviewDeps) port.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Rules == nil {
		deps.Rules = rules.All()
	}
	return func(ctx context.Context, payload json.RawMessage, progress func(int, string)) error {
		return runReview(ctx, deps, payload, progress)
	}
}

// runReview 是审查的完整流程：读文件 → 抽取 → 规则 → 取法条 → 落库。
func runReview(ctx context.Context, deps ReviewDeps, payload json.RawMessage, progress func(int, string)) error {
	var p ReviewPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		return port.Wrap(port.CodeInvalidArgument, err, "审查任务载荷不是合法的 JSON")
	}
	if strings.TrimSpace(p.ReviewID) == "" || strings.TrimSpace(p.FileID) == "" || strings.TrimSpace(p.UserID) == "" {
		return port.New(port.CodeInvalidArgument, "审查任务载荷缺少 review_id、file_id 或 user_id")
	}
	if deps.Files == nil || deps.Reviews == nil || deps.Extractor == nil {
		return port.New(port.CodeInternal, "审查链路依赖未装配")
	}

	notify(progress, 5, stageReadFile)
	file, err := deps.Files.Get(ctx, p.UserID, p.FileID)
	if err != nil {
		if port.IsCode(err, port.CodeNotFound) {
			return port.New(port.CodeNotFound, "合同文件不存在或不属于当前用户")
		}
		return err
	}

	pages, err := deps.Files.LoadPages(ctx, p.FileID)
	if err != nil {
		if port.IsCode(err, port.CodeNotFound) {
			return port.New(port.CodeNoTextLayer, "合同尚未解析完成，无法审查")
		}
		return err
	}

	text, blocks := flattenPages(pages)
	if strings.TrimSpace(text) == "" {
		return port.New(port.CodeNoTextLayer, "合同没有可审查的文本内容")
	}

	// 先把 review 标成 running：否则前端轮询到的是 pending，
	// 用户会以为任务没被 worker 领走，反复点"重新审查"。
	if err := deps.Reviews.Update(ctx, &port.Review{
		ID:        p.ReviewID,
		FileID:    file.ID,
		Status:    port.TaskRunning,
		UpdatedAt: time.Now().UTC(),
	}); err != nil {
		return err
	}

	// 从这里往后任何一条错误路径都必须把 review 落成终态。
	// 否则 GET /tasks 显示 failed、GET /reviews 永远显示 running，两个页面
	// 自相矛盾；worker 超时会 cancel ctx，连"下一次重试再写"的机会都没有。
	committed := false
	defer func() {
		if committed {
			return
		}
		// 写库不能复用原 ctx：超时/取消的 ctx 一写就报 context deadline exceeded，
		// 那正是需要落终态的那一刻。
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if uerr := deps.Reviews.Update(cctx, &port.Review{
			ID:        p.ReviewID,
			Status:    port.TaskFailed,
			UpdatedAt: time.Now().UTC(),
		}); uerr != nil && deps.Logger != nil {
			deps.Logger.Warn("review mark failed", "review_id", p.ReviewID, "err", uerr)
		}
	}()

	notify(progress, 10, stageExtract)
	doc, err := deps.Extractor.Extract(ctx, text)
	if err != nil {
		return err
	}
	doc.Clauses = locateClauses(doc.Clauses, blocks)
	notify(progress, 30, stageExtracted)

	notify(progress, 50, stageRules)
	hits := runRules(doc.Contract, deps.Rules, p.RuleScope, deps.Logger)
	notify(progress, 70, stageStatutes)

	lookup := newStatuteLookup(deps.Statutes, deps.Logger)
	findings := buildFindings(ctx, p.ReviewID, hits, lookup)
	stats := summarize(hits, len(doc.Clauses))

	notify(progress, 90, stageSave)
	if err := deps.Reviews.SaveFindings(ctx, p.ReviewID, findings); err != nil {
		return err
	}
	if err := deps.Reviews.Update(ctx, &port.Review{
		ID:             p.ReviewID,
		FileID:         file.ID,
		Status:         port.TaskSucceeded,
		RiskLevel:      stats.riskLevel,
		Score:          stats.score,
		ClausePassRate: stats.passRate,
		Counts:         stats.counts,
		UpdatedAt:      time.Now().UTC(),
	}); err != nil {
		return err
	}
	// 成功终态已经落库，defer 里的兜底必须让路。
	committed = true

	notify(progress, 100, stageDone)
	return nil
}

// hit 把一条命中与产生它的规则绑在一起。
//
// rules.Fragment 刻意不带 Severity/Title：规则片段只描述"哪里违反了哪条规则"。
// 但落到 findings 里必须带上，所以在编排层配对，而不是反过来污染规则包。
type hit struct {
	Rule     rules.Rule
	Fragment rules.Fragment
}

// reviewStats 是一次审查的汇总指标。
type reviewStats struct {
	counts    port.ReviewCounts
	riskLevel port.Severity
	score     int
	passRate  float64
}

// summarize 计算计数、风险等级、评分与条款通过率。
//
// 评分口径：满分 100，每条高危 -25、中危 -10、低危 -5，下限 0。
// 这不是法律意义上的"合规分数"，只是让列表页能在 0-100 之间排序；
// 真正的结论是 findings 里的每一条，分数只做导航用，报告里要讲清楚。
func summarize(hits []hit, totalClauses int) reviewStats {
	counts := port.ReviewCounts{}
	flagged := make(map[string]bool, len(hits))
	penalty := 0
	for _, h := range hits {
		switch h.Rule.Severity {
		case port.SeverityHigh:
			counts.High++
			penalty += 25
		case port.SeverityMedium:
			counts.Medium++
			penalty += 10
		default:
			counts.Low++
			penalty += 5
		}
		if h.Fragment.Section != "" {
			flagged[h.Fragment.Section] = true
		}
	}

	score := 100 - penalty
	if score < 0 {
		score = 0
	}

	risk := port.SeverityLow
	switch {
	case counts.High > 0:
		risk = port.SeverityHigh
	case counts.Medium > 0:
		risk = port.SeverityMedium
	}

	// 通过率按"被点名的条款 / 全部条款"算。分母为 0 时（模型没抽出条款）
	// 给 1.0：没有条款可查不等于全部不合格，0 会让前端显示满屏红线。
	passRate := 1.0
	if totalClauses > 0 {
		passRate = float64(totalClauses-len(flagged)) / float64(totalClauses)
		if passRate < 0 {
			passRate = 0
		}
	}
	return reviewStats{counts: counts, riskLevel: risk, score: score, passRate: passRate}
}

// runRules 按顺序跑规则，并把 panic 收敛成 50002 日志。
//
// 单条规则出问题时只让那一条降级：一条试用期规则写错，不该让整份合同的
// 审查结果变成"失败"——用户至少该看到其它七条规则的结论。
func runRules(c rules.Contract, registry []rules.Rule, scope []string, log *slog.Logger) []hit {
	selected := registry
	if len(scope) > 0 {
		want := make(map[string]bool, len(scope))
		for _, id := range scope {
			want[id] = true
		}
		selected = make([]rules.Rule, 0, len(registry))
		for _, r := range registry {
			if want[r.ID] {
				selected = append(selected, r)
			}
		}
	}

	out := make([]hit, 0, 8)
	for _, r := range selected {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("rule panicked",
						"rule_id", r.ID,
						"code", port.CodeRuleEngine.String(),
						"panic", fmt.Sprint(rec))
				}
			}()
			for _, frag := range r.Check(c) {
				out = append(out, hit{Rule: r, Fragment: frag})
			}
		}()
	}
	return out
}

// buildFindings 把命中补成可落库的 Finding，并填入法条原文。
func buildFindings(ctx context.Context, reviewID string, hits []hit, lookup *statuteLookup) []port.Finding {
	out := make([]port.Finding, 0, len(hits))
	for _, h := range hits {
		out = append(out, port.Finding{
			ReviewID:   reviewID,
			RuleID:     h.Rule.ID,
			Severity:   h.Rule.Severity,
			Title:      h.Rule.Title,
			ClauseRef:  h.Fragment.ClauseRef(),
			Evidence:   h.Fragment.Evidence,
			LegalBasis: lookup.resolve(ctx, h.Rule.Articles),
			Suggestion: h.Fragment.Suggestion,
		})
	}
	return out
}

// statuteLookup 按 (法律, 条号) 取条文原文，并在单次审查内缓存。
//
// 为什么不用 SearchText：检索走 text 列 LIKE，而种子数据里正文不含
// "第十九条"这类条号前缀，按条号检索会稳定命中 0 条。
// 因此走 Get + 种子 ID 规则（LCL-/LL- 前缀），取不到就不给 legal_basis——
// 宁可少一条依据，也不许手写条文。
type statuteLookup struct {
	store port.StatuteStore
	log   *slog.Logger
	cache map[rules.ArticleRef][]port.LegalBasis
}

// newStatuteLookup 构造缓存表；statutes 未装配时返回一个永远查不到的空表。
func newStatuteLookup(store port.StatuteStore, log *slog.Logger) *statuteLookup {
	if log == nil {
		log = slog.Default()
	}
	return &statuteLookup{store: store, log: log, cache: make(map[rules.ArticleRef][]port.LegalBasis)}
}

// resolve 返回该组法条引用对应的原文；一条都查不到时返回 nil。
func (l *statuteLookup) resolve(ctx context.Context, refs []rules.ArticleRef) []port.LegalBasis {
	if l == nil || l.store == nil || len(refs) == 0 {
		return nil
	}
	var out []port.LegalBasis
	for _, ref := range refs {
		cached, ok := l.cache[ref]
		if !ok {
			cached = l.fetch(ctx, ref)
			l.cache[ref] = cached
		}
		out = append(out, cached...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fetch 按种子 ID 规则取一条法条。
func (l *statuteLookup) fetch(ctx context.Context, ref rules.ArticleRef) []port.LegalBasis {
	id, ok := statuteID(ref.Law, ref.Article)
	if !ok {
		l.log.Debug("statute not in seed library", "law", ref.Law, "article", ref.Article)
		return nil
	}
	st, err := l.store.Get(ctx, id)
	if err != nil {
		l.log.Warn("statute lookup degraded", "statute_id", id, "err", err.Error())
		return nil
	}
	return []port.LegalBasis{{Law: st.Law, Article: st.Article, Text: st.Text}}
}

// statuteID 把 (法律名, 中文条号) 映射成种子数据的主键。
//
// 前缀来自 cmd/statuteseed 的导入规则：劳动合同法 LCL-、劳动法 LL-。
// 未入库的法律（如社会保险法）返回 ok=false，调用方据此留空 legal_basis。
func statuteID(law, article string) (string, bool) {
	var prefix string
	switch law {
	case rules.LawLaborContract:
		prefix = "LCL-"
	case rules.LawLabor:
		prefix = "LL-"
	default:
		return "", false
	}
	n, ok := chineseNumber(article)
	if !ok {
		return "", false
	}
	return prefix + strconv.Itoa(n), true
}

// chineseNumber 解析"第十九条""第七十二条"里的整数。
func chineseNumber(article string) (int, bool) {
	article = strings.TrimPrefix(strings.TrimSpace(article), "第")
	digits := map[rune]int{'一': 1, '二': 2, '三': 3, '四': 4, '五': 5, '六': 6, '七': 7, '八': 8, '九': 9}
	total, section := 0, 0
	seen := false
	for _, r := range article {
		switch {
		case r == '十':
			if section == 0 {
				section = 1
			}
			total += section * 10
			section = 0
			seen = true
		case r == '百':
			if section == 0 {
				section = 1
			}
			total += section * 100
			section = 0
			seen = true
		case r == '条':
			total += section
			if !seen || total == 0 {
				return 0, false
			}
			return total, true
		default:
			d, ok := digits[r]
			if !ok {
				return 0, false
			}
			section = d
			seen = true
		}
	}
	return 0, false
}

// flattenPages 把解析后的文本块拼成全文，并保留块坐标供条款定位使用。
//
// 按 CharStart 排序而不是相信数组顺序：CharStart 是跨页连续偏移，
// 由 A 侧统一产出，是唯一可靠的阅读顺序；页内数组顺序在 OCR 兜底时可能乱。
func flattenPages(pages []port.ParsedPage) (string, []port.TextBlock) {
	blocks := make([]port.TextBlock, 0, len(pages)*4)
	for _, page := range pages {
		blocks = append(blocks, page.Blocks...)
	}
	sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].CharStart < blocks[j].CharStart })

	var sb strings.Builder
	for _, b := range blocks {
		sb.WriteString(strings.TrimSpace(b.Text))
		sb.WriteString("\n")
	}
	return sb.String(), blocks
}

// locateClauses 用原文块校正模型给出的条款坐标。
//
// 模型经常把 page 填 0、bbox 填 0：它只看到文本，看不到版面。
// 这里按"条款原文包含于某块"回填真实坐标，前端才能把风险标到正确位置。
// 匹配不到的保留原值——标到第 0 点也比丢进"未定位"列表好。
func locateClauses(clauses []rules.Clause, blocks []port.TextBlock) []rules.Clause {
	if len(clauses) == 0 || len(blocks) == 0 {
		return clauses
	}
	normalized := make([]string, len(blocks))
	for i, b := range blocks {
		normalized[i] = squash(b.Text)
	}
	out := make([]rules.Clause, len(clauses))
	copy(out, clauses)
	for i := range out {
		text := squash(out[i].Text)
		if text == "" {
			continue
		}
		for j, blockText := range normalized {
			if blockText == "" {
				continue
			}
			if strings.Contains(blockText, text) || strings.Contains(text, blockText) {
				out[i].Page = blocks[j].Page
				out[i].BBox = blocks[j].BBox
				break
			}
		}
	}
	return out
}

// squash 去掉所有空白，用于"这段文字是否出现在原文里"的比对。
//
// 只去空白而不去标点：标点被模型改写时（，→ ,）仍应视为不同，
// 否则会把一条"看起来像"的条款错配到别的段落坐标上。
func squash(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r', '\u3000':
			continue
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// notify 在 progress 为 nil 时静默跳过：单测与本地脚本经常不传回调。
func notify(progress func(int, string), percent int, stage string) {
	if progress != nil {
		progress(percent, stage)
	}
}
