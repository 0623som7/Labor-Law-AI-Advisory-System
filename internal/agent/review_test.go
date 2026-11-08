package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"laborlaw/internal/extract"
	"laborlaw/internal/llm"
	"laborlaw/internal/port"
	"laborlaw/internal/rules"
)

// reviewTestUser 与 reviewTestFile 是测试里固定的一组归属。
// 换掉它们不会影响断言，但所有 fake 都按这组值查，避免"越权"与"不存在"混淆。
const (
	reviewTestUser = "u-001"
	reviewTestFile = "f-001"
	reviewTestRev  = "r-001"
)

// ---- fake ----

// fakeFileStore 只实现审查链路用到的方法，其余通过内嵌接口留空。
type fakeFileStore struct {
	port.FileStore
	record    port.FileRecord
	recordErr error
	pages     []port.ParsedPage
	pagesErr  error
	gets      int
}

func (f *fakeFileStore) Get(_ context.Context, _, _ string) (*port.FileRecord, error) {
	f.gets++
	if f.recordErr != nil {
		return nil, f.recordErr
	}
	rec := f.record
	return &rec, nil
}

func (f *fakeFileStore) LoadPages(_ context.Context, _ string) ([]port.ParsedPage, error) {
	return f.pages, f.pagesErr
}

// fakeReviewStore 记录写入，供断言"落库只发生一次"与"状态最终是 succeeded"。
type fakeReviewStore struct {
	port.ReviewStore
	updates []port.Review
	saves   int
	saved   []port.Finding
	saveErr error
	// ctxErrs 与 updates 一一对应：记录每次 Update 拿到的 ctx 是否已经取消。
	// 失败路径必须用 WithoutCancel 派生的 ctx 写库，否则记下来的就是取消态。
	ctxErrs []error
}

func (f *fakeReviewStore) Update(ctx context.Context, r *port.Review) error {
	f.updates = append(f.updates, *r)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return nil
}

func (f *fakeReviewStore) SaveFindings(_ context.Context, _ string, findings []port.Finding) error {
	f.saves++
	f.saved = findings
	return f.saveErr
}

// fakeStatuteStore 用一张固定表模拟 205 条种子库的一小部分。
type fakeStatuteStore struct {
	port.StatuteStore
	rows map[string]port.Statute
	err  error
	gets int
}

func (f *fakeStatuteStore) Get(_ context.Context, id string) (*port.Statute, error) {
	f.gets++
	if f.err != nil {
		return nil, f.err
	}
	st, ok := f.rows[id]
	if !ok {
		return nil, port.New(port.CodeNotFound, "法条不存在")
	}
	return &st, nil
}

// seedStatutes 返回测试用的法条表，正文用可辨识的占位文本。
func seedStatutes() map[string]port.Statute {
	law := rules.LawLaborContract
	return map[string]port.Statute{
		"LCL-9":  {ID: "LCL-9", Law: law, Article: "第九条", Text: "用人单位招用劳动者，不得要求劳动者提供担保或者以其他名义向劳动者收取财物。"},
		"LCL-12": {ID: "LCL-12", Law: law, Article: "第十二条", Text: "劳动合同分为固定期限劳动合同、无固定期限劳动合同和以完成一定工作任务为期限的劳动合同。"},
		"LCL-17": {ID: "LCL-17", Law: law, Article: "第十七条", Text: "劳动合同应当具备以下条款：……（七）社会保险；"},
		"LCL-19": {ID: "LCL-19", Law: law, Article: "第十九条", Text: "劳动合同期限三个月以上不满一年的，试用期不得超过一个月；……"},
		"LCL-20": {ID: "LCL-20", Law: law, Article: "第二十条", Text: "劳动者在试用期的工资不得低于本单位相同岗位最低档工资或者劳动合同约定工资的百分之八十。"},
		"LL-44":  {ID: "LL-44", Law: rules.LawLabor, Article: "第四十四条", Text: "有下列情形之一的，用人单位应当按照下列标准支付高于劳动者正常工作时间工资的工资报酬：……"},
		"LL-48":  {ID: "LL-48", Law: rules.LawLabor, Article: "第四十八条", Text: "用人单位支付劳动者的工资不得低于当地最低工资标准。"},
		"LL-72":  {ID: "LL-72", Law: rules.LawLabor, Article: "第七十二条", Text: "用人单位和劳动者必须依法参加社会保险，缴纳社会保险费。"},
	}
}

