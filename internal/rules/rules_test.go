package rules

import (
	"strings"
	"testing"

	"laborlaw/internal/port"
)

// boolPtr 让指针字段的测试用例写起来短一点。
func boolPtr(b bool) *bool { return &b }

// findRule 按 ID 取规则，取不到直接失败：规则 ID 是落库与前端 switch 的键，改名即破坏契约。
func findRule(t *testing.T, id string) Rule {
	t.Helper()
	for _, r := range All() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("规则 %s 不存在", id)
	return Rule{}
}

// mustHit 断言命中恰好 n 条。
func mustHit(t *testing.T, r Rule, c Contract, n int) []Fragment {
	t.Helper()
	frags := r.Check(c)
	if len(frags) != n {
		t.Fatalf("%s 命中 %d 条，期望 %d 条；输入=%+v", r.ID, len(frags), n, c)
	}
	return frags
}

func TestRuleRegistry(t *testing.T) {
	all := All()
	if len(all) != 8 {
		t.Fatalf("规则数 %d，期望 8", len(all))
	}
	seen := make(map[string]bool, len(all))
	for _, r := range all {
		if r.ID == "" || r.Title == "" || r.Check == nil {
			t.Fatalf("规则字段不完整：%+v", r)
		}
		if seen[r.ID] {
			t.Fatalf("规则 ID 重复：%s", r.ID)
		}
		seen[r.ID] = true
		if len(r.Articles) == 0 {
			t.Fatalf("%s 没有法条引用，报告里会出现没有依据的结论", r.ID)
		}
		if r.Severity != port.SeverityHigh && r.Severity != port.SeverityMedium && r.Severity != port.SeverityLow {
			t.Fatalf("%s 严重度非法：%s", r.ID, r.Severity)
		}
	}
	ids := RuleIDs()
	if len(ids) != 8 {
		t.Fatalf("RuleIDs 返回 %d 个，期望 8", len(ids))
	}
}

func TestProbationMatch(t *testing.T) {
	r := findRule(t, "R-PROBATION-MATCH")
	base := Contract{
		TermStart:       "2024-01-01",
		TermEnd:         "2027-01-01", // 36 个月，上限 6 个月
		ProbationMonths: 7,
		Clauses:         []Clause{{Section: "第二条", Page: 0, BBox: [4]float64{10, 20, 300, 40}, Text: "试用期为七个月。"}},
	}
	frags := mustHit(t, r, base, 1)
	if frags[0].Section != "第二条" || frags[0].BBox[2] != 300 {
		t.Fatalf("片段定位错误：%+v", frags[0])
	}
	if !strings.Contains(frags[0].Evidence, "上限为 6 个月") {
		t.Fatalf("证据没写清上限：%s", frags[0].Evidence)
	}
	if frags[0].Suggestion.Type != "rewrite" || frags[0].Suggestion.Reason == "" {
		t.Fatalf("建议不完整：%+v", frags[0].Suggestion)
	}

	// 负例：一个月试用期在 36 个月的合同里合法。
	ok := base
	ok.ProbationMonths = 1
	mustHit(t, r, ok, 0)

	// 负例：期限缺失时无法判断，不应凭空报高危。
	unknown := base
	unknown.TermStart = ""
	unknown.TermEnd = ""
	mustHit(t, r, unknown, 0)
}

