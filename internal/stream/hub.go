package stream

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"laborlaw/internal/port"
)

// DefaultStreamTTL 是没有任何订阅者之后，帧缓冲还能活多久。
//
// 2 分钟：够一次正常的断线重连（浏览器重试通常 1–5 秒），又不至于把
// 用户关掉页面后的整段回答一直挂在内存里。
const DefaultStreamTTL = 2 * time.Minute

// Hub 按 message_id 保存帧缓冲与订阅者，是断线续传的落点。
//
// 为什么需要它：SSE 断线后客户端会带着 Last-Event-ID 重连，而那时原来的
// HTTP 请求已经结束、写协程已经退出。只有把已发帧留在别处（这里），
// 新连接才有东西可补发。
//
// 不额外起清理协程：每次 Attach / Publish 时顺手清掉过期的空流，
// 逻辑简单，也没有"清理协程忘了停"的风险。
type Hub struct {
	mu      sync.Mutex
	streams map[string]*stream
	opts    Options
	ttl     time.Duration
	log     *slog.Logger
	now     func() time.Time

	// subs 是当前挂着的订阅者总数。
	//
	// 用原子计数而不是每次遍历 map 统计：/metrics 每次抓取都会读它，
	// 遍历要在锁内扫所有流的 subs 集合，而计数器只是读一个 int64。
	subs atomic.Int64
}

// stream 是一条逻辑消息流：一个自增 seq、一段可续传的帧缓冲、若干订阅者。
type stream struct {
	key  string
	seq  int
	buf  *frameBuffer
	subs map[*Writer]struct{}
	// last 是"最后一个订阅者离开"（或建流）的时间，用于 TTL 回收。
	last time.Time
}

// NewHub 构造 Hub。ttl <= 0 时用默认值。
func NewHub(opts Options, ttl time.Duration, log *slog.Logger) *Hub {
	if ttl <= 0 {
		ttl = DefaultStreamTTL
	}
	if log == nil {
		log = slog.Default()
	}
	return &Hub{
		streams: make(map[string]*stream),
		opts:    opts.withDefaults(),
		ttl:     ttl,
		log:     log,
		now:     time.Now,
	}
}

// Attach 为一条消息挂一个 SSE 连接，并补发 seq > lastEventID 的历史帧。
//
// lastEventID <= 0 表示新连接，一样会补发：Agent 可能比浏览器先跑了几帧。
// 返回的 Writer 用完必须 Close，否则写协程会一直活着直到 ctx 取消。
func (h *Hub) Attach(ctx context.Context, key string, lastEventID int, w http.ResponseWriter) (*Writer, error) {
	var writer *Writer

	writer, err := newWriter(key, w, h.opts, h.log, func() {
		h.detach(key, writer)
	})
	if err != nil {
		return nil, err
	}

	h.mu.Lock()
	h.evictLocked()
	s := h.streamLocked(key)
	s.subs[writer] = struct{}{}
	h.subs.Add(1)
	// 在锁内取快照：保证"补发到哪一帧"与"从哪一帧开始收实时"是一致的。
	writer.backlog = s.buf.from(lastEventID)
	h.mu.Unlock()

	// 协程在订阅登记之后才启动：反过来会出现"实时帧已经进队列、历史帧还没准备好"
	// 的乱序窗口。
	go writer.run(ctx)
	return writer, nil
}

// Publish 分发一条 Agent 事件：分配 seq、写入缓冲、推给所有订阅者。
//
// 没有订阅者时也会创建流并缓冲：Agent 可能比浏览器先开始跑，
// 客户端晚几百毫秒连上时应当能看到从头开始的完整回答。
func (h *Hub) Publish(key string, ev port.Event) (int, error) {
	return h.publish(key, ev.Type, func(seq int) wireEvent {
		w := toWire(ev)
		w.Seq = seq
		return w
	})
}

// PublishMeta 发送首帧。message_id / session_id 不在 port.Event 里，
// 所以单独给一个入口，而不是往共享契约里加字段。
func (h *Hub) PublishMeta(key, messageID, sessionID, model string) (int, error) {
	return h.publish(key, port.AgentEventMeta, func(seq int) wireEvent {
		return wireEvent{Seq: seq, MessageID: messageID, SessionID: sessionID, Model: model}
	})
}

