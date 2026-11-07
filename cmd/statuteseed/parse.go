package main

import (
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"laborlaw/internal/port"
)

// sourceSpec 描述一部法律的权威原文来源与编号规则。
//
// Expected 是硬校验：解析出来的条数、条号连续性都必须与之一致，否则直接失败。
// 宁可生成不了种子，也不能让缺条、串条的法条进库——用户拿到的条文必须逐字可信。
type sourceSpec struct {
	// File 是 source 目录下的 HTML 文件名。
	File string
	// Law 是法律全称，与 statutes.law 一致。
	Law string
	// IDPrefix 是业务 ID 前缀，如 LCL。
	IDPrefix string
	// URL 是官方来源链接，随条文进库供前端跳转核对。
	URL string
	// Effective 是施行日期，格式 2006-01-02。
	Effective string
	// Expected 是该法现行有效版本的总条数。
	Expected int
}

// sources 是当前收录的两部法律。
//
// 《劳动合同法》必须用 2012 年修正版：2007 年原文的劳务派遣条款（第五十七条注册资本、
// 第六十六条岗位限制、第九十二条罚则）已被修改，抓错版本会把已废止的规定推给用户。
var sources = []sourceSpec{
	{
		File:      "labor_contract_law.html",
		Law:       "中华人民共和国劳动合同法",
		IDPrefix:  "LCL",
		URL:       "https://www.shanghai.gov.cn/rcjygj/20250606/658d064be68c46fc892530a8cde92258.html",
		Effective: "2008-01-01",
		Expected:  98,
	},
	{
		File:      "labor_law.html",
		Law:       "中华人民共和国劳动法",
		IDPrefix:  "LL",
		URL:       "http://www.gd.gov.cn/zwgk/wjk/zcfgk/content/mpost_2532147.html",
		Effective: "1995-01-01",
		Expected:  107,
	},
}

var (
	paragraphRe = regexp.MustCompile(`(?is)<p\b[^>]*>(.*?)</p>`)
	breakRe     = regexp.MustCompile(`(?i)<br\s*/?>`)
	tagRe       = regexp.MustCompile(`(?s)<[^>]*>`)
	// 条号与条旨、正文之间可能是全角空格 U+3000，Go 的 \s 只认 ASCII 空白，必须显式列出。
	articleRe = regexp.MustCompile(`^第([零一二三四五六七八九十百千]+)条[\s　]*(?:【(.*?)】)?[\s　]*(.*)$`)
	chapterRe = regexp.MustCompile(`^第[零一二三四五六七八九十百千]+[章节]`)
)

// statuteJSON 是种子文件的序列化形状。
//
// 不直接序列化 port.Statute：它没有 json tag，输出会是大写字段名，
// 而种子文件要给人看、给版本管理 diff，统一用 snake_case。
type statuteJSON struct {
	ID          string `json:"id"`
	Law         string `json:"law"`
	Article     string `json:"article"`
	Title       string `json:"title,omitempty"`
	Text        string `json:"text"`
	URL         string `json:"url"`
	EffectiveAt string `json:"effective_at"`
}

func toJSON(s port.Statute) statuteJSON {
	return statuteJSON{
		ID:          s.ID,
		Law:         s.Law,
		Article:     s.Article,
		Title:       s.Title,
		Text:        s.Text,
		URL:         s.URL,
		EffectiveAt: s.EffectiveAt.Format("2006-01-02"),
	}
}

// paragraphs 把 HTML 切成去掉标签的非空文本行。
//
// 只认 <p> 是因为两个权威来源的正文都包在 <p> 里，页脚的脚本、分享组件不是，
// 这个限制顺带把导航与页脚噪声挡在外面。
func paragraphs(raw string) []string {
	matches := paragraphRe.FindAllStringSubmatch(raw, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		s := breakRe.ReplaceAllString(m[1], "\n")
		s = tagRe.ReplaceAllString(s, "")
		s = html.UnescapeString(s)
		for _, line := range strings.Split(s, "\n") {
			// TrimSpace 走 unicode.IsSpace，全角空格 U+3000 也在其中。
			if line = strings.TrimSpace(line); line != "" {
				out = append(out, line)
			}
		}
	}
	return out
}

