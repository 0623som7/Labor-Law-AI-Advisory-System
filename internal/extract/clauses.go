package extract

import (
	"regexp"
	"strings"

	"laborlaw/internal/port"
	"laborlaw/internal/rules"
)

// reArticle 匹配条款开头："第一条"、"第十二条"、"第 二 条"。
//
// 只看块首：条款正文里也会出现"依照第三十八条"，那不是新条款的开始。
var reArticle = regexp.MustCompile(`^第([一二三四五六七八九十百零两]+)条`)

// reArticleSpaces 用来剥掉块首的字间空格，"第 二 条"要先压成"第二条"。
var reArticleSpaces = regexp.MustCompile(`\s+`)

// clausesFromBlocks 从解析出的文本块切分条款，坐标取真实版面值。
//
// 模型看不到 PDF 版面，它给的 page/bbox 是猜的；这里直接从 TextBlock 拿，
// 顺带把 review.go 里"先让模型编坐标、再用字符串回填"的绕路整条拆掉。
// 一个条号都没命中时返回空切片（不是 nil）：调用方据此退回模型给的条款。
func clausesFromBlocks(blocks []port.TextBlock) []rules.Clause {
	out := make([]rules.Clause, 0, 16)

	var current *rules.Clause
	var texts []string
	hasBox := false
	var box [4]float64

	flush := func() {
		if current == nil {
			return
		}
		current.Text = strings.Join(texts, "\n")
		if hasBox {
			current.BBox = box
		}
		out = append(out, *current)
	}

	for _, b := range blocks {
		text := strings.TrimSpace(b.Text)
		if text == "" {
			continue
		}
		// 只对块首做去空格匹配：整块去掉空白会影响正文该有的形态。
		head := reArticleSpaces.ReplaceAllString(text, "")
		if m := reArticle.FindStringSubmatch(head); m != nil {
			flush()
			current = &rules.Clause{
				Section: "第" + m[1] + "条",
				Page:    b.Page,
			}
			texts = []string{text}
			box = b.BBox
			hasBox = true
			continue
		}

		if current == nil {
			// 第一个条号之前的抬头、当事人信息不属于任何条款，丢掉。
			continue
		}
		texts = append(texts, text)
		if !hasBox {
			box, hasBox = b.BBox, true
			continue
		}
		box = unionBox(box, b.BBox)
	}

	flush()
	return out
}

// unionBox 取两个 bbox 的并集。
//
// 一条条款常跨两三个块（模板按行切块），只标第一块会让用户以为漏标了一半。
func unionBox(a, b [4]float64) [4]float64 {
	return [4]float64{
		minf(a[0], b[0]),
		minf(a[1], b[1]),
		maxf(a[2], b[2]),
		maxf(a[3], b[3]),
	}
}

func minf(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

func maxf(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
