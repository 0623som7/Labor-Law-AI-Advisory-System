package stream

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
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
}

// stream 是一条逻辑消息流：一个自增 seq、一段可续传的帧缓冲、若干订阅者。
type stream struct {
	key  string
	seq  int
	buf  *frameBuffer
	subs map[*Writer]struct{}
	// last 是"最后一个订阅者离开"的时间，用于 TTL 回收。
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
	o := opts.withDefaults()
	return &Hub{
		streams: make(map[string]*stream),
		opts:    o,
		ttl:     ttl,
		log:     log,
		now:     time.Now,
	}
}

// Attach 为一条消息挂一个 SSE 连接，并补发 seq > lastEventID 的历史帧。
//
// lastEventID <= 0 表示新连接，不补发。返回的 Writer 用完必须 Close，
// 否则写协程会一直活着直到 ctx 取消。
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
	// 无论 lastEventID 是否为 0 都取一次快照：lastEventID=0 表示"新连接"，
	// 但 Agent 可能已经先跑了几帧（浏览器加载慢、重连时丢了 header），
	// 这些帧必须补上，否则用户看到的是从半句话开始的回答。
	writer.backlog = s.buf.from(lastEventID)
	h.mu.Unlock()

	// 协程在订阅登记之后才启动：反过来会出现"实时帧已经进队列、历史帧还没准备好"
	// 的乱序窗口。
	go writer.run(ctx)
	return writer, nil
}

// Publish 给一条消息分配 seq、写入缓冲、并按订阅者分发。
//
// 没有订阅者时也会创建流并缓冲：Agent 可能比浏览器先开始跑，
// 客户端晚几百毫秒连上时应当能看到从头开始的完整回答。
func (h *Hub) Publish(key string, ev port.Event) (int, error) {
	h.mu.Lock()
	h.evictLocked()
	s := h.streamLocked(key)

	s.seq++
	ev.Seq = s.seq

	encoded, err := encodeFrame(ev)
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

// Streams 返回当前存活的流数量，用于 /metrics 观察内存是否泄漏。
func (h *Hub) Streams() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams)
}

// detach 摘除订阅；最后一个订阅者离开时记下时间，交给 TTL 回收。
func (h *Hub) detach(key string, w *Writer) {
	h.mu.Lock()
	defer h.mu.Unlock()

	s, ok := h.streams[key]
	if !ok {
		return
	}
	delete(s.subs, w)
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
