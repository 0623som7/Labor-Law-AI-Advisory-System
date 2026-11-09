package extract

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"laborlaw/internal/llm"
	"laborlaw/internal/port"
)

// recordingClient 记录入参，正文由内嵌的 FakeClient 提供。
//
// 内嵌而不是重写全部方法：ChatStream 的语义由 FakeClient 保证，
// 这里只需要在 Chat 上多抓一份请求，用来断言场景、schema 与 prompt 约束。
type recordingClient struct {
	llm.FakeClient
	got   port.ChatRequest
	calls int
}

func (c *recordingClient) Chat(ctx context.Context, req port.ChatRequest) (port.ChatResponse, error) {
	c.calls++
	c.got = req
	return c.FakeClient.Chat(ctx, req)
}

const fullJSON = `{
  "party_a": "东莞某某科技有限公司",
  "party_b": "张三",
  "contract_type": "固定期限",
  "term_start": "2024-01-01",
  "term_end": "2027-01-01",
  "probation_months": 3,
  "salary_monthly": 8000,
  "salary_probation": 5000,
  "social_insurance": false,
  "work_location": "东莞市南城区",
  "position": "后端开发",
  "working_hours": "标准工时",
  "has_overtime_pay": false,
  "deposit": true,
  "raw_clauses": [
    {"section": "第二条", "page": 0, "bbox": [10, 700, 300, 720], "text": "试用期为三个月。"},
    {"section": "第四条", "page": 1, "bbox": [10, 100, 300, 120], "text": "乙方入职时缴纳服装押金 500 元。"}
  ]
}`

func TestParseFullDocument(t *testing.T) {
	doc, err := Parse(fullJSON)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if doc.PartyA != "东莞某某科技有限公司" || doc.Position != "后端开发" {
		t.Fatalf("字符串字段错误：%+v", doc.Contract)
	}
	if doc.ProbationMonths != 3 || doc.SalaryMonthly != 8000 || doc.SalaryProbation != 5000 {
		t.Fatalf("数字字段错误：%+v", doc.Contract)
	}
	if doc.SocialInsurance == nil || *doc.SocialInsurance {
		t.Fatalf("social_insurance 应为明确的 false：%+v", doc.SocialInsurance)
	}
	if doc.Deposit == nil || !*doc.Deposit {
		t.Fatalf("deposit 应为 true：%+v", doc.Deposit)
	}
	if len(doc.Clauses) != 2 {
		t.Fatalf("条款数 %d，期望 2", len(doc.Clauses))
	}
	if doc.Clauses[1].Section != "第四条" || doc.Clauses[1].Page != 1 || doc.Clauses[1].BBox[3] != 120 {
		t.Fatalf("条款定位错误：%+v", doc.Clauses[1])
	}
	if len(doc.Missing) != 0 {
		t.Fatalf("完整文档不应有缺失字段：%v", doc.Missing)
	}
	if math.Abs(doc.Confidence-1) > 1e-9 {
		t.Fatalf("confidence=%v，期望 1", doc.Confidence)
	}
}

func TestParseCodeFence(t *testing.T) {
	fenced := "```json\n" + fullJSON + "\n```"
	doc, err := Parse(fenced)
	if err != nil {
		t.Fatalf("带围栏的正文应能解析：%v", err)
	}
	if doc.SalaryMonthly != 8000 {
		t.Fatalf("围栏解析后字段丢失：%+v", doc.Contract)
	}

	// 围栏后带空白的变体。
	spaced := "\n\n```\n{\"salary_monthly\": 6000}\n```\n\n"
	doc, err = Parse(spaced)
	if err != nil {
		t.Fatalf("无语言标记的围栏应能解析：%v", err)
	}
	if doc.SalaryMonthly != 6000 {
		t.Fatalf("salar_monthly 解析错误：%v", doc.SalaryMonthly)
	}
}