// extractReply 是一份"八项风险全中"的抽取结果。
//
// 用它而不是真实模型：这条测试要验证的是编排（次数、顺序、落库字段），
// 模型输出必须稳定；抽取质量由 internal/extract 的用例单独覆盖。
const extractReply = `{
  "party_a": "某某科技有限公司",
  "party_b": "张三",
  "contract_type": "固定期限劳动合同",
  "term_start": "2024-03-01",
  "term_end": "",
  "probation_months": 8,
  "salary_monthly": 1500,
  "salary_probation": 1000,
  "social_insurance": false,
  "work_location": "东莞市",
  "position": "销售",
  "working_hours": "标准工时",
  "has_overtime_pay": false,
  "deposit": true,
  "raw_clauses": [
    {"section": "第二条", "page": 1, "bbox": [72, 640, 520, 668], "text": "试用期为八个月，试用期内工资为每月 1000 元。"},
    {"section": "第五条", "page": 1, "bbox": [72, 500, 520, 528], "text": "试用期届满后再次入职的，另行约定试用期为六个月；入职时乙方需向甲方缴纳押金 2000 元。"},
    {"section": "第六条", "page": 1, "bbox": [72, 460, 520, 488], "text": "乙方自愿加班，甲方不另行支付加班费。"}
  ]
}`

// pagesFixture 返回一页解析结果，文本块与 extractReply 的条款一一对应。
func pagesFixture() []port.ParsedPage {
	return []port.ParsedPage{{
		Index:  1,
		Width:  595,
		Height: 842,
		Blocks: []port.TextBlock{
			{ID: "b1", Page: 1, CharStart: 0, CharEnd: 20, BBox: [4]float64{72, 760, 520, 780}, Text: "劳动合同"},
			{ID: "b2", Page: 1, CharStart: 21, CharEnd: 80, BBox: [4]float64{72, 640, 520, 668}, Text: "第二条 试用期为八个月，试用期内工资为每月 1000 元。"},
			{ID: "b3", Page: 1, CharStart: 81, CharEnd: 140, BBox: [4]float64{72, 500, 520, 528}, Text: "第五条 试用期届满后再次入职的，另行约定试用期为六个月；入职时乙方需向甲方缴纳押金 2000 元。"},
			{ID: "b4", Page: 1, CharStart: 141, CharEnd: 200, BBox: [4]float64{72, 460, 520, 488}, Text: "第六条 乙方自愿加班，甲方不另行支付加班费。"},
		},
	}}
}

// newReviewFixture 组装一套默认依赖。
func newReviewFixture() (port.Handler, *fakeFileStore, *fakeReviewStore, *fakeStatuteStore) {
	files := &fakeFileStore{
		record: port.FileRecord{ID: reviewTestFile, UserID: reviewTestUser, Filename: "劳动合同.pdf"},
		pages:  pagesFixture(),
	}
	reviews := &fakeReviewStore{}
	statutes := &fakeStatuteStore{rows: seedStatutes()}
	handler := NewReviewHandler(ReviewDeps{
		Files:     files,
		Reviews:   reviews,
		Statutes:  statutes,
		Extractor: extract.New(&llm.FakeClient{Reply: extractReply}),
	})
	return handler, files, reviews, statutes
}

// payloadFor 生成合法载荷。
func payloadFor() json.RawMessage {
	b, _ := json.Marshal(ReviewPayload{
		ReviewID: reviewTestRev,
		FileID:   reviewTestFile,
		UserID:   reviewTestUser,
	})
	return b
}

// ---- 用例 ----

