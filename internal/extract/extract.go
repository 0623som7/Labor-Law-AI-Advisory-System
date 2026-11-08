// Package extract 从合同全文中抽取审查所需的结构化要素。
//
// 为什么单独成包而不写在 agent 里：
//   - 抽取是整条审查链路里唯一"不确定"的一步，必须能被固定样例反复测；
//   - 解析规则（围栏、字段缺失、类型纠偏）与编排失败重试是两件事，混在一起以后
//     调抽取 prompt 会牵动 worker 的重试语义。
//
// 铁律：模型只做抽取，不做判断。抽取结果里出现的每一条 raw_clauses 都必须能在
// 原文中找到，找不到就是模型的幻觉，规则层会据此给出错误建议——所以 prompt 里
// 明令"不许改写、不许合并、不许补写"。
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"laborlaw/internal/port"
	"laborlaw/internal/rules"
)

// Scene 是抽取调用在 A 层场景路由里的键。
//
// 与 review.advise 分开：抽取要便宜且快的模型，出建议要强模型，
// 用同一个 scene 会让成本闸失去意义。
const Scene = "review.extract"

// 输入与输出的规模上限。
//
// 输入截断：整份合同塞进 prompt 会顶爆上下文，而审查需要的条款集中在正文前中部；
// 12000 个字符约等于 8 页 A4，超过这个长度说明用户上传的不是一份普通劳动合同。
// 输出上限与 15 个字段 + 若干条 raw_clauses 的规模匹配，给多了模型只会开始编。
const (
	maxInputRunes   = 12000
	maxOutputTokens = 2000
)

// trackedFields 参与 confidence 计算的字段数（14 个标量要素）。
//
// raw_clauses 不计入：一份合同没有条款列表本身就是异常，若把它算进分母，
// 抽取失败时会以"confidence 略低"的温和形态掩盖过去，而不是让调用方看见 Missing。
const trackedFields = 14

// Document 是一次抽取的完整结果，也是 contract_extracts.payload 的字段定义。
type Document struct {
	rules.Contract

	// Missing 列出模型没有给出的字段（JSON 名），规则层据此避免误判。
	Missing []string `json:"missing"`
	// Truncated 表示原文超过 maxInputRunes 被截断，审查结论只覆盖前 TruncatedAt 字。
	//
	// 必须透出来：截断是"看起来正常但结论不完整"，比直接报错更危险——
	// 20 页合同只分析了前 8 页，后面的风险条款一条都报不出来，界面上却毫无提示。
	Truncated bool `json:"truncated"`
	// TruncatedAt 是实际参与抽取的字符数（rune）；未截断时等于原文长度。
	TruncatedAt int `json:"truncated_at"`
	// Confidence 是命中字段数 / 14，用于前端提示"识别可能不完整"。
	Confidence float64 `json:"confidence"`
}

// ParseError 表示模型返回的正文不是可用的抽取结果。
//
// 刻意不用 port.AppError：这类失败重试一次还是同样的输出（模型对同一段文本
// 的格式错误是稳定的），按不可重试处理，worker 不会为它烧三次 token。
type ParseError struct {
	// Reason 是给日志看的原因，不进响应体。
	Reason string
	// Raw 是模型原文的前 200 字，便于排查"为什么解析不了"。
	Raw string
}

// Error 实现 error。
func (e *ParseError) Error() string {
	return fmt.Sprintf("抽取结果解析失败：%s（原文开头：%q）", e.Reason, e.Raw)
}

// Extractor 把合同全文转成结构化要素。
type Extractor struct {
	client port.Client
}

// New 构造抽取器。client 为 nil 时不 panic，调用 Extract 才返回可读的错误——
// 装配期少传一个参数，应当在请求进来时以 50001 暴露，而不是让进程起不来。
func New(client port.Client) *Extractor { return &Extractor{client: client} }

