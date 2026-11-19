package extract

import (
	"context"
	"errors"
	"strings"
	"testing"

	"laborlaw/internal/port"
)

// truncatingClient 模拟上游把 JSON 切在半截：FinishReason=length，正文不合法。
type truncatingClient struct {
	reply string
}

func (c *truncatingClient) Chat(context.Context, port.ChatRequest) (port.ChatResponse, error) {
	return port.ChatResponse{Content: c.reply, FinishReason: "length"}, nil
}

func (c *truncatingClient) ChatStream(context.Context, port.ChatRequest, func(string) error) (port.ChatResponse, error) {
	return port.ChatResponse{Content: c.reply, FinishReason: "length"}, nil
}

// 线上真实形态：40 页合同输出 2000 token 到顶，被切成 `"social_insurance": tr`。
const truncatedJSON = `{
  "party_a": "东莞某某科技有限公司",
  "party_b": "张三",
  "contract_type": "固定期限",
  "salary_monthly": 8000,
  "social_insurance": tr`

func truncatedLayoutBlocks() []port.TextBlock {
	return []port.TextBlock{
		{ID: "b1", Page: 1, BBox: [4]float64{72, 700, 520, 720}, Text: "第一条 合同期限"},
		{ID: "b2", Page: 1, BBox: [4]float64{72, 680, 520, 700}, Text: "本合同为固定期限劳动合同。"},
		{ID: "b3", Page: 2, BBox: [4]float64{72, 640, 520, 660}, Text: "第二条 工作内容与地点"},
		{ID: "b4", Page: 2, BBox: [4]float64{72, 620, 520, 640}, Text: "乙方从事后端开发工作。"},
	}
}

