package file

import (
	"encoding/xml"
	"fmt"
	"strings"
	"unicode"
)

// 本文件只做一件事：把 poppler 的 -bbox-layout XHTML 转成 port 的坐标协议。
//
// 输出结构（poppler 25.x 实测）：
//
//	<html><body><doc>
//	  <page width="595.32" height="841.92">
//	    <flow><block><line xMin yMin xMax yMax>
//	      <word xMin yMin xMax yMax>文本</word>
//	    </line></block></flow>
//	  </page>
//	</doc></body></html>

// bboxDocument 是 poppler 输出的根。
type bboxDocument struct {
	Pages []bboxPage `xml:"body>doc>page"`
}

// bboxPage 是一页。width/height 单位 pt，来自页面 MediaBox。
type bboxPage struct {
	Width  float64    `xml:"width,attr"`
	Height float64    `xml:"height,attr"`
	Flows  []bboxFlow `xml:"flow"`
}

type bboxFlow struct {
	Blocks []bboxBlock `xml:"block"`
}

type bboxBlock struct {
	Lines []bboxLine `xml:"line"`
}

// bboxLine 是一行。poppler 的坐标原点在**左上角、y 向下**，
// 而 port 要求左下角、y 向上，转换只在 toBBox 一处做。
type bboxLine struct {
	XMin  float64    `xml:"xMin,attr"`
	YMin  float64    `xml:"yMin,attr"`
	XMax  float64    `xml:"xMax,attr"`
	YMax  float64    `xml:"yMax,attr"`
	Words []bboxWord `xml:"word"`
}

type bboxWord struct {
	XMin float64 `xml:"xMin,attr"`
	YMin float64 `xml:"yMin,attr"`
	XMax float64 `xml:"xMax,attr"`
	YMax float64 `xml:"yMax,attr"`
	Text string  `xml:",chardata"`
}

// layoutPage 是转换后的中间结构：已经去掉了 flow/block 层级，只留"页 + 行"。
//
// 为什么要拍平成行：port 的 TextBlock 是"块"的语义，而前端标注与 B 的抽取都以
// "看得见的一行"为单位最合适；poppler 的 block 分组在合同这种表格化排版上很不稳定，
// 同一段话会被拆进不同 flow。
type layoutPage struct {
	Width  float64
	Height float64
	Lines  []layoutLine
}

type layoutLine struct {
	BBox [4]float64
	Text string
}

// parsePoppler 解析 XML 并拍平成页/行。
func parsePoppler(data []byte) ([]layoutPage, error) {
	var doc bboxDocument
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("解析 pdftotext 输出: %w", err)
	}
	if len(doc.Pages) == 0 {
		return nil, fmt.Errorf("pdftotext 没有输出任何页面")
	}

	pages := make([]layoutPage, 0, len(doc.Pages))
	for _, p := range doc.Pages {
		if p.Width <= 0 || p.Height <= 0 {
			return nil, fmt.Errorf("页面尺寸非法: %.2f x %.2f", p.Width, p.Height)
		}

		lp := layoutPage{Width: p.Width, Height: p.Height}
		for _, flow := range p.Flows {
			for _, block := range flow.Blocks {
				for _, line := range block.Lines {
					lb, ok := toBBox(line, p.Width, p.Height)
					if !ok {
						continue
					}
					text := joinWords(line.Words)
					if strings.TrimSpace(text) == "" {
						continue
					}
					lp.Lines = append(lp.Lines, layoutLine{BBox: lb, Text: text})
				}
			}
		}
		pages = append(pages, lp)
	}
	return pages, nil
}

// toBBox 把 poppler 的左上原点坐标翻成 port 的左下原点坐标，并夹到页面范围内。
//
// 夹取是必要的：poppler 对越界字形会给出负的 yMin 或超出页宽的 xMax（页眉页脚、
// 水印很常见），而 port 的 Valid() 会判它非法。夹到边界比整块丢弃更合理——
// 丢一块可能正好丢掉"试用期三个月"那句。
func toBBox(line bboxLine, pageWidth, pageHeight float64) ([4]float64, bool) {
	if len(line.Words) == 0 {
		return [4]float64{}, false
	}

	x0 := line.XMin
	x1 := line.XMax
	y0 := pageHeight - line.YMax // 下边界
	y1 := pageHeight - line.YMin // 上边界

	x0 = clamp(x0, 0, pageWidth)
	x1 = clamp(x1, 0, pageWidth)
	y0 = clamp(y0, 0, pageHeight)
	y1 = clamp(y1, 0, pageHeight)

	if x1-x0 <= 0 || y1-y0 <= 0 {
		return [4]float64{}, false
	}
	return [4]float64{x0, y0, x1, y1}, true
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// joinWords 把一行内的词拼回句子。
//
// 中文不能加空格（"劳动 合同法" 会变成错词），英文必须加空格（"fixed term" 不能粘成
// "fixedterm"）。判定依据是相邻字符是否属于 CJK。
func joinWords(words []bboxWord) string {
	if len(words) == 0 {
		return ""
	}

	var b strings.Builder
	for i, w := range words {
		text := strings.TrimSpace(w.Text)
		if text == "" {
			continue
		}
		if i > 0 && b.Len() > 0 && needSpace(lastRune(b.String()), firstRune(text)) {
			b.WriteByte(' ')
		}
		b.WriteString(text)
	}
	return b.String()
}

// needSpace 判断两个词之间是否需要补空格。
func needSpace(prev, next rune) bool {
	if isCJK(prev) || isCJK(next) {
		return false
	}
	return true
}

// isCJK 覆盖汉字、CJK 标点与全角符号。
//
// 全角标点必须算进来：否则 "。" 后面会被补一个半角空格，落到前端就是
// "……的。 建议" 这种多余空隙。
func isCJK(r rune) bool {
	switch {
	case unicode.Is(unicode.Han, r):
		return true
	case r >= 0x3000 && r <= 0x303F: // CJK 标点
		return true
	case r >= 0xFF00 && r <= 0xFFEF: // 全角字符
		return true
	case r >= 0x3040 && r <= 0x30FF: // 日文假名（合同里偶有）
		return true
	default:
		return false
	}
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	var last rune
	for _, r := range s {
		last = r
	}
	return last
}
