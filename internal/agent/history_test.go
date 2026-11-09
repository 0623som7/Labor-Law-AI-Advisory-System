package agent

import (
	"testing"

	"laborlaw/internal/port"
)

func msg(role port.Role, content string) port.Message {
	return port.Message{Role: role, Content: content}
}

func TestTrimHistoryKeepsAllWhenBudgetRoomy(t *testing.T) {
	msgs := []port.Message{
		msg(port.RoleUser, "公司不给我交社保"),
		msg(port.RoleAssistant, "可以解除并要求经济补偿。"),
		msg(port.RoleUser, "要多少"),
	}

	kept, dropped := trimHistory(msgs, 1000)
	if dropped != 0 {
		t.Errorf("dropped = %d，期望 0", dropped)
	}
	if len(kept) != 3 {
		t.Fatalf("kept 长度 = %d，期望 3", len(kept))
	}
	for i := range msgs {
		// port.Message 含 []ToolCall 切片，不能整体比较。
		if kept[i].Role != msgs[i].Role || kept[i].Content != msgs[i].Content {
			t.Errorf("第 %d 条变了: %+v", i, kept[i])
		}
	}
}

func TestTrimHistoryKeepsNewest(t *testing.T) {
	// 每条 100 个汉字 = 100 token，预算 250 只装得下最后两条。
	body := ""
	for i := 0; i < 100; i++ {
		body += "字"
	}
	msgs := []port.Message{
		msg(port.RoleUser, body),
		msg(port.RoleAssistant, body),
		msg(port.RoleUser, body),
		msg(port.RoleAssistant, body),
	}

	kept, dropped := trimHistory(msgs, 250)
	if dropped != 2 {
		t.Errorf("dropped = %d，期望 2", dropped)
	}
	// 最后两条里首条是 user，所以不需要再剔。
	if len(kept) != 2 {
		t.Fatalf("kept 长度 = %d，期望 2", len(kept))
	}
	if kept[0].Content != msgs[2].Content || kept[1].Content != msgs[3].Content {
		t.Errorf("保留的不是最后两条: %d 条", len(kept))
	}
	if kept[0].Role != port.RoleUser {
		t.Errorf("首条角色 = %q，期望 user", kept[0].Role)
	}
}

func TestTrimHistoryDropsLeadingAssistant(t *testing.T) {
	// 三条各 100 token，预算 150 只装得下最后一条 assistant——
	// 留下它就是"开场就是模型的独白"，必须继续往前丢掉。
	body := ""
	for i := 0; i < 100; i++ {
		body += "字"
	}
	msgs := []port.Message{
		msg(port.RoleUser, body),
		msg(port.RoleAssistant, body),
		msg(port.RoleAssistant, body),
	}

	kept, dropped := trimHistory(msgs, 150)
	if len(kept) != 0 {
		t.Fatalf("kept = %d 条，期望全被剔除（首条是 assistant）", len(kept))
	}
	if dropped != 3 {
		t.Errorf("dropped = %d，期望 3", dropped)
	}
}

func TestTrimHistoryZeroBudget(t *testing.T) {
	msgs := []port.Message{
		msg(port.RoleUser, "在吗"),
		msg(port.RoleAssistant, "在"),
	}

	kept, dropped := trimHistory(msgs, 0)
	if len(kept) != 0 {
		t.Errorf("kept = %d 条，期望 0", len(kept))
	}
	if dropped != len(msgs) {
		t.Errorf("dropped = %d，期望 %d", dropped, len(msgs))
	}
}

func TestEstimateTokensCountsRunes(t *testing.T) {
	// 这里钉的是"当前实现的特征"（按 rune 数计），不是分词器的正确值：
	// 将来若换成真正的分词器，这三个数字会变，用例要跟着改而不是当成回归。
	cases := []struct {
		in   string
		want int
	}{
		{"你好", 2},
		{"hello", 5},
		{"你好hello", 7},
		{"", 0},
	}
	for _, c := range cases {
		if got := estimateTokens(c.in); got != c.want {
			t.Errorf("estimateTokens(%q) = %d，期望 %d", c.in, got, c.want)
		}
	}
}

func TestTrimHistoryNeverReturnsNil(t *testing.T) {
	msgs := []port.Message{msg(port.RoleUser, "很长的一句话")}

	kept, _ := trimHistory(msgs, 1)
	if kept == nil {
		t.Fatal("kept 为 nil，调用方 append 时会与空切片表现不同")
	}
	if len(kept) != 0 {
		t.Errorf("kept = %d 条，期望 0", len(kept))
	}

	// 空输入也不该返回 nil。
	if kept, _ := trimHistory(nil, 1000); kept == nil {
		t.Fatal("空输入时 kept 为 nil")
	}
}