// Extract 调用模型并严格解析结果。
func (e *Extractor) Extract(ctx context.Context, text string) (*Document, error) {
	if e.client == nil {
		return nil, port.New(port.CodeInternal, "模型客户端未装配，无法抽取合同要素")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, port.New(port.CodeInvalidArgument, "合同正文为空，无法抽取要素")
	}
	// 先记全文长度再截：rune 计数口径与 truncateRunes 一致，
	// 否则中文合同上两个数字会差三倍。
	totalRunes := len([]rune(text))
	text = truncateRunes(text, maxInputRunes)

	resp, err := e.client.Chat(ctx, port.ChatRequest{
		Scene: Scene,
		Messages: []port.Message{
			{Role: port.RoleSystem, Content: systemPrompt},
			{Role: port.RoleUser, Content: text},
		},
		MaxTokens:  maxOutputTokens,
		JSONSchema: &port.JSONSchema{Name: "contract_extract", Schema: json.RawMessage(schemaJSON), Strict: true},
	})
	if err != nil {
		return nil, wrapUpstream(err)
	}

	doc, err := Parse(resp.Content)
	if err != nil {
		return nil, err
	}
	// 截断标记挂在结果上而不是日志里：调用方要据此告诉用户"只看了前 N 字"，
	// 只写日志等于把它埋进运维视野。
	doc.TruncatedAt = len([]rune(text))
	doc.Truncated = totalRunes > maxInputRunes
	return doc, nil
}

// Parse 把模型正文解析成 Document，独立导出以便用固定样例测解析规则。
func Parse(content string) (*Document, error) {
	body := stripCodeFence(content)
	if body == "" {
		return nil, &ParseError{Reason: "模型返回空内容", Raw: ""}
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		return nil, &ParseError{Reason: "不是合法的 JSON 对象", Raw: head(content)}
	}
	if top == nil {
		return nil, &ParseError{Reason: "顶层不是 JSON 对象", Raw: head(content)}
	}

	doc := &Document{}
	present := 0

	stringFields := []struct {
		key  string
		dest *string
	}{
		{"party_a", &doc.PartyA},
		{"party_b", &doc.PartyB},
		{"contract_type", &doc.ContractType},
		{"term_start", &doc.TermStart},
		{"term_end", &doc.TermEnd},
		{"work_location", &doc.WorkLocation},
		{"position", &doc.Position},
		{"working_hours", &doc.WorkingHours},
	}
	for _, f := range stringFields {
		v, ok, err := decodeString(top, f.key)
		if err != nil {
			return nil, err
		}
		if ok && v != "" {
			*f.dest = v
			present++
		}
	}

	numberFields := []struct {
		key  string
		dest *float64
	}{
		{"probation_months", &doc.ProbationMonths},
		{"salary_monthly", &doc.SalaryMonthly},
		{"salary_probation", &doc.SalaryProbation},
	}
	for _, f := range numberFields {
		v, ok, err := decodeNumber(top, f.key)
		if err != nil {
			return nil, err
		}
		if ok {
			*f.dest = v
			present++
		}
	}

	boolFields := []struct {
		key  string
		dest **bool
	}{
		{"social_insurance", &doc.SocialInsurance},
		{"has_overtime_pay", &doc.HasOvertimePay},
		{"deposit", &doc.Deposit},
	}
	for _, f := range boolFields {
		v, ok, err := decodeBool(top, f.key)
		if err != nil {
			return nil, err
		}
		if ok {
			*f.dest = boolPtr(v)
			present++
		}
	}

	clauses, err := decodeClauses(top)
	if err != nil {
		return nil, err
	}
	doc.Clauses = clauses

	doc.Missing = missingFields(doc)
	doc.Confidence = float64(present) / float64(trackedFields)
	if doc.Confidence > 1 {
		doc.Confidence = 1
	}
	return doc, nil
}

// ---- 解码工具 ----

// decodeString 读取一个字符串字段；字段缺失返回 ok=false，类型不对返回 ParseError。
//
// 类型不对必须报错而不是当缺失：把 {"salary_monthly":"面议"} 静默吞成 0 会让
// 后续"低于最低工资"规则不触发，用户看到一份"没有风险"的合同，比报错危险得多。
func decodeString(top map[string]json.RawMessage, key string) (string, bool, error) {
	raw, exists := top[key]
	if !exists || isJSONNull(raw) {
		return "", false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false, &ParseError{Reason: fmt.Sprintf("字段 %s 不是字符串", key), Raw: head(string(raw))}
	}
	return strings.TrimSpace(s), true, nil
}

