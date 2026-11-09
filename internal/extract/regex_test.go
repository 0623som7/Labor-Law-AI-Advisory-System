package extract

import (
	"strings"
	"testing"
)

// contractSample 是一份"该有的写法都有"的劳动合同正文，14 个字段全覆盖。
// 用的都是真实合同里的句式，不是给正则量身定做的字符串。
const contractSample = `劳动合同书
甲方（用人单位）：广州示例科技有限公司
法定代表人：张伟
乙方（劳动者）：李小雨
身份证号码：440106199804123456

第一条 合同期限
本合同为固定期限劳动合同。合同期限自2026年1月1日起至2029年1月1日止，共计三年。

第二条 试用期
双方约定试用期为八个月，自2026年1月1日起至2026年8月31日止。

第三条 工作内容与工作地点
乙方同意根据甲方工作需要，担任前端开发工程师岗位工作，工作地点为广州市天河区。

第四条 工作时间与休息休假
乙方执行标准工时制度，每日工作八小时，每周工作五天，每周至少休息一日。

第五条 劳动报酬
乙方试用期月工资为人民币5000元，试用期满转正后月工资为人民币8000元。

第六条 社会保险与福利待遇
乙方应当自行办理社会保险参保手续并缴纳社会保险费，甲方不再另行承担。

第九条 入职材料与押金
乙方应当在入职时向甲方交付身份证原件供甲方查验并留存复印件，同时交纳服装及工牌押金人民币500元。

（加班条款）甲方另行安排调休，不另计发加班工资。
`

func TestExtractByTextAllFields(t *testing.T) {
	c, missing := extractByText(contractSample)

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"party_a", c.PartyA, "广州示例科技有限公司"},
		{"party_b", c.PartyB, "李小雨"},
		{"contract_type", c.ContractType, "固定期限"},
		{"term_start", c.TermStart, "2026-01-01"},
		{"term_end", c.TermEnd, "2029-01-01"},
		{"work_location", c.WorkLocation, "广州市天河区"},
		{"position", c.Position, "前端开发工程师"},
		{"working_hours", c.WorkingHours, "标准工时"},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}

	if c.ProbationMonths != 8 {
		t.Errorf("probation_months = %v, want 8", c.ProbationMonths)
	}
	// 转正后 8000 是正式工资；先匹配"月工资"会把试用期的 5000 当成正式工资，
	// 直接反过来影响最低工资规则。
	if c.SalaryMonthly != 8000 {
		t.Errorf("salary_monthly = %v, want 8000", c.SalaryMonthly)
	}
	if c.SalaryProbation != 5000 {
		t.Errorf("salary_probation = %v, want 5000", c.SalaryProbation)
	}
	if c.SocialInsurance == nil || *c.SocialInsurance {
		t.Errorf("social_insurance = %v, want false", c.SocialInsurance)
	}
	if c.HasOvertimePay == nil || *c.HasOvertimePay {
		t.Errorf("has_overtime_pay = %v, want false", c.HasOvertimePay)
	}
	if c.Deposit == nil || !*c.Deposit {
		t.Errorf("deposit = %v, want true", c.Deposit)
	}

	if len(missing) != 0 {
		t.Errorf("这份样本应当 14 个字段全命中，missing = %v", missing)
	}
}

func TestExtractByTextNormalizesDates(t *testing.T) {
	cases := []struct{ text, wantStart, wantEnd string }{
		{"合同期限自2026年1月1日起至2029年1月1日止", "2026-01-01", "2029-01-01"},
		{"期限：2026-01-05 至 2028-12-31", "2026-01-05", "2028-12-31"},
		{"期限自 2026/3/7 起至 2027/3/6 止", "2026-03-07", "2027-03-06"},
	}
	for _, tc := range cases {
		gotStart, gotEnd := extractDates(normalize(tc.text))
		if gotStart != tc.wantStart || gotEnd != tc.wantEnd {
			t.Errorf("%q → %q~%q, want %q~%q", tc.text, gotStart, gotEnd, tc.wantStart, tc.wantEnd)
		}
	}
}

// 同一份合同里试用期起止与合同起止是两段日期，不能把试用期结束日当成合同终止日。
func TestExtractByTextIgnoresLaterDates(t *testing.T) {
	text := "合同期限自2026年1月1日起至2029年1月1日止。试用期自2026年1月1日起至2026年8月31日止。"
	start, end := extractDates(normalize(text))
	if start != "2026-01-01" || end != "2029-01-01" {
		t.Fatalf("start=%q end=%q, want 2026-01-01 ~ 2029-01-01", start, end)
	}
}

func TestExtractByTextReadsChineseMonths(t *testing.T) {
	cases := map[string]float64{
		"双方约定试用期为八个月。":   8,
		"双方约定试用期为十八个月。":  18,
		"双方约定试用期为 6 个月。": 6,
		"双方约定试用期为两个月。":   2,
		"双方约定试用期二十四个月。":  24,
	}
	for text, want := range cases {
		c, _ := extractByText(text)
		if c.ProbationMonths != want {
			t.Errorf("%q → probation=%v, want %v", text, c.ProbationMonths, want)
		}
	}
}

