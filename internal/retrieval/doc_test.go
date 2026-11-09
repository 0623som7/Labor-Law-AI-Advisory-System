package retrieval

import (
	"strings"
	"testing"

	"laborlaw/internal/port"
)

// lcl38 用种子里的真实条文做 fixture：文本一旦被改写，向量库里 205 条向量
// 与新文本就对不上了，这条测试就是那个回归守卫。
var lcl38 = port.Statute{
	ID:      "LCL-38",
	Law:     "中华人民共和国劳动合同法",
	Article: "第三十八条",
	Title:   "劳动者单方解除劳动合同",
	Text: "用人单位有下列情形之一的，劳动者可以解除劳动合同：\n" +
		"（一）未按照劳动合同约定提供劳动保护或者劳动条件的；\n" +
		"（二）未及时足额支付劳动报酬的；\n" +
		"（三）未依法为劳动者缴纳社会保险费的；\n" +
		"（四）用人单位的规章制度违反法律、法规的规定，损害劳动者权益的；\n" +
		"（五）因本法第二十六条第一款规定的情形致使劳动合同无效的；\n" +
		"（六）法律、行政法规规定劳动者可以解除劳动合同的其他情形。\n" +
		"用人单位以暴力、威胁或者非法限制人身自由的手段强迫劳动者劳动的，或者用人单位违章指挥、强令冒险作业危及劳动者人身安全的，劳动者可以立即解除劳动合同，不需事先告知用人单位。",
}

func TestStatuteDocumentKeepsFormat(t *testing.T) {
	got := StatuteDocument(lcl38)
	prefix := "中华人民共和国劳动合同法 第三十八条 劳动者单方解除劳动合同 用人单位有下列情形之一的"
	if !strings.HasPrefix(got, prefix) {
		t.Fatalf("前缀不对：\n%s", got[:min(len(got), 120)])
	}
	if !strings.Contains(got, "\n（三）未依法为劳动者缴纳社会保险费的；") {
		t.Error("正文换行结构被改动")
	}
	// 四个字段各出现一次，用空格连成一个字符串——这就是当初喂给嵌入模型的形状。
	if n := strings.Count(got, "中华人民共和国劳动合同法"); n != 1 {
		t.Errorf("法律名出现 %d 次", n)
	}
}

func TestStatuteDocumentSkipsEmptyFields(t *testing.T) {
	got := StatuteDocument(port.Statute{Law: "中华人民共和国劳动法", Article: "第四十四条", Text: "加班工资"})
	// 标题为空时不能留两个连续空格：同一批法条的文本格式必须一致，
	// 否则嵌入模型看到的形状在条目之间不一致。
	if got != "中华人民共和国劳动法 第四十四条 加班工资" {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "  ") {
		t.Errorf("出现连续空格：%q", got)
	}
}

func TestStatuteDocumentTrimsFields(t *testing.T) {
	got := StatuteDocument(port.Statute{Law: " 中华人民共和国劳动法 ", Article: " 第四十四条 ", Title: " ", Text: "\n加班工资\n"})
	if got != "中华人民共和国劳动法 第四十四条 加班工资" {
		t.Fatalf("got %q", got)
	}
}
