package retrieval

import (
	"strings"

	"laborlaw/internal/port"
)

// StatuteDocument 把一条法条拼成用于向量化与重排的文本。
//
// 法律名与条号必须进去：只嵌正文时"第八十五条"与"第八十七条"的文本几乎一样，
// 语义向量也几乎一样，用户问"二倍工资"会两条都召回到差不多高的位置。
//
// 空字段直接跳过而不是留占位：种子数据里标题可能为空，拼出两个连续空格会让
// 同一批法条的文本格式前后不一致，而向量是对这种差异敏感的。
//
// 抽成共享函数的理由：重排时喂给交叉编码器的文本必须与建向量时喂给嵌入模型的
// 文本完全一致，否则"索引里算的相似度"和"重排时读到的内容"是两个东西，
// 精度对不上时根本无从排查到底差在哪一环。
func StatuteDocument(s port.Statute) string {
	parts := make([]string, 0, 4)
	for _, part := range []string{s.Law, s.Article, s.Title, s.Text} {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return strings.Join(parts, " ")
}
