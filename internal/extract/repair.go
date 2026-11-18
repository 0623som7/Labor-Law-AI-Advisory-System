package extract

import "strings"

// repairTruncatedObject 把被上游截断的 JSON 对象补成能解析的样子。
//
// 为什么需要它：max_tokens 到顶时上游不会报错，只会把正文切在半截上
// （线上 40 页合同拿到的是 `"social_insurance": tr`），整份抽取结果
// 于是一个字段都留不下。而截断前的那些字段其实是有效证据，丢掉等于
// 把"能审出来的部分"也一起扔了。
//
// 做法只认一种情况：对象被从中间截断。找到最后一个"在对象里、不在字符串里"
// 的逗号，从那里砍掉残尾，再按当时的嵌套深度补上右括号。砍不出任何字段
// 时退回空对象——空对象仍是合法 JSON，走正常"字段缺失"路径，比整单失败好。
//
// 刻意不做"前后各垫一段解释文字"的抢救：那种宽容会让 prompt 退化不被发现。
func repairTruncatedObject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}

	depth := 0
	inString := false
	escaped := false
	cutAt := -1
	cutDepth := 0

	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth < 0 {
				// 多出来的右括号说明这根本不是"被截断"，交给严格解析报错。
				return "", false
			}
		case ',':
			if depth >= 1 {
				cutAt = i
				cutDepth = depth
			}
		}
	}

	if depth == 0 && !inString {
		// 括号已经配平，调用方本不该走到这里；原样返回交给严格解析判断。
		return s, true
	}
	if cutAt <= 0 {
		// 连一个完整字段都没有（例如 `{"party_a": "东`）：这不是"能救一半"，
		// 而是完全无法判断内容。交回调用方按解析失败处理，别把垃圾当空对象放过。
		return "", false
	}
	return s[:cutAt] + strings.Repeat("}", cutDepth), true
}