// TestReviewHandlerHappyPath 覆盖主路径：8 条规则全中、落库一次、状态 succeeded。
func TestReviewHandlerHappyPath(t *testing.T) {
	handler, _, reviews, statutes := newReviewFixture()

	var percents []int
	var stages []string
	progress := func(p int, stage string) {
		percents = append(percents, p)
		stages = append(stages, stage)
	}

	if err := handler(context.Background(), payloadFor(), progress); err != nil {
		t.Fatalf("run review: %v", err)
	}

	if reviews.saves != 1 {
		t.Fatalf("SaveFindings calls = %d, want 1", reviews.saves)
	}
	if len(reviews.saved) != 8 {
		t.Fatalf("findings = %d, want 8; got %+v", len(reviews.saved), ruleIDsOf(reviews.saved))
	}

	want := []string{
		"R-PROBATION-MATCH", "R-PROBATION-SALARY", "R-PROBATION-ONCE", "R-DEPOSIT",
		"R-NO-SOCIAL-INS", "R-WAGE-UNDER-MIN", "R-TERM-NO-END", "R-OVERTIME-FREE",
	}
	got := ruleIDsOf(reviews.saved)
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rule order = %v, want %v", got, want)
		}
	}

	for _, f := range reviews.saved {
		if f.ReviewID != reviewTestRev {
			t.Fatalf("finding %s review_id = %q", f.RuleID, f.ReviewID)
		}
		if f.Title == "" || f.Evidence == "" || f.Suggestion.Reason == "" {
			t.Fatalf("finding %s 字段不完整: %+v", f.RuleID, f)
		}
		if f.Severity != port.SeverityHigh && f.Severity != port.SeverityMedium {
			t.Fatalf("finding %s severity = %q", f.RuleID, f.Severity)
		}
	}

	// 法条必须来自库：抽一条核对正文与 fake 表逐字一致。
	var deposit *port.Finding
	for i := range reviews.saved {
		if reviews.saved[i].RuleID == "R-DEPOSIT" {
			deposit = &reviews.saved[i]
		}
	}
	if deposit == nil || len(deposit.LegalBasis) != 1 {
		t.Fatalf("R-DEPOSIT legal_basis = %+v", deposit)
	}
	if deposit.LegalBasis[0].Text != seedStatutes()["LCL-9"].Text {
		t.Fatalf("legal_basis text 与库不一致: %q", deposit.LegalBasis[0].Text)
	}

	// 社保规则引两条法条，且都来自库。
	for _, f := range reviews.saved {
		if f.RuleID == "R-NO-SOCIAL-INS" && len(f.LegalBasis) != 2 {
			t.Fatalf("R-NO-SOCIAL-INS legal_basis = %+v", f.LegalBasis)
		}
	}

	if statutes.gets == 0 {
		t.Fatal("未查询法条库")
	}

	// 坐标校正：模型给的 bbox 与解析块一致时保持，条款页码来自文本块。
	if reviews.saved[0].ClauseRef.Section != "第二条" {
		t.Fatalf("clause_ref.section = %q", reviews.saved[0].ClauseRef.Section)
	}
	if reviews.saved[0].ClauseRef.Page != 1 || reviews.saved[0].ClauseRef.BBox != [4]float64{72, 640, 520, 668} {
		t.Fatalf("clause_ref = %+v", reviews.saved[0].ClauseRef)
	}

	// 状态：先 running 后 succeeded，且计数与评分一致（5 高危 + 3 中危 → 0 分）。
	if len(reviews.updates) != 2 {
		t.Fatalf("Update calls = %d, want 2", len(reviews.updates))
	}
	first, last := reviews.updates[0], reviews.updates[1]
	if first.Status != port.TaskRunning {
		t.Fatalf("first status = %q", first.Status)
	}
	if last.Status != port.TaskSucceeded {
		t.Fatalf("last status = %q", last.Status)
	}
	if last.Counts != (port.ReviewCounts{High: 5, Medium: 3}) {
		t.Fatalf("counts = %+v", last.Counts)
	}
	if last.RiskLevel != port.SeverityHigh || last.Score != 0 {
		t.Fatalf("risk=%q score=%d", last.RiskLevel, last.Score)
	}
	if last.ClausePassRate < 0 || last.ClausePassRate > 1 {
		t.Fatalf("clause_pass_rate = %v", last.ClausePassRate)
	}

	// 进度单调不减，且以 100 收尾。
	for i := 1; i < len(percents); i++ {
		if percents[i] < percents[i-1] {
			t.Fatalf("progress not monotonic: %v", percents)
		}
	}
	if percents[len(percents)-1] != 100 || stages[len(stages)-1] != stageDone {
		t.Fatalf("final progress = %d %q", percents[len(percents)-1], stages[len(stages)-1])
	}
}

