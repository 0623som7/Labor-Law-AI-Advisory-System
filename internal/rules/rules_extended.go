package rules

import (
	"fmt"
	"regexp"
	"strings"

	"laborlaw/internal/port"
)

// 扩展规则与既有规则引用的专门法规。名称必须与 deploy/seed/statutes.json 的 law 列一致，
// 否则审查编排按 (法律名, 条号) 取不到原文。
const (
	LawWorkInjury        = "工伤保险条例"
	LawDispatch          = "劳务派遣暂行规定"
	LawAnnualLeave       = "职工带薪年休假条例"
	LawFemaleWorker      = "女职工劳动保护特别规定"
	LawLaborContractImpl = "中华人民共和国劳动合同法实施条例"
	LawLaborDispute      = "中华人民共和国劳动争议调解仲裁法"
)

// 扩展规则的匹配口径：只抓合同里“明确写入违法安排或免除责任”的表述。
//
// 文本规则天然有边界，宁可少报也不把“依法办理”“不得安排”这类保护性表述误判成风险。
var (
	penaltyRe       = regexp.MustCompile(`违约金`)
	servicePeriodRe = regexp.MustCompile(`服务期|竞业限制|保密`)

	invalidExemptionRes = []*regexp.Regexp{
		regexp.MustCompile(`工伤[^。；]{0,12}概不负责`),
		regexp.MustCompile(`(?:伤亡|人身损害)[^。；]{0,12}概不负责`),
		regexp.MustCompile(`(?:不承担|免除)[^。；]{0,10}(?:工伤|人身损害)[^。；]{0,6}(?:责任|赔偿)`),
		regexp.MustCompile(`人身[^。；]{0,8}(?:后果|损失)[^。；]{0,12}(?:自负|自行承担|乙方承担)`),
		regexp.MustCompile(`(?:放弃|不得行使)[^。；]{0,10}(?:仲裁|诉讼)[^。；]{0,6}权利`),
	}

	overtimeLowRateRes = []*regexp.Regexp{
		regexp.MustCompile(`(?:加班|延长工作时间)[^。；]{0,24}(?:^|[^0-9])(?:0\.5倍|50%|一倍|百分之五十|百分之百|100%)`),
		regexp.MustCompile(`(?:加班|延长工作时间)[^。；]{0,24}(?:工资|报酬)[^。；]{0,10}(?:0\.5倍|一倍|百分之五十|百分之百|100%)`),
	}

	dailyExcessWorkRe  = regexp.MustCompile(`每日.{0,12}\s*(?:9|[1-9]\d)\s*小时`)
	weeklyExcessWorkRe = regexp.MustCompile(`每周.{0,12}\s*(?:4[5-9]|[5-9]\d)\s*小时`)

	dailyOvertimeCapForwardRe = regexp.MustCompile(`(?:每日|每天)[^，。；]{0,16}超过\s*(?:[4-9]|[1-9]\d)\s*小时`)
	dailyOvertimeCapRe        = regexp.MustCompile(`(?:加班|延长工作时间)[^，。；]{0,16}(?:每日|每天)?[^，。；]{0,8}超过\s*(?:[4-9]|[1-9]\d)\s*小时`)
	monthlyOvertimeCapRe      = regexp.MustCompile(`每月[^，。；]{0,8}加班[^，。；]{0,10}超过\s*(?:3[7-9]|[4-9]\d|1\d{2})\s*小时`)

	parttimeDailyHoursRe  = regexp.MustCompile(`(?:每日|每天).{0,8}\s*(?:[5-9]|[1-9]\d)\s*小时`)
	parttimeWeeklyHoursRe = regexp.MustCompile(`每周.{0,10}(?:累计)?工作.{0,6}\s*(?:2[5-9]|[3-9]\d)\s*小时`)

	annualLeaveForfeitRes = []*regexp.Regexp{
		regexp.MustCompile(`(?:放弃|自动放弃)[^。；]{0,6}年假`),
		regexp.MustCompile(`年假[^。；]{0,10}(?:清零|作废|不予?补偿|不支付)`),
		regexp.MustCompile(`未休年假[^。；]{0,10}(?:不予?补偿|不支付|作废)`),
	}

	pregnantNightRe = regexp.MustCompile(`(?:怀孕|孕期)[^。；]{0,24}(?:仍?(?:安排|要求|继续)?[^。；]{0,10})?(?:夜班|延长工作时间|加班)`)
	femaleHazardRe  = regexp.MustCompile(`(?:怀孕|哺乳|孕期|哺乳期)[^。；]{0,20}(?:仍?(?:安排|要求|继续)?[^。；]{0,8})?(?:井下|高处|低温|冷水|高温|有毒|有害)`)

	dismissalCompWaiverRes = []*regexp.Regexp{
		regexp.MustCompile(`解除[^。；]{0,20}(?:不支付|不予支付|不给予)经济补偿`),
		regexp.MustCompile(`辞退[^。；]{0,15}不[^。；]{0,6}(?:补偿|赔偿)`),
		regexp.MustCompile(`(?:离职|终止)[^。；]{0,15}不[^。；]{0,6}(?:经济补偿|补偿金)`),
	}

	certificateRefusalRes = []*regexp.Regexp{
		regexp.MustCompile(`离职[^。；]{0,15}(?:不开具|不出具|拒绝开具|拒绝出具)[^。；]{0,10}(?:离职|终止|解除)[^。；]{0,4}证明`),
		regexp.MustCompile(`离职[^。；]{0,15}不[^。；]{0,8}(?:办理|转移)[^。；]{0,10}(?:档案|社保)`),
		regexp.MustCompile(`拒绝[^。；]{0,10}(?:办理|转移)[^。；]{0,10}(?:档案|社保)`),
	}
)

