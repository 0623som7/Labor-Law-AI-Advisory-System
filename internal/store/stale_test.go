package store

import (
	"context"
	"testing"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// 启动对账的第二步：任务收尾之后，review 也要跟着收尾。
//
// 只测一条原则：任务已经失败的那些 review 才收拾，任务还活着的绝不碰。

// seedReviewWithTask 建一条任务 + 一条绑在它上面的 review，返回两者。
// status 决定 review 的初始状态，taskFailed 决定任务最终成不成功。
func seedReviewWithTask(t *testing.T, reviewStatus port.TaskStatus, taskFailed bool) (*port.Review, *port.Task) {
	t.Helper()
	db := testDB(t)
	ctx := context.Background()
	tasks := NewTasks(db)

	task := newTask()
	cleanupTasks(t, db, task.ID)
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatalf("Create task: %v", err)
	}
	if err := tasks.UpdateProgress(ctx, task.ID, 10, "跑"); err != nil {
		t.Fatalf("UpdateProgress: %v", err)
	}
	if taskFailed {
		if err := tasks.Finish(ctx, task.ID, port.TaskFailed, "服务重启，任务中断"); err != nil {
			t.Fatalf("Finish task: %v", err)
		}
	}

	rv := newReview(id.New())
	rv.TaskID = task.ID
	rv.Status = reviewStatus
	cleanupReviews(t, db, rv.ID)
	if err := NewReviews(db).Create(ctx, rv); err != nil {
		t.Fatalf("Create review: %v", err)
	}
	return rv, task
}

func TestFailStaleReviewsMarksFailedWhenTaskFailed(t *testing.T) {
	rv, _ := seedReviewWithTask(t, port.TaskRunning, true)
	db := testDB(t)

	n, err := FailStaleReviews(context.Background(), db)
	if err != nil {
		t.Fatalf("FailStaleReviews: %v", err)
	}
	// 库里可能还有别的遗留行，只断言至少命中我们这一条。
	if n < 1 {
		t.Fatalf("affected = %d，期望至少 1", n)
	}
	got, err := NewReviews(db).Get(context.Background(), rv.UserID, rv.ID)
	if err != nil {
		t.Fatalf("Get review: %v", err)
	}
	if got.Status != port.TaskFailed {
		t.Fatalf("review 没被收尾: %+v", got)
	}
}

func TestFailStaleReviewsAlsoClearsPendingReview(t *testing.T) {
	rv, _ := seedReviewWithTask(t, port.TaskPending, true)
	db := testDB(t)

	if _, err := FailStaleReviews(context.Background(), db); err != nil {
		t.Fatalf("FailStaleReviews: %v", err)
	}
	got, err := NewReviews(db).Get(context.Background(), rv.UserID, rv.ID)
	if err != nil {
		t.Fatalf("Get review: %v", err)
	}
	if got.Status != port.TaskFailed {
		t.Fatalf("pending review 没被收尾: %+v", got)
	}
}

func TestFailStaleReviewsLeavesLiveTaskAlone(t *testing.T) {
	rv, _ := seedReviewWithTask(t, port.TaskRunning, false)
	db := testDB(t)

	if _, err := FailStaleReviews(context.Background(), db); err != nil {
		t.Fatalf("FailStaleReviews: %v", err)
	}
	got, err := NewReviews(db).Get(context.Background(), rv.UserID, rv.ID)
	if err != nil {
		t.Fatalf("Get review: %v", err)
	}
	if got.Status != port.TaskRunning {
		t.Fatalf("任务还在跑，review 不该被改: %+v", got)
	}
}

// task_id 为空串的 review（历史数据 / 正在创建的记录）没有任务可对账，
// 既不报错也不能被误判成失败。
func TestFailStaleReviewsIgnoresEmptyTaskID(t *testing.T) {
	db := testDB(t)
	rv := newReview(id.New())
	rv.TaskID = ""
	rv.Status = port.TaskRunning
	cleanupReviews(t, db, rv.ID)
	if err := NewReviews(db).Create(context.Background(), rv); err != nil {
		t.Fatalf("Create review: %v", err)
	}

	if _, err := FailStaleReviews(context.Background(), db); err != nil {
		t.Fatalf("FailStaleReviews: %v", err)
	}
	got, err := NewReviews(db).Get(context.Background(), rv.UserID, rv.ID)
	if err != nil {
		t.Fatalf("Get review: %v", err)
	}
	if got.Status != port.TaskRunning {
		t.Fatalf("没有任务的 review 被误伤: %+v", got)
	}
}

func TestFailStaleReviewsNilDB(t *testing.T) {
	n, err := FailStaleReviews(context.Background(), nil)
	if err != nil || n != 0 {
		t.Fatalf("nil db 应返回 (0, nil)，实际 (%d, %v)", n, err)
	}
}
