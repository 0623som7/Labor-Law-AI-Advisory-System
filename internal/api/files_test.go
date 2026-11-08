package api

import (
	"context"
	"errors"
	"testing"

	"laborlaw/internal/port"
)

// plainParseFiles 是只关心"解析结果怎么落库"的假 FileStore，故意不带 FinishParse，
// 用来覆盖 fallback 路径。内嵌 port.FileStore 让未实现的方法在调用时 panic。
type plainParseFiles struct {
	port.FileStore

	hasFinish bool
	finishErr error
	pagesErr  error
	updateErr error

	finishCalls int
	saveCalls   int
	updateCalls int
	gotPages    []port.ParsedPage
	gotRec      port.FileRecord
}

func (f *plainParseFiles) SavePages(_ context.Context, _ string, pages []port.ParsedPage) error {
	f.saveCalls++
	f.gotPages = pages
	return f.pagesErr
}

func (f *plainParseFiles) Update(_ context.Context, rec *port.FileRecord) error {
	f.updateCalls++
	f.gotRec = *rec
	return f.updateErr
}

// finishCapableFiles 模拟真实现：在同一个假 store 上多一个 FinishParse。
type finishCapableFiles struct {
	*plainParseFiles
	err error

	calls    int
	pageCnt  int
	pagesGot []port.ParsedPage
}

func (f *finishCapableFiles) FinishParse(_ context.Context, _ string, pageCount int, _ bool, _ float64, pages []port.ParsedPage) error {
	f.calls++
	f.pageCnt = pageCount
	f.pagesGot = pages
	return f.err
}

// fakeParseStore 只实现 File()。
type fakeParseStore struct {
	port.Store
	files port.FileStore
}

func (s *fakeParseStore) File() port.FileStore { return s.files }

func newParseAPI(files port.FileStore) *API {
	return New(testConfig(""), discardLogger(), WithStore(&fakeParseStore{files: files}))
}

// TestSaveParseUsesSingleTransaction 守住优先走 FinishParse：
// 元数据与页面必须一次写完，不能再出现"先 Update 再 SavePages"的两次写。
func TestSaveParseUsesSingleTransaction(t *testing.T) {
	base := &plainParseFiles{hasFinish: true}
	files := &finishCapableFiles{plainParseFiles: base}
	a := newParseAPI(files)

	rec := &port.FileRecord{ID: "F-1", UserID: "U-1", PageCount: 3, OCRUsed: true, Confidence: 0.9}
	pages := []port.ParsedPage{{Index: 0}}

	if err := a.saveParse(context.Background(), rec, pages); err != nil {
		t.Fatalf("saveParse 返回错误: %v", err)
	}
	if files.calls != 1 {
		t.Fatalf("FinishParse 调用次数 = %d, want 1", files.calls)
	}
	if files.pageCnt != 3 || len(files.pagesGot) != 1 {
		t.Fatalf("FinishParse 参数不对: pageCount=%d pages=%d", files.pageCnt, len(files.pagesGot))
	}
	if base.saveCalls != 0 || base.updateCalls != 0 {
		t.Fatalf("走了单事务却仍然单独写了: SavePages=%d Update=%d", base.saveCalls, base.updateCalls)
	}
}

// TestSaveParseFallbackWritesPagesFirst 守住兜底路径的顺序。
//
// 没有 FinishParse 时也必须先写页面再回写元数据：反过来的话，SavePages 失败就会
// 留下 page_count＞0 但没有页面的坏文件（LoadPages 40401、重传被跳过解析）。
func TestSaveParseFallbackWritesPagesFirst(t *testing.T) {
	base := &plainParseFiles{pagesErr: errors.New("磁盘满了")}
	a := newParseAPI(base)

	rec := &port.FileRecord{ID: "F-1", UserID: "U-1", PageCount: 3, Confidence: 0.9}
	err := a.saveParse(context.Background(), rec, []port.ParsedPage{{Index: 0}})
	if err == nil {
		t.Fatal("SavePages 失败时 saveParse 应当返回错误")
	}
	if base.saveCalls != 1 {
		t.Fatalf("SavePages 调用次数 = %d, want 1", base.saveCalls)
	}
	if base.updateCalls != 0 {
		t.Fatalf("页面都没写成功就回写了元数据（Update 次数 = %d），会留下坏文件", base.updateCalls)
	}
}

func TestSaveParseFallbackSuccessOrder(t *testing.T) {
	base := &plainParseFiles{}
	a := newParseAPI(base)

	rec := &port.FileRecord{ID: "F-1", UserID: "U-1", PageCount: 2, OCRUsed: false, Confidence: 0.8}
	if err := a.saveParse(context.Background(), rec, []port.ParsedPage{{Index: 0}}); err != nil {
		t.Fatalf("saveParse 返回错误: %v", err)
	}
	if base.saveCalls != 1 || base.updateCalls != 1 {
		t.Fatalf("调用次数 = SavePages:%d Update:%d, want 1/1", base.saveCalls, base.updateCalls)
	}
	if base.gotRec.PageCount != 2 {
		t.Fatalf("回写的 page_count = %d, want 2", base.gotRec.PageCount)
	}
}
