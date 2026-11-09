package stream

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"laborlaw/internal/port"
)

// 默认参数。都可以被 Options 覆盖，这里只是让调用方不必每次写全。
const (
	// DefaultHeartbeatInterval 是心跳间隔。
	//
	// 15s 是权衡后的值：Nginx 默认 60s 空闲超时、手机运营商 NAT 常见 30–120s，
	// 15s 留了足够余量；再短会增加无谓的帧与日志噪音。
	DefaultHeartbeatInterval = 15 * time.Second

	// DefaultQueueSize 是每连接的待发帧队列长度。
	//
	// 256 帧足够吸收一次网络抖动；再大只会让"慢客户端"占更多内存。
	DefaultQueueSize = 256

	// DefaultBufferBytes 是续传缓冲的字节上限（每 message_id）。
	DefaultBufferBytes = 256 << 10

	// closeTimeout 是关闭时等待写协程退出的上限。
	//
	// 不能无限等：写协程可能卡在 TCP 写阻塞上，等它会让 handler 永远不返回。
	closeTimeout = 5 * time.Second
)

// Options 是 Writer 的可调参数。
type Options struct {
	// HeartbeatInterval 是注释帧间隔；<=0 时用默认值。
	HeartbeatInterval time.Duration
	// QueueSize 是待发队列长度；<=0 时用默认值。
	QueueSize int
	// BufferBytes 是续传缓冲上限；<=0 时用默认值。
	BufferBytes int
	// CloseTimeout 是关闭时等待写协程退出的上限；<=0 时用默认值。
	CloseTimeout time.Duration
}

func (o Options) withDefaults() Options {
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if o.QueueSize <= 0 {
		o.QueueSize = DefaultQueueSize
	}
	if o.BufferBytes <= 0 {
		o.BufferBytes = DefaultBufferBytes
	}
	if o.CloseTimeout <= 0 {
		o.CloseTimeout = closeTimeout
	}
	return o
}

// Writer 是一条 SSE 连接的写端。
//
// 生命周期：由 Hub.Attach 创建并启动唯一的写协程；调用方用完必须 Close。
// 那个协程只做两件事——从队列取帧写出去、定时写心跳；它由 ctx 或 Close 结束，
// 结束后关闭 done，Close 靠 done 判断"真的退出了"而不是"我以为它退出了"。
type Writer struct {
	key     string
	w       http.ResponseWriter
	flush   http.Flusher
	log     *slog.Logger
	opts    Options
	onClose func()

	// backlog 是连接建立时要补发的历史帧，由 run 在进入事件循环前一次写完。
	// 放在字段里而不是先塞 channel，是为了保证"历史帧一定排在实时帧前面"。
	backlog []frame

	ch   chan frame
	done chan struct{}

	closeOnce sync.Once
	mu        sync.Mutex
	writeErr  error
	dropped   int
	closed    bool
}

// newWriter 准备响应头并返回写端。它不启动协程——协程由 Hub 在登记完订阅后启动，
// 否则可能出现"协程已经开始写，但订阅还没登记"的丢帧窗口。
func newWriter(key string, w http.ResponseWriter, opts Options, log *slog.Logger, onClose func()) (*Writer, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, port.New(port.CodeInternal, "响应写入器不支持 Flush，无法建立 SSE 连接")
	}
	if log == nil {
		log = slog.Default()
	}

	o := opts.withDefaults()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Nginx 看到这个头会关掉自己的缓冲；少了它，帧会攒够一个 buffer 才下发，
	// 表现是"答完了一次性出现"，TTFB 指标直接崩。
	h.Set("X-Accel-Buffering", "no")

	return &Writer{
		key:     key,
		w:       w,
		flush:   flusher,
		log:     log,
		opts:    o,
		onClose: onClose,
		ch:      make(chan frame, o.QueueSize),
		done:    make(chan struct{}),
	}, nil
}

// run 是这条连接唯一的写协程。
//
// 谁回收：ctx 取消、写失败、Close 三者任一都会让它返回；
// 返回时关闭 done，Close 与 Hub 都靠它判断退出，不存在"没人管的协程"。
func (w *Writer) run(ctx context.Context) {
	ticker := time.NewTicker(w.opts.HeartbeatInterval)
	defer func() {
		ticker.Stop()
		close(w.done)
	}()

	// 先补历史，再进循环：这样续传的帧一定不会被实时帧插队。
	for _, f := range w.backlog {
		if err := w.write(f.data); err != nil {
			w.recordErr(err)
			return
		}
	}
	w.backlog = nil

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.write([]byte(": ping\n\n")); err != nil {
				w.recordErr(err)
				return
			}
		case f, ok := <-w.ch:
			if !ok {
				return
			}
			if err := w.write(f.data); err != nil {
				w.recordErr(err)
				return
			}
		}
	}
}

func (w *Writer) write(b []byte) error {
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	w.flush.Flush()
	return nil
}

func (w *Writer) recordErr(err error) {
	w.mu.Lock()
	if w.writeErr == nil {
		w.writeErr = err
	}
	w.mu.Unlock()
	// 客户端断开是常态（关页面、切网络），记 Warn 不记 Error，
	// 否则真正的服务端故障会被这些噪音淹掉。
	w.log.Warn("sse write failed", "stream", w.key, "err", err.Error())
}

// enqueue 尝试把一帧放进队列。
//
// 返回 false 表示订阅者已经慢到不可救药（关键帧放不进去），Hub 会把它踢掉。
// 这里绝不阻塞：Publish 持有 Hub 锁，一阻塞就会拖住所有其它连接。
func (w *Writer) enqueue(f frame) bool {
	select {
	case w.ch <- f:
		return true
	default:
	}

	if droppable(f.typ) {
		w.mu.Lock()
		w.dropped++
		w.mu.Unlock()
		// 单订阅计数之外再记一笔全局量，/metrics 才有整机视角（见 metrics.go）。
		droppedTotal.Add(1)
		return true
	}
	return false
}

// Dropped 返回被丢弃的帧数，用于 /metrics 与压测断言。
func (w *Writer) Dropped() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.dropped
}

// Close 停止写协程并从 Hub 摘除订阅。可重复调用。
func (w *Writer) Close() error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		w.mu.Unlock()

		// 先从 Hub 摘除，再关 channel。反过来的话，并发的 Publish 可能正往这个
		// channel 里发——向已关闭的 channel 发送会直接 panic，且 recover 不到。
		if w.onClose != nil {
			w.onClose()
		}

		close(w.ch)
		select {
		case <-w.done:
		case <-time.After(w.opts.CloseTimeout):
			w.log.Warn("sse writer did not stop in time", "stream", w.key)
		}
	})

	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeErr
}

// Done 在写协程退出后关闭，供调用方 select 使用。
func (w *Writer) Done() <-chan struct{} {
	return w.done
}
