package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/gin-gonic/gin"

	"laborlaw/internal/id"
	"laborlaw/internal/port"
)

// pdfMagic 是 PDF 文件头。校验它而不是扩展名：一个 .txt 改名成 .pdf 也能叫 .pdf，
// 而 pdftotext 对非 PDF 输入的错误信息极难分辨，41501 是更诚实的答复。
const pdfMagic = "%PDF-"

// 本文件的四个 handler 都只做"翻译"：HTTP → port 调用 → Envelope。
// 校验顺序是有成本的顺序：先拦大小、再验魔数、再算哈希查缓存，最后才解析。
// 反过来（先解析再校验）会让每个垃圾请求都吃掉一次 pdftotext 的 CPU 预算。

// uploadFile 处理 POST /api/v1/files。
//
// 步骤（顺序写死）：
//  1. MaxBytesReader 兜住请求体，防止一个超大 multipart 把内存打满；
//  2. multipart 解析 + 单文件大小上限，超了 41301；
//  3. 前 5 字节必须是 %PDF-，否则 41501；
//  4. 落盘时边写边算 sha256（一次读，不把文件读进内存两遍）；
//  5. 同用户同哈希直接返回已有记录，跳过解析；
//  6. 落盘 + Create + 解析 + Update + SavePages；
//  7. 解析返回 42201 时，仍然把 file 记录与 confidence 落库，然后再返回 42201。
func (a *API) uploadFile(c *gin.Context) {
	ctx := c.Request.Context()
	userID := UserID(c)

	if a.store == nil || a.blobs == nil || a.extractor == nil {
		a.notImplemented("上传合同")(c)
		return
	}

	// 1. 请求体硬上限。MaxBytesReader 触发时是 *http.MaxBytesError，
	//    与"文件本身超限"区分开，两者都归 41301 但文案不同。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, a.cfg.HTTP.MaxBodyBytes)

	// 2. 解析 multipart。内存预算直接给到请求体上限：本项目上限只有 12MB，
	//    全部放内存比"写临时文件再删"省一次磁盘往返。
	if err := c.Request.ParseMultipartForm(a.cfg.HTTP.MaxBodyBytes); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			FailCode(c, port.CodeFileTooLarge, "请求体超过 %d 字节", a.cfg.HTTP.MaxBodyBytes)
			return
		}
		FailCode(c, port.CodeInvalidArgument,
			"无法解析上传内容：请使用 multipart/form-data 并在 file 字段上传 PDF")
		return
	}

	fh, err := c.FormFile("file")
	if err != nil {
		FailCode(c, port.CodeInvalidArgument, "缺少 file 字段")
		return
	}
	if maxBytes := int64(a.cfg.Storage.MaxUploadMB) << 20; fh.Size > maxBytes {
		FailCode(c, port.CodeFileTooLarge, "文件超过 %d MB", a.cfg.Storage.MaxUploadMB)
		return
	}

	f, err := fh.Open()
	if err != nil {
		Fail(c, port.Wrap(port.CodeInternal, err, "读取上传内容失败"))
		return
	}
	defer f.Close()

	// 3. 魔数。读满 5 字节才算数：只读到 2 字节就断言"不是 PDF"会把
	//    上传被截断的情况误报成类型错误。
	head := make([]byte, len(pdfMagic))
	if _, err := io.ReadFull(f, head); err != nil {
		FailCode(c, port.CodeUnsupportedMedia, "文件内容不足 %d 字节，不是 PDF", len(pdfMagic))
		return
	}
	if string(head) != pdfMagic {
		FailCode(c, port.CodeUnsupportedMedia, "文件内容不是 PDF（扩展名不算数）")
		return
	}
	// 已读的 5 字节要接回去，否则落盘的文件会少一个文件头。
	body := io.MultiReader(bytes.NewReader(head), f)

	// 4. 落盘并同时得到 sha256 与真实字节数。
	fileID := id.New()
	relPath, size, sha, err := a.blobs.Put(userID, fileID, body)
	if err != nil {
		Fail(c, port.Wrap(port.CodeInternal, err, "保存上传文件失败"))
		return
	}

	// 5. 同一用户重复上传同一份合同：命中就不再解析一遍。
	//    但"命中"有两种：解析成功过，和上次解析到一半就失败了（page_count == 0）。
	//    后者不能当成成功返回——一份永远没有文本层的记录会让用户重传一百次都没有结果。
	//    这种情况下用已有记录的 StoragePath 就地补一次解析，再把新落盘的副本删掉。
	existing, err := a.store.File().GetByDigest(ctx, userID, sha)
	switch {
	case err == nil:
		_ = a.blobs.Remove(relPath)
		if existing.PageCount == 0 {
			a.finishParse(c, existing)
			return
		}
		OK(c, toFileRecordDTO(existing))
		return
	case port.CodeOf(err) != port.CodeNotFound:
		_ = a.blobs.Remove(relPath)
		Fail(c, err)
		return
	}

	// 6. 建元数据 → 解析 → 回填 → 存页。
	rec := &port.FileRecord{
		ID:          fileID,
		UserID:      userID,
		Filename:    displayName(fh.Filename),
		Ext:         "pdf",
		SizeBytes:   size,
		SHA256:      sha,
		StoragePath: relPath,
	}
	if err := a.store.File().Create(ctx, rec); err != nil {
		_ = a.blobs.Remove(relPath)
		// 并发重复：两个请求同时上传同一份文件，两次 GetByDigest 都没命中，
		// 唯一索引 (user_id, sha256) 拦住后插入者。这里换回已存在的那条记录，
		// 而不是把 40901 抛给用户——用户看到的是"上传成功，拿到同一个 id"。
		if port.CodeOf(err) == port.CodeConflict {
			if got, gerr := a.store.File().GetByDigest(ctx, userID, sha); gerr == nil {
				OK(c, toFileRecordDTO(got))
				return
			}
		}
		Fail(c, err)
		return
	}

	// 读回一次拿 DB 生成的 created_at：应用层自己填时间会让"同一文件在两台机器上
	// created_at 差 8 小时"这类问题到跨时区部署才暴露。
	if stored, gerr := a.store.File().Get(ctx, userID, fileID); gerr == nil {
		rec = stored
	}

	a.finishParse(c, rec)
}

