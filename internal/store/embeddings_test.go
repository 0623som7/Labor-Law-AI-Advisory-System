package store

import (
	"context"
	"database/sql"
	"reflect"
	"testing"

	"laborlaw/internal/port"
)

// embeddingTestOwner 是集成测试专用的 owner_type。
//
// 真实向量用 "statute"；测试用 "test" 隔离，清理时只删这一片，不会碰到
// 205 条法条的向量（也不能用 preserveStatutes，那是给 ReplaceAll 全表重建
// 做快照用的，它备份的是整张 statutes 表）。
const embeddingTestOwner = "test"

// embeddingsTestDB 起一个库并保证退出时清掉 test 命名空间下的向量。
func embeddingsTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db := testDB(t)
	t.Cleanup(func() {
		if _, err := db.ExecContext(context.Background(),
			`DELETE FROM embeddings WHERE owner_type = ?`, embeddingTestOwner); err != nil {
			t.Logf("清理测试向量失败: %v", err)
		}
	})
	return db
}

// countTestEmbeddings 数当前测试命名空间下的行数。
func countTestEmbeddings(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM embeddings WHERE owner_type = ?`, embeddingTestOwner).Scan(&n); err != nil {
		t.Fatalf("统计测试向量失败: %v", err)
	}
	return n
}

func TestSaveLoadEmbeddingsRoundTrip(t *testing.T) {
	db := embeddingsTestDB(t)
	s := NewStatutes(db)

	items := []port.Embedding{
		{OwnerType: embeddingTestOwner, OwnerID: "test-a", Model: "m1", Vector: []float32{0.1, -0.2, 0.3, 0.4}},
		{OwnerType: embeddingTestOwner, OwnerID: "test-b", Model: "m1", Vector: []float32{1, 2, 3, 4}},
		{OwnerType: embeddingTestOwner, OwnerID: "test-c", Model: "m1", Vector: []float32{0, 0, 0.5, 0.25}},
	}
	if err := s.SaveEmbeddings(context.Background(), items); err != nil {
		t.Fatalf("SaveEmbeddings 返回错误: %v", err)
	}

	got, err := s.LoadEmbeddings(context.Background(), embeddingTestOwner, "m1")
	if err != nil {
		t.Fatalf("LoadEmbeddings 返回错误: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("读回 %d 条，want 3", len(got))
	}
	want := map[string][]float32{"test-a": items[0].Vector, "test-b": items[1].Vector, "test-c": items[2].Vector}
	for _, e := range got {
		if e.OwnerType != embeddingTestOwner || e.Model != "m1" {
			t.Fatalf("回填字段不对: %+v", e)
		}
		if !reflect.DeepEqual(e.Vector, want[e.OwnerID]) {
			t.Fatalf("%s 向量 = %v, want %v", e.OwnerID, e.Vector, want[e.OwnerID])
		}
	}
}

func TestSaveEmbeddingsOverwritesSameKey(t *testing.T) {
	db := embeddingsTestDB(t)
	s := NewStatutes(db)

	first := []port.Embedding{{OwnerType: embeddingTestOwner, OwnerID: "test-x", Model: "m1", Vector: []float32{1, 1}}}
	second := []port.Embedding{{OwnerType: embeddingTestOwner, OwnerID: "test-x", Model: "m1", Vector: []float32{7, 8}}}

	if err := s.SaveEmbeddings(context.Background(), first); err != nil {
		t.Fatalf("第一次写入失败: %v", err)
	}
	if err := s.SaveEmbeddings(context.Background(), second); err != nil {
		t.Fatalf("第二次写入失败: %v", err)
	}

	if n := countTestEmbeddings(t, db); n != 1 {
		t.Fatalf("同 key 覆盖后行数 = %d, want 1", n)
	}
	got, err := s.LoadEmbeddings(context.Background(), embeddingTestOwner, "m1")
	if err != nil {
		t.Fatalf("LoadEmbeddings 返回错误: %v", err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Vector, []float32{7, 8}) {
		t.Fatalf("读回的是旧值: %+v", got)
	}
}

func TestLoadEmbeddingsSeparatesModels(t *testing.T) {
	db := embeddingsTestDB(t)
	s := NewStatutes(db)

	items := []port.Embedding{
		{OwnerType: embeddingTestOwner, OwnerID: "test-same", Model: "model-a", Vector: []float32{1, 0}},
		{OwnerType: embeddingTestOwner, OwnerID: "test-same", Model: "model-b", Vector: []float32{0, 1, 0}},
	}
	if err := s.SaveEmbeddings(context.Background(), items[:1]); err != nil {
		t.Fatalf("写入 model-a 失败: %v", err)
	}
	if err := s.SaveEmbeddings(context.Background(), items[1:]); err != nil {
		t.Fatalf("写入 model-b 失败: %v", err)
	}

	a, err := s.LoadEmbeddings(context.Background(), embeddingTestOwner, "model-a")
	if err != nil {
		t.Fatalf("读 model-a 失败: %v", err)
	}
	b, err := s.LoadEmbeddings(context.Background(), embeddingTestOwner, "model-b")
	if err != nil {
		t.Fatalf("读 model-b 失败: %v", err)
	}
	if len(a) != 1 || len(a[0].Vector) != 2 {
		t.Fatalf("model-a 读回 %+v", a)
	}
	if len(b) != 1 || len(b[0].Vector) != 3 {
		t.Fatalf("model-b 读回 %+v", b)
	}
}

func TestLoadEmbeddingsEmptyIsNotNil(t *testing.T) {
	db := embeddingsTestDB(t)
	s := NewStatutes(db)

	got, err := s.LoadEmbeddings(context.Background(), embeddingTestOwner, "no-such-model")
	if err != nil {
		t.Fatalf("空结果不该报错: %v", err)
	}
	if got == nil {
		t.Fatal("空结果必须是长度 0 的切片，不能是 nil")
	}
	if len(got) != 0 {
		t.Fatalf("空结果长度 = %d", len(got))
	}
}

func TestSaveEmbeddingsRejectsAndWritesNothing(t *testing.T) {
	db := embeddingsTestDB(t)
	s := NewStatutes(db)

	cases := map[string][]port.Embedding{
		"空 OwnerID": {
			{OwnerType: embeddingTestOwner, OwnerID: "test-ok", Model: "m1", Vector: []float32{1, 2}},
			{OwnerType: embeddingTestOwner, OwnerID: "", Model: "m1", Vector: []float32{3, 4}},
		},
		"维度不一致": {
			{OwnerType: embeddingTestOwner, OwnerID: "test-ok", Model: "m1", Vector: []float32{1, 2}},
			{OwnerType: embeddingTestOwner, OwnerID: "test-bad", Model: "m1", Vector: []float32{1, 2, 3}},
		},
		"空向量": {
			{OwnerType: embeddingTestOwner, OwnerID: "test-bad", Model: "m1", Vector: nil},
		},
		"空模型": {
			{OwnerType: embeddingTestOwner, OwnerID: "test-bad", Model: "", Vector: []float32{1}},
		},
	}

	for name, items := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.SaveEmbeddings(context.Background(), items)
			if !hasCode(err, port.CodeInvalidArgument) {
				t.Fatalf("错误 = %v, want 40001", err)
			}
			if n := countTestEmbeddings(t, db); n != 0 {
				t.Fatalf("被拒绝的批次写进了 %d 行", n)
			}
		})
	}
}

func TestLoadEmbeddingsOrdersByOwnerID(t *testing.T) {
	db := embeddingsTestDB(t)
	s := NewStatutes(db)

	// 插入顺序故意是 c、a、b。
	items := []port.Embedding{
		{OwnerType: embeddingTestOwner, OwnerID: "test-c", Model: "m1", Vector: []float32{3}},
		{OwnerType: embeddingTestOwner, OwnerID: "test-a", Model: "m1", Vector: []float32{1}},
		{OwnerType: embeddingTestOwner, OwnerID: "test-b", Model: "m1", Vector: []float32{2}},
	}
	if err := s.SaveEmbeddings(context.Background(), items); err != nil {
		t.Fatalf("SaveEmbeddings 返回错误: %v", err)
	}

	got, err := s.LoadEmbeddings(context.Background(), embeddingTestOwner, "m1")
	if err != nil {
		t.Fatalf("LoadEmbeddings 返回错误: %v", err)
	}
	want := []string{"test-a", "test-b", "test-c"}
	if len(got) != len(want) {
		t.Fatalf("读回 %d 条", len(got))
	}
	for i, id := range want {
		if got[i].OwnerID != id {
			t.Fatalf("第 %d 条 = %s, want %s（必须按 owner_id 升序）", i, got[i].OwnerID, id)
		}
	}
}

func TestSaveEmbeddingsRejectsEmptyTypeAndLongFields(t *testing.T) {
	s := NewStatutes(nil) // 校验在触碰数据库之前就该拒绝，nil db 也能跑

	long := make([]byte, 33)
	for i := range long {
		long[i] = 'a'
	}

	cases := map[string]port.Embedding{
		"空 OwnerType": {OwnerType: "", OwnerID: "x", Model: "m", Vector: []float32{1}},
		"OwnerType 超长": {
			OwnerType: "0123456789abcdefg", OwnerID: "x", Model: "m", Vector: []float32{1},
		},
		"OwnerID 超长": {OwnerType: "test", OwnerID: string(long), Model: "m", Vector: []float32{1}},
	}
	for name, item := range cases {
		t.Run(name, func(t *testing.T) {
			err := s.SaveEmbeddings(context.Background(), []port.Embedding{item})
			if !hasCode(err, port.CodeInvalidArgument) {
				t.Fatalf("错误 = %v, want 40001", err)
			}
		})
	}
}