// TestReviewHandlerRuleScope 覆盖"某条规则不命中"：只跑押金规则且合同没有押金。
func TestReviewHandlerRuleScope(t *testing.T) {
	reply := strings.Replace(extractReply, `"deposit": true`, `"deposit": false`, 1)
	reply = strings.Replace(reply, "押金 2000 元", "绩效奖金 2000 元", 1)

	files := &fakeFileStore{
		record: port.FileRecord{ID: reviewTestFile, UserID: reviewTestUser},
		pages:  pagesFixture(),
	}
	reviews := &fakeReviewStore{}
	handler := NewReviewHandler(ReviewDeps{
		Files:     files,
		Reviews:   reviews,
		Statutes:  &fakeStatuteStore{rows: seedStatutes()},
		Extractor: extract.New(&llm.FakeClient{Reply: reply}),
		Rules:     rules.Filter([]string{"R-DEPOSIT"}),
	})

	payload, _ := json.Marshal(ReviewPayload{
		ReviewID:  reviewTestRev,
		FileID:    reviewTestFile,
		UserID:    reviewTestUser,
		RuleScope: []string{"R-DEPOSIT"},
	})
	if err := handler(context.Background(), payload, nil); err != nil {
		t.Fatalf("run review: %v", err)
	}
	if reviews.saves != 1 {
		t.Fatalf("SaveFindings calls = %d", reviews.saves)
	}
	if len(reviews.saved) != 0 {
		t.Fatalf("findings = %+v, want empty", ruleIDsOf(reviews.saved))
	}
	last := reviews.updates[len(reviews.updates)-1]
	if last.Counts != (port.ReviewCounts{}) || last.Score != 100 || last.RiskLevel != port.SeverityLow {
		t.Fatalf("empty review counts=%+v score=%d risk=%q", last.Counts, last.Score, last.RiskLevel)
	}
	if last.ClausePassRate != 1 {
		t.Fatalf("clause_pass_rate = %v, want 1", last.ClausePassRate)
	}
}

// TestReviewHandlerRetryableLLMError 覆盖可重试错误：上游 429 必须原码透出。
func TestReviewHandlerRetryableLLMError(t *testing.T) {
	files := &fakeFileStore{
		record: port.FileRecord{ID: reviewTestFile, UserID: reviewTestUser},
		pages:  pagesFixture(),
	}
	reviews := &fakeReviewStore{}
	handler := NewReviewHandler(ReviewDeps{
		Files:     files,
		Reviews:   reviews,
		Statutes:  &fakeStatuteStore{rows: seedStatutes()},
		Extractor: extract.New(&llm.FakeClient{Err: port.New(port.CodeRateLimited, "上游限流")}),
	})

	err := handler(context.Background(), payloadFor(), nil)
	if err == nil {
		t.Fatal("want error")
	}
	if !port.IsCode(err, port.CodeRateLimited) {
		t.Fatalf("code = %v, err=%v", port.CodeOf(err), err)
	}
	if !port.IsRetryable(err) {
		t.Fatal("429 必须可重试，否则 worker 不会重排")
	}
	if reviews.saves != 0 {
		t.Fatal("抽取失败时不得落库")
	}
}