// AllExtended 返回新增的 12 条扩展规则。
//
// 单独导出是为了让测试逐条定位；All() 仍保持固定顺序，数据库里的 finding
// 顺序也因此可复现。
func AllExtended() []Rule {
	return []Rule{
		rulePenaltyUnlawful(),
		ruleInvalidExemption(),
		ruleOvertimeLowRate(),
		ruleWorkHoursExcess(),
		ruleOvertimeCap(),
		ruleProbationNonFullTime(),
		ruleParttimeHours(),
		ruleAnnualLeaveForfeit(),
		rulePregnantNightWork(),
		ruleFemaleHazardWork(),
		ruleDismissalCompensationWaiver(),
		ruleCertificateRefusal(),
	}
}

// rulePenaltyUnlawful 检查劳动者违约金是否超出服务期/竞业限制两个法定场景。
func rulePenaltyUnlawful() Rule {
	return Rule{
		ID:       "R-PENALTY-UNLAWFUL",
		Severity: port.SeverityHigh,
		Title:    "违法约定劳动者违约金",
		Articles: []ArticleRef{{Law: LawLaborContract, Article: "第二十五条"}},
		Check: func(c Contract) []Fragment {
			var frags []Fragment
			for _, cl := range c.Clauses {
				if !penaltyRe.MatchString(cl.Text) || servicePeriodRe.MatchString(cl.Text) {
					continue
				}
				frags = append(frags, newFragmentAt(cl, "合同约定："+strings.TrimSpace(cl.Text),
					port.Suggestion{
						Type:     "delete",
						Original: strings.TrimSpace(cl.Text),
						Revised:  "（删除本条违约金约定）",
						Reason:   "劳动合同法第二十五条规定，除服务期和竞业限制两种法定情形外，用人单位不得与劳动者约定由劳动者承担违约金。",
					}))
			}
			return frags
		},
	}
}

// ruleInvalidExemption 检查免除工伤/人身责任或排除仲裁、诉讼权利的无效条款。
func ruleInvalidExemption() Rule {
	return textRule(
		"R-INVALID-EXEMPTION",
		port.SeverityHigh,
		"免除法定责任或排除劳动者权利",
		[]ArticleRef{{Law: LawLaborContract, Article: "第二十六条"}},
		invalidExemptionRes,
		"免除工伤、人身损害责任或排除劳动者的争议解决权利",
		port.Suggestion{
			Type:    "delete",
			Revised: "（删除免除用人单位法定责任、排除劳动者权利的条款）",
			Reason:  "劳动合同法第二十六条规定，用人单位免除自己的法定责任、排除劳动者权利的条款无效。",
		},
	)
}

