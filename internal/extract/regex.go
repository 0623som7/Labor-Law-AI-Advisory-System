package extract

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"laborlaw/internal/rules"
)

// 合同要素是"格式文本"：甲方乙方、起止日期、金额、试用期都有固定写法。
// 这些正则就是把那些写法钉下来，取原文里的数字，不猜。
//
// 只在包级编译一次：每次抽取重新 MustCompile 十几次，会把"省下的模型调用"
// 换成另一笔 CPU 开销，而这十四个模式在整份合同上要各扫一遍。
var (
	// 甲方后面通常跟一个括号说明（"（用人单位）"），中间可能还有全角空格。
	rePartyA = regexp.MustCompile(`甲[ \t]*方[ \t]*(?:（[^）]{0,10}）|\([^)]{0,10}\))?[ \t]*[：:][ \t]*([^\n，,。；;]{2,40})`)
	rePartyB = regexp.MustCompile(`乙[ \t]*方[ \t]*(?:（[^）]{0,10}）|\([^)]{0,10}\))?[ \t]*[：:][ \t]*([^\n，,。；;]{2,20})`)

	// 日期统一成 2026年1月1日 / 2026-01-01 / 2026/1/1 三种写法，日尾的"日"可选。
	reDate = regexp.MustCompile(`(\d{4})\s*[年\-/.]\s*(\d{1,2})\s*[月\-/.]\s*(\d{1,2})\s*日?`)

	// 试用期月数。"个?月"要容忍"试用期八个月"与"试用期 6 月"两种写法。
	reProbation = regexp.MustCompile(`试用期[^\n。；;]{0,24}?([一二三四五六七八九十百两\d]+)[ \t]*个?月`)

	// 月薪按优先级依次尝试：不这么写的话，"试用期月工资 5000 元，转正后 8000 元"
	// 会先命中"月工资"把 5000 当成正式工资，直接反过来影响最低工资规则的判定。
	reSalaryMonthly = []*regexp.Regexp{
		regexp.MustCompile(`转正后[^\n。；;]{0,20}?(\d{3,7}(?:\.\d+)?)\s*元`),
		regexp.MustCompile(`正式[^\n。；;]{0,20}?(\d{3,7}(?:\.\d+)?)\s*元`),
		regexp.MustCompile(`(?:月工资|工资标准|月薪)[^\n。；;]{0,20}?(\d{3,7}(?:\.\d+)?)\s*元`),
	}
	reSalaryProbation = regexp.MustCompile(`试用期[^\n。；;]{0,20}?(?:工资|报酬)[^\n。；;]{0,10}?(\d{3,7}(?:\.\d+)?)\s*元`)

	// 社保：先找"乙方自己交/放弃"，再找"甲方依法缴"。顺序不能反——
	// "乙方自行缴纳，甲方不再承担"里同时含有甲方的表述。
	reSocialNegative = []*regexp.Regexp{
		regexp.MustCompile(`(?:乙方|劳动者)[^。；;\n]{0,15}(?:自行|自己|个人)[^。；;\n]{0,10}(?:缴纳|承担|办理)`),
		regexp.MustCompile(`(?:放弃|不缴|不缴纳|不予缴纳)[^。；;\n]{0,12}社会保险`),
	}
	reSocialPositive = regexp.MustCompile(`(?:甲方|用人单位)[^。；;\n]{0,20}(?:缴纳|办理|代缴)[^。；;\n]{0,10}社会保险`)

	// 加班费：否定式必须先于肯定式判断——"不另计发加班工资"里也含"计发加班工资"。
	// 动词列到"计发/计付"是因为合同更爱写"另行安排调休，不另计发加班工资"。
	reOvertimeNegative = regexp.MustCompile(`(?:不另行|不予|不再|不另|不)(?:支付|发放|计发|计付|安排)[^。；;\n]{0,12}加班`)
	reOvertimePositive = regexp.MustCompile(`(?:支付|发放|计发|计付)[^。；;\n]{0,10}加班(?:工资|费)`)

	// 押金/担保物权。命中"元"或证件类物品才算实际收取，只出现"押金"两个字不算。
	reDeposit = regexp.MustCompile(`(?:押金|保证金|抵押|扣押)[^。；;\n]{0,15}(?:元|身份证|证件|毕业证)`)

	reWorkLocation = regexp.MustCompile(`(?:工作地点|工作地址|办公地点)[ \t]*[：:为]?[ \t]*([^\n。；;]{2,30})`)
	// "担任前端开发工程师岗位工作"这种写法里，职务在"岗位"之前，先试它。
	rePositionRole = regexp.MustCompile(`担任([^\n，,。；;]{2,20}?)(?:岗位|职位|职务|工作)`)
	rePosition     = regexp.MustCompile(`(?:岗位|职位|职务)[ \t]*[：:为]?[ \t]*([^\n。；;]{2,20})`)

	reWorkingHours = []*regexp.Regexp{
		regexp.MustCompile(`标准工时`),
		regexp.MustCompile(`综合计算工时`),
		regexp.MustCompile(`不定时工作制`),
		regexp.MustCompile(`每天[^\n。；;]{0,8}小时`),
	}

	reSpaces = regexp.MustCompile(`[ \t\x{00a0}\x{3000}]+`)
)

