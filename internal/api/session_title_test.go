package api

import (
	"strings"
	"testing"
)

// TestSessionTitleKeepsRuleID 守住「预填问句的标题必须带规则号」。
//
// 缺陷形态：审查页「在对话页继续」发出去的原话是
//
//	合同「第七条」 · 岗位调整后重新约定试用期 · 结论 R-PROBATION-ONCE
//
// 按 20 字硬截只剩「合同「第七条」 · 岗位调整后重新约」，规则号被切掉，
// 侧边栏于是出现三条一模一样的标题，用户分不清哪条问的是哪个问题。
func TestSessionTitleKeepsRuleID(t *testing.T) {
	ask := "合同「第七条」 · 试用期两个月，同一单位第二次约定 · 结论 R-PROBATION-ONCE\n\n公司不同意改，我该怎么办？"
	got := sessionTitleFromInput(ask)
	if !strings.Contains(got, "R-PROBATION-ONCE") {
		t.Fatalf("标题里丢了规则号：%q", got)
	}
	if !strings.Contains(got, "第七条") {
		t.Fatalf("标题里丢了条款位置：%q", got)
	}
	if n := len([]rune(got)); n > chatSessionTitleStructuredRunes {
		t.Fatalf("标题长度 %d 超过 %d：%q", n, chatSessionTitleStructuredRunes, got)
	}
}

// TestSessionTitleRuleIDWinsOverLongSection 条款很长时先砍条款、保住规则号。
func TestSessionTitleRuleIDWinsOverLongSection(t *testing.T) {
	long := strings.Repeat("很长的条款描述", 6)
	got := sessionTitleFromInput("合同「" + long + "」 · 结论 R-WAGE-UNDER-MIN")
	if !strings.Contains(got, "R-WAGE-UNDER-MIN") {
		t.Fatalf("条款再长也不能砍掉规则号：%q", got)
	}
	if n := len([]rune(got)); n > chatSessionTitleStructuredRunes {
		t.Fatalf("标题长度 %d 超过 %d：%q", n, chatSessionTitleStructuredRunes, got)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("截断的条款应带省略号：%q", got)
	}
}

// TestSessionTitleFallsBackForPlainQuestion 认不出结构的普通提问，行为不变。
func TestSessionTitleFallsBackForPlainQuestion(t *testing.T) {
	plain := "公司一直不跟我签劳动合同该怎么赔，能要双倍工资吗"
	got := sessionTitleFromInput(plain)
	if n := len([]rune(got)); n > chatSessionTitleRunes+1 { // +1 是省略号
		t.Fatalf("长度失控：%d %q", n, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("超长普通提问应截断加省略号：%q", got)
	}

	short := "试用期能辞退吗"
	if got := sessionTitleFromInput(short); got != short {
		t.Fatalf("短提问应原样保留，实际 %q", got)
	}
}

// TestSessionTitleWithoutRuleIDFallsBack 只有「合同「…」」没有结论号时不硬拼。
func TestSessionTitleWithoutRuleIDFallsBack(t *testing.T) {
	got := sessionTitleFromInput("合同「第四条」这个押金条款有效吗，能不能要求退回来，公司说要扣")
	if strings.Contains(got, "·") {
		t.Fatalf("没有规则号就不该走结构化拼接：%q", got)
	}
	if n := len([]rune(got)); n > chatSessionTitleRunes+1 {
		t.Fatalf("长度失控：%d %q", n, got)
	}
}