// TestProbationMatchBoundaries 钉住第十九条的档位边界：三个月、一年、三年这三处
// 都是"以上"进高档，所以 12 个月上限是 2 个月、36 个月上限是 6 个月。
func TestProbationMatchBoundaries(t *testing.T) {
	r := findRule(t, "R-PROBATION-MATCH")
	cases := []struct {
		name   string
		start  string
		end    string
		months float64
		want   int
	}{
		{"2 个月合同不得约定试用期", "2024-01-01", "2024-03-01", 1, 1},
		{"11 个月合同一个月合法", "2024-01-01", "2024-12-01", 1, 0},
		{"11 个月合同两个月违规", "2024-01-01", "2024-12-01", 2, 1},
		{"12 个月合同两个月合法", "2024-01-01", "2025-01-01", 2, 0},
		{"35 个月合同两个月合法", "2024-01-01", "2026-12-01", 2, 0},
		{"36 个月合同六个月合法", "2024-01-01", "2027-01-01", 6, 0},
		{"两年合同上限两个月", "2024-01-01", "2026-01-01", 3, 1},
		{"五年合同上限六个月", "2024-01-01", "2029-01-01", 7, 1},
		{"中文日期写法同样识别", "2024年1月1日", "2029年1月1日", 7, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Contract{TermStart: tc.start, TermEnd: tc.end, ProbationMonths: tc.months}
			if got := len(r.Check(c)); got != tc.want {
				t.Fatalf("命中 %d 条，期望 %d 条", got, tc.want)
			}
		})
	}
}

func TestProbationSalary(t *testing.T) {
	r := findRule(t, "R-PROBATION-SALARY")
	bad := Contract{
		SalaryMonthly:   10000,
		SalaryProbation: 7000,
		Clauses:         []Clause{{Section: "第三条", Text: "试用期工资为每月 7000 元。"}},
	}
	frags := mustHit(t, r, bad, 1)
	if !strings.Contains(frags[0].Evidence, "8000") {
		t.Fatalf("证据应写出 80%% 下限 8000：%s", frags[0].Evidence)
	}
	mustHit(t, r, Contract{SalaryMonthly: 10000, SalaryProbation: 8000}, 0)
	mustHit(t, r, Contract{SalaryMonthly: 10000, SalaryProbation: 0}, 0)
}

func TestProbationOnce(t *testing.T) {
	r := findRule(t, "R-PROBATION-ONCE")

	cases := []struct {
		name    string
		clauses []Clause
		want    int
	}{
		{
			name:    "只约定一次",
			clauses: []Clause{{Section: "第二条", Text: "双方约定试用期为八个月。"}},
			want:    0,
		},
		{
			// 线上误报原型：两段都写"试用期"，但长度只有一个，"试用期内""试用期工资"
			// 只是对同一次试用期的说明，不是第二次约定。
			name: "提三次同一长度",
			clauses: []Clause{
				{Section: "第二条", Text: "双方约定试用期为八个月，自 2026 年 1 月 1 日起至 2026 年 8 月 31 日止。"},
				{Section: "第二条", Text: "试用期内，甲方有权根据乙方的工作表现决定是否继续履行本合同。"},
				{Section: "第三条", Text: "试用期工资为每月 5000 元。"},
			},
			want: 0,
		},
		{
			name: "两个不同长度",
			clauses: []Clause{
				{Section: "第二条", Text: "试用期为三个月。"},
				{Section: "第五条", Text: "再次入职时试用期为 6 个月。"},
			},
			want: 1,
		},
		{
			// "三个月" 与 "3 个月" 是同一个长度，不能因为写法不同就当成两次约定。
			name: "中文数字与阿拉伯数字同值",
			clauses: []Clause{
				{Section: "第二条", Text: "试用期为三个月。"},
				{Section: "第八条", Text: "试用期 3 个月届满前七日，甲方应书面告知考核结果。"},
			},
			want: 0,
		},
		{
			name: "只提不约定",
			clauses: []Clause{
				{Section: "第二条", Text: "试用期内甲方有权解除本合同。"},
				{Section: "第三条", Text: "试用期届满后转正。"},
			},
			want: 0,
		},
		{
			name:    "空条款",
			clauses: nil,
			want:    0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			frags := mustHit(t, r, Contract{ProbationMonths: 8, Clauses: tc.clauses}, tc.want)
			if tc.want == 0 {
				return
			}
			// 用户看不懂"第二条、第五条"，证据里只列不同长度，不列位置。
			if !strings.Contains(frags[0].Evidence, "3 个月") || !strings.Contains(frags[0].Evidence, "6 个月") {
				t.Fatalf("证据应列出不同长度：%s", frags[0].Evidence)
			}
			if strings.Contains(frags[0].Evidence, "第二条") {
				t.Fatalf("证据不应出现条款位置：%s", frags[0].Evidence)
			}
			if frags[0].Section != "第二条" {
				t.Fatalf("定位应取第一处约定，实际 %s", frags[0].Section)
			}
		})
	}
}