// ruleOvertimeLowRate 检查加班工资倍率是否低于法定标准。
func ruleOvertimeLowRate() Rule {
	return textRule(
		"R-OVERTIME-LOW-RATE",
		port.SeverityMedium,
		"加班工资标准低于法定倍率",
		[]ArticleRef{
			{Law: LawLaborContract, Article: "第三十一条"},
			{Law: LawLabor, Article: "第四十四条"},
		},
		overtimeLowRateRes,
		"合同将加班工资约定为低于 150%/200%/300% 法定标准的倍率",
		port.Suggestion{
			Type:    "rewrite",
			Revised: "乙方延长工作时间的，甲方按不低于工资的 150% 支付加班工资；休息日不能补休的按 200%，法定休假日按 300% 支付。",
			Reason:  "劳动法第四十四条规定了延长工作时间、休息日和法定节假日的最低加班工资标准。",
		},
	)
}

// ruleWorkHoursExcess 检查标准工时制度下每日或每周工作时长是否超过法定上限。
func ruleWorkHoursExcess() Rule {
	res := append([]*regexp.Regexp{}, dailyExcessWorkRe, weeklyExcessWorkRe)
	return textRule(
		"R-WORK-HOURS-EXCESS",
		port.SeverityMedium,
		"约定的工作时长超过法定工时",
		[]ArticleRef{{Law: LawLabor, Article: "第三十六条"}},
		res,
		"合同约定的每日或每周工作时间超过法定上限",
		port.Suggestion{
			Type:    "rewrite",
			Revised: "乙方每日工作时间不超过 8 小时，平均每周工作时间不超过 44 小时。",
			Reason:  "劳动法第三十六条规定，劳动者每日工作时间不超过 8 小时，平均每周工作时间不超过 44 小时。",
		},
	)
}

// ruleOvertimeCap 检查是否约定超过每日 3 小时或每月 36 小时的加班上限。
func ruleOvertimeCap() Rule {
	res := append([]*regexp.Regexp{}, dailyOvertimeCapForwardRe, dailyOvertimeCapRe, monthlyOvertimeCapRe)
	return textRule(
		"R-OVERTIME-CAP",
		port.SeverityMedium,
		"约定加班时长超过法定上限",
		[]ArticleRef{{Law: LawLabor, Article: "第四十一条"}},
		res,
		"合同约定的加班时长超过法定每日、每月上限",
		port.Suggestion{
			Type:    "rewrite",
			Revised: "确需延长工作时间的，一般每日不得超过 1 小时；特殊原因下每日不超过 3 小时，每月累计不超过 36 小时。",
			Reason:  "劳动法第四十一条规定了延长工作时间的协商条件和每日、每月上限。",
		},
	)
}

// ruleProbationNonFullTime 检查非全日制用工是否约定试用期。
func ruleProbationNonFullTime() Rule {
	return Rule{
		ID:       "R-PROBATION-NONFULLTIME",
		Severity: port.SeverityHigh,
		Title:    "非全日制用工约定试用期",
		Articles: []ArticleRef{{Law: LawLaborContract, Article: "第七十条"}},
		Check: func(c Contract) []Fragment {
			if !strings.Contains(c.ContractType, "非全日制") || c.ProbationMonths <= 0 {
				return nil
			}
			at := firstClause(c)
			if hits := clausesMentioning(c, "试用期"); len(hits) > 0 {
				at = hits[0]
			}
			return []Fragment{newFragmentAt(at,
				fmt.Sprintf("合同类型为非全日制用工，同时约定试用期 %.2f 个月", c.ProbationMonths),
				port.Suggestion{
					Type:     "delete",
					Original: clauseText(c, "试用期"),
					Revised:  "（删除试用期条款）",
					Reason:   "劳动合同法第七十条规定，非全日制用工双方当事人不得约定试用期。",
				})}
		},
	}
}

