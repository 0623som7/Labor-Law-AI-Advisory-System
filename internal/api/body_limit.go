package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxAuthBodyBytes 是账号域请求体的上限。
//
// 注册/登录/续期的真实载荷不到 200 字节，64 KiB 留了三个数量级的余量；
// 没有上限时 ShouldBindJSON 会把整个 body 读进内存再解析，随便一个
// Content-Length: 2G 就能把进程的内存吃掉。
const maxAuthBodyBytes int64 = 64 << 10

// bodyLimit 给请求体套一层硬上限。
//
// 用 http.MaxBytesReader 而不是只看 Content-Length：分块传输根本没有长度头，
// 只看头等于把口子留着。越界时 MaxBytesReader 会让后续 Read 返回
// *http.MaxBytesError，由 bindAuthBody 翻成 41301。
func bodyLimit(n int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, n)
		}
		c.Next()
	}
}
