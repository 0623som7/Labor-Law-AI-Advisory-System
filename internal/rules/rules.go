// Package rules 是合同审查的规则引擎。
//
// 设计约束（三条，都不许破）：
//  1. 纯函数：零 IO、零网络、零时间依赖。给定同一份 Contract，输出必须逐字相同，
//     否则同一条规则在 worker 重试时会产出不同 results，审查结果就不可复现。
//  2. 不认识模型：抽取结果由 internal/extract 负责，本包只消费结构体。
//     这样规则可以脱离 LLM 单测，每条规则的误报/漏报都能用固定样例钉死。
//  3. 不写法条原文：Fragment 只带 ArticleRef（法律名 + 条号），原文由编排层
//     从 statutes 表取出。规则里写死条文，一次修法就会让全库引用失真。
package rules

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"laborlaw/internal/port"
)

// 法律名称常量，与 statutes 表 law 列逐字一致（见 deploy/seed/statutes.json）。
const (
	LawLaborContract = "中华人民共和国劳动合同法"
	LawLabor         = "中华人民共和国劳动法"
	LawSocialInsure  = "中华人民共和国社会保险法"
)

// DefaultMinMonthlyWage 是最低工资兜底值（元/月）。
//
// 取东莞市现行标准 1900 元。真实场景应由 config 注入（各地差异大），
// 这里给默认值的用途只有一个：单测与本地演示。规则命中时会把这个数写进
// Evidence，用户看到的是一个具体数字，不是"低于当地最低工资"这种空话。
const DefaultMinMonthlyWage = 1900.0

// Clause 是一段带定位信息的合同原文。
//
// 字段与 extract 落库的 raw_clauses 一致；Section/Page/BBox 最终原样进
// Finding.clause_ref，前端靠它把风险标在 PDF 的对应位置上。
type Clause struct {
	Section string     `json:"section"`
	Page    int        `json:"page"`
	BBox    [4]float64 `json:"bbox"`
	Text    string     `json:"text"`
}

// Contract 是规则引擎的输入，也就是合同要素抽取的产物。
//
// 指针字段的语义是"抽到了/没抽到"：nil 表示模型没给出该要素，
// false 表示合同里明确写了否定约定。这两者不能合并——"没抽到社保"
// 与"约定不缴社保"在审查结论里是完全不同的两句话。
type Contract struct {
	PartyA          string   `json:"party_a"`
	PartyB          string   `json:"party_b"`
	ContractType    string   `json:"contract_type"`
	TermStart       string   `json:"term_start"`
	TermEnd         string   `json:"term_end"`
	ProbationMonths float64  `json:"probation_months"`
	SalaryMonthly   float64  `json:"salary_monthly"`
	SalaryProbation float64  `json:"salary_probation"`
	SocialInsurance *bool    `json:"social_insurance"`
	WorkLocation    string   `json:"work_location"`
	Position        string   `json:"position"`
	WorkingHours    string   `json:"working_hours"`
	HasOvertimePay  *bool    `json:"has_overtime_pay"`
	Deposit         *bool    `json:"deposit"`
	Clauses         []Clause `json:"raw_clauses"`

	// MinMonthlyWage 允许调用方覆盖最低工资标准；0 表示用 DefaultMinMonthlyWage。
	MinMonthlyWage float64 `json:"-"`
}

// ArticleRef 是一条法条引用，只含定位信息，不含正文。
type ArticleRef struct {
	Law     string `json:"law"`
	Article string `json:"article"`
}

// Fragment 是规则命中的片段。
//
// 与 port.Finding 的区别：Finding 需要 ID/ReviewID/LegalBasis 这些落库字段，
// 而 Fragment 只描述"哪里违反了哪条规则"。补全交给编排层，规则就不必知道
// 数据库长什么样，也不必为生成 ULID 引入随机性。
type Fragment struct {
	Section    string
	Page       int
	BBox       [4]float64
	Evidence   string
	Suggestion port.Suggestion
}

// ClauseRef 把定位信息转成接口层结构。
func (f Fragment) ClauseRef() port.ClauseRef {
	return port.ClauseRef{Section: f.Section, Page: f.Page, BBox: f.BBox}
}

// Rule 是一条规则的定义。
//
// Check 返回空切片表示未命中；命中多条时返回多条（例如两份不同条款都约定试用期）。
type Rule struct {
	ID       string
	Severity port.Severity
	Title    string
	Articles []ArticleRef
	Check    func(Contract) []Fragment
}

