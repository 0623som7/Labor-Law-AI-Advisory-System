package port

import (
	"reflect"
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

// 这条用例原来断言 want := `"BBox":[1,2,3,4]`，也就是把 "port 结构直接序列化"
// 当成了线上契约。那是错的：JSON 形状（file_id / bbox / char_start / ocr_used）
// 由 internal/api 的 DTO 负责（见 internal/api/dto_file.go），port 是 A 与 B
// 共用且已冻结的契约，它一旦长上 json tag，B 侧的序列化行为会跟着变，
// 前端拿到的字段名也会随 port 的字段重构而漂移。
// 改写后守 port 侧真正不变的两件事：
//  1. BBox 的类型固定是 [4]float64，顺序 x0,y0,x1,y1 —— 换顺序等于整层标注歪；
//  2. TextBlock / ParsedPage / ParsedDoc 的任何字段都不许带 json tag。
func TestPortStructsCarryNoJSONTags(t *testing.T) {
	bbox, ok := reflect.TypeOf(TextBlock{}).FieldByName("BBox")
	if !ok {
		t.Fatal("TextBlock 缺少 BBox 字段")
	}
	if want := reflect.TypeOf([4]float64{}); bbox.Type != want {
		t.Fatalf("BBox 类型 = %v, want %v（顺序 x0,y0,x1,y1）", bbox.Type, want)
	}

	for _, v := range []any{TextBlock{}, ParsedPage{}, ParsedDoc{}} {
		tp := reflect.TypeOf(v)
		for i := 0; i < tp.NumField(); i++ {
			f := tp.Field(i)
			if tag, ok := f.Tag.Lookup("json"); ok {
				t.Fatalf("%s.%s 带了 json tag %q：JSON 形状必须由 api 层 DTO 负责，port 只承载类型",
					tp.Name(), f.Name, tag)
			}
		}
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