// ruleParttimeHours 检查非全日制用工是否超过平均每日 4 小时或每周 24 小时。
func ruleParttimeHours() Rule {
	return Rule{
		ID:       "R-PARTTIME-HOURS",
		Severity: port.SeverityMedium,
		Title:    "非全日制用工时长超过法定上限",
		Articles: []ArticleRef{{Law: LawLaborContract, Article: "第六十八条"}},
		Check: func(c Contract) []Fragment {
			if !strings.Contains(c.ContractType, "非全日制") {
				return nil
			}
			var hits []Clause
			for _, cl := range c.Clauses {
				if parttimeDailyHoursRe.MatchString(cl.Text) || parttimeWeeklyHoursRe.MatchString(cl.Text) {
					hits = append(hits, cl)
				}
			}
			if len(hits) == 0 &&
				(parttimeDailyHoursRe.MatchString(c.WorkingHours) || parttimeWeeklyHoursRe.MatchString(c.WorkingHours)) {
				hits = append(hits, firstClause(c))
			}
			if len(hits) == 0 {
				return nil
			}
			var frags []Fragment
			for _, cl := range hits {
				frags = append(frags, newFragmentAt(cl, "非全日制用工约定："+strings.TrimSpace(cl.Text),
					port.Suggestion{
						Type:     "rewrite",
						Original: strings.TrimSpace(cl.Text),
						Revised:  "乙方平均每日工作时间不超过 4 小时，每周工作时间累计不超过 24 小时。",
						Reason:   "劳动合同法第六十八条规定，非全日制用工平均每日工作时间不超过 4 小时，每周工作时间累计不超过 24 小时。",
					}))
			}
			return frags
		},
	}
}

// ruleAnnualLeaveForfeit 检查是否约定未休年假清零、不予补偿。
func ruleAnnualLeaveForfeit() Rule {
	return textRule(
		"R-ANNUAL-LEAVE-FORFEIT",
		port.SeverityMedium,
		"未休年假被约定清零或不予补偿",
		[]ArticleRef{
			{Law: LawLabor, Article: "第四十五条"},
			{Law: LawAnnualLeave, Article: "第五条"},
		},
		annualLeaveForfeitRes,
		"合同约定未休年假自动放弃、清零或不予补偿",
		port.Suggestion{
			Type:    "rewrite",
			Revised: "乙方应休未休的年休假天数，甲方按乙方日工资收入的 300% 支付年休假工资报酬。",
			Reason:  "职工带薪年休假条例第五条规定，经职工同意不安排年休假的，应按日工资收入的 300% 支付年休假工资报酬。",
		},
	)
}

// rulePregnantNightWork 检查是否约定怀孕女职工继续上夜班或加班。
func rulePregnantNightWork() Rule {
	return textRule(
		"R-PREGNANT-NIGHT-WORK",
		port.SeverityMedium,
		"怀孕女职工被安排夜班或加班",
		[]ArticleRef{{Law: LawFemaleWorker, Article: "第六条"}},
		[]*regexp.Regexp{pregnantNightRe},
		"合同约定或允许怀孕女职工继续从事夜班、加班或延长工作时间",
		port.Suggestion{
			Type:    "rewrite",
			Revised: "对怀孕 7 个月以上的女职工，甲方不得延长劳动时间或安排夜班劳动，并应在劳动时间内安排适当休息。",
			Reason:  "女职工劳动保护特别规定第六条规定，怀孕 7 个月以上的女职工不得被安排延长劳动时间或夜班劳动。",
		},
	)
}