// "不另计发加班工资"里也含"计发加班工资"，否定式必须先判。
func TestExtractByTextPrefersNegativeOverPositive(t *testing.T) {
	c, _ := extractByText("甲方安排加班的，另行安排调休，不另计发加班工资。")
	if c.HasOvertimePay == nil || *c.HasOvertimePay {
		t.Fatalf("has_overtime_pay = %v, want false", c.HasOvertimePay)
	}

	c2, _ := extractByText("甲方安排加班的，按法定标准支付加班工资。")
	if c2.HasOvertimePay == nil || !*c2.HasOvertimePay {
		t.Fatalf("has_overtime_pay = %v, want true", c2.HasOvertimePay)
	}
}

// TestExtractByTextIgnoresProbationSalaryInFallback 兜底档不能把"试用期月工资"当成正式工资。
//
// 反例原文没有"转正"二字，兜底档会在"试用期月工资"处首次命中，
// 于是 salary_monthly 与 salary_probation 都是 5000，试用期工资比例那条规则静默漏报。
func TestExtractByTextIgnoresProbationSalaryInFallback(t *testing.T) {
	c, _ := extractByText("乙方试用期月工资为人民币 5000 元，试用期满后月工资为人民币 8000 元。")
	if c.SalaryMonthly != 8000 {
		t.Fatalf("salary_monthly = %v, want 8000", c.SalaryMonthly)
	}
	if c.SalaryProbation != 5000 {
		t.Fatalf("salary_probation = %v, want 5000", c.SalaryProbation)
	}
}

// TestExtractByTextSalaryFallbackWhenOnlyOneAmount 只有一个金额时兜底档仍然工作（守卫不能把正常情况一起挡掉）。
func TestExtractByTextSalaryFallbackWhenOnlyOneAmount(t *testing.T) {
	c, _ := extractByText("甲方每月支付乙方工资，月工资为人民币 8000 元。")
	if c.SalaryMonthly != 8000 {
		t.Fatalf("salary_monthly = %v, want 8000", c.SalaryMonthly)
	}
}

func TestExtractByTextReportsMissing(t *testing.T) {
	// 无固定期限合同没有终止日；这正是 R-TERM-NO-END 该豁免的写法。
	c, missing := extractByText("本合同为无固定期限劳动合同，自2026年1月1日起。")

	if c.TermEnd != "" {
		t.Fatalf("term_end = %q, want 空", c.TermEnd)
	}
	if !contains(missing, "term_end") {
		t.Fatalf("missing 里应有 term_end，实际 %v", missing)
	}
	if c.ContractType != "无固定期限" {
		t.Fatalf("contract_type = %q", c.ContractType)
	}
}

func TestExtractByTextSocialInsurancePositive(t *testing.T) {
	c, _ := extractByText("甲方依法为乙方缴纳社会保险。")
	if c.SocialInsurance == nil || !*c.SocialInsurance {
		t.Fatalf("social_insurance = %v, want true", c.SocialInsurance)
	}
	// 完全没提社保时是 nil：规则据此跳过，而不是当成"没缴"。
	empty, _ := extractByText("甲方与乙方签订本合同。")
	if empty.SocialInsurance != nil {
		t.Fatalf("没提社保时应当是 nil，实际 %v", *empty.SocialInsurance)
	}
}

func TestParseSmallNumber(t *testing.T) {
	cases := map[string]int{"8": 8, "八": 8, "两": 2, "十": 10, "十八": 18, "二十": 20, "二十四": 24}
	for in, want := range cases {
		got, ok := parseSmallNumber(in)
		if !ok || got != want {
			t.Errorf("parseSmallNumber(%q) = %d,%v, want %d", in, got, ok, want)
		}
	}
	if _, ok := parseSmallNumber("面议"); ok {
		t.Fatal("非数字应当返回 ok=false")
	}
}

func TestExtractByTextIsDeterministic(t *testing.T) {
	a, ma := extractByText(contractSample)
	b, mb := extractByText(contractSample)
	// 逐字段比：Contract 里带切片，结构体本身不可比较。
	if a.PartyA != b.PartyA || a.TermStart != b.TermStart || a.TermEnd != b.TermEnd ||
		a.ProbationMonths != b.ProbationMonths || a.SalaryMonthly != b.SalaryMonthly ||
		a.SalaryProbation != b.SalaryProbation || a.WorkLocation != b.WorkLocation ||
		a.Position != b.Position || a.WorkingHours != b.WorkingHours ||
		a.ContractType != b.ContractType {
		t.Fatal("同一份文本两次抽取结果必须完全一致")
	}
	if strings.Join(ma, ",") != strings.Join(mb, ",") {
		t.Fatal("missing 列表两次必须一致")
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