func TestParseMissingFields(t *testing.T) {
	doc, err := Parse(`{"party_a": "A 公司", "probation_months": 2}`)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if doc.PartyA != "A 公司" || doc.ProbationMonths != 2 {
		t.Fatalf("已给字段解析错误：%+v", doc.Contract)
	}
	wantMissing := map[string]bool{
		"party_b": true, "contract_type": true, "term_start": true, "term_end": true,
		"salary_monthly": true, "salary_probation": true, "social_insurance": true,
		"work_location": true, "position": true, "working_hours": true,
		"has_overtime_pay": true, "deposit": true,
	}
	if len(doc.Missing) != len(wantMissing) {
		t.Fatalf("缺失字段 %v，期望 %d 个", doc.Missing, len(wantMissing))
	}
	for _, m := range doc.Missing {
		if !wantMissing[m] {
			t.Fatalf("多报了缺失字段：%s", m)
		}
	}
	// 14 个字段里命中 2 个。
	if math.Abs(doc.Confidence-2.0/14.0) > 1e-9 {
		t.Fatalf("confidence=%v，期望 %v", doc.Confidence, 2.0/14.0)
	}
}

func TestParseInvalidJSON(t *testing.T) {
	for _, body := range []string{
		"抱歉，我无法完成该任务。",
		`{"party_a": "A 公司"`,
		`["不是对象"]`,
		"",
		"```json\n```",
	} {
		doc, err := Parse(body)
		if err == nil {
			t.Fatalf("正文 %q 应当解析失败，却得到 %+v", body, doc)
		}
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("错误类型应为 *ParseError，实际 %T：%v", err, err)
		}
		if !strings.Contains(err.Error(), "解析失败") {
			t.Fatalf("错误信息不可读：%v", err)
		}
	}
}

func TestParseWrongTypes(t *testing.T) {
	cases := []string{
		`{"salary_monthly": {"amount": 8000}}`,
		`{"salary_monthly": "面议"}`,
		`{"social_insurance": 1}`,
		`{"raw_clauses": "试用期三个月"}`,
		`{"raw_clauses": [{"text": "x", "bbox": [1, 2, 3]}]}`,
		`{"raw_clauses": [{"text": "x", "page": "第一页"}]}`,
	}
	for _, body := range cases {
		_, err := Parse(body)
		var pe *ParseError
		if !errors.As(err, &pe) {
			t.Fatalf("正文 %s 应以 ParseError 失败，实际 %v", body, err)
		}
	}
}

func TestParseLooseValues(t *testing.T) {
	doc, err := Parse(`{"salary_monthly": "8,000 元", "probation_months": "1.5 个月", "social_insurance": "是", "has_overtime_pay": "否"}`)
	if err != nil {
		t.Fatalf("宽松写法应能解析：%v", err)
	}
	if doc.SalaryMonthly != 8000 || doc.ProbationMonths != 1.5 {
		t.Fatalf("数值纠偏失败：%+v", doc.Contract)
	}
	if doc.SocialInsurance == nil || !*doc.SocialInsurance {
		t.Fatalf("中文布尔纠偏失败：%+v", doc.SocialInsurance)
	}
	if doc.HasOvertimePay == nil || *doc.HasOvertimePay {
		t.Fatalf("中文布尔纠偏失败：%+v", doc.HasOvertimePay)
	}

	doc, err = Parse(`{"salary_monthly": "1.2万"}`)
	if err != nil {
		t.Fatalf("万元写法应能解析：%v", err)
	}
	if doc.SalaryMonthly != 12000 {
		t.Fatalf("万元换算错误：%v", doc.SalaryMonthly)
	}
}

