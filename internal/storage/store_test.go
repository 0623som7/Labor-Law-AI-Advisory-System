package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testUser = "01K9ZZZZZZZZZZZZZZZZZZZZZZ"
	testFile = "01K9YYYYYYYYYYYYYYYYYYYYYY"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	// 故意多一层不存在的目录：New 必须自己 MkdirAll，而不是要求部署脚本先建好。
	root := filepath.Join(t.TempDir(), "nested", "storage")
	s, err := New(root)
	if err != nil {
		t.Fatalf("New() 返回错误: %v", err)
	}
	return s
}

// TestPutOpenRoundTrip 守住"写进去什么、读回来就是什么"，并且 sha256 与内容一致。
func TestPutOpenRoundTrip(t *testing.T) {
	s := newStore(t)
	content := []byte("%PDF-1.7\n合同正文\n")
	wantSum := sha256.Sum256(content)

	rel, size, sum, err := s.Put(testUser, testFile, bytes.NewReader(content))
	if err != nil {
		t.Fatalf("Put() 返回错误: %v", err)
	}
	if size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", size, len(content))
	}
	if want := hex.EncodeToString(wantSum[:]); sum != want {
		t.Fatalf("sha256 = %s, want %s", sum, want)
	}

	// 相对路径的形状：<userID 前两位>/<userID>/<fileID>.pdf，且必须是 "/" 分隔。
	wantRel := testUser[:2] + "/" + testUser + "/" + testFile + ".pdf"
	if rel != wantRel {
		t.Fatalf("relPath = %q, want %q", rel, wantRel)
	}

	rc, err := s.Open(rel)
	if err != nil {
		t.Fatalf("Open() 返回错误: %v", err)
	}
	got, err := io.ReadAll(rc)
	if err != nil {
		rc.Close()
		t.Fatalf("读取失败: %v", err)
	}
	// Windows 上文件被打开时删不掉，必须先关闭再 Remove。
	if err := rc.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("读回内容不一致: %q", got)
	}

	// 临时文件必须已经被 rename 掉，不能留下 .part。
	entries, err := os.ReadDir(filepath.Dir(filepath.Join(s.Root(), filepath.FromSlash(rel))))
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}

	// Remove 之后 Open 必须失败。
	if err := s.Remove(rel); err != nil {
		t.Fatalf("Remove() 返回错误: %v", err)
	}
	if _, err := s.Open(rel); err == nil {
		t.Fatal("删除后仍能打开")
	}
}

// TestPutRejectsPathTraversal 守住"上传里的 ID 不能改变落盘位置"。
//
// fileID 来自我们的 ULID，但 Put 是导出函数、也可能被别的调用方用；一旦有人把
// 用户可控的字符串传进来，"../" 就能把文件写到 root 外面，而 FileRecord.Filename
// 的注释里明确禁止的就是这条路径。
func TestPutRejectsPathTraversal(t *testing.T) {
	s := newStore(t)

	cases := []struct{ name, userID, fileID string }{
		{"fileID 含上跳", testUser, "../evil"},
		{"fileID 是 ..", testUser, ".."},
		{"userID 含上跳", "../../etc", testFile},
		{"userID 是 .", ".", testFile},
		{"fileID 含反斜杠", testUser, `..\evil`},
		{"fileID 含盘符", testUser, "C:evil"},
		{"空 fileID", testUser, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rel, size, sum, err := s.Put(c.userID, c.fileID, bytes.NewReader([]byte("%PDF-1.7")))
			if err == nil {
				t.Fatalf("期望报错，实际成功: rel=%q size=%d sum=%q", rel, size, sum)
			}
			if rel != "" || size != 0 || sum != "" {
				t.Fatalf("失败时不应返回任何值: rel=%q size=%d sum=%q", rel, size, sum)
			}
		})
	}

	// root 的父目录里不能出现被写出来的文件。
	parent := filepath.Dir(s.Root())
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatalf("读取父目录失败: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "evil.pdf" || e.Name() == "etc" {
			t.Fatalf("文件逃出了 root: %s", filepath.Join(parent, e.Name()))
		}
	}
}

// TestPathRejectsEscape 守住从数据库读回来的相对路径同样不能越界。
func TestPathRejectsEscape(t *testing.T) {
	s := newStore(t)

	for _, rel := range []string{
		"../outside.pdf",
		"../../etc/passwd",
		"/etc/passwd",
		"",
		".",
		"..",
	} {
		if _, err := s.Path(rel); err == nil {
			t.Fatalf("Path(%q) 应当报错", rel)
		}
	}

	ok, err := s.Path(testUser[:2] + "/" + testUser + "/" + testFile + ".pdf")
	if err != nil {
		t.Fatalf("合法路径被拒绝: %v", err)
	}
	if !strings.HasPrefix(ok, s.Root()) {
		t.Fatalf("解析结果 %q 不在 root %q 下", ok, s.Root())
	}
}

// TestNewCreatesRoot 守住"root 不存在时自己建"——部署时少一个 mkdir 不该是启动失败的理由。
func TestNewCreatesRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "a", "b", "c")
	if _, err := New(root); err != nil {
		t.Fatalf("New() 返回错误: %v", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatalf("root 未创建: %v", err)
	}
	if !info.IsDir() {
		t.Fatal("root 不是目录")
	}

	if _, err := New("  "); err == nil {
		t.Fatal("空 root 应当报错")
	}
}
