package extract

import "testing"

// E.pdf 的原文节选（服务器 pdftotext -layout 的真实输出）。
// 它缺 term_end 与 salary_probation，且两个字段在原文里都没有凭据：
// 第一条只写生效日，第十五条只写"百分之九十"没有金额。
const ePdfExcerpt = `第一条 本合同为固定期限劳动合同，自 2026 年 7 月 1 日起生效。
第三条 乙方月工资 1800 元，甲方于每月 15 日前以银行转账方式发放上月工资。
第四条 甲方自用工之日起为乙方办理社会保险登记，按时足额缴纳基本养老保险、基本医疗保险。
第七条 本合同试用期二个月，自用工之日起计算。试用期满经考核合格，乙方转为正式员工。
第十五条 乙方岗位调整后，重新约定试用期三个月，试用期工资按新岗位标准的百分之九十发放。
第十六条 乙方提前三十日以书面形式通知甲方，可以解除本合同。`

// TestAttainableFalseOnEPdf：这是线上 6.8 秒的全部来源。两个缺失字段原文都没有凭据，
// 必须判定为"不可达"，否则模型每次都被白调一次并原样返回空值。
func TestAttainableFalseOnEPdf(t *testing.T) {
	if hasAttainableCriticalMissing(ePdfExcerpt, []string{"term_end", "salary_probation"}) {
		t.Fatal("E.pdf 的 term_end / salary_probation 在原文里没有凭据，不该判为可达")
	}
}

func TestAttainable(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		missing []string
		want    bool
	}{
		{
			name:    "明确终止日期",
			text:    "本合同自2026年1月1日起至2029年1月1日止。",
			missing: []string{"term_end"},
			want:    true,
		},
		{
			name:    "写到终止日期四个字",
			text:    "合同终止日期：2029 年 1 月 1 日。",
			missing: []string{"term_end"},
			want:    true,
		},
		{
			name:    "只写解除不写终止——不算终止凭据",
			text:    "乙方提前三十日通知甲方，可以解除本合同。",
			missing: []string{"term_end"},
			want:    false,
		},
		{
			name:    "试用期工资带金额",
			text:    "试用期工资为 6400 元/月，转正后另议。",
			missing: []string{"salary_probation"},
			want:    true,
		},
		{
			name:    "试用期工资只写比例——合同里没有这个数，模型也编不出",
			text:    "试用期工资按新岗位标准的百分之九十发放。",
			missing: []string{"salary_probation"},
			want:    false,
		},
		{
			name:    "一个可达一个不可达——仍要调模型",
			text:    "试用期工资为 6400 元/月。",
			missing: []string{"term_end", "salary_probation"},
			want:    true,
		},
		{
			name:    "非关键字段不影响判定",
			text:    "第一条 自 2026 年 7 月 1 日起生效。",
			missing: []string{"work_location", "employer_name"},
			want:    false,
		},
		{
			name:    "表里没登记的关键字段保守调模型",
			text:    "第一条 自 2026 年 7 月 1 日起生效。",
			missing: []string{"brand_new_field"},
			want:    false,
		},
		{
			name:    "什么都不缺",
			text:    ePdfExcerpt,
			missing: nil,
			want:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasAttainableCriticalMissing(c.text, c.missing); got != c.want {
				t.Fatalf("hasAttainableCriticalMissing = %v, want %v", got, c.want)
			}
		})
	}
}

// TestAttainableUnknownCriticalField：criticalFields 以后加人却没登记凭据时，
// 必须退化成"调模型"，不能让规则静默跳过。
func TestAttainableUnknownCriticalField(t *testing.T) {
	original := criticalFields
	criticalFields = append(append([]string{}, original...), "unregistered_field")
	t.Cleanup(func() { criticalFields = original })

	if !hasAttainableCriticalMissing("第一条 自 2026 年 7 月 1 日起生效。", []string{"unregistered_field"}) {
		t.Fatal("未登记凭据的关键字段应保守判为可达")
	}
}