// TestRepairTruncatedObject 守住截断修复的四种形态。
func TestRepairTruncatedObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "完整对象原样返回",
			in:   `{"a": 1}`,
			want: `{"a": 1}`,
			ok:   true,
		},
		{
			name: "截在字段值中间",
			in:   `{"a": 1, "b": "东`,
			want: `{"a": 1}`,
			ok:   true,
		},
		{
			name: "截在嵌套数组里",
			in:   `{"a": [{"x": 1}, {"x": 2}], "b": 3, "c": [{"y": "东`,
			want: `{"a": [{"x": 1}, {"x": 2}], "b": 3}`,
			ok:   true,
		},
		{
			name: "一个完整字段都没有",
			in:   `{"party_a": "东`,
			want: "",
			ok:   false,
		},
		{
			// 线上 40 页合同的形态：标量抽完了，正在写 clauses 数组。
			// 只按深度补 `}` 会得到 `[{"x": 1}}}`，方括号没闭合、JSON 非法。
			name: "截在对象数组里（线上 40 页合同的形态）",
			in:   `{"a": 1, "clauses": [{"x": 1}, {"y": "东`,
			want: `{"a": 1, "clauses": [{"x": 1}]}`,
			ok:   true,
		},
		{
			name: "括号不配对（多一个右方括号）",
			in:   `{"a": 1]`,
			want: "",
			ok:   false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := repairTruncatedObject(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("修复结果 = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseSurvivesTruncatedClauses 守住"截断点落在条款数组里"的形态。
//
// 这是线上 40 页合同真正的形态：标量全抽完了，正在写 raw_clauses，
// token 到顶被切在数组中间。只按深度补右括号会得到非法 JSON，
// 修复函数便对条款数组从未生效——这条用例把发令枪钉在这里。
func TestParseSurvivesTruncatedClauses(t *testing.T) {
	const truncated = `{
  "party_a": "东莞某某科技有限公司",
  "party_b": "张三",
  "salary_monthly": 8000,
  "raw_clauses": [{"section": "第一条", "text": "本合同为固定期限劳动合同"}, {"section": "第二条", "te`

	doc, err := Parse(truncated)
	if err != nil {
		t.Fatalf("截在条款数组里的正文应当被修复，实际报错：%v", err)
	}
	// 截断之前的标量一个都不许丢：丢了它们就会进 Missing，规则层当成"合同没写"。
	if doc.PartyA != "东莞某某科技有限公司" || doc.PartyB != "张三" || doc.SalaryMonthly != 8000 {
		t.Fatalf("已闭合标量丢失：%+v", doc.Contract)
	}
	for _, key := range []string{"party_a", "party_b", "salary_monthly"} {
		if hasField(doc.Missing, key) {
			t.Fatalf("%s 明明救回来了，不该进 Missing：%v", key, doc.Missing)
		}
	}
	// 数组里已闭合的那一条也要留下，残尾那一条丢掉。
	if len(doc.Clauses) != 1 || doc.Clauses[0].Section != "第一条" {
		t.Fatalf("条款数组应只保留已闭合的一条，实际：%+v", doc.Clauses)
	}
}

// TestParseSurvivesTruncatedOutput 守住"截断的正文仍能救回已闭合字段"。
//
// 缺陷形态（线上 40 页合同）：json.Unmarshal 报 unexpected end of JSON input，
// 整个 Parse 失败，14 个字段与条款一起丢，页面 0 张卡。
func TestParseSurvivesTruncatedOutput(t *testing.T) {
	doc, err := Parse(truncatedJSON)
	if err != nil {
		t.Fatalf("截断正文应当被修复，实际报错：%v", err)
	}
	if doc.PartyA != "东莞某某科技有限公司" || doc.SalaryMonthly != 8000 {
		t.Fatalf("已闭合字段丢失：%+v", doc.Contract)
	}
	if doc.SocialInsurance != nil {
		t.Fatalf("被截断的字段不应被当成有值：%v", *doc.SocialInsurance)
	}
	if !hasField(doc.Missing, "social_insurance") {
		t.Fatalf("截断字段应进 Missing：%v", doc.Missing)
	}
}

// TestExtractDegradesWhenModelTruncated 守住"版面有条款时截断不判整单失败"。
func TestExtractDegradesWhenModelTruncated(t *testing.T) {
	// 正文要有关键字段的凭据，否则短路逻辑根本不会调模型；回复又是"完全修不回来"
	// 的截断（连一个完整字段都没有），才会走到降级分支。
	ex := New(&truncatingClient{reply: `{"social_insurance": tr`})
	doc, err := ex.Extract(context.Background(),
		"第一条 合同期限\n合同终止日期：以实际离职日为准。", truncatedLayoutBlocks())
	if err != nil {
		t.Fatalf("版面已切出条款时不应失败：%v", err)
	}
	if doc.Source != SourceRule {
		t.Fatalf("Source = %q, want %q（降级到纯规则）", doc.Source, SourceRule)
	}
	if len(doc.Clauses) == 0 || doc.Clauses[0].Section != "第一条" {
		t.Fatalf("条款应来自版面：%+v", doc.Clauses)
	}
	if doc.PartyA != "" {
		t.Fatalf("降级后不应混入模型的部分结果：%q", doc.PartyA)
	}
	if !doc.ModelTruncated {
		t.Fatal("必须标记 ModelTruncated，前端才能提示结论不完整")
	}
}

// TestExtractClassifiesTruncationWithoutLayout 守住"没有版面可降级时给分类错误"。
//
// 用户看到的必须是"合同过长…"而不是"服务内部错误"，否则他不知道下一步做什么。
func TestExtractClassifiesTruncationWithoutLayout(t *testing.T) {
	// 正文里要有关键字段的凭据，否则短路逻辑会直接跳过模型调用，测不到分类错误。
	ex := New(&truncatingClient{reply: `{"social_insurance": tr`})
	_, err := ex.Extract(context.Background(), "合同终止日期：以实际离职日为准。\n社会保险由乙方自行缴纳。", nil)
	if err == nil {
		t.Fatal("没有版面兜底时必须返回 error")
	}
	var appErr *port.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("必须是分类错误，实际 %T：%v", err, err)
	}
	if appErr.Code != port.CodeInvalidArgument {
		t.Fatalf("错误码 = %v, want 40001（不可重试）", appErr.Code)
	}
	if !strings.Contains(appErr.Message, "合同过长") {
		t.Fatalf("Message = %q, 应提示合同过长", appErr.Message)
	}
}

// TestExtractKeepsUpstreamErrorOnLayout 守住"降级只对解析失败生效"。
//
// 上游超时/断网不能降级：那种情况版面还在，但用户拿到的会是"审查通过"，
// 而 worker 也该按码重试。
func TestExtractKeepsUpstreamErrorOnLayout(t *testing.T) {
	errClient := &truncatingErrClient{err: port.New(port.CodeUpstreamTimeout, "上游超时")}
	_, err := New(errClient).Extract(context.Background(),
		"第一条 合同期限\n合同终止日期：以实际离职日为准。\n社会保险由乙方自行缴纳。", truncatedLayoutBlocks())
	if err == nil {
		t.Fatal("上游超时必须上抛")
	}
	if code := port.CodeOf(err); code != port.CodeUpstreamTimeout {
		t.Fatalf("错误码 = %v, want 50401", code)
	}
}

type truncatingErrClient struct{ err error }

func (c *truncatingErrClient) Chat(context.Context, port.ChatRequest) (port.ChatResponse, error) {
	return port.ChatResponse{}, c.err
}

func (c *truncatingErrClient) ChatStream(context.Context, port.ChatRequest, func(string) error) (port.ChatResponse, error) {
	return port.ChatResponse{}, c.err
}

// hasField 判断字符串是否在切片里（本包已有 contains，换个名字避免重复声明）。
func hasField(items []string, want string) bool {
	for _, it := range items {
		if it == want {
			return true
		}
	}
	return false
}