// decodeNumber 读取数字字段，容忍模型把数字写成 "8000" 或 "8000元"。
//
// 容忍字符串是必要的：真实调用里模型经常把金额写成带单位的字符串，
// 严格按数字解析会让一份本来能审的合同直接失败。
func decodeNumber(top map[string]json.RawMessage, key string) (float64, bool, error) {
	raw, exists := top[key]
	if !exists || isJSONNull(raw) {
		return 0, false, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err == nil {
		return f, true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if v, ok := parseLooseNumber(s); ok {
			return v, true, nil
		}
		return 0, false, &ParseError{Reason: fmt.Sprintf("字段 %s 无法解析为数字：%q", key, s), Raw: head(string(raw))}
	}
	return 0, false, &ParseError{Reason: fmt.Sprintf("字段 %s 既不是数字也不是字符串", key), Raw: head(string(raw))}
}

// decodeBool 读取布尔字段，容忍 "是/否/有/无" 这类中文回答。
func decodeBool(top map[string]json.RawMessage, key string) (bool, bool, error) {
	raw, exists := top[key]
	if !exists || isJSONNull(raw) {
		return false, false, nil
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, true, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		switch strings.TrimSpace(s) {
		case "true", "是", "有", "已缴纳", "缴纳", "有社保":
			return true, true, nil
		case "false", "否", "无", "未缴纳", "不缴纳", "没有":
			return false, true, nil
		}
		return false, false, &ParseError{Reason: fmt.Sprintf("字段 %s 无法解析为布尔值：%q", key, s), Raw: head(string(raw))}
	}
	return false, false, &ParseError{Reason: fmt.Sprintf("字段 %s 既不是布尔也不是字符串", key), Raw: head(string(raw))}
}

// decodeClauses 读取条款列表；缺失视为空列表，而不是错误。
func decodeClauses(top map[string]json.RawMessage) ([]rules.Clause, error) {
	raw, exists := top["raw_clauses"]
	if !exists || isJSONNull(raw) {
		return nil, nil
	}
	var items []struct {
		Section string      `json:"section"`
		Page    json.Number `json:"page"`
		BBox    []float64   `json:"bbox"`
		Text    string      `json:"text"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, &ParseError{Reason: "raw_clauses 不是对象数组", Raw: head(string(raw))}
	}
	out := make([]rules.Clause, 0, len(items))
	for i, it := range items {
		var page int
		if it.Page != "" {
			v, err := it.Page.Int64()
			if err != nil {
				return nil, &ParseError{Reason: fmt.Sprintf("raw_clauses[%d].page 不是整数", i), Raw: head(string(raw))}
			}
			page = int(v)
		}
		var bbox [4]float64
		if len(it.BBox) == 4 {
			copy(bbox[:], it.BBox)
		} else if len(it.BBox) != 0 {
			return nil, &ParseError{Reason: fmt.Sprintf("raw_clauses[%d].bbox 必须是 4 个数字", i), Raw: head(string(raw))}
		}
		out = append(out, rules.Clause{
			Section: strings.TrimSpace(it.Section),
			Page:    page,
			BBox:    bbox,
			Text:    strings.TrimSpace(it.Text),
		})
	}
	return out, nil
}

// missingFields 列出没有抽到的字段，顺序稳定（与 trackedFields 的字段顺序一致）。
func missingFields(doc *Document) []string {
	missing := make([]string, 0, 4)
	if doc.PartyA == "" {
		missing = append(missing, "party_a")
	}
	if doc.PartyB == "" {
		missing = append(missing, "party_b")
	}
	if doc.ContractType == "" {
		missing = append(missing, "contract_type")
	}
	if doc.TermStart == "" {
		missing = append(missing, "term_start")
	}
	if doc.TermEnd == "" {
		missing = append(missing, "term_end")
	}
	if doc.ProbationMonths == 0 {
		missing = append(missing, "probation_months")
	}
	if doc.SalaryMonthly == 0 {
		missing = append(missing, "salary_monthly")
	}
	if doc.SalaryProbation == 0 {
		missing = append(missing, "salary_probation")
	}
	if doc.SocialInsurance == nil {
		missing = append(missing, "social_insurance")
	}
	if doc.WorkLocation == "" {
		missing = append(missing, "work_location")
	}
	if doc.Position == "" {
		missing = append(missing, "position")
	}
	if doc.WorkingHours == "" {
		missing = append(missing, "working_hours")
	}
	if doc.HasOvertimePay == nil {
		missing = append(missing, "has_overtime_pay")
	}
	if doc.Deposit == nil {
		missing = append(missing, "deposit")
	}
	return missing
}

// stripCodeFence 去掉 ```json 围栏。
//
// 即便开了 Strict 的 JSON schema，仍有上游会包一层 markdown 围栏；
// 这里只剥最外层围栏，不做"找第一个 { 到最后一个 }"那种宽容兜底——
// 那种兜底会把"解释文字 + JSON + 解释文字"也抢救回来，让 prompt 退化不被发现。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if idx := strings.Index(s, "\n"); idx >= 0 {
		s = s[idx+1:]
	}
	if idx := strings.LastIndex(s, "```"); idx >= 0 {
		s = s[:idx]
	}
	return strings.TrimSpace(s)
}

// wrapUpstream 给上游错误补上下文，但保留原错误码。
//
// 保留码是关键：worker 按 Code.Retryable() 决定要不要重试，把 50201 折成 50001
// 会让一次上游抖动变成"不可重试的失败"，用户必须手动再点一次。
func wrapUpstream(err error) error {
	code := port.CodeOf(err)
	switch code {
	case port.CodeUpstream, port.CodeUpstreamTimeout, port.CodeRateLimited:
		return port.Wrap(code, err, "调用模型抽取合同要素失败")
	default:
		return err
	}
}

// isJSONNull 判断字段是否为显式 null。
func isJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// parseLooseNumber 解析 "8000"、"8000元"、"8,000"、"1.5万" 这类写法。
func parseLooseNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, ",", "")
	multiplier := 1.0
	for _, suffix := range []string{"元/月", "元", "个月", "月"} {
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	if strings.HasSuffix(s, "万") {
		s = strings.TrimSuffix(s, "万")
		multiplier = 10000
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0, false
	}
	return v * multiplier, true
}

