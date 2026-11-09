package main

import (
	"strings"
	"testing"

	"laborlaw/internal/port"
)

func TestEmbedTextIncludesLawAndArticle(t *testing.T) {
	s := port.Statute{
		ID:      "LCL-38",
		Law:     "中华人民共和国劳动合同法",
		Article: "第三十八条",
		Title:   "劳动者单方解除劳动合同",
		Text:    "用人单位有下列情形之一的，劳动者可以解除劳动合同：……",
	}
	got := embedText(s)

	// 法律名与条号必须在里面：只嵌正文时，"第八十五条"与"第八十七条"的文本
	// 几乎一样，向量也几乎一样，召回会互相挤占名额。
	for _, want := range []string{"中华人民共和国劳动合同法", "第三十八条", "劳动者单方解除劳动合同", "用人单位有下列情形之一的"} {
		if !strings.Contains(got, want) {
			t.Fatalf("嵌入文本缺少 %q：%q", want, got)
		}
	}
	if strings.HasPrefix(got, " ") || strings.HasSuffix(got, " ") {
		t.Fatalf("嵌入文本首尾不该有空格：%q", got)
	}
	if strings.Contains(got, "  ") {
		t.Fatalf("字段之间应为单个空格：%q", got)
	}

	// 空标题不能留下两个连续空格。
	bare := embedText(port.Statute{Law: "中华人民共和国劳动法", Article: "第四十四条"})
	if bare != "中华人民共和国劳动法 第四十四条" {
		t.Fatalf("空字段时嵌入文本 = %q", bare)
	}

	// 标题空、正文不空：正文要紧跟在条号后面，不能多出一个空格。
	noTitle := embedText(port.Statute{Law: "中华人民共和国劳动法", Article: "第四十四条", Text: "安排劳动者延长工作时间的，支付百分之一百五十的工资报酬。"})
	if strings.Contains(noTitle, "  ") {
		t.Fatalf("空标题时出现了连续空格：%q", noTitle)
	}
}

func TestClampBatch(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{50, 10},
		{0, 10},
		{3, 3},
		{10, 10},
		{1, 1},
		{-2, 10},
	}
	for _, c := range cases {
		if got := clampBatch(c.in); got != c.want {
			t.Errorf("clampBatch(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
