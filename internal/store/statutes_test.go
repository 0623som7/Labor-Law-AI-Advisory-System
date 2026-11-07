package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"laborlaw/internal/port"
)

// preserveStatutes 快照并在用例结束后恢复整张法条表。
//
// ReplaceAll 的语义就是全表重建，没法像 files 那样按 user_id 隔离用例。
// 不恢复的话，本地跑一次 go test 就把刚导入的 205 条法条换成了三条测试数据，
// 而法条表是问答链路的数据源，这种副作用不能留。
func preserveStatutes(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx := context.Background()

	rows, err := db.QueryContext(ctx, `SELECT `+statuteColumns+` FROM statutes`)
	if err != nil {
		t.Fatalf("快照法条表失败: %v", err)
	}
	var backup []port.Statute
	for rows.Next() {
		var (
			st        port.Statute
			effective sql.NullTime
		)
		if err := rows.Scan(&st.ID, &st.Law, &st.Article, &st.Title, &st.Text, &st.URL, &effective); err != nil {
			rows.Close()
			t.Fatalf("读取法条快照失败: %v", err)
		}
		if effective.Valid {
			st.EffectiveAt = effective.Time
		}
		backup = append(backup, st)
	}
	rows.Close()

	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM statutes`); err != nil {
			t.Errorf("清空测试法条失败: %v", err)
			return
		}
		if len(backup) == 0 {
			return
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Errorf("开启恢复事务失败: %v", err)
			return
		}
		for start := 0; start < len(backup); start += insertBatchSize {
			end := start + insertBatchSize
			if end > len(backup) {
				end = len(backup)
			}
			if err := insertStatutes(ctx, tx, backup[start:end]); err != nil {
				tx.Rollback()
				t.Errorf("恢复法条快照失败: %v", err)
				return
			}
		}
		if err := tx.Commit(); err != nil {
			t.Errorf("提交法条快照恢复失败: %v", err)
		}
	})
}

func sampleStatutes() []port.Statute {
	return []port.Statute{
		{
			ID: "LCL-47", Law: "中华人民共和国劳动合同法", Article: "第四十七条",
			Title: "经济补偿的计算",
			Text:  "经济补偿按劳动者在本单位工作的年限，每满一年支付一个月工资的标准向劳动者支付。经济补偿的年限最高不超过十二年。",
			URL:   "https://example.test/lcl",
			// 带时区的时间：DATE 列只保留日期，比对时也必须只比日期。
			EffectiveAt: time.Date(2008, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			ID: "LCL-87", Law: "中华人民共和国劳动合同法", Article: "第八十七条",
			Title: "违法解除或者终止劳动合同的法律责任",
			Text:  "用人单位违反本法规定解除或者终止劳动合同的，应当依照本法第四十七条规定的经济补偿标准的二倍向劳动者支付赔偿金。",
			URL:   "https://example.test/lcl",
		},
		{
			ID: "LL-44", Law: "中华人民共和国劳动法", Article: "第四十四条",
			Text: "有下列情形之一的，用人单位应当按照下列标准支付高于劳动者正常工作时间工资的工资报酬：安排劳动者延长工作时间的，支付不低于工资的百分之一百五十的工资报酬。",
			URL:  "https://example.test/ll",
		},
	}
}

func TestStatutesReplaceAllAndGet(t *testing.T) {
	db := testDB(t)
	preserveStatutes(t, db)
	ctx := context.Background()
	st := NewStatutes(db)
	items := sampleStatutes()

	if err := st.ReplaceAll(ctx, items); err != nil {
		t.Fatalf("ReplaceAll 失败: %v", err)
	}
	n, err := st.Count(ctx)
	if err != nil || n != len(items) {
		t.Fatalf("Count = %d, err = %v，应为 %d", n, err, len(items))
	}

	got, err := st.Get(ctx, "LCL-47")
	if err != nil {
		t.Fatalf("Get 失败: %v", err)
	}
	want := items[0]
	if got.Law != want.Law || got.Article != want.Article || got.Title != want.Title ||
		got.Text != want.Text || got.URL != want.URL {
		t.Errorf("读回内容与写入不一致: %+v", got)
	}
	if got.EffectiveAt.Format("2006-01-02") != "2008-01-01" {
		t.Errorf("施行日期 = %v，应为 2008-01-01", got.EffectiveAt)
	}

	// 没写施行日期的条文必须保持零值，不能读出 0000-00-00 之外的怪值。
	if zero, err := st.Get(ctx, "LCL-87"); err != nil || !zero.EffectiveAt.IsZero() {
		t.Errorf("未设施行日期时应为零值，got %v, err %v", zero.EffectiveAt, err)
	}

	if _, err := st.Get(ctx, "LCL-999"); !hasCode(err, port.CodeNotFound) {
		t.Errorf("不存在的法条应返回 40401，got %v", err)
	}
	if _, err := st.Get(ctx, "  "); !hasCode(err, port.CodeInvalidArgument) {
		t.Errorf("空 ID 应返回 40001，got %v", err)
	}
}

// ReplaceAll 传空等于清库，必须被挡住，而且不能动到已有数据。
func TestStatutesReplaceAllRejectsEmpty(t *testing.T) {
	db := testDB(t)
	preserveStatutes(t, db)
	ctx := context.Background()
	st := NewStatutes(db)

	if err := st.ReplaceAll(ctx, sampleStatutes()); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}
	if err := st.ReplaceAll(ctx, nil); !hasCode(err, port.CodeInvalidArgument) {
		t.Fatalf("空列表应返回 40001，got %v", err)
	}
	if n, _ := st.Count(ctx); n != 3 {
		t.Errorf("被拒绝的 ReplaceAll 不应清空表，当前 %d 行", n)
	}

	broken := sampleStatutes()
	broken[1].Text = "   "
	if err := st.ReplaceAll(ctx, broken); !hasCode(err, port.CodeInvalidArgument) {
		t.Errorf("空正文应返回 40001，got %v", err)
	}
}

func TestStatutesSearchTextRanksByHits(t *testing.T) {
	db := testDB(t)
	preserveStatutes(t, db)
	ctx := context.Background()
	st := NewStatutes(db)
	if err := st.ReplaceAll(ctx, sampleStatutes()); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	hits, err := st.SearchText(ctx, "经济补偿", "", 0)
	if err != nil {
		t.Fatalf("SearchText 失败: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("命中 %d 条，应为 2（LCL-47 与 LCL-87）", len(hits))
	}
	// LCL-47 出现两次，LCL-87 出现一次，命中次数多的排前面。
	if hits[0].Statute.ID != "LCL-47" || hits[1].Statute.ID != "LCL-87" {
		t.Errorf("排序错误: %s, %s", hits[0].Statute.ID, hits[1].Statute.ID)
	}
	if hits[0].Score <= hits[1].Score {
		t.Errorf("Score 未按命中次数递减: %v, %v", hits[0].Score, hits[1].Score)
	}

	if limited, err := st.SearchText(ctx, "经济补偿", "", 1); err != nil || len(limited) != 1 {
		t.Errorf("topK=1 应只返回 1 条，got %d, err %v", len(limited), err)
	}
}

func TestStatutesSearchTextFiltersByLaw(t *testing.T) {
	db := testDB(t)
	preserveStatutes(t, db)
	ctx := context.Background()
	st := NewStatutes(db)
	if err := st.ReplaceAll(ctx, sampleStatutes()); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	hits, err := st.SearchText(ctx, "工资", "中华人民共和国劳动法", 0)
	if err != nil {
		t.Fatalf("SearchText 失败: %v", err)
	}
	for _, h := range hits {
		if h.Statute.Law != "中华人民共和国劳动法" {
			t.Errorf("law 过滤失效，混入了 %s", h.Statute.Law)
		}
	}
}

// 用户问"扣 50% 工资合法吗"，% 不转义就是全表通配。
func TestStatutesSearchTextEscapesWildcards(t *testing.T) {
	db := testDB(t)
	preserveStatutes(t, db)
	ctx := context.Background()
	st := NewStatutes(db)

	items := []port.Statute{
		{ID: "T-1", Law: "测试法", Article: "第一条", Text: "用人单位扣减工资不得超过百分之五十，即 50%。"},
		{ID: "T-2", Law: "测试法", Article: "第二条", Text: "这里的 500 元罚款不包含百分号。"},
	}
	if err := st.ReplaceAll(ctx, items); err != nil {
		t.Fatalf("准备数据失败: %v", err)
	}

	hits, err := st.SearchText(ctx, "50%", "", 0)
	if err != nil {
		t.Fatalf("SearchText 失败: %v", err)
	}
	if len(hits) != 1 || hits[0].Statute.ID != "T-1" {
		t.Fatalf("%% 必须按字面匹配，命中 %+v", hits)
	}

	if under, err := st.SearchText(ctx, "_", "", 0); err != nil || len(under) != 0 {
		t.Errorf("下划线不应匹配任意字符，got %d, err %v", len(under), err)
	}
}

func TestStatutesSearchTextBlankQuery(t *testing.T) {
	db := testDB(t)
	st := NewStatutes(db)

	hits, err := st.SearchText(context.Background(), "   ", "", 0)
	if err != nil {
		t.Fatalf("空白查询不应报错: %v", err)
	}
	if hits == nil || len(hits) != 0 {
		t.Errorf("空白查询应返回空切片而非 nil，got %#v", hits)
	}
}

// 判例与向量没做就是没做，接口上必须是明确的"尚未实现"，不能返回空结果假装"没查到"。
// 项目约定这个状态用 50001 表达（见 errNotImplemented 的注释），所以同时钉码与文案。
func TestStatutesUnimplementedDomains(t *testing.T) {
	db := testDB(t)
	st := NewStatutes(db)
	ctx := context.Background()

	cases := map[string]error{
		"ReplaceCases":   st.ReplaceCases(ctx, nil),
		"SaveEmbeddings": st.SaveEmbeddings(ctx, nil),
	}
	_, err := st.SearchCases(ctx, "q", "", 0)
	cases["SearchCases"] = err
	_, err = st.LoadEmbeddings(ctx, "", "")
	cases["LoadEmbeddings"] = err

	for name, err := range cases {
		if !hasCode(err, port.CodeInternal) || !strings.Contains(err.Error(), "尚未实现") {
			t.Errorf("%s 应返回 50001 且文案含\"尚未实现\"，got %v", name, err)
		}
	}
}

// hasCode 用 AppError.Is 的"只比错误码"语义判断错误类别。
func hasCode(err error, code port.ErrorCode) bool {
	return errors.Is(err, &port.AppError{Code: code})
}

// 纯单元测试：转义函数本身不该依赖数据库才能验证。
func TestEscapeLike(t *testing.T) {
	cases := map[string]string{
		`50%`:   `50\%`,
		`a_b`:   `a\_b`,
		`a\b`:   `a\\b`,
		`100%%`: `100\%\%`,
		`普通词`:   `普通词`,
	}
	for in, want := range cases {
		if got := escapeLike(in); got != want {
			t.Errorf("escapeLike(%q) = %q，应为 %q", in, got, want)
		}
	}
}