// truncateRunes 按字符截断，避免把汉字劈成半个。
func truncateRunes(s string, limit int) string {
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// head 截取前 200 个字符用于错误信息。
func head(s string) string {
	return truncateRunes(strings.TrimSpace(s), 200)
}

// boolPtr 返回指向 b 的指针。
func boolPtr(b bool) *bool { return &b }

// systemPrompt 是抽取提示词。
//
// 三条硬约束按重要性排序：
//  1. 只输出 JSON——多一句解释就解析失败，重试还要再花钱；
//  2. 不许编造条款——raw_clauses 是风险定位与证据的唯一来源，写进来的每条都要能在原文里找到；
//  3. 抽不到留空——"猜一个合理值"会让规则层给出错误的合规结论。
const systemPrompt = `你是劳动法合同要素抽取器。从用户给出的劳动合同全文中抽取结构化要素。

输出要求：
1. 只输出一个 JSON 对象，不要 markdown 围栏，不要任何解释文字。
2. 只抽取原文中明确出现的信息，不许编造、不许改写、不许合并条款；原文没有的字段留空（字符串 ""、数字 0、布尔 null），并在 missing 里列出字段名。
3. raw_clauses 必须逐条摘录原文：section 为条款序号（如"第三条"），page 为所在页（从 0 开始，不确定填 0），bbox 为 [x0,y0,x1,y1]（不确定填 [0,0,0,0]），text 为原文逐字摘录，不得改写或概括。
4. 金额单位统一为元/月，试用期长度单位为月；字段名严格使用下面的键名。

字段：
party_a 甲方名称；party_b 乙方姓名；contract_type 合同类型（固定期限/无固定期限/以完成一定工作任务为期限）；
term_start 起始日（YYYY-MM-DD）；term_end 终止日（YYYY-MM-DD，无固定期限留空）；
probation_months 试用期月数（数字）；salary_monthly 转正月工资（数字）；salary_probation 试用期月工资（数字）；
social_insurance 是否约定缴纳社保（true/false）；work_location 工作地点；position 岗位；
working_hours 工作时间制度；has_overtime_pay 是否约定加班费（true/false）；
deposit 是否约定押金/扣押证件（true/false）；raw_clauses 条款数组。`

// schemaJSON 是给上游的结构化输出约束。
//
// 与 prompt 重复不是冗余：prompt 管语义，schema 管形状。
// 只有 schema 能保证"字段名不变、类型不变"，而语义仍然要靠 prompt 约束。
const schemaJSON = `{
  "type": "object",
  "properties": {
    "party_a": {"type": "string"},
    "party_b": {"type": "string"},
    "contract_type": {"type": "string"},
    "term_start": {"type": "string"},
    "term_end": {"type": "string"},
    "probation_months": {"type": "number"},
    "salary_monthly": {"type": "number"},
    "salary_probation": {"type": "number"},
    "social_insurance": {"type": ["boolean", "null"]},
    "work_location": {"type": "string"},
    "position": {"type": "string"},
    "working_hours": {"type": "string"},
    "has_overtime_pay": {"type": ["boolean", "null"]},
    "deposit": {"type": ["boolean", "null"]},
    "raw_clauses": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "section": {"type": "string"},
          "page": {"type": "integer"},
          "bbox": {"type": "array", "items": {"type": "number"}},
          "text": {"type": "string"}
        },
        "required": ["section", "text"]
      }
    }
  },
  "required": ["raw_clauses"]
}`
