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
// 的逗号，从那里砍掉残尾，再按当时的括号栈反向补齐——栈里记的是**种类**
// （`{` 与 `[`），不是深度。只记深度会在条款数组里补错：截断点在
// `"clauses": [{"section": …` 时，补 `}}}` 得到 `[{"…"}}}`，方括号永远没闭合，
// 修复函数返回 ok=true 却给出非法 JSON，等于这段修复对条款数组从未生效过
// （而条款数组正是 40 页合同真正被截断的部分）。
//
// 砍不出任何字段时退回空对象——空对象仍是合法 JSON，走正常"字段缺失"路径，
// 比整单失败好。
//
// 刻意不做"前后各垫一段解释文字"的抢救：那种宽容会让 prompt 退化不被发现。
func repairTruncatedObject(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}

	// stack 只存 '{' 与 '['，用于校验配对并在收尾时反向补齐。
	stack := make([]byte, 0, 8)
	inString := false
	escaped := false
	cutAt := -1
	var cutStack []byte

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
			stack = append(stack, c)
		case '}', ']':
			want := byte('{')
			if c == ']' {
				want = '['
			}
			if len(stack) == 0 || stack[len(stack)-1] != want {
				// 括号不配对说明这根本不是"被截断"，交给严格解析报错。
				return "", false
			}
			stack = stack[:len(stack)-1]
		case ',':
			if len(stack) >= 1 && (len(stack) == 1 || stack[len(stack)-1] == '[') {
				// 只认"元素之间"的逗号：根对象的一级字段之间，或数组的相邻元素之间。
				// 对象内部（`{"section": "第二条", "te` 那种）的逗号不能当切点——
				// 在那里砍会留下一个缺字段的半条条款，规则层拿到空 Text 反而更难判断。
				cutAt = i
				// 快照必须是**此刻**的栈：循环结束时的栈还包含被砍掉的那截残尾开出的括号。
				cutStack = append(cutStack[:0], stack...)
			}
		}
	}

	if len(stack) == 0 && !inString {
		// 括号已经配平，调用方本不该走到这里；原样返回交给严格解析判断。
		return s, true
	}
	if cutAt <= 0 {
		// 连一个完整字段都没有（例如 `{"party_a": "东`）：这不是"能救一半"，
		// 而是完全无法判断内容。交回调用方按解析失败处理，别把垃圾当空对象放过。
		return "", false
	}

	var b strings.Builder
	b.WriteString(s[:cutAt])
	for i := len(cutStack) - 1; i >= 0; i-- {
		if cutStack[i] == '[' {
			b.WriteByte(']')
			continue
		}
		b.WriteByte('}')
	}
	return b.String(), true
}