// parseStatutes 从权威原文 HTML 解析出一部法律的全部条文。
//
// 两种排版都支持：
//   - 条号独立成段：「第一条【立法宗旨】」后接若干正文段（劳动合同法）
//   - 条号与正文同段：「第一条　为了保护…」（劳动法）
func parseStatutes(raw string, spec sourceSpec) ([]port.Statute, error) {
	effective, err := time.Parse("2006-01-02", spec.Effective)
	if err != nil {
		return nil, fmt.Errorf("%s: 施行日期: %w", spec.Law, err)
	}

	var (
		out  []port.Statute
		cur  *port.Statute
		body []string
	)
	flush := func() {
		if cur == nil {
			return
		}
		cur.Text = strings.TrimSpace(strings.Join(body, "\n"))
		out = append(out, *cur)
		cur, body = nil, nil
	}

	next := 1
	for _, line := range paragraphs(raw) {
		if m := articleRe.FindStringSubmatch(line); m != nil {
			n, convErr := chineseNumber(m[1])
			if convErr != nil {
				return nil, fmt.Errorf("%s: 条号 %q: %w", spec.Law, m[1], convErr)
			}
			// 只接受"正好是下一条"的条号。
			// 条文内部可能以"第四十七条规定…"这样的引用起头，若不加这道闸，
			// 引用会被当成新条文，把一条完整的条文拦腰切断。
			if n != next {
				if cur != nil {
					body = append(body, line)
				}
				continue
			}
			flush()
			cur = &port.Statute{
				ID:          spec.IDPrefix + "-" + strconv.Itoa(n),
				Law:         spec.Law,
				Article:     "第" + m[1] + "条",
				Title:       strings.TrimSpace(m[2]),
				URL:         spec.URL,
				EffectiveAt: effective,
			}
			body = nil
			if rest := strings.TrimSpace(m[3]); rest != "" {
				body = append(body, rest)
			}
			next++
			continue
		}
		if cur == nil {
			continue // 目录、页眉、法律名称等前置内容
		}
		if chapterRe.MatchString(line) {
			continue // 章/节标题不属于条文原文
		}
		body = append(body, line)
	}
	flush()

	if err := validate(out, spec); err != nil {
		return nil, err
	}
	return out, nil
}

// validate 校验条数、条号连续性与正文非空。
func validate(out []port.Statute, spec sourceSpec) error {
	if len(out) != spec.Expected {
		return fmt.Errorf("%s: 解析出 %d 条，预期 %d 条，原文可能抓取不完整", spec.Law, len(out), spec.Expected)
	}
	seen := make(map[int]bool, len(out))
	for _, s := range out {
		n, err := strconv.Atoi(strings.TrimPrefix(s.ID, spec.IDPrefix+"-"))
		if err != nil {
			return fmt.Errorf("%s: 非法 ID %q", spec.Law, s.ID)
		}
		if seen[n] {
			return fmt.Errorf("%s: 条号重复 %s", spec.Law, s.ID)
		}
		seen[n] = true
		if strings.TrimSpace(s.Text) == "" {
			return fmt.Errorf("%s: %s 正文为空", spec.Law, s.ID)
		}
	}
	for n := 1; n <= spec.Expected; n++ {
		if !seen[n] {
			return fmt.Errorf("%s: 缺第 %d 条", spec.Law, n)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, _ := strconv.Atoi(strings.TrimPrefix(out[i].ID, spec.IDPrefix+"-"))
		b, _ := strconv.Atoi(strings.TrimPrefix(out[j].ID, spec.IDPrefix+"-"))
		return a < b
	})
	return nil
}

// chineseNumber 把「三十八」「一百零七」这类中文数词转成阿拉伯数字。
//
// 条号是中文的，所以 ID、排序、引用跳转都要先过这一步。
// 支持到千位，远超两部法律的最大条号。
func chineseNumber(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("空数词")
	}
	digits := map[rune]int{
		'零': 0, '一': 1, '二': 2, '三': 3, '四': 4,
		'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
	}
	section, number := 0, 0
	for _, r := range s {
		if d, ok := digits[r]; ok {
			number = d
			continue
		}
		var unit int
		switch r {
		case '十':
			unit = 10
		case '百':
			unit = 100
		case '千':
			unit = 1000
		default:
			return 0, fmt.Errorf("无法识别的数词字符 %q", r)
		}
		// 「十五」的「十」前省略「一」。
		if number == 0 {
			number = 1
		}
		section += number * unit
		number = 0
	}
	return section + number, nil
}