func TestExtractSendsPromptAndSchema(t *testing.T) {
	client := &recordingClient{FakeClient: llm.FakeClient{Reply: fullJSON}}
	doc, err := New(client).Extract(context.Background(), "  劳动合同正文  ", nil)
	if err != nil {
		t.Fatalf("抽取失败：%v", err)
	}
	if doc.SalaryMonthly != 8000 {
		t.Fatalf("抽取结果错误：%+v", doc.Contract)
	}
	if client.got.Scene != Scene || Scene != "review.extract" {
		t.Fatalf("场景错误：%q", client.got.Scene)
	}
	if client.got.JSONSchema == nil || client.got.JSONSchema.Name != "contract_extract" || !client.got.JSONSchema.Strict {
		t.Fatalf("未携带结构化输出约束：%+v", client.got.JSONSchema)
	}
	if len(client.got.Messages) != 2 {
		t.Fatalf("消息条数 %d，期望 2", len(client.got.Messages))
	}
	sys := client.got.Messages[0]
	if sys.Role != port.RoleSystem {
		t.Fatalf("首条必须是 system，实际 %s", sys.Role)
	}
	for _, must := range []string{"不许编造", "不得改写", "只输出一个 JSON 对象", "raw_clauses"} {
		if !strings.Contains(sys.Content, must) {
			t.Fatalf("系统提示词缺少约束 %q：%s", must, sys.Content)
		}
	}
	if client.got.Messages[1].Content != "劳动合同正文" {
		t.Fatalf("用户消息未去除首尾空白：%q", client.got.Messages[1].Content)
	}
	if client.got.MaxTokens != maxOutputTokens {
		t.Fatalf("MaxTokens=%d，期望 %d", client.got.MaxTokens, maxOutputTokens)
	}
}

func TestExtractTruncatesLongInput(t *testing.T) {
	client := &recordingClient{FakeClient: llm.FakeClient{Reply: fullJSON}}
	long := strings.Repeat("劳", maxInputRunes+500)
	if _, err := New(client).Extract(context.Background(), long, nil); err != nil {
		t.Fatalf("长文本不应失败：%v", err)
	}
	got := []rune(client.got.Messages[1].Content)
	if len(got) != maxInputRunes {
		t.Fatalf("输入截断到 %d 字，期望 %d", len(got), maxInputRunes)
	}
}

func TestExtractUpstreamErrorKeepsCode(t *testing.T) {
	upstream := llm.FakeClient{Err: port.New(port.CodeRateLimited, "上游限流")}
	_, err := New(&upstream).Extract(context.Background(), "合同正文", nil)
	if port.CodeOf(err) != port.CodeRateLimited {
		t.Fatalf("错误码应保留 42901，实际 %v", port.CodeOf(err))
	}
	if !port.IsRetryable(err) {
		t.Fatalf("限流错误应可重试，worker 否则会直接放弃")
	}

	timeout := llm.FakeClient{Err: port.New(port.CodeUpstreamTimeout, "上游超时")}
	if _, err := New(&timeout).Extract(context.Background(), "合同正文", nil); port.CodeOf(err) != port.CodeUpstreamTimeout {
		t.Fatalf("超时码应保留 50301，实际 %v", port.CodeOf(err))
	}

	bad := llm.FakeClient{Err: port.New(port.CodeInvalidArgument, "请求不合法")}
	if _, err := New(&bad).Extract(context.Background(), "合同正文", nil); port.CodeOf(err) != port.CodeInvalidArgument {
		t.Fatalf("参数错误应原样返回，实际 %v", port.CodeOf(err))
	}
}

func TestExtractGuards(t *testing.T) {
	if _, err := New(nil).Extract(context.Background(), "合同正文", nil); port.CodeOf(err) != port.CodeInternal {
		t.Fatalf("未装配客户端应返回 50001，实际 %v", port.CodeOf(err))
	}
	client := &recordingClient{FakeClient: llm.FakeClient{Reply: fullJSON}}
	if _, err := New(client).Extract(context.Background(), "   ", nil); port.CodeOf(err) != port.CodeInvalidArgument {
		t.Fatalf("空正文应返回 40001，实际 %v", port.CodeOf(err))
	}
	if client.calls != 0 {
		t.Fatalf("空正文不应调用模型，实际调用 %d 次", client.calls)
	}
}

func TestParseConfidenceCap(t *testing.T) {
	// 字段全给但 confidence 通过 missing 反推：确认不会超过 1。
	doc, err := Parse(fullJSON)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if doc.Confidence > 1 {
		t.Fatalf("confidence 超过 1：%v", doc.Confidence)
	}
	// payload 必须可序列化：contract_extracts.payload 是 JSON 列。
	if _, err := json.Marshal(doc); err != nil {
		t.Fatalf("抽取结果无法序列化：%v", err)
	}
}

