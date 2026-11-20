package rules

import (
	"regexp"
	"strings"
	"testing"

	"laborlaw/internal/port"
)

func findExtendedRule(t *testing.T, id string) Rule {
	t.Helper()
	for _, r := range AllExtended() {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("扩展规则 %s 不存在", id)
	return Rule{}
}

func TestExtendedRuleRegistry(t *testing.T) {
	all := All()
	if len(all) != 21 {
		t.Fatalf("规则数 %d，期望 21", len(all))
	}
	seen := make(map[string]bool)
	for _, r := range all {
		if seen[r.ID] {
			t.Fatalf("规则 ID 重复：%s", r.ID)
		}
		seen[r.ID] = true
		if r.ID == "" || r.Title == "" || r.Check == nil || len(r.Articles) == 0 {
			t.Fatalf("规则字段不完整：%+v", r)
		}
	}
	if len(AllExtended()) != 13 {
		t.Fatalf("扩展规则数 %d，期望 13", len(AllExtended()))
	}
}

func TestPenaltyUnlawful(t *testing.T) {
	r := findExtendedRule(t, "R-PENALTY-UNLAWFUL")
	frags := mustHit(t, r, Contract{Clauses: []Clause{{
		Section: "第九条",
		Text:    "乙方提前离职应向甲方支付违约金 3 万元。",
	}}}, 1)
	if frags[0].Suggestion.Type != "delete" || !strings.Contains(frags[0].Evidence, "违约金") {
		t.Fatalf("命中或建议不正确：%+v", frags[0])
	}
	// 服务期与竞业限制是法律允许约定违约金的两个场景。
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "乙方违反服务期约定的，应支付违约金。"}}}, 0)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "乙方违反竞业限制约定的，应支付违约金。"}}}, 0)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "甲方违约的，应向乙方支付违约金。"}}}, 0)
}

func TestInvalidExemption(t *testing.T) {
	r := findExtendedRule(t, "R-INVALID-EXEMPTION")
	frags := mustHit(t, r, Contract{Clauses: []Clause{{
		Section: "第八条",
		Text:    "乙方在工作中发生工伤概不负责。",
	}}}, 1)
	if frags[0].Section != "第八条" || !strings.Contains(frags[0].Evidence, "工伤概不负责") {
		t.Fatalf("证据或定位错误：%+v", frags[0])
	}
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "双方依法享有仲裁和诉讼权利。"}}}, 0)
}

func TestOvertimeLowRate(t *testing.T) {
	r := findExtendedRule(t, "R-OVERTIME-LOW-RATE")
	frags := mustHit(t, r, Contract{Clauses: []Clause{{Text: "工作日加班工资按工资的 100% 支付。"}}}, 1)
	if !strings.Contains(frags[0].Evidence, "100%") {
		t.Fatalf("证据应包含违法倍率：%s", frags[0].Evidence)
	}
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "工作日加班支付不低于工资的 150%。"}}}, 0)
}

func TestWorkHoursExcess(t *testing.T) {
	r := findExtendedRule(t, "R-WORK-HOURS-EXCESS")
	t.Logf("daily regex: %q", dailyExcessWorkRe.String())
	t.Logf("daily match: %v", dailyExcessWorkRe.MatchString("每日工作时间为 9 小时。"))
	t.Logf("simple match: %v", regexp.MustCompile(`每日.*9.*小时`).MatchString("每日工作时间为 9 小时。"))
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "每日工作时间为 9 小时。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "每周累计工作 45 小时。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "每日工作时间为 8 小时，每周不超过 44 小时。"}}}, 0)
}

func TestOvertimeCap(t *testing.T) {
	r := findExtendedRule(t, "R-OVERTIME-CAP")
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "业务高峰期每日加班不得超过 4 小时。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "每月加班累计不超过 37 小时。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "每日加班不得超过 3 小时，每月不超过 36 小时。"}}}, 0)
}

