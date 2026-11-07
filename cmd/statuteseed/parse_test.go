package main

import (
	"strings"
	"testing"
)

func TestChineseNumber(t *testing.T) {
	cases := map[string]int{
		"一": 1, "九": 9, "十": 10, "十一": 11, "十五": 15,
		"二十": 20, "三十八": 38, "四十七": 47, "九十八": 98,
		"一百": 100, "一百零一": 101, "一百零七": 107, "一百一十": 110,
		"二百": 200, "一千": 1000,
	}
	for in, want := range cases {
		got, err := chineseNumber(in)
		if err != nil {
			t.Fatalf("chineseNumber(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("chineseNumber(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestChineseNumberRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "ABC", "第", "十万"} {
		if _, err := chineseNumber(in); err == nil {
			t.Errorf("chineseNumber(%q) 应当报错", in)
		}
	}
}

// testSpec 用两条的微型法律做测试，避免用例被真实条数绑死。
func testSpec(expected int) sourceSpec {
	return sourceSpec{
		File:      "test.html",
		Law:       "测试法",
		IDPrefix:  "T",
		URL:       "https://example.gov.cn/law",
		Effective: "2008-01-01",
		Expected:  expected,
	}
}

func TestParseSplitTitleAndBody(t *testing.T) {
	raw := `<html><body>
<p style="text-align: center;"><strong>第一章 总则</strong></p>
<p><strong>　　第一条【立法宗旨】</strong></p>
<p>　　为了保护劳动者，制定本法。</p>
<p><strong>　　第二条【适用范围】</strong></p>
<p>　　境内用人单位适用本法。</p>
<p>　　国家机关参照执行。</p>
</body></html>`

	got, err := parseStatutes(raw, testSpec(2))
	if err != nil {
		t.Fatalf("parseStatutes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d statutes, want 2", len(got))
	}
	if got[0].ID != "T-1" || got[0].Article != "第一条" || got[0].Title != "立法宗旨" {
		t.Errorf("首条字段不符: %+v", got[0])
	}
	if got[0].Text != "为了保护劳动者，制定本法。" {
		t.Errorf("首条正文不符: %q", got[0].Text)
	}
	// 多段正文要用换行保留段落边界，不能粘成一长串。
	if got[1].Text != "境内用人单位适用本法。\n国家机关参照执行。" {
		t.Errorf("次条多段正文不符: %q", got[1].Text)
	}
	if got[1].EffectiveAt.Format("2006-01-02") != "2008-01-01" {
		t.Errorf("施行日期不符: %v", got[1].EffectiveAt)
	}
}

func TestParseInlineBodyWithFullWidthSpace(t *testing.T) {
	raw := `<p>　　第一条　为了保护劳动者的合法权益，制定本法。</p>
<p>　　第二条　境内用人单位适用本法。</p>
<p>　　国家机关参照执行。</p>`

	got, err := parseStatutes(raw, testSpec(2))
	if err != nil {
		t.Fatalf("parseStatutes: %v", err)
	}
	if got[0].Text != "为了保护劳动者的合法权益，制定本法。" {
		t.Errorf("全角空格未正确剥离: %q", got[0].Text)
	}
	if got[0].Title != "" {
		t.Errorf("无条旨时 Title 应为空串, got %q", got[0].Title)
	}
	if got[1].Text != "境内用人单位适用本法。\n国家机关参照执行。" {
		t.Errorf("次条正文不符: %q", got[1].Text)
	}
}

func TestParseSkipsTableOfContents(t *testing.T) {
	raw := `<p>目　　录</p>
<p>第一章　总则</p>
<p>第二章　附则</p>
<p>第一章　总则</p>
<p>第一条　正文一。</p>
<p>第二章　附则</p>
<p>第二条　正文二。</p>`

	got, err := parseStatutes(raw, testSpec(2))
	if err != nil {
		t.Fatalf("parseStatutes: %v", err)
	}
	if got[0].Text != "正文一。" || got[1].Text != "正文二。" {
		t.Errorf("目录或章标题混入正文: %+v", got)
	}
}

func TestParseIgnoresInTextArticleReference(t *testing.T) {
	// 条文里引用别的条号时不能当成新条文，否则一条完整条文会被拦腰切断。
	raw := `<p>第一条　用人单位应当支付报酬。</p>
<p>第三条规定的特殊情形除外。</p>
<p>第二条　本法自2008年1月1日起施行。</p>`

	got, err := parseStatutes(raw, testSpec(2))
	if err != nil {
		t.Fatalf("parseStatutes: %v", err)
	}
	if !strings.Contains(got[0].Text, "第三条规定的特殊情形除外。") {
		t.Errorf("条文内引用被误当新条: %q", got[0].Text)
	}
}

func TestParseFailsOnMissingArticle(t *testing.T) {
	raw := `<p>第一条　正文一。</p><p>第三条　正文三。</p>`
	if _, err := parseStatutes(raw, testSpec(2)); err == nil {
		t.Fatal("缺条时应当报错")
	}
}

func TestParseFailsOnCountMismatch(t *testing.T) {
	raw := `<p>第一条　正文一。</p>`
	if _, err := parseStatutes(raw, testSpec(5)); err == nil {
		t.Fatal("条数与预期不符时应当报错")
	}
}

func TestParseFailsOnEmptyBody(t *testing.T) {
	raw := `<p>第一条【只有条旨】</p><p>第二章　附则</p><p>第二条　正文二。</p>`
	if _, err := parseStatutes(raw, testSpec(2)); err == nil {
		t.Fatal("正文为空时应当报错")
	}
}

func TestParagraphsDropsScriptsAndDecodesEntities(t *testing.T) {
	raw := `<p>用人单位与劳动者&nbsp;应当遵循&nbsp;&quot;合法&quot;原则。</p><script>var a = 1;</script>`
	lines := paragraphs(raw)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], `"合法"`) {
		t.Errorf("HTML 实体未解码: %q", lines[0])
	}
}
