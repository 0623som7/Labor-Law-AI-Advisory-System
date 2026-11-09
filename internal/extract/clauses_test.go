package extract

import (
	"testing"

	"laborlaw/internal/port"
)

// block 造一个文本块，省得每个用例都写全字段。
func block(id string, page int, x0, y0, x1, y1 float64, text string) port.TextBlock {
	return port.TextBlock{
		ID:   id,
		Page: page,
		BBox: [4]float64{x0, y0, x1, y1},
		Text: text,
		// CharStart/CharEnd 在切条款时不用，但 contract 的其它环节会读，填个递增值
		// 免得出现"测试里能用、真实数据里越界"的错位。
		CharStart: len(text),
		CharEnd:   len(text),
	}
}

func TestClausesFromBlocksGroupsByArticle(t *testing.T) {
	blocks := []port.TextBlock{
		block("b0", 1, 72, 760, 520, 780, "劳动合同书"),
		block("b1", 1, 72, 700, 520, 720, "第一条 合同期限"),
		block("b2", 1, 72, 680, 520, 700, "本合同为固定期限劳动合同。"),
		block("b3", 1, 72, 640, 520, 660, "自2026年1月1日起至2029年1月1日止。"),
		block("b4", 1, 72, 600, 520, 620, "第二条 试用期"),
		block("b5", 2, 72, 760, 520, 780, "试用期为六个月。"),
	}

	got := clausesFromBlocks(blocks)
	if len(got) != 2 {
		t.Fatalf("条款数 = %d, want 2（%+v）", len(got), got)
	}
	if got[0].Section != "第一条" || got[1].Section != "第二条" {
		t.Fatalf("section = %q,%q, want 第一条,第二条", got[0].Section, got[1].Section)
	}
	// 第一条跨三块，正文必须按块序拼起来，且不改写原文。
	if got[0].Text != "第一条 合同期限\n本合同为固定期限劳动合同。\n自2026年1月1日起至2029年1月1日止。" {
		t.Fatalf("第一条 text = %q", got[0].Text)
	}
	// 第二条正文跨到第 2 页，但 page 取起始块所在页（1）：前端按"条款从哪页开始"
	// 决定跳转目标，取末页会跳到读者找不到开头的地方。
	if got[1].Page != 1 {
		t.Fatalf("第二条 page = %d, want 1", got[1].Page)
	}
	if got[1].Text != "第二条 试用期\n试用期为六个月。" {
		t.Fatalf("第二条 text = %q（跨页条款要把两块拼起来）", got[1].Text)
	}
}

func TestClausesFromBlocksUnionsBBox(t *testing.T) {
	blocks := []port.TextBlock{
		block("b1", 1, 72, 700, 520, 720, "第一条 合同期限"),
		block("b2", 1, 80, 680, 480, 700, "本合同为固定期限劳动合同。"),
		block("b3", 1, 56, 660, 540, 680, "自2026年1月1日起至2029年1月1日止。"),
	}

	got := clausesFromBlocks(blocks)
	if len(got) != 1 {
		t.Fatalf("条款数 = %d", len(got))
	}
	want := [4]float64{56, 660, 540, 720}
	if got[0].BBox != want {
		t.Fatalf("bbox = %v, want %v（多块条款必须取并集，只标第一块会让人以为漏标）", got[0].BBox, want)
	}
}

func TestClausesFromBlocksReturnsEmptyWithoutArticles(t *testing.T) {
	blocks := []port.TextBlock{
		block("b1", 1, 72, 700, 520, 720, "这是一份没有条号的合同。"),
		block("b2", 1, 72, 680, 520, 700, "全都写成整段文字。"),
	}

	got := clausesFromBlocks(blocks)
	if got == nil {
		t.Fatal("没切出条款时应返回空切片而不是 nil（调用方据此退回模型路径）")
	}
	if len(got) != 0 {
		t.Fatalf("条款数 = %d, want 0", len(got))
	}
}

// "第 二 条"这种字间空格在模板排版里很常见，不去空格就一条都切不出来。
func TestClausesFromBlocksToleratesSpacedArticle(t *testing.T) {
	blocks := []port.TextBlock{
		block("b1", 1, 72, 700, 520, 720, "第 二 条  试用期"),
		block("b2", 1, 72, 680, 520, 700, "试用期为六个月。"),
	}

	got := clausesFromBlocks(blocks)
	if len(got) != 1 {
		t.Fatalf("条款数 = %d, want 1", len(got))
	}
	if got[0].Section != "第二条" {
		t.Fatalf("section = %q, want 第二条", got[0].Section)
	}
}