func TestProbationNonFullTime(t *testing.T) {
	r := findExtendedRule(t, "R-PROBATION-NONFULLTIME")
	c := Contract{
		ContractType:    "非全日制用工",
		ProbationMonths: 1,
		Clauses:         []Clause{{Section: "第二条", Text: "试用期一个月。"}},
	}
	frags := mustHit(t, r, c, 1)
	if frags[0].Section != "第二条" {
		t.Fatalf("应定位到试用期条款：%+v", frags[0])
	}
	mustHit(t, r, Contract{ContractType: "固定期限劳动合同", ProbationMonths: 1}, 0)
	mustHit(t, r, Contract{ContractType: "非全日制用工", ProbationMonths: 0}, 0)
}

func TestParttimeHours(t *testing.T) {
	r := findExtendedRule(t, "R-PARTTIME-HOURS")
	c := Contract{ContractType: "非全日制用工", Clauses: []Clause{{Text: "乙方每日工作 5 小时。"}}}
	mustHit(t, r, c, 1)
	c = Contract{ContractType: "非全日制用工", Clauses: []Clause{{Text: "每周累计工作 25 小时。"}}}
	mustHit(t, r, c, 1)
	mustHit(t, r, Contract{ContractType: "固定期限劳动合同", Clauses: []Clause{{Text: "每日工作 8 小时。"}}}, 0)
	mustHit(t, r, Contract{ContractType: "非全日制用工", Clauses: []Clause{{Text: "每日工作 4 小时。"}}}, 0)
}

func TestAnnualLeaveForfeit(t *testing.T) {
	r := findExtendedRule(t, "R-ANNUAL-LEAVE-FORFEIT")
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "年假到期未休的自动清零，不予补偿。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "未休年假按日工资的 300% 支付补偿。"}}}, 0)
}

func TestPregnantNightWork(t *testing.T) {
	r := findExtendedRule(t, "R-PREGNANT-NIGHT-WORK")
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "怀孕七个月以上的女职工仍安排夜班。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "怀孕七个月以上不得安排夜班。"}}}, 0)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "怀孕期间不得延长工作时间。"}}}, 0)
}

func TestFemaleHazardWork(t *testing.T) {
	r := findExtendedRule(t, "R-FEMALE-HAZARD-WORK")
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "哺乳期女职工继续从事有毒有害作业。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "不得安排怀孕女职工从事有毒有害作业。"}}}, 0)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "哺乳期女职工不得从事井下作业。"}}}, 0)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "孕期女职工禁止从事高温作业。"}}}, 0)
}

func TestDismissalCompensationWaiver(t *testing.T) {
	r := findExtendedRule(t, "R-DISMISSAL-COMP-WAIVER")
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "解除劳动合同的，甲方不支付经济补偿。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "依法不支付经济补偿的，按法律规定办理。"}}}, 0)
}

func TestCertificateRefusal(t *testing.T) {
	r := findExtendedRule(t, "R-CERTIFICATE-REFUSAL")
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "离职后甲方不出具离职证明。"}}}, 1)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "离职后甲方在十五日内办理档案和社保转移。"}}}, 0)
}

func TestTextRuleProtectiveStatements(t *testing.T) {
	rule := textRule("R-TEST", port.SeverityLow, "测试", []ArticleRef{},
		[]*regexp.Regexp{regexp.MustCompile("加班")}, "测试命中",
		port.Suggestion{Type: "rewrite"})
	mustHit(t, rule, Contract{Clauses: []Clause{{Text: "不得安排加班。"}}}, 0)
}

// 演示合同第十二条：乙方与「应当」之间隔 19 个字（「在合同期内未经甲方同意擅自离职的，」），
// 旧窗口 {0,6} 抓不到，整条漏报。窗口放宽到 {0,30} 后必须命中。
func TestPenaltyUnlawfulCatchesResignWithoutNotice(t *testing.T) {
	r := findExtendedRule(t, "R-PENALTY-UNLAWFUL")
	frags := mustHit(t, r, Contract{Clauses: []Clause{{
		Section: "第十二条",
		Text:    "乙方在合同期内未经甲方同意擅自离职的，应当向甲方支付违约金人民币 20000 元。",
	}}}, 1)
	if frags[0].Section != "第十二条" {
		t.Fatalf("命中条款 = %q，期望第十二条", frags[0].Section)
	}
	if !strings.Contains(frags[0].Evidence, "违约金人民币 20000 元") {
		t.Fatalf("evidence 未指向该条原文：%q", frags[0].Evidence)
	}
}

