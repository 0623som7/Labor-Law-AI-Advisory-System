package port

import (
	"encoding/json"
	"strings"
	"testing"
)

// BBox 校验是坐标协议的唯一执行点，错一个分支就整层标注歪。
func TestTextBlockValid(t *testing.T) {
	const w, h = 595.0, 842.0 // A4，单位 pt

	cases := []struct {
		name    string
		bbox    [4]float64
		width   float64
		height  float64
		wantErr bool
	}{
		{"正常块", [4]float64{112.4, 320.7, 468.1, 336.2}, w, h, false},
		{"贴左下角", [4]float64{0, 0, 10, 10}, w, h, false},
		{"贴右上角", [4]float64{585, 832, w, h}, w, h, false},
		{"零面积点", [4]float64{100, 100, 100, 100}, w, h, false},
		{"x 反序", [4]float64{468.1, 320.7, 112.4, 336.2}, w, h, true},
		{"y 反序(上下颠倒)", [4]float64{112.4, 336.2, 468.1, 320.7}, w, h, true},
		{"x 越右边界", [4]float64{112.4, 320.7, 600, 336.2}, w, h, true},
		{"y 越上边界", [4]float64{112.4, 320.7, 468.1, 900}, w, h, true},
		{"负坐标", [4]float64{-1, 0, 10, 10}, w, h, true},
		{"NaN", [4]float64{112.4, nan(), 468.1, 336.2}, w, h, true},
		{"Inf", [4]float64{112.4, 320.7, inf(), 336.2}, w, h, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blk := TextBlock{ID: "blk_1", Page: 0, BBox: c.bbox}
			err := blk.Valid(c.width, c.height)
			if c.wantErr && err == nil {
				t.Fatalf("期望报错，实际通过: %v", c.bbox)
			}
			if !c.wantErr && err != nil {
				t.Fatalf("期望通过，实际报错: %v", err)
			}
		})
	}
}

func nan() float64 { var f float64; return f / f }
func inf() float64 { var f float64; return 1 / f }

func TestTextBlockCharSpan(t *testing.T) {
	cases := []struct {
		name  string
		start int
		end   int
		want  int
	}{
		{"正常", 0, 6, 6},
		{"空 span", 12, 12, 0},
		{"反序防御", 20, 12, 0},
		{"跨页连续偏移", 810, 826, 16},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blk := TextBlock{CharStart: c.start, CharEnd: c.end}
			if got := blk.CharSpan(); got != c.want {
				t.Fatalf("CharSpan() = %d, want %d", got, c.want)
			}
		})
	}
}

// CoordsJSON 的契约形状：BBox 必须序列化成 4 个数的数组，不能变成 {"x0":...} 对象，
// 否则前端标注层的解构会静默拿到 undefined，画出一个 0×0 的框。
func TestBBoxSerializesAsArray(t *testing.T) {
	blk := TextBlock{ID: "b1", Page: 2, BBox: [4]float64{1, 2, 3, 4}, Text: "试用期三个月"}
	raw, err := json.Marshal(blk)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}
	want := `"BBox":[1,2,3,4]`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("序列化结果不含 %s: %s", want, raw)
	}
}

func TestTaskStatusTransitions(t *testing.T) {
	cases := []struct {
		from, to TaskStatus
		want     bool
	}{
		{TaskPending, TaskRunning, true},
		{TaskPending, TaskFailed, true}, // 入库成功但派发失败
		{TaskPending, TaskCancelled, true},
		{TaskRunning, TaskSucceeded, true},
		{TaskRunning, TaskFailed, true}, // 超时 / 崩溃后对账
		{TaskRunning, TaskCancelled, true},
		{TaskPending, TaskSucceeded, false}, // 跳过执行
		{TaskSucceeded, TaskRunning, false}, // 终态复活
		{TaskFailed, TaskRunning, false},
		{TaskCancelled, TaskSucceeded, false},
		{TaskSucceeded, TaskFailed, false},
	}
	for _, c := range cases {
		t.Run(string(c.from)+"->"+string(c.to), func(t *testing.T) {
			if got := c.from.CanTransition(c.to); got != c.want {
				t.Fatalf("CanTransition(%s→%s) = %v, want %v", c.from, c.to, got, c.want)
			}
		})
	}
}

func TestTaskStatusTerminal(t *testing.T) {
	cases := []struct {
		status TaskStatus
		want   bool
	}{
		{TaskPending, false},
		{TaskRunning, false},
		{TaskSucceeded, true},
		{TaskFailed, true},
		{TaskCancelled, true},
	}
	for _, c := range cases {
		t.Run(string(c.status), func(t *testing.T) {
			if got := c.status.Terminal(); got != c.want {
				t.Fatalf("Terminal() = %v, want %v", got, c.want)
			}
		})
	}
}