// TestProbationOnceOnRealContract 用线上暴露误报的真实合同原文做回归。
// 文本照抄自实际合同第二条，不要改写：改了就不再是回归用例。
func TestProbationOnceOnRealContract(t *testing.T) {
	r := findRule(t, "R-PROBATION-ONCE")
	c := Contract{
		TermStart:       "2026-01-01",
		TermEnd:         "2028-12-31",
		ProbationMonths: 8,
		Clauses: []Clause{
			{Section: "第二条", Page: 0, Text: "双方约定试用期为八个月，自 2026 年 1 月 1 日起至 2026 年 8 月 31 日止。\n试用期包含在合同期限内。"},
			{Section: "第二条", Page: 0, Text: "试用期内，甲方有权根据乙方的工作表现决定是否继续履行本合同；乙方在试用期内提前三日通知甲方，可以解除本合同。"},
		},
	}
	mustHit(t, r, c, 0)
}

func TestDeposit(t *testing.T) {
	r := findRule(t, "R-DEPOSIT")
	explicit := Contract{Deposit: boolPtr(true), Clauses: []Clause{{Section: "第四条", Text: "乙方入职时缴纳服装押金 500 元。"}}}
	if frags := mustHit(t, r, explicit, 1); frags[0].Suggestion.Type != "delete" {
		t.Fatalf("押金条款应建议删除，实际 %s", frags[0].Suggestion.Type)
	}
	// 负例只需要一个字段意义上的否定。
	mustHit(t, r, Contract{Deposit: boolPtr(false)}, 0)
	// 文本兜底：模型没抽到 deposit，但条款里写了扣押证件。
	fromText := Contract{Clauses: []Clause{{Section: "第五条", Text: "公司可扣押员工身份证原件。"}}}
	mustHit(t, r, fromText, 1)
}

func TestNoSocialInsurance(t *testing.T) {
	r := findRule(t, "R-NO-SOCIAL-INS")
	mustHit(t, r, Contract{SocialInsurance: boolPtr(false)}, 1)
	// nil 且全篇没有社保字样：按未约定处理。
	mustHit(t, r, Contract{Clauses: []Clause{{Section: "第一条", Text: "乙方同意自行承担保险费用。"}}}, 1)
	// nil 但条款提到了社保：视为已约定，避免抽取漏字段导致误报。
	mustHit(t, r, Contract{Clauses: []Clause{{Section: "第六条", Text: "甲方依法为乙方缴纳社会保险。"}}}, 0)
	mustHit(t, r, Contract{SocialInsurance: boolPtr(true)}, 0)
}

func TestWageUnderMin(t *testing.T) {
	r := findRule(t, "R-WAGE-UNDER-MIN")
	frags := mustHit(t, r, Contract{SalaryMonthly: 1500, Clauses: []Clause{{Section: "第三条", Text: "月工资 1500 元。"}}}, 1)
	if !strings.Contains(frags[0].Evidence, "1900") {
		t.Fatalf("证据应写出默认最低工资 1900：%s", frags[0].Evidence)
	}
	// 调用方注入标准时以注入值为准（深圳 2360）。
	mustHit(t, r, Contract{SalaryMonthly: 2000, MinMonthlyWage: 2360}, 1)
	mustHit(t, r, Contract{SalaryMonthly: 2000, MinMonthlyWage: 1900}, 0)
	mustHit(t, r, Contract{SalaryMonthly: 0}, 0)
}