// All 返回全部规则的固定顺序副本。
//
// 顺序固定是为了让 findings 的落库顺序稳定：同一次审查重试两次，
// 数据库里的行序应当一致，前端 diff 才不会闪。
func All() []Rule {
	return []Rule{
		ruleProbationMatch(),
		ruleProbationSalary(),
		ruleProbationOnce(),
		ruleDeposit(),
		ruleNoSocialInsurance(),
		ruleWageUnderMin(),
		ruleTermNoEnd(),
		ruleOvertimeFree(),
	}
}

// RuleIDs 返回全部规则 ID，装配与文档用。
func RuleIDs() []string {
	all := All()
	ids := make([]string, 0, len(all))
	for _, r := range all {
		ids = append(ids, r.ID)
	}
	return ids
}

// Filter 按给定 ID 集合过滤规则；ids 为空表示全选。
//
// 未知 ID 被忽略而不是报错：前端传了旧版本规则名时，审查应当按已知规则继续，
// 而不是整单失败——规则集随时会加，报错会让旧前端直接不可用。
func Filter(ids []string) []Rule {
	if len(ids) == 0 {
		return All()
	}
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := make([]Rule, 0, len(want))
	for _, r := range All() {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out
}

// Check 对一份合同跑完给定规则，返回命中片段按规则顺序排列。
func Check(c Contract, rs []Rule) []Fragment {
	var out []Fragment
	for _, r := range rs {
		out = append(out, r.Check(c)...)
	}
	return out
}

// ---- 规则实现 ----

// ruleProbationMatch 校验试用期长度与合同期限是否匹配（劳动合同法第 19 条）。
func ruleProbationMatch() Rule {
	return Rule{
		ID:       "R-PROBATION-MATCH",
		Severity: port.SeverityHigh,
		Title:    "试用期长度超过法定上限",
		Articles: []ArticleRef{{Law: LawLaborContract, Article: "第十九条"}},
		Check: func(c Contract) []Fragment {
			limit, ok := probationLimit(c)
			if !ok || c.ProbationMonths <= float64(limit) {
				return nil
			}
			months, _ := contractMonths(c.TermStart, c.TermEnd)
			evidence := fmt.Sprintf("约定试用期 %s 个月，合同期限 %d 个月，法定上限为 %d 个月",
				formatMonths(c.ProbationMonths), months, limit)
			return []Fragment{newFragment(c, []string{"试用期"},
				evidence,
				port.Suggestion{
					Type:     "rewrite",
					Original: clauseText(c, "试用期"),
					Revised:  fmt.Sprintf("本合同试用期不超过 %d 个月。", limit),
					Reason:   "劳动合同法第十九条规定：合同期限三个月以上不满一年的，试用期不得超过一个月；一年以上不满三年的，不得超过二个月；三年以上固定期限和无固定期限的，不得超过六个月。三个月以下及以完成一定工作任务为期限的合同不得约定试用期。",
				})}
		},
	}
}

// ruleProbationSalary 校验试用期工资是否低于转正工资的 80%（第 20 条）。
func ruleProbationSalary() Rule {
	return Rule{
		ID:       "R-PROBATION-SALARY",
		Severity: port.SeverityHigh,
		Title:    "试用期工资低于转正工资的 80%",
		Articles: []ArticleRef{{Law: LawLaborContract, Article: "第二十条"}},
		Check: func(c Contract) []Fragment {
			if c.SalaryProbation <= 0 || c.SalaryMonthly <= 0 {
				return nil
			}
			floor := c.SalaryMonthly * 0.8
			if c.SalaryProbation >= floor {
				return nil
			}
			evidence := fmt.Sprintf("试用期工资 %s 元/月，低于转正工资 %s 元/月的 80%%（%s 元/月）",
				formatMoney(c.SalaryProbation), formatMoney(c.SalaryMonthly), formatMoney(floor))
			return []Fragment{newFragment(c, []string{"试用期"},
				evidence,
				port.Suggestion{
					Type:     "rewrite",
					Original: clauseText(c, "试用期", "工资", "薪资"),
					Revised:  fmt.Sprintf("试用期工资为每月 %s 元，不低于本单位相同岗位最低档工资或劳动合同约定工资的 80%%（%s 元）。", formatMoney(floor), formatMoney(floor)),
					Reason:   "劳动合同法第二十条规定，劳动者在试用期的工资不得低于本单位相同岗位最低档工资或者劳动合同约定工资的 80%，并不得低于用人单位所在地的最低工资标准。",
				})}
		},
	}
}

// ruleProbationOnce 校验同一岗位是否被多次约定试用期（第 19 条：同一用人单位与同一劳动者只能约定一次试用期）。
func ruleProbationOnce() Rule {
	return Rule{
		ID:       "R-PROBATION-ONCE",
		Severity: port.SeverityHigh,
		Title:    "同一份合同多次约定试用期",
		Articles: []ArticleRef{{Law: LawLaborContract, Article: "第十九条"}},
		Check: func(c Contract) []Fragment {
			agreements := clausesAgreeingProbation(c)
			// 去重口径：同一份合同里出现两个**不同长度**的试用期约定才算多次。
			// 例："试用期 3 个月" 与 "试用期 6 个月" → 命中；
			//     两段都写"试用期 8 个月"（一段是重申、一段限制权利）→ 不命中。
			seen := make(map[string]bool, len(agreements))
			lengths := make([]string, 0, len(agreements))
			for _, a := range agreements {
				if seen[a.months] {
					continue
				}
				seen[a.months] = true
				lengths = append(lengths, a.months)
			}
			if len(lengths) < 2 {
				return nil
			}
			evidence := fmt.Sprintf("合同中有 %d 处互不相同的试用期约定：%s 个月",
				len(lengths), strings.Join(lengths, " 个月、"))
			return []Fragment{newFragmentAt(agreements[0].clause,
				evidence,
				port.Suggestion{
					Type:     "rewrite",
					Original: strings.TrimSpace(agreements[0].clause.Text),
					Revised:  "本合同仅约定一次试用期，试用期自用工之日起计算。",
					Reason:   "劳动合同法第十九条规定，同一用人单位与同一劳动者只能约定一次试用期；续订劳动合同不得再次约定试用期。",
				})}
		},
	}
}

// ruleDeposit 校验是否收取押金、扣押证件（第 9 条）。
func ruleDeposit() Rule {
	return Rule{
		ID:       "R-DEPOSIT",
		Severity: port.SeverityHigh,
		Title:    "约定收取押金或扣押证件",
		Articles: []ArticleRef{{Law: LawLaborContract, Article: "第九条"}},
		Check: func(c Contract) []Fragment {
			explicit := c.Deposit != nil && *c.Deposit
			hits := clausesMentioning(c, "押金", "保证金", "扣押", "扣留身份证", "服装费", "培训费")
			if !explicit && len(hits) == 0 {
				return nil
			}
			evidence := "合同约定用人单位向劳动者收取押金/保证金或扣押证件"
			var at Clause
			original := ""
			if len(hits) > 0 {
				at = hits[0]
				original = strings.TrimSpace(at.Text)
				evidence = "合同约定：" + original
			} else {
				at = firstClause(c)
			}
			return []Fragment{newFragmentAt(at, evidence,
				port.Suggestion{
					Type:     "delete",
					Original: original,
					Revised:  "（删除押金/扣押证件条款）",
					Reason:   "劳动合同法第九条规定，用人单位招用劳动者，不得要求劳动者提供担保或者以其他名义向劳动者收取财物，不得扣押劳动者的居民身份证和其他证件。",
				})}
		},
	}
}

// ruleNoSocialInsurance 校验是否未约定社会保险缴纳（第 72 条 / 社会保险法）。
func ruleNoSocialInsurance() Rule {
	return Rule{
		ID:       "R-NO-SOCIAL-INS",
		Severity: port.SeverityHigh,
		Title:    "未约定缴纳社会保险",
		Articles: []ArticleRef{
			{Law: LawLaborContract, Article: "第十七条"},
			{Law: LawLabor, Article: "第七十二条"},
		},
		Check: func(c Contract) []Fragment {
			if c.SocialInsurance != nil && *c.SocialInsurance {
				return nil
			}
			// 模型没抽到该字段时，用条款原文兜底判断：只要有一处提到社保就不算"未约定"，
			// 否则每次抽取漏字段都会给用户一条高危误报，误报多了整份报告就没人看。
			if c.SocialInsurance == nil && len(clausesMentioning(c, "社会保险", "社保", "五险", "公积金")) > 0 {
				return nil
			}
			at := firstClause(c)
			return []Fragment{newFragmentAt(at,
				"合同中未见用人单位为劳动者缴纳社会保险的约定",
				port.Suggestion{
					Type:    "add",
					Revised: "甲方自用工之日起为乙方办理社会保险登记并按时足额缴纳基本养老保险、基本医疗保险、失业保险、工伤保险和生育保险。",
					Reason:  "劳动法第七十二条规定，用人单位和劳动者必须依法参加社会保险、缴纳社会保险费；劳动合同法第十七条把社会保险列为劳动合同必备条款。",
				})}
		},
	}
}

// ruleWageUnderMin 校验约定工资是否低于当地最低工资（第 48 条）。
func ruleWageUnderMin() Rule {
	return Rule{
		ID:       "R-WAGE-UNDER-MIN",
		Severity: port.SeverityMedium,
		Title:    "约定工资低于当地最低工资标准",
		Articles: []ArticleRef{{Law: LawLabor, Article: "第四十八条"}},
		Check: func(c Contract) []Fragment {
			if c.SalaryMonthly <= 0 {
				return nil
			}
			min := c.MinMonthlyWage
			if min <= 0 {
				min = DefaultMinMonthlyWage
			}
			if c.SalaryMonthly >= min {
				return nil
			}
			evidence := fmt.Sprintf("约定月工资 %s 元，低于最低工资标准 %s 元/月",
				formatMoney(c.SalaryMonthly), formatMoney(min))
			return []Fragment{newFragment(c, []string{"工资", "薪资", "劳动报酬"},
				evidence,
				port.Suggestion{
					Type:     "rewrite",
					Original: clauseText(c, "工资", "薪资", "劳动报酬"),
					Revised:  fmt.Sprintf("甲方每月支付乙方工资不低于 %s 元（不低于当地最低工资标准）。", formatMoney(min)),
					Reason:   "劳动法第四十八条规定国家实行最低工资保障制度，用人单位支付劳动者的工资不得低于当地最低工资标准。",
				})}
		},
	}
}

// ruleTermNoEnd 校验是否未约定合同期限（第 12 条）。
func ruleTermNoEnd() Rule {
	return Rule{
		ID:       "R-TERM-NO-END",
		Severity: port.SeverityMedium,
		Title:    "未约定劳动合同期限",
		Articles: []ArticleRef{{Law: LawLaborContract, Article: "第十二条"}},
		Check: func(c Contract) []Fragment {
			start := strings.TrimSpace(c.TermStart)
			end := strings.TrimSpace(c.TermEnd)
			// 无固定期限与以完成一定工作任务为期限的合同本来就没有终止日，不算缺失。
			if strings.Contains(c.ContractType, "无固定期限") || strings.Contains(c.ContractType, "以完成一定工作任务") {
				return nil
			}
			evidence := "合同未见起始日与终止日的约定"
			switch {
			case start == "" && end == "":
				// 完全没写期限，走默认证据文案。
			case start != "" && end == "":
				// 有起始日没有终止日：固定期限合同必须两者都有，
				// 只写一半在争议时会被认定为未约定期限（甚至被视为无固定期限）。
				evidence = "合同只约定了起始日（" + start + "），未约定终止日"
			case start == "" && end != "":
				evidence = "合同只约定了终止日（" + end + "），未约定起始日"
			default:
				return nil
			}
			at := firstClause(c)
			return []Fragment{newFragmentAt(at,
				evidence,
				port.Suggestion{
					Type:    "add",
					Revised: "本合同为固定期限劳动合同，期限自 ____ 年 __ 月 __ 日起至 ____ 年 __ 月 __ 日止。",
					Reason:  "劳动合同法第十二条规定，劳动合同分为固定期限、无固定期限和以完成一定工作任务为期限三种；期限是合同的必备条款之一。",
				})}
		},
	}
}

// ruleOvertimeFree 校验是否约定无偿加班或不支付加班费（第 44 条）。
func ruleOvertimeFree() Rule {
	return Rule{
		ID:       "R-OVERTIME-FREE",
		Severity: port.SeverityMedium,
		Title:    "约定无偿加班或不支付加班费",
		Articles: []ArticleRef{{Law: LawLabor, Article: "第四十四条"}},
		Check: func(c Contract) []Fragment {
			explicit := c.HasOvertimePay != nil && !*c.HasOvertimePay
			hits := clausesMentioning(c, "不支付加班费", "加班费不予支付", "无加班费", "不另计加班费",
				"不另行支付加班费", "无偿加班", "自愿加班", "不计算加班")
			if !explicit && len(hits) == 0 {
				return nil
			}
			var at Clause
			evidence := "合同未约定加班工资的支付方式"
			original := ""
			if len(hits) > 0 {
				at = hits[0]
				original = strings.TrimSpace(at.Text)
				evidence = "合同约定：" + original
			} else {
				at = firstClause(c)
			}
			return []Fragment{newFragmentAt(at, evidence,
				port.Suggestion{
					Type:     "rewrite",
					Original: original,
					Revised:  "乙方延长工作时间的，甲方按下列标准支付加班工资：工作日延长工作时间支付不低于工资的 150%；休息日安排工作又不能安排补休的支付不低于 200%；法定休假日安排工作的支付不低于 300%。",
					Reason:   "劳动法第四十四条规定了延长工作时间、休息日、法定休假日三种情形下加班工资的法定标准，约定不支付或低于法定标准的部分无效。",
				})}
		},
	}
}

// ---- 公共工具 ----

// probationLimit 返回该合同允许的试用期上限（月），ok=false 表示期限缺失无法判断。
func probationLimit(c Contract) (int, bool) {
	if c.ProbationMonths <= 0 {
		return 0, false
	}
	if months, ok := contractMonths(c.TermStart, c.TermEnd); ok {
		switch {
		// 档位边界按第十九条的原话："三个月以上不满一年"、"一年以上不满三年"、
		// "三年以上固定期限和无固定期限"。所以 12 个月属于"一年以上"（上限 2 个月），
		// 36 个月属于"三年以上"（上限 6 个月），两处边界都归高一档。
		case months < 3:
			return 0, true
		case months < 12:
			return 1, true
		case months < 36:
			return 2, true
		default:
			return 6, true
		}
	}
	// 只有起始日、没有终止日，通常是"无固定期限"写法；此时上限六个月。
	if strings.TrimSpace(c.TermStart) != "" && strings.TrimSpace(c.TermEnd) == "" {
		return 6, true
	}
	if strings.Contains(c.ContractType, "无固定期限") {
		return 6, true
	}
	return 0, false
}

// contractMonths 计算合同期限的整月数。
//
// 手写月份差而不是用 time.AddDate 逐月累加：累加在大区间上是 O(月数)，
// 且遇到 1 月 31 日这类日期会因为月份天数不同跳错。这里只做"年差×12+月差"，
// 再按日回退一个月，结果与劳动法解释里的"满 X 个月"口径一致。
func contractMonths(start, end string) (int, bool) {
	s, ok := parseDate(start)
	if !ok {
		return 0, false
	}
	e, ok := parseDate(end)
	if !ok {
		return 0, false
	}
	months := (e.Year()-s.Year())*12 + int(e.Month()) - int(s.Month())
	if e.Day() < s.Day() {
		months--
	}
	if months <= 0 {
		months = 1
	}
	return months, true
}

// dateLayouts 覆盖中文合同里常见的日期写法。
var dateLayouts = []string{
	"2006-01-02",
	"2006/01/02",
	"2006.01.02",
	"2006年1月2日",
	"2006年01月02日",
	"2006-01",
	"2006年1月",
}

// parseDate 解析合同日期，允许带时间与时区以外的前后缀空白。
func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// clausesMentioning 返回正文包含任一关键词的条款，按出现顺序。
func clausesMentioning(c Contract, keywords ...string) []Clause {
	var out []Clause
	for _, cl := range c.Clauses {
		for _, kw := range keywords {
			if strings.Contains(cl.Text, kw) {
				out = append(out, cl)
				break
			}
		}
	}
	return out
}

// clauseText 返回第一段命中的原文，未命中返回空串。
func clauseText(c Contract, keywords ...string) string {
	hits := clausesMentioning(c, keywords...)
	if len(hits) == 0 {
		return ""
	}
	return strings.TrimSpace(hits[0].Text)
}

// firstClause 在没有命中条款时给定位一个兜底：用第一段原文的页与坐标。
//
// 返回空 Clause 会让前端把标记画在 (0,0)，看起来像渲染 bug；
// 指向文档开头至少能让人点进去看到"报告在这里"。
func firstClause(c Contract) Clause {
	if len(c.Clauses) == 0 {
		return Clause{}
	}
	return c.Clauses[0]
}

// newFragment 按关键词找定位后构造片段。
func newFragment(c Contract, keywords []string, evidence string, sug port.Suggestion) Fragment {
	hits := clausesMentioning(c, keywords...)
	at := firstClause(c)
	if len(hits) > 0 {
		at = hits[0]
	}
	return newFragmentAt(at, evidence, sug)
}

// newFragmentAt 用指定条款的定位构造片段。
func newFragmentAt(at Clause, evidence string, sug port.Suggestion) Fragment {
	return Fragment{
		Section:    at.Section,
		Page:       at.Page,
		BBox:       at.BBox,
		Evidence:   evidence,
		Suggestion: sug,
	}
}

// probationAgreementRe 只匹配"明确约定了试用期长度"的写法，例如：
//
//	试用期为三个月 / 试用期：3 个月 / 试用期 八个月 / 试用期 1.5 个月
//
// 不匹配"试用期内""试用期工资""试用期届满""试用期自 X 年 X 月 X 日起"这类
// 只提到、不约定长度的表述——旧实现只要条款里出现"试用期"三个字就计一次，
// 真实合同里"试用期工资""试用期内……"几乎必然出现两处以上，导致条条误报。
var probationAgreementRe = regexp.MustCompile(`试用期(?:为|是|[:：])?\s*([一二三四五六七八九十两]+|\d+(?:\.\d+)?)\s*个?月`)

// probationAgreement 是一次"约定了试用期长度"的记录。
type probationAgreement struct {
	clause Clause
	months string // 规范化后的月数：八个月 → "8"，3.5 个月 → "3.5"
}

// clausesAgreeingProbation 返回明确约定了试用期长度的条款，按出现顺序。
//
// 每段只取第一个匹配：一段里把同一个长度写两次（"试用期 3 个月……试用期 3 个月满后"）
// 仍然是一次约定，不能算成两次。
func clausesAgreeingProbation(c Contract) []probationAgreement {
	var out []probationAgreement
	for _, cl := range c.Clauses {
		m := probationAgreementRe.FindStringSubmatch(cl.Text)
		if m == nil {
			continue
		}
		months, ok := normalizeProbationMonths(m[1])
		if !ok {
			continue
		}
		out = append(out, probationAgreement{clause: cl, months: months})
	}
	return out
}

// normalizeProbationMonths 把正则捕获到的月数统一成数字字符串，用于去重比较。
func normalizeProbationMonths(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	if v, err := strconv.ParseFloat(raw, 64); err == nil {
		return formatMonths(v), true
	}
	v, ok := chineseMonths(raw)
	if !ok {
		return "", false
	}
	return formatMonths(v), true
}

// chineseMonths 解析"八""两""十二""二十"这类中文月数，只覆盖 1–99 的整数。
//
// 不认识的写法（"捌""廿""壹拾贰"）一律返回 false，该条款按"未明确约定"处理。
// 取向是宁可漏报也不误报：认不出长度就当没约定，比把"试用期届满"当成一次约定安全。
func chineseMonths(s string) (float64, bool) {
	digits := map[rune]float64{'一': 1, '二': 2, '两': 2, '三': 3, '四': 4, '五': 5,
		'六': 6, '七': 7, '八': 8, '九': 9}
	rs := []rune(s)
	switch len(rs) {
	case 0:
		return 0, false
	case 1:
		if rs[0] == '十' {
			return 10, true
		}
		v, ok := digits[rs[0]]
		return v, ok
	}
	idx := strings.IndexRune(s, '十')
	if idx < 0 {
		return 0, false // "一二"这类连写不支持
	}
	tens := 1.0
	if left := s[:idx]; left != "" {
		lr := []rune(left)
		if len(lr) != 1 {
			return 0, false
		}
		v, ok := digits[lr[0]]
		if !ok {
			return 0, false
		}
		tens = v
	}
	ones := 0.0
	if right := s[idx+len("十"):]; right != "" {
		rr := []rune(right)
		if len(rr) != 1 {
			return 0, false
		}
		v, ok := digits[rr[0]]
		if !ok {
			return 0, false
		}
		ones = v
	}
	return tens*10 + ones, true
}

// formatMoney 去掉整数金额的小数尾巴：1980.00 显示成 1980，1980.50 保留两位。
func formatMoney(v float64) string {
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d", int64(v))
	}
	return fmt.Sprintf("%.2f", v)
}

// formatMonths 同理，试用期常写成 0.5 个月（半个月）。
func formatMonths(v float64) string {
	return formatMoney(v)
}
