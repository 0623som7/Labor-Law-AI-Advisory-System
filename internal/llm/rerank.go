package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"laborlaw/internal/config"
	"laborlaw/internal/port"
)

// maxRerankDocuments 是单次重排的文档条数上限。
//
// 文档给的上限是 500。我们只有 205 条法条，正常永远到不了；写成硬上限是为了
// 在调用方把全体法条塞进来时立刻报错，而不是让上游按 400 回一句看不懂的话。
// defaultRerankTimeout 是重排请求的兜底超时，与 config 的默认值一致。
const defaultRerankTimeout = 8 * time.Second

const maxRerankDocuments = 500

// maxRerankDocRunes 是单条文档的截断长度。
//
// 单条上限 4000 token，法条原文最长不到 1200 字，3000 是给"将来条文变长"留的
// 余量。截断会记 Debug：静默截断在 extract 那里已经修过一次了。
const maxRerankDocRunes = 3000

// RerankClient 调用 DashScope 的 text-rerank 端点。
//
// 不复用 Client 也不复用 EmbeddingClient 的 Option：三者的装配方式不同，
// 但错误映射（errors_map.go）与传输层（newTransport）是共用的。
//
// 它不记 usage_records：重排是检索内部开销，记进用户的对话成本会让"这句话
// 花了多少钱"变得不可解释——用户没有要求模型读那 20 条候选。
type RerankClient struct {
	cfg      config.LLMConfig
	http     *http.Client
	log      *slog.Logger
	endpoint string
}

// RerankOption 是 RerankClient 的依赖注入点。
type RerankOption func(*RerankClient)

// WithRerankLogger 注入结构化日志。
func WithRerankLogger(l *slog.Logger) RerankOption {
	return func(r *RerankClient) {
		if l != nil {
			r.log = l
		}
	}
}

// WithRerankHTTPClient 注入 HTTP 客户端，测试用它指向 httptest 服务器。
func WithRerankHTTPClient(h *http.Client) RerankOption {
	return func(r *RerankClient) {
		if h != nil {
			r.http = h
		}
	}
}

