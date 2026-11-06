package file

import (
	"testing"

	"laborlaw/internal/port"
)

// sampleXML 是 poppler -bbox-layout 的真实结构（尺寸与坐标按 A4 量级缩小，便于断言）。
const sampleXML = `<?xml version="1.0" encoding="UTF-8"?>
<html xmlns="http://www.w3.org/1999/xhtml">
<body><doc>
  <page width="600" height="800">
    <flow><block>
      <line xMin="50" yMin="100" xMax="300" yMax="120">
        <word xMin="50" yMin="100" xMax="120" yMax="120">甲方</word>
        <word xMin="120" yMin="100" xMax="200" yMax="120">乙方</word>
        <word xMin="200" yMin="100" xMax="300" yMax="120">签署</word>
      </line>
      <line xMin="50" yMin="130" xMax="400" yMax="150">
        <word xMin="50" yMin="130" xMax="400" yMax="150">The fixed term contract</word>
      </line>
    </block></flow>
    <flow><block>
      <line xMin="50" yMin="200" xMax="200" yMax="220">
        <word xMin="50" yMin="200" xMax="200" yMax="220">试用期三个月</word>
      </line>
    </block></flow>
  </page>
  <page width="600" height="800">
    <flow><block>
      <line xMin="50" yMin="-5" xMax="400" yMax="20">
        <word xMin="50" yMin="-5" xMax="400" yMax="20">页眉超出页面</word>
      </line>
    </block></flow>
  </page>
</doc></body></html>`

func TestParsePoppler(t *testing.T) {
	pages, err := parsePoppler([]byte(sampleXML))
	if err != nil {
		t.Fatalf("parsePoppler() 返回错误: %v", err)
	}
	if len(pages) != 2 {
		t.Fatalf("页数 = %d, want 2", len(pages))
	}

	if pages[0].Width != 600 || pages[0].Height != 800 {
		t.Fatalf("页面尺寸 = %.0fx%.0f, want 600x800", pages[0].Width, pages[0].Height)
	}
	if len(pages[0].Lines) != 3 {
		t.Fatalf("第 1 页行数 = %d, want 3（两个 flow 都要收）", len(pages[0].Lines))
	}

	// 第一行：y 方向翻转。poppler yMin=100/yMax=120，页高 800
	// → 下边界 800-120=680，上边界 800-100=700。
	want := [4]float64{50, 680, 300, 700}
	if pages[0].Lines[0].BBox != want {
		t.Fatalf("第 1 行 BBox = %v, want %v", pages[0].Lines[0].BBox, want)
	}
	if pages[0].Lines[0].Text != "甲方乙方签署" {
		t.Fatalf("中文拼接 = %q, want 甲方乙方签署", pages[0].Lines[0].Text)
	}
	if pages[0].Lines[1].Text != "The fixed term contract" {
		t.Fatalf("英文拼接 = %q", pages[0].Lines[1].Text)
	}
}

// TestParsePopplerClampsOutOfPage：页眉/水印常常落到页外，必须夹住而不是整块丢掉。
func TestParsePopplerClampsOutOfPage(t *testing.T) {
	pages, err := parsePoppler([]byte(sampleXML))
	if err != nil {
		t.Fatalf("parsePoppler() 返回错误: %v", err)
	}

	line := pages[1].Lines[0]
	if line.BBox[1] < 0 || line.BBox[3] > 800 {
		t.Fatalf("越界坐标没有被夹住: %v", line.BBox)
	}
	// yMin=-5 → 下边界 800-20=780；上边界 800-(-5)=805 → 夹到 800。
	if line.BBox[3] != 800 {
		t.Fatalf("上边界 = %.1f, want 800", line.BBox[3])
	}
}

func TestParsePopplerErrors(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{name: "空文档", in: `<html><body><doc></doc></body></html>`},
		{name: "非法 XML", in: `<html><body>`},
		{name: "页面尺寸为零", in: `<html><body><doc><page width="0" height="0"></page></doc></body></html>`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parsePoppler([]byte(tc.in)); err == nil {
				t.Fatal("期望报错，实际通过")
			}
		})
	}
}

func TestJoinWords(t *testing.T) {
	tests := []struct {
		name  string
		words []string
		want  string
	}{
		{name: "中文不加空格", words: []string{"劳动", "合同法", "第三十八条"}, want: "劳动合同法第三十八条"},
		{name: "英文加空格", words: []string{"fixed", "term", "contract"}, want: "fixed term contract"},
		{name: "中英混排", words: []string{"签订", "fixed", "term"}, want: "签订fixed term"},
		{name: "全角标点不补空格", words: []string{"约定。", "建议"}, want: "约定。建议"},
		{name: "空列表", words: nil, want: ""},
		{name: "空白词被忽略", words: []string{"  ", "合同", " "}, want: "合同"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			words := make([]bboxWord, 0, len(tc.words))
			for _, w := range tc.words {
				words = append(words, bboxWord{Text: w})
			}
			if got := joinWords(words); got != tc.want {
				t.Fatalf("joinWords() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsCJK(t *testing.T) {
	cases := map[rune]bool{
		'劳': true,
		'。': true,
		'，': true,
		'Ａ': true, // 全角
		'a': false,
		'1': false,
		',': false,
	}
	for r, want := range cases {
		if got := isCJK(r); got != want {
			t.Fatalf("isCJK(%q) = %v, want %v", r, got, want)
		}
	}
}

// TestBBoxWithinPage 用 port 自带的 Valid 复核：解析层产出的坐标必须能通过契约校验。
func TestBBoxWithinPage(t *testing.T) {
	pages, err := parsePoppler([]byte(sampleXML))
	if err != nil {
		t.Fatalf("parsePoppler() 返回错误: %v", err)
	}
	for pi, p := range pages {
		for li, line := range p.Lines {
			block := port.TextBlock{
				ID:        "blk",
				Page:      pi,
				BBox:      line.BBox,
				Text:      line.Text,
				CharStart: li,
				CharEnd:   li + 1,
			}
			if err := block.Valid(p.Width, p.Height); err != nil {
				t.Fatalf("第 %d 页第 %d 行坐标非法: %v (bbox=%v)", pi, li, err, line.BBox)
			}
		}
	}
}