func TestTermNoEnd(t *testing.T) {
	r := findRule(t, "R-TERM-NO-END")
	if frags := mustHit(t, r, Contract{}, 1); frags[0].Suggestion.Type != "add" {
		t.Fatalf("缺期限应建议补充条款，实际 %s", frags[0].Suggestion.Type)
	}
	mustHit(t, r, Contract{TermStart: "2024-01-01", TermEnd: "2026-01-01"}, 0)
	// 只写起始日、类型又是固定期限：同样是期限约定不完整。
	if frags := mustHit(t, r, Contract{TermStart: "2024-01-01", ContractType: "固定期限"}, 1); !strings.Contains(frags[0].Evidence, "2024-01-01") {
		t.Fatalf("证据应写出已约定的起始日：%s", frags[0].Evidence)
	}
	mustHit(t, r, Contract{ContractType: "无固定期限劳动合同"}, 0)
	mustHit(t, r, Contract{ContractType: "以完成一定工作任务为期限的劳动合同"}, 0)
}

func TestOvertimeFree(t *testing.T) {
	r := findRule(t, "R-OVERTIME-FREE")
	explicit := Contract{HasOvertimePay: boolPtr(false)}
	mustHit(t, r, explicit, 1)
	fromText := Contract{Clauses: []Clause{{Section: "第七条", Text: "乙方自愿加班，甲方不支付加班费。"}}}
	frags := mustHit(t, r, fromText, 1)
	if !strings.Contains(frags[0].Evidence, "自愿加班") {
		t.Fatalf("证据应引用原文：%s", frags[0].Evidence)
	}
	// 正常约定加班费不应命中。
	mustHit(t, r, Contract{Clauses: []Clause{{Section: "第七条", Text: "加班按国家规定支付加班费。"}}}, 0)
	mustHit(t, r, Contract{HasOvertimePay: boolPtr(true)}, 0)
}

func TestCheckOrderAndFilter(t *testing.T) {
	c := Contract{
		TermStart:       "2024-01-01",
		TermEnd:         "2024-03-01",
		ProbationMonths: 2,
		Deposit:         boolPtr(true),
		SalaryMonthly:   1500,
	}
	all := Check(c, All())
	// 至少命中：试用期超限、押金、最低工资、无社保、无期限之外的四类。
	if len(all) < 4 {
		t.Fatalf("全量规则命中 %d 条，期望至少 4 条", len(all))
	}
	if all[0].Evidence == "" {
		t.Fatalf("片段缺少证据")
	}

	only := Filter([]string{"R-DEPOSIT"})
	if len(only) != 1 || only[0].ID != "R-DEPOSIT" {
		t.Fatalf("Filter 结果错误：%+v", only)
	}
	if got := Filter(nil); len(got) != 8 {
		t.Fatalf("空过滤器应返回全部规则，实际 %d", len(got))
	}
	if got := Filter([]string{"R-NOT-EXIST"}); len(got) != 0 {
		t.Fatalf("未知规则名应被忽略，实际返回 %d 条", len(got))
	}

	frags := Check(c, Filter([]string{"R-DEPOSIT"}))
	if len(frags) != 1 {
		t.Fatalf("按规则过滤后应只跑一条，实际 %d", len(frags))
	}
	ref := frags[0].ClauseRef()
	if ref.Section != frags[0].Section || ref.Page != frags[0].Page || ref.BBox != frags[0].BBox {
		t.Fatalf("ClauseRef 与片段不一致：%+v vs %+v", ref, frags[0])
	}
}

func TestContractMonths(t *testing.T) {
	cases := []struct {
		start, end string
		want       int
		ok         bool
	}{
		{"2024-01-01", "2024-02-01", 1, true},
		{"2024-01-15", "2024-02-14", 1, true},
		{"2024-01-01", "2027-01-01", 36, true},
		{"2024-01-31", "2024-03-30", 1, true}, // 差一天不满两个月，按日回退一个月
		{"2024-01-31", "2024-03-31", 2, true},
		{"", "2024-03-01", 0, false},
		{"2024-01-01", "", 0, false},
		{"不是日期", "2024-03-01", 0, false},
	}
	for _, tc := range cases {
		got, ok := contractMonths(tc.start, tc.end)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Fatalf("contractMonths(%q,%q)=%d,%v 期望 %d,%v", tc.start, tc.end, got, ok, tc.want, tc.ok)
		}
	}
}