// ruleFemaleHazardWork 检查孕期、哺乳期女职工是否被安排禁忌劳动。
func ruleFemaleHazardWork() Rule {
	return textRule(
		"R-FEMALE-HAZARD-WORK",
		port.SeverityMedium,
		"孕期或哺乳期女职工被安排禁忌劳动",
		[]ArticleRef{
			{Law: LawFemaleWorker, Article: "第四条"},
			{Law: LawFemaleWorker, Article: "第六条"},
		},
		[]*regexp.Regexp{femaleHazardRe},
		"合同约定或允许孕期、哺乳期女职工从事禁忌劳动",
		port.Suggestion{
			Type:    "rewrite",
			Revised: "甲方不得安排孕期、哺乳期女职工从事国家规定的禁忌劳动范围岗位。",
			Reason:  "女职工劳动保护特别规定第四条、第六条要求用人单位保护孕期、哺乳期女职工，不得安排禁忌劳动。",
		},
	)
}

// ruleDismissalCompensationWaiver 检查是否约定解除时不支付法定经济补偿。
func ruleDismissalCompensationWaiver() Rule {
	return textRule(
		"R-DISMISSAL-COMP-WAIVER",
		port.SeverityHigh,
		"约定解除合同时不支付法定补偿",
		[]ArticleRef{
			{Law: LawLaborContract, Article: "第四十六条"},
			{Law: LawLaborContract, Article: "第八十七条"},
		},
		dismissalCompWaiverRes,
		"合同约定解除或终止时不支付法定经济补偿",
		port.Suggestion{
			Type:    "delete",
			Revised: "（删除免除法定经济补偿的约定；符合法定情形的，甲方仍应依法支付补偿或赔偿。）",
			Reason:  "劳动合同法第四十六条、第八十七条规定了经济补偿和违法解除赔偿金的法定义务，约定免除该义务无效。",
		},
	)
}

// ruleCertificateRefusal 检查是否约定离职后拒开证明或拒转档案、社保。
func ruleCertificateRefusal() Rule {
	return textRule(
		"R-CERTIFICATE-REFUSAL",
		port.SeverityMedium,
		"约定离职后拒开证明或拒转档案",
		[]ArticleRef{{Law: LawLaborContract, Article: "第五十条"}},
		certificateRefusalRes,
		"合同约定离职后不出具证明、不办理档案或社保转移",
		port.Suggestion{
			Type:    "rewrite",
			Revised: "劳动合同解除或终止时，甲方出具解除或终止劳动合同证明，并在 15 日内为乙方办理档案和社会保险关系转移手续。",
			Reason:  "劳动合同法第五十条规定了离职证明和档案、社保转移的法定义务。",
		},
	)
}

// textRule 生成“正则命中原文 → 引用原文 → 给出法定写法”的通用扩展规则。
//
// 正则命中保护性表述时跳过，避免把“不得安排……”这类守法条款误报。
func textRule(id string, severity port.Severity, title string, refs []ArticleRef,
	patterns []*regexp.Regexp, evidence string, suggestion port.Suggestion) Rule {
	return Rule{
		ID:       id,
		Severity: severity,
		Title:    title,
		Articles: refs,
		Check: func(c Contract) []Fragment {
			var frags []Fragment
			for _, cl := range c.Clauses {
				if !matchesAny(patterns, cl.Text) || isProtectiveStatement(cl.Text) {
					continue
				}
				text := evidence
				original := strings.TrimSpace(cl.Text)
				if original != "" {
					text = "合同约定：" + original
					suggestion.Original = original
				}
				frags = append(frags, newFragmentAt(cl, text, suggestion))
			}
			return frags
		},
	}
}

// matchesAny 返回任一正则是否命中文本。
func matchesAny(patterns []*regexp.Regexp, text string) bool {
	for _, re := range patterns {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// isProtectiveStatement 排除“不得安排/应当安排/依法”等守法或法定例外表述。
//
// 这里是粗粒度的安全阀：宁可少报，也不把法律明确要求的写法判成风险。
func isProtectiveStatement(text string) bool {
	if strings.Contains(text, "不得安排") || strings.Contains(text, "应当安排") {
		return true
	}
	if strings.Contains(text, "依法") && (strings.Contains(text, "不支付") || strings.Contains(text, "不给予")) {
		return true
	}
	return false
}