// finishParse 解析刚建好的文件记录，回填元数据、存文本块，然后写响应。
//
// 单独成方法是因为有两条入口：新上传，以及"命中了上次解析失败的记录"。
// 两条入口必须走同一段代码，否则补解析那条迟早会漏掉某个字段的回填。
func (a *API) finishParse(c *gin.Context, rec *port.FileRecord) {
	ctx := c.Request.Context()

	absPath, err := a.blobs.Path(rec.StoragePath)
	if err != nil {
		Fail(c, port.Wrap(port.CodeInternal, err, "定位已上传文件失败"))
		return
	}

	doc, parseErr := a.extractor.Parse(ctx, rec.ID, absPath)

	// 解析失败但拿到了 doc（最典型的是 42201 无文本层）时，必须先落库再返回错误：
	// 用户要能区分"传过但没文本层"和"从没传过"，否则这份合同在列表里根本不存在，
	// 重传还会再跑一遍解析。confidence 与 page_count 在这种状态下就是唯一的线索。
	if doc != nil {
		rec.PageCount = doc.PageCount
		rec.OCRUsed = doc.OCRUsed
		rec.Confidence = doc.Confidence
		if uerr := a.store.File().Update(ctx, rec); uerr != nil {
			// 连元数据都写不进去时，解析结果没有意义：直接报错，不要假装成功。
			Fail(c, uerr)
			return
		}
	}

	if parseErr != nil {
		Fail(c, parseErr)
		return
	}

	if err := a.store.File().SavePages(ctx, rec.ID, doc.Pages); err != nil {
		Fail(c, err)
		return
	}

	OK(c, toFileRecordDTO(rec))
}