// normalize 把全角空格换成半角、把连续空白压成一个空格，保留换行。
//
// 合同里"第 二 条""甲方 ："这种字间空格很常见（模板排版留下的），
// 不做这一步则所有带冒号的锚点都匹配不上。
func normalize(s string) string {
	return reSpaces.ReplaceAllString(strings.ReplaceAll(s, "\r\n", "\n"), " ")
}

// extractByText 用固定格式的正则从合同全文中抽取 14 个标量要素。
//
// 为什么要正则打头：合同要素是"格式文本"，甲方乙方、起止日期、金额都有固定写法，
// 而模型会把"8000 元"读成 80000、把"2026年1月1日"归一错月份。正则取原文数字，
// 不猜；真正需要理解语义的只剩"这是不是一份劳动合同"这种整体判断。
//
// 返回的第二个值是缺失字段（JSON 名），与模型路径的 missingFields 同一口径。
func extractByText(text string) (rules.Contract, []string) {
	var c rules.Contract
	body := normalize(text)

	if m := rePartyA.FindStringSubmatch(body); m != nil {
		c.PartyA = cleanName(m[1])
	}
	if m := rePartyB.FindStringSubmatch(body); m != nil {
		c.PartyB = cleanName(m[1])
	}

	switch {
	case strings.Contains(body, "无固定期限"):
		c.ContractType = "无固定期限"
	case strings.Contains(body, "以完成一定工作任务"):
		c.ContractType = "以完成一定工作任务为期限"
	case strings.Contains(body, "固定期限"):
		c.ContractType = "固定期限"
	}

	c.TermStart, c.TermEnd = extractDates(body)

	if m := reProbation.FindStringSubmatch(body); m != nil {
		if n, ok := parseSmallNumber(m[1]); ok {
			c.ProbationMonths = float64(n)
		}
	}

	for _, re := range reSalaryMonthly {
		if m := re.FindStringSubmatch(body); m != nil {
			c.SalaryMonthly = parseAmount(m[1])
			break
		}
	}
	if m := reSalaryProbation.FindStringSubmatch(body); m != nil {
		c.SalaryProbation = parseAmount(m[1])
	}

	switch {
	case matchAny(body, reSocialNegative):
		c.SocialInsurance = boolPtr(false)
	case reSocialPositive.MatchString(body):
		c.SocialInsurance = boolPtr(true)
	}

	if m := reWorkLocation.FindStringSubmatch(body); m != nil {
		c.WorkLocation = cleanName(m[1])
	}
	switch {
	case rePositionRole.MatchString(body):
		c.Position = cleanName(rePositionRole.FindStringSubmatch(body)[1])
	case rePosition.MatchString(body):
		c.Position = cleanName(rePosition.FindStringSubmatch(body)[1])
	}

	for _, re := range reWorkingHours {
		if loc := re.FindString(body); loc != "" {
			c.WorkingHours = loc
			break
		}
	}

	switch {
	case reOvertimeNegative.MatchString(body):
		c.HasOvertimePay = boolPtr(false)
	case reOvertimePositive.MatchString(body):
		c.HasOvertimePay = boolPtr(true)
	}

	if reDeposit.MatchString(body) {
		c.Deposit = boolPtr(true)
	}

	doc := &Document{Contract: c}
	return c, missingFields(doc)
}

