package extract

import "regexp"

// 为什么还要再分一层：Extract 的短路条件原本只看"缺不缺关键字段"
// （hasCriticalMissing），可"缺"有两种来源，模型只对其中一种有用。
//
//  1. 原文写了，正则没认出来（终止日写成"二〇二六年十二月三十一日止"、
//     工资写成"月薪壹万贰仟元"）——这种必须调模型，否则规则整条跳过，
//     用户看到的是"审查通过"，那是最危险的失败形态。
//  2. 原文根本没写。线上 E.pdf 就是这一类：第一条只写"自 2026 年 7 月 1 日起
//     生效"，没有终止日；第十五条只写"试用期工资按新岗位标准的百分之九十发放"，
//     没有金额。模型返回的这两个字段依旧是空，missing 仍然是
//     [term_end salary_probation]，却白花 6.8 秒——日志里
//     "合同要素抽取完成 source=rule+llm missing=[term_end salary_probation]"
//     每一次审查都在复现这件事。
//
// 所以判据换成"原文里有没有能支撑这个字段的凭据"：有凭据才值得付一次模型调用。
//
// 宁松勿紧：凭据写宽了只是多调一次模型（退化为今天的行为），写紧了会把
// 本该补的字段漏掉，让规则静默跳过。表里没登记的字段也一律按"可达"处理。
var fieldEvidence = map[string][]*regexp.Regexp{
	"contract_type": {
		regexp.MustCompile(`(固定期限|无固定期限|以完成一定工作任务为期限)`),
		// 劳务/派遣/实习这类字眼是"合同性质"的硬凭据：正则没认出类型，
		// 但原文显然谈了它，那就该让模型补一次。
		regexp.MustCompile(`(劳务|非全日制|劳务派遣|实习|聘用|承揽|返聘)`),
	},
	"term_start": {
		regexp.MustCompile(`(自|从|起始|开始|生效)\s*[〇零一二三四五六七八九0-9]{2,4}\s*年`),
	},
	// 「解除」不算终止凭据：第十六条那种"可以解除本合同"满地都是，
	// 放进来就等于这条门形同不存在。
	"term_end": {
		regexp.MustCompile(`(终止日期|终止时间|合同终止|期满终止|期限届满|届满日|合同到期|到期日|有效期至)`),
		regexp.MustCompile(`(至|到|止于)\s*[〇零一二三四五六七八九0-9]{2,4}\s*年`),
		regexp.MustCompile(`[〇零一二三四五六七八九0-9]{2,4}\s*年[^。；\n]{0,12}(止|终止|届满)`),
	},
	"probation_months": {
		regexp.MustCompile(`(试用期|见习期|试岗期)`),
	},
	"salary_monthly": {
		regexp.MustCompile(`(工资|月薪|薪金|劳动报酬|薪酬)[^。；\n]{0,16}[0-9〇零一二三四五六七八九十百千万壹贰叁肆伍陆柒捌玖拾]{1,8}\s*(元|万|块)`),
		regexp.MustCompile(`[0-9]{3,6}\s*(元|万)[^。；\n]{0,8}(每月|月薪|工资)`),
	},
	// 必须带金额：E.pdf 只写了"按新岗位标准的百分之九十"，合同里没有这个数，
	// 模型也只能编，R-PROBATION-SALARY 本来就该按缺失跳过。
	"salary_probation": {
		regexp.MustCompile(`试用期[^。；\n]{0,24}[0-9〇零一二三四五六七八九十百千万壹贰叁肆伍陆柒捌玖拾]{1,8}\s*(元|万|块)`),
		regexp.MustCompile(`[0-9]{3,6}\s*(元|万)[^。；\n]{0,12}试用期`),
		regexp.MustCompile(`(试用期工资|试用期月工资|试用期薪资)\s*[：:为是]?\s*[0-9]{3,6}`),
	},
	"social_insurance": {
		regexp.MustCompile(`(社会保险|社保|五险|养老保险|医疗保险|失业保险|工伤保险|住房公积金)`),
	},
	"deposit": {
		regexp.MustCompile(`(押金|保证金|抵押|扣押|扣留|担保金|风险金)`),
	},
	"has_overtime_pay": {
		regexp.MustCompile(`(加班|延长工作时间|加点)`),
	},
}

// isCriticalField 判断字段名是否属于规则依赖项。
func isCriticalField(field string) bool {
	for _, c := range criticalFields {
		if field == c {
			return true
		}
	}
	return false
}

// hasAttainableCriticalMissing 判断缺失的关键字段里，是否至少有一个"原文其实写了"。
// 返回 false 意味着模型补不出任何规则要用的东西，可以直接跳过这次调用。
func hasAttainableCriticalMissing(text string, missing []string) bool {
	for _, field := range missing {
		if !isCriticalField(field) {
			continue
		}
		patterns, known := fieldEvidence[field]
		if !known {
			// 新加了 criticalFields 却忘了登记凭据：保守处理，照旧调模型。
			return true
		}
		for _, re := range patterns {
			if re.MatchString(text) {
				return true
			}
		}
	}
	return false
}