// getFile 处理 GET /api/v1/files/{fileId}。
func (a *API) getFile(c *gin.Context) {
	if a.store == nil {
		a.notImplemented("文件详情")(c)
		return
	}
	rec, err := a.store.File().Get(c.Request.Context(), UserID(c), c.Param("fileId"))
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, toFileRecordDTO(rec))
}

// getFilePages 处理 GET /api/v1/files/{fileId}/pages。
//
// 先 Get 再 LoadPages 是两次查询而不是一次 JOIN：Get 顺带做了归属校验，
// 少了它的直接后果是"知道别人 fileID 就能读到别人的合同正文"。
func (a *API) getFilePages(c *gin.Context) {
	if a.store == nil {
		a.notImplemented("解析结果")(c)
		return
	}
	ctx := c.Request.Context()

	rec, err := a.store.File().Get(ctx, UserID(c), c.Param("fileId"))
	if err != nil {
		Fail(c, err)
		return
	}

	pages, err := a.store.File().LoadPages(ctx, rec.ID)
	if err != nil {
		// 文件存在但没解析结果 = 上次解析没成功（扫描件、超页数、崩溃）。
		// 折成 42201 而不是 404：404 会让前端以为文件被删了。
		if port.CodeOf(err) == port.CodeNotFound {
			FailCode(c, port.CodeNoTextLayer, "该文件尚未解析出文本层，请重新上传或改用 OCR")
			return
		}
		Fail(c, err)
		return
	}

	OK(c, toParsedDocDTO(&port.ParsedDoc{
		FileID:     rec.ID,
		PageCount:  rec.PageCount,
		Pages:      pages,
		OCRUsed:    rec.OCRUsed,
		Confidence: rec.Confidence,
	}))
}

// downloadFile 处理 GET /api/v1/files/{fileId}/content。
//
// 契约要求 Content-Disposition 为 inline：PDF.js 直接用它渲染底图，
// 用 attachment 会让浏览器弹下载框，标注层就永远等不到底图。
func (a *API) downloadFile(c *gin.Context) {
	if a.store == nil || a.blobs == nil {
		a.notImplemented("文件下载")(c)
		return
	}
	ctx := c.Request.Context()

	rec, err := a.store.File().Get(ctx, UserID(c), c.Param("fileId"))
	if err != nil {
		Fail(c, err)
		return
	}

	rc, err := a.blobs.Open(rec.StoragePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// 有元数据但没有文件：多半是 storage 卷没挂上或被手工清过。
			FailCode(c, port.CodeNotFound, "文件内容不存在")
			return
		}
		Fail(c, port.Wrap(port.CodeInternal, err, "读取文件内容失败"))
		return
	}
	defer rc.Close()

	// 原始文件名可能是中文，直接塞进 header 会破坏行格式。
	// 按 RFC 5987 同时给 ASCII 兜底名与 UTF-8 编码名。
	c.Header("Content-Disposition", fmt.Sprintf("inline; filename=%q; filename*=UTF-8''%s",
		rec.ID+".pdf", url.PathEscape(rec.Filename)))
	c.DataFromReader(http.StatusOK, rec.SizeBytes, "application/pdf", rc, nil)
}

// displayName 把上传时的原文件名变成可安全展示的字符串。
//
// 只取最后一段并剥掉控制字符：Filename 只用于展示，但它会进数据库、进日志、
// 进 Content-Disposition 头，任何一层被 "\r\n" 或超长内容污染都会很难查。
func displayName(raw string) string {
	name := strings.ReplaceAll(raw, `\`, "/")
	name = path.Base(name)
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == "/" {
		return "contract.pdf"
	}
	// 库里的 filename 是 VARCHAR(255)，按字符截断而不是字节，避免切出半个汉字。
	if runes := []rune(name); len(runes) > 200 {
		name = string(runes[:200])
	}
	return name
}