// extractDates 按出现顺序取"第一个起始日 + 第一个终止日"。
//
// 只锚定第一次出现：合同里同一份日期会在多条里复述（试用期起止、合同起止），
// 全取会把试用期的结束日当成合同终止日。
func extractDates(text string) (start, end string) {
	for _, loc := range reDate.FindAllStringSubmatchIndex(text, -1) {
		y := text[loc[2]:loc[3]]
		mo := text[loc[4]:loc[5]]
		d := text[loc[6]:loc[7]]

		mi, err1 := strconv.Atoi(mo)
		di, err2 := strconv.Atoi(d)
		if err1 != nil || err2 != nil || mi < 1 || mi > 12 || di < 1 || di > 31 {
			continue
		}
		value := fmt.Sprintf("%s-%02d-%02d", y, mi, di)

		prefix := text[clip(loc[0]-8, 0, loc[0]):loc[0]]
		wantsEnd := strings.ContainsAny(prefix, "至到")
		wantsStart := strings.ContainsAny(prefix, "自立")

		switch {
		case wantsEnd && end == "" && start != "":
			end = value
		case wantsStart && start == "":
			start = value
		case start == "":
			start = value
		case end == "":
			end = value
		}
	}
	return start, end
}

// parseSmallNumber 解析"8"、"八"、"十八"、"二十四"这类不超过百位的数字。
//
// 只做到九十九：试用期最大是违法值"十八个月"，再大就不是合同而是血压了。
func parseSmallNumber(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}

	digits := map[rune]int{
		'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
		'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
	}
	total, current := 0, 0
	seen := false
	for _, r := range s {
		switch r {
		case '十':
			if current == 0 {
				current = 1
			}
			total += current * 10
			current = 0
			seen = true
		case '百':
			if current == 0 {
				current = 1
			}
			total += current * 100
			current = 0
			seen = true
		default:
			d, ok := digits[r]
			if !ok {
				return 0, false
			}
			current = d
			seen = true
		}
	}
	if !seen {
		return 0, false
	}
	return total + current, true
}

// parseAmount 把 "8,000" / "1.2" 读成数字。
func parseAmount(s string) float64 {
	if v, ok := parseLooseNumber(s); ok {
		return v
	}
	return 0
}

// cleanName 去掉抽取结果里的空白与常见尾缀标点。
func cleanName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, " \t，,；;：:")
	return s
}

func matchAny(s string, res []*regexp.Regexp) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

func clip(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// criticalFields 是规则层真正会读的字段。
//
// 缺这些必须补模型：规则在字段缺失时会整条跳过，缺陷于是变成"审查通过"。
// 这也是唯一值得为它付一次模型调用的情形。
var criticalFields = []string{
	"contract_type", "term_start", "term_end", "probation_months",
	"salary_monthly", "salary_probation", "social_insurance",
	"deposit", "has_overtime_pay",
}

// hasCriticalMissing 判断缺失字段里是否有规则依赖项。
func hasCriticalMissing(missing []string) bool {
	for _, m := range missing {
		for _, c := range criticalFields {
			if m == c {
				return true
			}
		}
	}
	return false
}

// mergeMissing 只把模型给出的、正则没抽到的字段填进来。
//
// 正则抽到的值一律保留：日期与金额模型会做归一，但也会读错数位——
// "8000 元"读成 80000 这种错误在真实调用里出现过。
func mergeMissing(byRule, byLLM rules.Contract, missing []string) rules.Contract {
	merged := byRule
	for _, field := range missing {
		switch field {
		case "party_a":
			merged.PartyA = byLLM.PartyA
		case "party_b":
			merged.PartyB = byLLM.PartyB
		case "contract_type":
			merged.ContractType = byLLM.ContractType
		case "term_start":
			merged.TermStart = byLLM.TermStart
		case "term_end":
			merged.TermEnd = byLLM.TermEnd
		case "probation_months":
			merged.ProbationMonths = byLLM.ProbationMonths
		case "salary_monthly":
			merged.SalaryMonthly = byLLM.SalaryMonthly
		case "salary_probation":
			merged.SalaryProbation = byLLM.SalaryProbation
		case "social_insurance":
			merged.SocialInsurance = byLLM.SocialInsurance
		case "work_location":
			merged.WorkLocation = byLLM.WorkLocation
		case "position":
			merged.Position = byLLM.Position
		case "working_hours":
			merged.WorkingHours = byLLM.WorkingHours
		case "has_overtime_pay":
			merged.HasOvertimePay = byLLM.HasOvertimePay
		case "deposit":
			merged.Deposit = byLLM.Deposit
		}
	}
	return merged
}