// TestExtractMarksTruncation 守住"超长合同被截断时必须留下标记"。
//
// 缺陷形态：truncateRunes 截完什么都不记，20 页合同只分析了前 8 页，
// 后面的风险条款一条都报不出来，而界面上看起来一切正常。
func TestExtractMarksTruncation(t *testing.T) {
	long := strings.Repeat("劳", 13000)
	c := &recordingClient{FakeClient: llm.FakeClient{Reply: `{"party_a":"甲公司","contract_type":"固定期限劳动合同"}`}}
	doc, err := New(c).Extract(context.Background(), long, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if !doc.Truncated {
		t.Fatal("13000 字必须标记为已截断")
	}
	if doc.TruncatedAt != maxInputRunes {
		t.Fatalf("TruncatedAt = %d, want %d", doc.TruncatedAt, maxInputRunes)
	}
	// 送给模型的正文必须真的是砍过的：多送一个字都会顶爆上下文。
	if got := len([]rune(c.got.Messages[1].Content)); got != maxInputRunes {
		t.Fatalf("送进模型的正文 %d 字, want %d", got, maxInputRunes)
	}
}

// TestExtractNotTruncated 守住"没截断时标记为 false、长度等于原文"。
func TestExtractNotTruncated(t *testing.T) {
	text := strings.Repeat("合", 8000)
	c := &recordingClient{FakeClient: llm.FakeClient{Reply: `{"party_a":"乙公司"}`}}
	doc, err := New(c).Extract(context.Background(), text, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if doc.Truncated {
		t.Fatal("8000 字不该标记为已截断")
	}
	if doc.TruncatedAt != 8000 {
		t.Fatalf("TruncatedAt = %d, want 8000", doc.TruncatedAt)
	}
}

// ---- 正则优先路径（第二步改造）----

// layoutBlocks 造出与 contractSample 对应的版面块，条号带真实 bbox。
func layoutBlocks() []port.TextBlock {
	return []port.TextBlock{
		block("b0", 1, 72, 760, 520, 780, "劳动合同书"),
		block("b1", 1, 72, 700, 520, 720, "第一条 合同期限"),
		block("b2", 1, 72, 680, 520, 700, "本合同为固定期限劳动合同，自2026年1月1日起至2029年1月1日止。"),
		block("b3", 1, 72, 640, 520, 660, "第二条 试用期"),
		block("b4", 1, 72, 620, 520, 640, "双方约定试用期为八个月。"),
	}
}

// TestExtractSkipsLLMWhenComplete：14 个要素全能正则命中、条款又能从版面切出来时，
// 一次模型调用都不该发生——这是把 18 秒压到 1 秒的全部意义。
func TestExtractSkipsLLMWhenComplete(t *testing.T) {
	client := &recordingClient{FakeClient: llm.FakeClient{Reply: fullJSON}}

	doc, err := New(client).Extract(context.Background(), contractSample, layoutBlocks())
	if err != nil {
		t.Fatalf("抽取失败：%v", err)
	}
	if client.calls != 0 {
		t.Fatalf("要素齐全时不该调模型，实际调用 %d 次", client.calls)
	}
	if doc.Source != SourceRule {
		t.Fatalf("source = %q, want %q", doc.Source, SourceRule)
	}
	if doc.ClausesSource != ClauseSourceLayout {
		t.Fatalf("clause_source = %q, want layout", doc.ClausesSource)
	}
	if len(doc.Clauses) != 2 {
		t.Fatalf("条款数 = %d, want 2", len(doc.Clauses))
	}
	if doc.Confidence != 1 {
		t.Fatalf("confidence = %v, want 1", doc.Confidence)
	}
}

// TestExtractCallsLLMWhenCriticalMissing：没有试用期信息时，试用期三条规则会整条跳过，
// 用户看到的是"审查通过"——必须补一次模型。
func TestExtractCallsLLMWhenCriticalMissing(t *testing.T) {
	text := strings.Replace(contractSample, "双方约定试用期为八个月，自2026年1月1日起至2026年8月31日止。", "（本合同未约定试用期。）", 1)
	client := &recordingClient{FakeClient: llm.FakeClient{Reply: fullJSON}}

	doc, err := New(client).Extract(context.Background(), text, layoutBlocks())
	if err != nil {
		t.Fatalf("抽取失败：%v", err)
	}
	if client.calls != 1 {
		t.Fatalf("缺关键字段时应调用模型 1 次，实际 %d", client.calls)
	}
	if doc.Source != SourceRuleLLM {
		t.Fatalf("source = %q, want %q", doc.Source, SourceRuleLLM)
	}
	if doc.ProbationMonths != 3 {
		t.Fatalf("probation_months = %v, want 3（来自模型补齐）", doc.ProbationMonths)
	}
	// 模型给的坐标一律丢弃：版面块才是真值。
	if doc.ClausesSource != ClauseSourceLayout || len(doc.Clauses) != 2 {
		t.Fatalf("条款应仍取自版面：source=%q n=%d", doc.ClausesSource, len(doc.Clauses))
	}
}

// TestExtractKeepsRegexValues：模型把 8000 读成 80000 时，以正则抽到的原文数字为准。
func TestExtractKeepsRegexValues(t *testing.T) {
	text := strings.Replace(contractSample, "双方约定试用期为八个月，自2026年1月1日起至2026年8月31日止。", "（本合同未约定试用期。）", 1)
	reply := strings.Replace(fullJSON, `"salary_monthly": 8000`, `"salary_monthly": 80000`, 1)
	client := &recordingClient{FakeClient: llm.FakeClient{Reply: reply}}

	doc, err := New(client).Extract(context.Background(), text, layoutBlocks())
	if err != nil {
		t.Fatalf("抽取失败：%v", err)
	}
	if doc.SalaryMonthly != 8000 {
		t.Fatalf("salary_monthly = %v, want 8000（正则抽到的原文数字不许被模型覆盖）", doc.SalaryMonthly)
	}
}

// TestExtractUsesLayoutClausesAndSkipLocate：版面条款的 bbox 是并集，page 是起始块页。
func TestExtractUsesLayoutClauses(t *testing.T) {
	client := &recordingClient{FakeClient: llm.FakeClient{Reply: fullJSON}}
	blocks := []port.TextBlock{
		block("b1", 2, 72, 700, 520, 720, "第一条 合同期限"),
		block("b2", 2, 80, 680, 480, 700, "本合同为固定期限劳动合同。"),
	}

	doc, err := New(client).Extract(context.Background(), contractSample, blocks)
	if err != nil {
		t.Fatalf("抽取失败：%v", err)
	}
	if len(doc.Clauses) != 1 {
		t.Fatalf("条款数 = %d, want 1", len(doc.Clauses))
	}
	if doc.Clauses[0].Page != 2 {
		t.Fatalf("page = %d, want 2", doc.Clauses[0].Page)
	}
	want := [4]float64{72, 680, 520, 720}
	if doc.Clauses[0].BBox != want {
		t.Fatalf("bbox = %v, want %v", doc.Clauses[0].BBox, want)
	}
}

// TestExtractFallsBackToModelClauses：版面里没有条号（例如扫描件只给了整段文本）时，
// 条款只能来自模型，ClausesSource 要如实标成 model，review.go 才会去回填坐标。
func TestExtractFallsBackToModelClauses(t *testing.T) {
	client := &recordingClient{FakeClient: llm.FakeClient{Reply: fullJSON}}
	blocks := []port.TextBlock{
		block("b1", 1, 72, 700, 520, 720, "整段合同文本，没有条号。"),
	}

	doc, err := New(client).Extract(context.Background(), contractSample, blocks)
	if err != nil {
		t.Fatalf("抽取失败：%v", err)
	}
	if doc.ClausesSource != ClauseSourceModel {
		t.Fatalf("clause_source = %q, want model", doc.ClausesSource)
	}
	if client.calls == 0 {
		t.Fatal("版面切不出条款时必须退回模型")
	}
	if len(doc.Clauses) != 2 {
		t.Fatalf("条款数 = %d, want 2（来自模型 raw_clauses）", len(doc.Clauses))
	}
}
