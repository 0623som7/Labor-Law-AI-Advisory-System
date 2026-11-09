package store

import (
	"context"
	"database/sql"
	"testing"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// 任务表的语义最容易错的两点：状态机（pending→running→终态）与进度覆盖。
// 前者错了任务会永远停在 pending，后者错了进度会倒退。

func cleanupTasks(t *testing.T, db *sql.DB, ids ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, tid := range ids {
			if _, err := db.Exec(`DELETE FROM tasks WHERE id = ?`, tid); err != nil {
				t.Logf("清理失败: %v", err)
			}
		}
	})
}

func newTask() *port.Task {
	return &port.Task{
		ID:      id.New(),
		Type:    "review.run",
		Status:  port.TaskPending,
		Payload: []byte(`{"review_id":"R-1"}`),
	}
}

func TestTasksCreateGet(t *testing.T) {
	db := testDB(t)
	task := newTask()
	cleanupTasks(t, db, task.ID)
	s := NewTasks(db)
	ctx := context.Background()

	if err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(ctx, task.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Type != "review.run" || got.Status != port.TaskPending || got.Progress != 0 {
		t.Fatalf("初始状态错误: %+v", got)
	}
	if string(got.Payload) != `{"review_id": "R-1"}` {
		t.Fatalf("payload = %s", got.Payload)
	}
	if got.Attempt != 0 || got.Error != "" {
		t.Fatalf("初始 attempt/error 错误: %+v", got)
	}
}

func TestTasksCreateRejectsNonPending(t *testing.T) {
	db := testDB(t)
	task := newTask()
	task.Status = port.TaskRunning
	cleanupTasks(t, db, task.ID)

	err := NewTasks(db).Create(context.Background(), task)
	if !port.IsCode(err, port.CodeConflict) {
		t.Fatalf("非 pending 建任务应当 40901，实际: %v", err)
	}
}

func TestTasksCreateRejectsBadPayload(t *testing.T) {
	db := testDB(t)
	task := newTask()
	task.Payload = []byte(`not-json`)
	cleanupTasks(t, db, task.ID)

	err := NewTasks(db).Create(context.Background(), task)
	if !port.IsCode(err, port.CodeInvalidArgument) {
		t.Fatalf("非法 payload 应当 40001，实际: %v", err)
	}
}

func TestTasksUpdateProgressFlipsStatus(t *testing.T) {
	db := testDB(t)
	task := newTask()
	cleanupTasks(t, db, task.ID)
	s := NewTasks(db)
	ctx := context.Background()
	if err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.UpdateProgress(ctx, task.ID, 30, "正在抽取合同要素"); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	got, _ := s.Get(ctx, task.ID)
	if got.Status != port.TaskRunning {
		t.Fatalf("进度上报后状态 = %s，期望 running（否则永远无法 Finish）", got.Status)
	}
	if got.Progress != 30 || got.Stage != "正在抽取合同要素" {
		t.Fatalf("进度未写入: %+v", got)
	}

	// 覆盖语义 + 夹取。
	if err := s.UpdateProgress(ctx, task.ID, 150, ""); err != nil {
		t.Fatalf("UpdateProgress 越界: %v", err)
	}
	got, _ = s.Get(ctx, task.ID)
	if got.Progress != 100 || got.Stage != "" {
		t.Fatalf("进度应当被夹到 100 且覆盖 stage: %+v", got)
	}
	if err := s.UpdateProgress(ctx, task.ID, -5, "x"); err != nil {
		t.Fatalf("UpdateProgress 负数: %v", err)
	}
	got, _ = s.Get(ctx, task.ID)
	if got.Progress != 0 {
		t.Fatalf("负数应当夹到 0: %+v", got)
	}

	if err := s.UpdateProgress(ctx, id.New(), 10, "x"); !port.IsCode(err, port.CodeNotFound) {
		t.Fatalf("不存在的任务应当 40401，实际: %v", err)
	}
}

func TestTasksFinishStateMachine(t *testing.T) {
	db := testDB(t)
	task := newTask()
	cleanupTasks(t, db, task.ID)
	s := NewTasks(db)
	ctx := context.Background()
	if err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// pending 直接 succeeded 必须被拒（CanTransition 只允许 pending→running/failed/cancelled）。
	if err := s.Finish(ctx, task.ID, port.TaskSucceeded, ""); !port.IsCode(err, port.CodeConflict) {
		t.Fatalf("pending→succeeded 应当 40901，实际: %v", err)
	}

	if err := s.UpdateProgress(ctx, task.ID, 10, "开始"); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	if err := s.Finish(ctx, task.ID, port.TaskSucceeded, ""); err != nil {
		t.Fatalf("running→succeeded: %v", err)
	}
	// 终态不可再变。
	if err := s.Finish(ctx, task.ID, port.TaskFailed, "晚了"); !port.IsCode(err, port.CodeConflict) {
		t.Fatalf("终态再转移应当 40901，实际: %v", err)
	}
	got, _ := s.Get(ctx, task.ID)
	if got.Status != port.TaskSucceeded {
		t.Fatalf("最终状态 = %s", got.Status)
	}
}

func TestTasksFinishCancelled(t *testing.T) {
	db := testDB(t)
	task := newTask()
	cleanupTasks(t, db, task.ID)
	s := NewTasks(db)
	ctx := context.Background()
	if err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := s.Finish(ctx, task.ID, port.TaskCancelled, "用户取消"); err != nil {
		t.Fatalf("pending→cancelled: %v", err)
	}
	got, _ := s.Get(ctx, task.ID)
	if got.Status != port.TaskCancelled || got.Error != "用户取消" {
		t.Fatalf("取消后: %+v", got)
	}
}

func TestTasksFinishTruncatesError(t *testing.T) {
	db := testDB(t)
	task := newTask()
	cleanupTasks(t, db, task.ID)
	s := NewTasks(db)
	ctx := context.Background()
	if err := s.Create(ctx, task); err != nil {
		t.Fatalf("Create: %v", err)
	}

	long := make([]rune, 400)
	for i := range long {
		long[i] = '错'
	}
	if err := s.Finish(ctx, task.ID, port.TaskFailed, string(long)); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, _ := s.Get(ctx, task.ID)
	if n := len([]rune(got.Error)); n != 255 {
		t.Fatalf("error 长度 = %d，期望截到 255（列宽 VARCHAR(255)，超长会直接报错）", n)
	}
}

func TestTasksListByStatusAndFailRunning(t *testing.T) {
	db := testDB(t)
	running, done := newTask(), newTask()
	cleanupTasks(t, db, running.ID, done.ID)
	s := NewTasks(db)
	ctx := context.Background()

	for _, task := range []*port.Task{running, done} {
		if err := s.Create(ctx, task); err != nil {
			t.Fatalf("Create: %v", err)
		}
		if err := s.UpdateProgress(ctx, task.ID, 5, "跑"); err != nil {
			t.Fatalf("UpdateProgress: %v", err)
		}
	}
	if err := s.Finish(ctx, done.ID, port.TaskSucceeded, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	list, err := s.ListByStatus(ctx, port.TaskRunning, 10)
	if err != nil {
		t.Fatalf("ListByStatus: %v", err)
	}
	found := false
	for _, item := range list {
		if item.ID == done.ID {
			t.Fatal("已结束的任务不该出现在 running 列表里")
		}
		if item.ID == running.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("running 列表里找不到刚创建的任务")
	}

	// FailRunning 只影响本进程启动前遗留的 running 任务。
	if _, err := s.FailRunning(ctx, "服务重启，任务中断"); err != nil {
		t.Fatalf("FailRunning: %v", err)
	}
	got, _ := s.Get(ctx, running.ID)
	if got.Status != port.TaskFailed || got.Error != "服务重启，任务中断" {
		t.Fatalf("FailRunning 后: %+v", got)
	}
	if other, _ := s.Get(ctx, done.ID); other.Status != port.TaskSucceeded {
		t.Fatalf("FailRunning 误伤了已结束任务: %+v", other)
	}
}

// 排队中的任务也是脏状态：进程被 kill 时它们还在池子里没人领，重启后队列是空的，
// 不清理就会永远停在 pending，前端一直转圈。
func TestFailRunningAlsoClearsPending(t *testing.T) {
	db := testDB(t)
	pending, running, done := newTask(), newTask(), newTask()
	cleanupTasks(t, db, pending.ID, running.ID, done.ID)
	s := NewTasks(db)
	ctx := context.Background()

	for _, task := range []*port.Task{pending, running, done} {
		if err := s.Create(ctx, task); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	// 只推进 running 与 done，pending 保持原状。
	for _, task := range []*port.Task{running, done} {
		if err := s.UpdateProgress(ctx, task.ID, 5, "跑"); err != nil {
			t.Fatalf("UpdateProgress: %v", err)
		}
	}
	if err := s.Finish(ctx, done.ID, port.TaskSucceeded, ""); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	if _, err := s.FailRunning(ctx, "服务重启，任务中断"); err != nil {
		t.Fatalf("FailRunning: %v", err)
	}

	gotPending, _ := s.Get(ctx, pending.ID)
	if gotPending.Status != port.TaskFailed {
		t.Fatalf("pending 任务没被清理: %+v", gotPending)
	}
	gotRunning, _ := s.Get(ctx, running.ID)
	if gotRunning.Status != port.TaskFailed {
		t.Fatalf("running 任务没被清理: %+v", gotRunning)
	}
	gotDone, _ := s.Get(ctx, done.ID)
	if gotDone.Status != port.TaskSucceeded {
		t.Fatalf("已结束任务被误伤: %+v", gotDone)
	}
}
