package prompt

import (
	"strings"
	"testing"

	"laborlaw/internal/port"
)

// 提示词是产品口径：改一个字就是换一份产品承诺，但此前这个包覆盖率是 0，
// 谁都能在调措辞时顺手把"不许编法条"那句删掉而没有任何东西拦得住。
// 这里只做字符串断言，不调模型——目的是钉住硬约束的存在，不是评价文笔。

func TestConsultSystemKeepsHardConstraints(t *testing.T) {
	s := ConsultSystem()
	if s == "" {
		t.Fatal("系统提示词为空")
	}

	// 四类约束各至少一个锚点：
	//  参考资料 —— 条文只能来自检索注入区块；
	//  不提供正式法律意见 —— 免责口径（原文没有"不替代"三个字，语义等价）；
	//  劳动 —— 话题边界；
	//  条号 —— 引用格式要求。
	musts := map[string]string{
		"参考资料":      "法条只能来自检索结果",
		"不提供正式法律意见": "免责声明必须由模型说出口",
		"劳动":        "话题边界限定为劳动用工",
		"条号":        "引用时只写法律名称与条号",
	}
	for kw, why := range musts {
		if !strings.Contains(s, kw) {
			t.Errorf("系统提示词缺少 %q（%s）", kw, why)
		}
	}

	// 最容易在调优时被删的一句：禁止凭记忆复述条文。
	if !strings.Contains(s, "禁止凭记忆") {
		t.Error("系统提示词缺少「禁止凭记忆」：这条是防编造法条的核心约束")
	}
}

func TestNoStatuteNoticeIsExplicit(t *testing.T) {
	if noStatuteNotice == "" {
		t.Fatal("没有检索结果时的说明为空：模型会自己补一条最像的法条")
	}
	if !strings.Contains(noStatuteNotice, "没有检索到") {
		t.Errorf("说明里必须明确「没有检索到」，实际: %q", noStatuteNotice)
	}
}

func TestConsultWithStatutesCapsAt8(t *testing.T) {
	hits := make([]port.StatuteHit, 0, 12)
	for i := 0; i < 12; i++ {
		hits = append(hits, port.StatuteHit{Statute: port.Statute{
			ID:      "LCL-" + string(rune('A'+i)),
			Law:     "中华人民共和国劳动合同法",
			Article: "第" + string(rune('一'+i)) + "条",
			Text:    "条文正文" + string(rune('A'+i)),
		}, Score: 1})
	}

	out := ConsultWithStatutes(hits)

	if got := strings.Count(out, "\n【"); got != 8 {
		t.Errorf("注入条数 = %d, want 8（超过 8 条会稀释注意力）", got)
	}
	// 第 9 条以后不该出现：截断要在拼接阶段生效，而不是靠模型自己忽略。
	if strings.Contains(out, "条文正文I") {
		t.Error("第 9 条之后的内容不应进入上下文")
	}
}

func TestStatuteContextHeaderMarksSource(t *testing.T) {
	hits := []port.StatuteHit{{Statute: port.Statute{
		Law: "中华人民共和国劳动法", Article: "第四十四条", Text: "加班工资标准",
	}}}

	out := ConsultWithStatutes(hits)

	// 区块必须带显式标题：用户消息里也可能贴一段"法条"，只有这个区块里的内容
	// 才允许被引用，否则伪造的条文会被当成依据。
	if !strings.Contains(out, statuteContextHeader) {
		t.Fatal("注入区块缺少分隔标题，模型无法区分系统给的原文与用户贴的文本")
	}
	if !strings.HasPrefix(out, consultSystem) {
		t.Error("系统提示词必须排在参考资料区块之前")
	}
	if !strings.Contains(out, "第四十四条") || !strings.Contains(out, "加班工资标准") {
		t.Error("命中条文的条号与正文都必须进上下文")
	}
}

// 空命中走的是另一条分支：不注入空区块，而是显式说"没检索到"。
func TestConsultWithStatutesEmptyUsesNotice(t *testing.T) {
	out := ConsultWithStatutes(nil)
	if !strings.Contains(out, noStatuteNotice) {
		t.Error("零命中时必须追加 noStatuteNotice")
	}
	if strings.Contains(out, statuteContextHeader) {
		t.Error("零命中时不应出现空的参考资料区块")
	}
}