// PublishDone 发送终止帧。它必须由 A 补发：B 的 Run 返回值里才有完整的
// usage 与 finish_reason，而这两项要等生成结束才拿得到。
func (h *Hub) PublishDone(key, messageID, finishReason string, usage port.Usage) (int, error) {
	return h.publish(key, port.AgentEventDone, func(seq int) wireEvent {
		return wireEvent{
			Seq:          seq,
			MessageID:    messageID,
			FinishReason: finishReason,
			Usage: &wireUsage{
				PromptTokens:     usage.PromptTokens,
				CompletionTokens: usage.CompletionTokens,
				TotalTokens:      usage.TotalTokens,
			},
		}
	})
}

// PublishError 发送异常终止帧。之后不会再有任何事件。
func (h *Hub) PublishError(key string, appErr *port.AppError) (int, error) {
	e := appErr
	if e == nil {
		e = port.New(port.CodeInternal, "服务内部错误")
	}
	return h.publish(key, port.AgentEventError, func(seq int) wireEvent {
		return wireEvent{Seq: seq, Code: int(e.Code), Message: e.Message}
	})
}

// publish 是三种事件共用的落库与分发路径。
//
// build 在持锁状态下调用，必须只做纯计算：任何阻塞都会卡住所有消息流。
func (h *Hub) publish(key string, typ port.AgentEventType, build func(seq int) wireEvent) (int, error) {
	h.mu.Lock()
	h.evictLocked()
	s := h.streamLocked(key)

	s.seq++
	encoded, err := encodeFrame(typ, build(s.seq))
	if err != nil {
		s.seq-- // 编码失败不该在 seq 上留洞
		h.mu.Unlock()
		return 0, err
	}
	s.buf.add(encoded)

	var slow []*Writer
	for w := range s.subs {
		if !w.enqueue(encoded) {
			slow = append(slow, w)
		}
	}
	seq := s.seq
	h.mu.Unlock()

	// 踢人必须放在锁外：Close 会等写协程退出（最长 5s），持锁等待会把所有消息流一起冻住。
	for _, w := range slow {
		h.log.Warn("dropping slow sse subscriber", "stream", key, "dropped_tool_calls", w.Dropped())
		if closeErr := w.Close(); closeErr != nil {
			h.log.Warn("close slow subscriber", "stream", key, "err", closeErr.Error())
		}
	}

	return seq, nil
}

// LastSeq 返回某条流当前最大的 seq；流不存在返回 0。供测试与调试使用。
func (h *Hub) LastSeq(key string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	if s, ok := h.streams[key]; ok {
		return s.seq
	}
	return 0
}

// Streams 返回 Hub 跟踪中的流数量（含 TTL 内已无订阅者的空闲流）。
//
// 空闲读数是懒回收的结果：evictLocked 只在 Attach / publish 时触发，
// 没有后台协程。要"此刻在线多少条"请用 Subscribers。
func (h *Hub) Streams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams)
}

// Subscribers 返回当前挂着的 SSE 订阅者总数。
//
// 与 Streams 的区别：Streams 数的是 Hub 跟踪的流（含 TTL 内已无订阅者的
// 空闲流，读数会停在下一次 attach/publish 之前），Subscribers 数的是此刻
// 真正挂着的连接。做告警阈值请用这个。
func (h *Hub) Subscribers() int {
	return int(h.subs.Load())
}

// detach 摘除订阅；最后一个订阅者离开时记下时间，交给 TTL 回收。
func (h *Hub) detach(key string, w *Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()

	s, ok := h.streams[key]
	if !ok {
		return
	}
	// 先确认确实挂着再减，否则重复 detach 会把计数减成负数。
	if _, ok := s.subs[w]; ok {
		delete(s.subs, w)
		h.subs.Add(-1)
	}
	if len(s.subs) == 0 {
		s.last = h.now()
	}
}

// evictLocked 清掉"没有订阅者且超过 TTL"的流。调用方必须持锁。
func (h *Hub) evictLocked() {
	now := h.now()
	for key, s := range h.streams {
		if len(s.subs) > 0 {
			continue
		}
		if now.Sub(s.last) > h.ttl {
			delete(h.streams, key)
		}
	}
}

// streamLocked 取或建一条流。调用方必须持锁。
func (h *Hub) streamLocked(key string) *stream {
	if s, ok := h.streams[key]; ok {
		return s
	}
	s := &stream{
		key:  key,
		buf:  newFrameBuffer(h.opts.BufferBytes),
		subs: make(map[*Writer]struct{}),
		// 建流时就记时间：一条"Agent 先跑、浏览器始终没连上"的流也必须能过期，
		// 否则它会在内存里留到进程重启。
		last: h.now(),
	}
	h.streams[key] = s
	return s
}
