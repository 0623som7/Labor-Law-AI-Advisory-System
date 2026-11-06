// Package storage 是合同原文的落盘层。
//
// 它只做四件事：拼安全路径、先写临时文件再 rename、边写边算 sha256、按相对路径读回。
// 不碰数据库、不碰配置：FileRecord.StoragePath 与实际落点的对齐由 api 层负责。
//
// 为什么不用 multipart 里的原文件名：FileRecord.Filename 的注释写死了"仅展示，
// 禁止用于拼磁盘路径"，一个叫 "../../etc/passwd" 的上传能把文件写到根目录外。
// 落盘名用 ULID（fileID），原文件名只进数据库当展示字段。
package storage

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Store 是文件根目录的句柄，零值不可用。
type Store struct {
	// root 是绝对的根目录，New 时解析一次。
	// 每个方法都基于绝对路径做越界判断，存相对路径会让"在哪个 cwd 下运行"决定安全性。
	root string
}

// New 构造落盘层，root 不存在时创建。
//
// 权限 0750：目录里有用户的合同原文，同机其他用户不需要读。容器里是单用户，无副作用。
func New(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("storage root 为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("解析 storage root: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("创建 storage root %s: %w", abs, err)
	}
	return &Store{root: abs}, nil
}

// Root 返回绝对根目录，供日志与排障使用。
func (s *Store) Root() string { return s.root }

// Put 把 r 的全部内容写进 <root>/<userID 前两位>/<userID>/<fileID>.pdf。
//
// 三重考虑：
//  1. 分两层目录（前两位 + 完整 ID）：单目录放十万个文件时，ext4 的目录项查找会退化成线性。
//  2. 先写 .part 再 rename：rename 在同一文件系统内是原子的，进程被杀时不会留下
//     一份"看起来存在、其实只有半页"的 PDF，而半截 PDF 会让解析层报出无法解释的错。
//  3. 边写边算 sha256：同一份 10MB 文件读两遍（一遍算哈希、一遍写盘）纯属浪费，
//     而且两遍之间的内容如果不同（并发写同一个文件）就没有一致的意义了。
//
// 返回的 size 是实际落盘的字节数，sha256Hex 是小写十六进制，relPath 用 "/" 分隔，
// 直接可以塞进 FileRecord.StoragePath。
func (s *Store) Put(userID, fileID string, r io.Reader) (relPath string, size int64, sha256Hex string, err error) {
	if err := safeSegment("user_id", userID); err != nil {
		return "", 0, "", err
	}
	if err := safeSegment("file_id", fileID); err != nil {
		return "", 0, "", err
	}

	dir := filepath.Join(s.root, shard(userID), userID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", 0, "", fmt.Errorf("创建用户目录: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".upload-*.part")
	if err != nil {
		return "", 0, "", fmt.Errorf("创建临时文件: %w", err)
	}
	tmpName := tmp.Name()
	// 任何失败路径都要把临时文件摘掉，否则存储目录会慢慢堆满 .part。
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		cleanup()
		return "", 0, "", fmt.Errorf("写入文件: %w", err)
	}
	// Sync 之后才 rename：否则崩溃时可能 rename 出一个内容还在页缓存里丢了一半的文件。
	if err := tmp.Sync(); err != nil {
		cleanup()
		return "", 0, "", fmt.Errorf("刷新文件: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, "", fmt.Errorf("关闭临时文件: %w", err)
	}

	final := filepath.Join(dir, fileID+".pdf")
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return "", 0, "", fmt.Errorf("落盘归档: %w", err)
	}

	rel, err := filepath.Rel(s.root, final)
	if err != nil {
		return "", 0, "", fmt.Errorf("计算相对路径: %w", err)
	}
	return filepath.ToSlash(rel), size, hex.EncodeToString(h.Sum(nil)), nil
}

// Open 按相对路径打开文件，供 GET /files/{id}/content 直接流给浏览器。
//
// 返回的 ReadCloser 由调用方负责关闭：handler 用 c.DataFromReader 时 gin 会关。
func (s *Store) Open(relPath string) (io.ReadCloser, error) {
	abs, err := s.Path(relPath)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// Remove 删除一个已落盘的文件，用于"缓存命中"与"后续步骤失败"的清理。
//
// 不返回"文件不存在"的错误：删两次和删一次的结果一样，调用方只关心文件还在不在。
func (s *Store) Remove(relPath string) error {
	abs, err := s.Path(relPath)
	if err != nil {
		return err
	}
	if err := os.Remove(abs); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Path 把数据库里的相对路径还原成绝对路径，并保证它没有逃出 root。
//
// 解析层（pdftotext）需要一个真实路径，所以这个方法是必需的；它同时也是
// StoragePath 从库里读回来之后唯一的信任边界——库里的值理论上只由 Put 写入，
// 但"理论上"不足以让 filepath.Join 直接拼一个来自数据库的字符串。
func (s *Store) Path(relPath string) (string, error) {
	clean := filepath.Clean(filepath.FromSlash(relPath))
	if clean == "." || clean == ".." ||
		filepath.IsAbs(clean) ||
		filepath.VolumeName(clean) != "" ||
		strings.HasPrefix(clean, string(filepath.Separator)) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("storage: 非法相对路径 %q", relPath)
	}

	abs := filepath.Join(s.root, clean)
	// Join 之后再验一次：Windows 下 "C:foo" 这类写法能绕过 IsAbs，
	// 而 Join 之后 Rel 能给出唯一的真实结论。
	rel, err := filepath.Rel(s.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("storage: 路径越界 %q", relPath)
	}
	return abs, nil
}

// safeSegment 校验一个路径段（userID / fileID）不会改变目录结构。
//
// 只允许字母数字与 "-" "_" "."：ULID 用的是 Crockford base32，本来就不需要别的字符，
// 收紧到白名单比"过滤 ../"更可靠——后者永远漏掉一种编码。
func safeSegment(name, v string) error {
	if v == "" || v == "." || v == ".." {
		return fmt.Errorf("storage: %s 非法 %q", name, v)
	}
	if strings.ContainsAny(v, `/\:`) || strings.ContainsRune(v, 0) {
		return fmt.Errorf("storage: %s 含路径分隔符 %q", name, v)
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("storage: %s 含非法字符 %q", name, v)
		}
	}
	return nil
}

// shard 取 userID 的前两位做一级目录。不足两位（不该发生）时退化成 "_"。
func shard(userID string) string {
	if len(userID) >= 2 {
		return userID[:2]
	}
	return "_"
}