// NewRerankClient 构造重排客户端。
//
// endpoint 从配置传进来而不是拼在 BaseURL 后面：rerank 不走 /compatible-mode
// 那条路（它是 DashScope 的原生 API），将来换成带 WorkspaceId 的业务空间地址
// 也只改环境变量，不改代码。
func NewRerankClient(cfg config.LLMConfig, endpoint string, opts ...RerankOption) *RerankClient {
	// 与 EmbeddingClient 同样的理由：零值 Timeout 会被 WithTimeout 当成立刻到期，
	// 失败又被检索层吃成"降级、保留融合顺序"，线上表现只是"重排好像没生效"。
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	// 重排有自己的超时：构造方可能只填了 Timeout（老配置、测试夹具），
	// 这时不能退回 60 秒——那正是要修的症状。
	if cfg.RerankTimeout <= 0 {
		cfg.RerankTimeout = defaultRerankTimeout
	}
	r := &RerankClient{
		cfg:      cfg,
		http:     &http.Client{Transport: newTransport(cfg)},
		log:      slog.Default(),
		endpoint: strings.TrimSpace(endpoint),
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// rerankRequest 是 DashScope text-rerank 的请求体。
//
// 不传 top_n：要全量分数自己排，让上游先截一刀就没法在下游稳定地"按分数重排
// 再截 topK"。不传 return_documents：文档原文我们手里就有，回传只是流量。
type rerankRequest struct {
	Model      string          `json:"model"`
	Input      rerankInput     `json:"input"`
	Parameters rerankParameter `json:"parameters"`
}

type rerankInput struct {
	Query     string   `json:"query"`
	Documents []string `json:"documents"`
}

type rerankParameter struct {
	Instruct string `json:"instruct"`
}

type rerankResponse struct {
	Output *struct {
		Results []rerankItem `json:"results"`
	} `json:"output"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type rerankItem struct {
	Index          int     `json:"index"`
	RelevanceScore float64 `json:"relevance_score"`
}

// rerankInstruct 是官方示例里的任务指令，原样照抄。
//
// 不自己改写：交叉编码器对指令措辞敏感，改一句中文听起来更贴切的话，
// 分数分布就会变，而我们没有评测集支撑这种改动。
const rerankInstruct = "Given a web search query, retrieve relevant passages that answer the query."

// Rerank 返回与 docs 下标一一对应的相关性分数。
//
// 上游的 output.results 是排好序的，直接当分数用就把"第 3 名"当成了第 3 条文档
// 的分数——embedding 那里踩过同一个坑，这里按 item.Index 回填。
func (r *RerankClient) Rerank(ctx context.Context, query string, docs []string) ([]float64, error) {
	if strings.TrimSpace(query) == "" {
		return nil, port.New(port.CodeInvalidArgument, "重排查询为空")
	}
	if len(docs) == 0 {
		// 空切片而不是 nil：调用方会按下标回写分数，nil 与空在这里没有语义差别，
		// 但"没有候选"和"取不到分数"在日志里是两件事，返回空更清楚。
		return []float64{}, nil
	}
	if len(docs) > maxRerankDocuments {
		return nil, port.New(port.CodeInvalidArgument, "重排候选过多：%d 条，上限 %d", len(docs), maxRerankDocuments)
	}
	if r.endpoint == "" {
		return nil, port.New(port.CodeInvalidArgument, "LLM_RERANK_URL 未配置")
	}

	trimmed := make([]string, len(docs))
	truncated := 0
	for i, doc := range docs {
		runes := []rune(doc)
		if len(runes) > maxRerankDocRunes {
			runes = runes[:maxRerankDocRunes]
			truncated++
		}
		trimmed[i] = string(runes)
	}
	if truncated > 0 {
		r.log.Debug("重排文档过长，已截断", "documents", truncated, "limit_runes", maxRerankDocRunes)
	}

	body, err := json.Marshal(rerankRequest{
		Model:      r.cfg.RerankModel,
		Input:      rerankInput{Query: query, Documents: trimmed},
		Parameters: rerankParameter{Instruct: rerankInstruct},
	})
	if err != nil {
		return nil, port.Wrap(port.CodeInvalidArgument, err, "重排请求体无法序列化")
	}

	callCtx, cancel := context.WithTimeout(ctx, r.cfg.RerankTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, port.Wrap(port.CodeInternal, err, "构造重排请求失败")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.cfg.APIKey)

	start := time.Now()
	resp, err := r.http.Do(req)
	if err != nil {
		// 不重试：重排是可选增强，失败就该立刻降级到融合顺序，
		// 在检索路径上退避重试只会把一次问答拖长好几秒。
		return nil, mapTransportError(r.cfg, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode != http.StatusOK {
		return nil, mapStatusError(r.cfg, resp.StatusCode, raw)
	}
	if readErr != nil {
		return nil, port.Wrap(port.CodeUpstream, mapReadErr(r.cfg, readErr), "读取重排响应失败")
	}

	var parsed rerankResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, port.Wrap(port.CodeUpstream, mapReadErr(r.cfg, err), "重排响应不是合法 JSON")
	}
	// DashScope 的鉴权失败可能回 200 + {"code":"InvalidApiKey"}，必须比 status 先看。
	if parsed.Code != "" {
		// 走 redactKey：这类网关常把请求头回显进 message。
		detail := truncate(redactKey(r.cfg, strings.TrimSpace(parsed.Code+" "+parsed.Message)), 200)
		return nil, port.New(port.CodeUpstream, "重排服务返回错误：%s", detail)
	}
	if parsed.Output == nil {
		return nil, port.New(port.CodeUpstream, "重排响应缺少 output")
	}
	// 打一次耗时：重排是检索路径上唯一的"新增一次公网往返"，
	// 线上首字变慢时要能立刻说清其中多少毫秒是它。
	r.log.Info("重排完成", "documents", len(docs), "elapsed_ms", time.Since(start).Milliseconds())
	return reorderRerank(parsed.Output.Results, len(docs))
}

// reorderRerank 校验条数与下标，并按 Index 回填成入参顺序。
func reorderRerank(items []rerankItem, want int) ([]float64, error) {
	if len(items) != want {
		return nil, port.New(port.CodeUpstream, "重排条数与请求不一致：请求 %d 条，返回 %d 条", want, len(items))
	}
	out := make([]float64, want)
	seen := make([]bool, want)
	for _, item := range items {
		if item.Index < 0 || item.Index >= want {
			return nil, port.New(port.CodeUpstream, "重排下标越界：%d 不在 [0,%d)", item.Index, want)
		}
		if seen[item.Index] {
			// 重复下标会让某条文档永远拿 0 分，排序结果看着像模型判断，
			// 实际是切片没写进去——这种错必须炸出来而不是静默。
			return nil, port.New(port.CodeUpstream, "重排下标重复：%d", item.Index)
		}
		seen[item.Index] = true
		out[item.Index] = item.RelevanceScore
	}
	return out, nil
}