// 窗口从 6 放宽到 30，必须配负用例：超出窗口、甲方违约金、法定可约定情形都不能命中。
func TestPenaltyUnlawfulWindowStaysBounded(t *testing.T) {
	r := findExtendedRule(t, "R-PENALTY-UNLAWFUL")
	// 中间 40 个字，仍应在窗口之外。
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "乙方在本合同存续期间未经甲方书面许可且未完成任何工作交接手续擅自离开岗位的，应当向甲方支付违约金。"}}}, 0)
	// 甲方违约金不归这条管。
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "甲方在合同期内未经乙方同意擅自调岗的，应当向乙方支付违约金人民币 20000 元。"}}}, 0)
	// 服务期、竞业限制是法定可约定违约金的情形。
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "乙方违反服务期约定的，应当向甲方支付违约金。"}}}, 0)
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "乙方违反竞业限制约定的，应当向甲方支付违约金。"}}}, 0)
}

// 演示合同第十五条：约定了竞业限制和 5 万违约金，但一个字没提经济补偿。
func TestCompeteWithoutPayReported(t *testing.T) {
	r := findExtendedRule(t, "R-COMPETE-NO-PAY")
	frags := mustHit(t, r, Contract{Clauses: []Clause{{
		Section: "第十五条",
		Text:    "乙方在任职期间及离职后两年内，不得在与甲方有竞争性的单位任职，亦不得自营与甲方同类的业务。乙方违反前述约定的，应当向甲方支付违约金人民币 50000 元。",
	}}}, 1)
	if frags[0].Section != "第十五条" {
		t.Fatalf("命中条款 = %q，期望第十五条", frags[0].Section)
	}
	if !strings.Contains(frags[0].Suggestion.Revised, "按月向乙方支付经济补偿") {
		t.Fatalf("修改建议未补充经济补偿：%q", frags[0].Suggestion.Revised)
	}
}

// 约定了补偿就合法，不报。
func TestCompeteWithPayNotReported(t *testing.T) {
	r := findExtendedRule(t, "R-COMPETE-NO-PAY")
	mustHit(t, r, Contract{Clauses: []Clause{{
		Text: "乙方在任职期间及离职后两年内，不得在与甲方有竞争性的单位任职。乙方违反前述约定的，应当向甲方支付违约金人民币 50000 元。甲方按月支付经济补偿 3000 元。",
	}}}, 0)
}

// 只提竞业限制、没写违约金：不进这条规则，避免把合规条款当风险。
func TestCompeteMentionWithoutPenalty(t *testing.T) {
	r := findExtendedRule(t, "R-COMPETE-NO-PAY")
	mustHit(t, r, Contract{Clauses: []Clause{{Text: "双方约定竞业限制，期限两年。"}}}, 0)
}

// 竞业条款常写成「不得在与甲方有竞争性的单位任职」，不含「竞业限制」四个字，
// servicePeriodRe 挡不住。这类条款的违约金是第二十五条允许的场景，
// 缺补偿的问题归 R-COMPETE-NO-PAY，不能被 R-PENALTY-UNLAWFUL 再报一次（同一个框报两条）。
func TestPenaltyUnlawfulLeavesCompeteClauseToCompeteRule(t *testing.T) {
	penalty := findExtendedRule(t, "R-PENALTY-UNLAWFUL")
	c := Contract{Clauses: []Clause{{
		Section: "第十五条",
		Text:    "乙方在任职期间及离职后两年内，不得在与甲方有竞争性的单位任职，亦不得自营与甲方同类的业务。乙方违反前述约定的，应当向甲方支付违约金人民币 50000 元。",
	}}}
	mustHit(t, penalty, c, 0)
	compete := findExtendedRule(t, "R-COMPETE-NO-PAY")
	mustHit(t, compete, c, 1)
}