// TestReviewHandlerBadPayload 覆盖非法载荷：直接 40001，不碰任何 store。
func TestReviewHandlerBadPayload(t *testing.T) {
	handler, files, reviews, statutes := newReviewFixture()

	cases := []struct {
		name    string
		payload json.RawMessage
	}{
		{"invalid json", json.RawMessage(`{"review_id":`)},
		{"missing ids", json.RawMessage(`{"review_id":"r-1"}`)},
		{"empty object", json.RawMessage(`{}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := handler(context.Background(), tc.payload, nil)
			if !port.IsCode(err, port.CodeInvalidArgument) {
				t.Fatalf("code = %v, err=%v", port.CodeOf(err), err)
			}
			if files.gets != 0 || reviews.saves != 0 || statutes.gets != 0 {
				t.Fatal("非法载荷不应触发任何 IO")
			}
		})
	}
}

// TestReviewHandlerUnparsedFile 覆盖文件未解析：42201 而不是 50001。
func TestReviewHandlerUnparsedFile(t *testing.T) {
	files := &fakeFileStore{
		record:   port.FileRecord{ID: reviewTestFile, UserID: reviewTestUser},
		pagesErr: port.New(port.CodeNotFound, "页面不存在"),
	}
	reviews := &fakeReviewStore{}
	handler := NewReviewHandler(ReviewDeps{
		Files:     files,
		Reviews:   reviews,
		Statutes:  &fakeStatuteStore{rows: seedStatutes()},
		Extractor: extract.New(&llm.FakeClient{Reply: extractReply}),
	})

	err := handler(context.Background(), payloadFor(), nil)
	if !port.IsCode(err, port.CodeNoTextLayer) {
		t.Fatalf("code = %v, err=%v", port.CodeOf(err), err)
	}
	if reviews.saves != 0 {
		t.Fatal("未解析文件不应落库")
	}
}

// TestReviewHandlerSaveFindingsError 覆盖落库失败：错误原样上抛，状态不置 succeeded。
func TestReviewHandlerSaveFindingsError(t *testing.T) {
	files := &fakeFileStore{
		record: port.FileRecord{ID: reviewTestFile, UserID: reviewTestUser},
		pages:  pagesFixture(),
	}
	reviews := &fakeReviewStore{saveErr: port.New(port.CodeInternal, "写库失败")}
	handler := NewReviewHandler(ReviewDeps{
		Files:     files,
		Reviews:   reviews,
		Statutes:  &fakeStatuteStore{rows: seedStatutes()},
		Extractor: extract.New(&llm.FakeClient{Reply: extractReply}),
	})

	if err := handler(context.Background(), payloadFor(), nil); err == nil {
		t.Fatal("want error")
	}
	for _, u := range reviews.updates {
		if u.Status == port.TaskSucceeded {
			t.Fatal("落库失败后不得标记 succeeded")
		}
	}
}

// cancellingClient 在第一次被调用时取消外层 ctx，再返回 ctx 的错误。
// 模拟"worker 超时正好发生在抽取那一刻"——真实日志里就是这个形态：
// 上游卡到 120 秒，worker 到时 cancel，抽取返回 context deadline exceeded。
type cancellingClient struct {
	*llm.FakeClient
	cancel context.CancelFunc
}

func (c *cancellingClient) Chat(ctx context.Context, _ port.ChatRequest) (port.ChatResponse, error) {
	c.cancel()
	return port.ChatResponse{}, ctx.Err()
}

// lastUpdate 返回最后一次写入 review 的状态与那次写入拿到的 ctx 错误。
func lastUpdate(t *testing.T, reviews *fakeReviewStore) (port.Review, error) {
	t.Helper()
	if len(reviews.updates) == 0 {
		t.Fatal("没有任何 review 写入")
	}
	last := len(reviews.updates) - 1
	return reviews.updates[last], reviews.ctxErrs[last]
}

// TestReviewMarksFailedOnExtractError 覆盖抽取报错：Run 返回 error，review 落成 failed。
//
// 这条是 P0 回归：失败路径原来直接 return，review 永远停在 running，
// 前端一直转圈，而 /tasks 已经是 failed。
func TestReviewMarksFailedOnExtractError(t *testing.T) {
	reviews := &fakeReviewStore{}
	handler := NewReviewHandler(ReviewDeps{
		Files:     &fakeFileStore{record: port.FileRecord{ID: reviewTestFile, UserID: reviewTestUser}, pages: pagesFixture()},
		Reviews:   reviews,
		Statutes:  &fakeStatuteStore{rows: seedStatutes()},
		Extractor: extract.New(&llm.FakeClient{Err: errors.New("抽取挂了")}),
	})

	if err := handler(context.Background(), payloadFor(), nil); err == nil {
		t.Fatal("want error")
	}

	last, _ := lastUpdate(t, reviews)
	if last.Status != port.TaskFailed {
		t.Fatalf("最后一次写入状态 = %q, want %q", last.Status, port.TaskFailed)
	}
	var sawRunning bool
	for _, u := range reviews.updates {
		if u.Status == port.TaskRunning {
			sawRunning = true
		}
	}
	if !sawRunning {
		t.Fatal("失败前应当先落过 running，否则分不清'没被领取'与'跑失败'")
	}
	if reviews.saves != 0 {
		t.Fatal("抽取失败时不得写 findings")
	}
}

// TestReviewMarksFailedAfterContextCancel 覆盖 worker 超时：ctx 已取消仍要落终态。
//
// 最容易漏的一条：取消后的 ctx 拿去写库必然 context deadline exceeded，
// 所以兜底必须用 context.WithoutCancel 派生的 ctx —— 断言 ctxErrs 最后一位为 nil 就是在验这件事。
func TestReviewMarksFailedAfterContextCancel(t *testing.T) {
	reviews := &fakeReviewStore{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	handler := NewReviewHandler(ReviewDeps{
		Files:     &fakeFileStore{record: port.FileRecord{ID: reviewTestFile, UserID: reviewTestUser}, pages: pagesFixture()},
		Reviews:   reviews,
		Statutes:  &fakeStatuteStore{rows: seedStatutes()},
		Extractor: extract.New(&cancellingClient{FakeClient: &llm.FakeClient{}, cancel: cancel}),
	})

	if err := handler(ctx, payloadFor(), nil); err == nil {
		t.Fatal("ctx 被取消时必须返回 error")
	}

	last, ctxErr := lastUpdate(t, reviews)
	if last.Status != port.TaskFailed {
		t.Fatalf("最后一次写入状态 = %q, want %q", last.Status, port.TaskFailed)
	}
	if ctxErr != nil {
		t.Fatalf("落终态用的 ctx 已被取消（%v），真实库上这次写入会失败", ctxErr)
	}
}

// TestReviewSuccessKeepsSucceeded 覆盖成功路径：兜底的 failed 不许覆盖 succeeded。
func TestReviewSuccessKeepsSucceeded(t *testing.T) {
	handler, _, reviews, _ := newReviewFixture()
	if err := handler(context.Background(), payloadFor(), nil); err != nil {
		t.Fatalf("run review: %v", err)
	}

	last, _ := lastUpdate(t, reviews)
	if last.Status != port.TaskSucceeded {
		t.Fatalf("最后一次写入状态 = %q, want %q", last.Status, port.TaskSucceeded)
	}
	for _, u := range reviews.updates {
		if u.Status == port.TaskFailed {
			t.Fatal("成功路径不得出现 failed 写入")
		}
	}
	if last.Score == 0 && last.Counts.High+last.Counts.Medium == 0 {
		t.Fatal("成功终态必须带评分与计数，否则 /reviews 上没有风险信息")
	}
}

// TestStatuteLookup 覆盖种子 ID 规则与中文数字解析。
func TestStatuteLookup(t *testing.T) {
	cases := []struct {
		law, article string
		wantID       string
		wantOK       bool
	}{
		{rules.LawLaborContract, "第九条", "LCL-9", true},
		{rules.LawLaborContract, "第十九条", "LCL-19", true},
		{rules.LawLabor, "第七十二条", "LL-72", true},
		{rules.LawLabor, "第四十八条", "LL-48", true},
		{rules.LawSocialInsure, "第五十八条", "", false},
		{rules.LawLabor, "第条", "", false},
	}
	for _, tc := range cases {
		id, ok := statuteID(tc.law, tc.article)
		if id != tc.wantID || ok != tc.wantOK {
			t.Fatalf("statuteID(%q,%q) = (%q,%v), want (%q,%v)", tc.law, tc.article, id, ok, tc.wantID, tc.wantOK)
		}
	}
}

// TestStatuteLookupDegrades 覆盖库故障：不报错、不编造，legal_basis 留空。
func TestStatuteLookupDegrades(t *testing.T) {
	store := &fakeStatuteStore{err: errors.New("db down")}
	lookup := newStatuteLookup(store, nil)
	basis := lookup.resolve(context.Background(), []rules.ArticleRef{{Law: rules.LawLaborContract, Article: "第十九条"}})
	if basis != nil {
		t.Fatalf("basis = %+v, want nil", basis)
	}

	empty := newStatuteLookup(nil, nil)
	if got := empty.resolve(context.Background(), []rules.ArticleRef{{Law: rules.LawLaborContract, Article: "第十九条"}}); got != nil {
		t.Fatalf("未装配 statutes 时 = %+v, want nil", got)
	}
}

// TestLocateClauses 覆盖坐标回填：模型给 0 坐标时按原文块补全。
func TestLocateClauses(t *testing.T) {
	blocks := pagesFixture()[0].Blocks
	in := []rules.Clause{{Section: "第二条", Text: "试用期为八个月，试用期内工资为每月 1000 元。"}}
	out := locateClauses(in, blocks)
	if out[0].Page != 1 || out[0].BBox != [4]float64{72, 640, 520, 668} {
		t.Fatalf("located = %+v", out[0])
	}

	miss := locateClauses([]rules.Clause{{Section: "第九条", Text: "完全不存在的条款"}}, blocks)
	if miss[0].Page != 0 || miss[0].BBox != [4]float64{} {
		t.Fatalf("未命中时不应改写坐标: %+v", miss[0])
	}
}

// ruleIDsOf 把 findings 的规则 ID 抽出来，便于按顺序断言。
func ruleIDsOf(findings []port.Finding) []string {
	out := make([]string, len(findings))
	for i, f := range findings {
		out[i] = f.RuleID
	}
	return out
}
