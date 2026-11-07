package api

import (
	"time"

	"laborlaw/internal/port"
)

// 本文件是审查与任务在 HTTP 上的形状，字段名逐字来自
// contract/openapi.yaml 的 components.schemas.Review / Finding / Task。
//
// 与 dto_file.go 同样的理由：port 结构体不带 json tag，直接序列化会漏出
// FindID / RiskLevel 这种内部命名；先例是 internal/stream/frame.go 的 wireEvent。

// reviewCountsDTO 对应 schema ReviewCounts。
type reviewCountsDTO struct {
	High   int `json:"high"`
	Medium int `json:"medium"`
	Low    int `json:"low"`
}

// reviewDTO 对应 schema Review。
type reviewDTO struct {
	ID             string          `json:"id"`
	FileID         string          `json:"file_id"`
	Status         string          `json:"status"`
	RiskLevel      string          `json:"risk_level"`
	Score          int             `json:"score"`
	ClausePassRate float64         `json:"clause_pass_rate"`
	Counts         reviewCountsDTO `json:"counts"`
	TaskID         string          `json:"task_id"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// toReviewDTO 映射审查主记录。nil 返回零值而不是 panic。
func toReviewDTO(rv *port.Review) reviewDTO {
	if rv == nil {
		return reviewDTO{}
	}
	return reviewDTO{
		ID:             rv.ID,
		FileID:         rv.FileID,
		Status:         string(rv.Status),
		RiskLevel:      string(rv.RiskLevel),
		Score:          rv.Score,
		ClausePassRate: rv.ClausePassRate,
		Counts: reviewCountsDTO{
			High:   rv.Counts.High,
			Medium: rv.Counts.Medium,
			Low:    rv.Counts.Low,
		},
		TaskID:    rv.TaskID,
		CreatedAt: rv.CreatedAt,
		UpdatedAt: rv.UpdatedAt,
	}
}

// toReviewDTOs 批量映射；空切片保证序列化成 [] 而不是 null。
func toReviewDTOs(items []port.Review) []reviewDTO {
	out := make([]reviewDTO, 0, len(items))
	for i := range items {
		out = append(out, toReviewDTO(&items[i]))
	}
	return out
}

// reviewAcceptedDTO 对应 schema ReviewAccepted。
type reviewAcceptedDTO struct {
	TaskID   string `json:"task_id"`
	ReviewID string `json:"review_id"`
	Status   string `json:"status"`
}

// clauseRefDTO 对应 schema ClauseRef。
//
// bbox 必须是 [4]float64：序列化成数组，前端 PDF 标注层直接按下标取。
// 改成对象会让前端拿到 undefined 并画出一个 0×0 的框，错得最安静。
type clauseRefDTO struct {
	Section string     `json:"section"`
	Page    int        `json:"page"`
	BBox    [4]float64 `json:"bbox"`
}

// legalBasisDTO 对应 schema LegalBasis。text 必须原样来自 statutes 表。
type legalBasisDTO struct {
	Law     string `json:"law"`
	Article string `json:"article"`
	Text    string `json:"text"`
}

// suggestionDTO 对应 schema Suggestion。
type suggestionDTO struct {
	Type     string `json:"type"`
	Original string `json:"original"`
	Revised  string `json:"revised"`
	Reason   string `json:"reason"`
}

// findingDTO 对应 schema Finding。
type findingDTO struct {
	ID         string          `json:"id"`
	ReviewID   string          `json:"review_id"`
	RuleID     string          `json:"rule_id"`
	Severity   string          `json:"severity"`
	Title      string          `json:"title"`
	ClauseRef  clauseRefDTO    `json:"clause_ref"`
	Evidence   string          `json:"evidence"`
	LegalBasis []legalBasisDTO `json:"legal_basis"`
	Suggestion suggestionDTO   `json:"suggestion"`
	// EconomicImpact 是指针：schema 里是 nullable，0 元与"未估算"不是一回事。
	EconomicImpact *float64 `json:"economic_impact"`
}

// toFindingDTO 映射一条风险发现。
func toFindingDTO(f *port.Finding) findingDTO {
	if f == nil {
		return findingDTO{}
	}
	basis := make([]legalBasisDTO, 0, len(f.LegalBasis))
	for _, b := range f.LegalBasis {
		basis = append(basis, legalBasisDTO{Law: b.Law, Article: b.Article, Text: b.Text})
	}
	return findingDTO{
		ID:       f.ID,
		ReviewID: f.ReviewID,
		RuleID:   f.RuleID,
		Severity: string(f.Severity),
		Title:    f.Title,
		ClauseRef: clauseRefDTO{
			Section: f.ClauseRef.Section,
			Page:    f.ClauseRef.Page,
			BBox:    f.ClauseRef.BBox,
		},
		Evidence:   f.Evidence,
		LegalBasis: basis,
		Suggestion: suggestionDTO{
			Type:     f.Suggestion.Type,
			Original: f.Suggestion.Original,
			Revised:  f.Suggestion.Revised,
			Reason:   f.Suggestion.Reason,
		},
		EconomicImpact: f.EconomicImpact,
	}
}

// toFindingDTOs 批量映射；空切片保证序列化成 []。
func toFindingDTOs(items []port.Finding) []findingDTO {
	out := make([]findingDTO, 0, len(items))
	for i := range items {
		out = append(out, toFindingDTO(&items[i]))
	}
	return out
}

// taskDTO 对应 schema Task。
type taskDTO struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Progress  int       `json:"progress"`
	Stage     string    `json:"stage"`
	Error     string    `json:"error"`
	Attempt   int       `json:"attempt"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// toTaskDTO 映射任务；payload 是内部载荷，绝不进响应体。
func toTaskDTO(t *port.Task) taskDTO {
	if t == nil {
		return taskDTO{}
	}
	return taskDTO{
		ID:        t.ID,
		Type:      t.Type,
		Status:    string(t.Status),
		Progress:  t.Progress,
		Stage:     t.Stage,
		Error:     t.Error,
		Attempt:   t.Attempt,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}
}
