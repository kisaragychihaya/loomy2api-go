package loomy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// 对应 Python loomy2api/server.py —— HTTP 网关：OpenAI 兼容 + Anthropic 兼容端点。
//
// 路由（_path 规范化、auth 门、错误形状、重试循环）逐条对齐 server.py 的 Handler 类。
// Python 的 BaseHTTPRequestHandler + ThreadingHTTPServer 换成 net/http：
//
//   - 每个 do_XXX 变成 apiHandler 上的一个方法，返回 error 由调用方统一翻译成
//     JSON 错误（对应 Python 的 except PoolError / except Exception）；
//   - resp.read(4096) 的流式转发换成 resp.Body.Read + 每块 Flush；
//   - HEAD 由 net/http 自动按 GET 处理并丢弃 body，无需特判；
//   - 客户端断开时 Python 靠 BrokenPipeError/ConnectionResetError 判断，这里看
//     写错误与 r.Context().Err()。

// ------------------------------------------------------------------ 日志

// Logger 对应 Python 的 Logger：带时间戳的文件 + 可选控制台日志，永不 panic。
type Logger struct {
	path    string
	console bool
	mu      sync.Mutex
}

// NewLogger 对应 Python Logger(log_dir, name="gateway.log", console=True)。
// name 为空时用 "gateway.log"；logDir 为空时写当前目录。
func NewLogger(logDir, name string, console bool) *Logger {
	if name == "" {
		name = "gateway.log"
	}
	dir := logDir
	if dir == "" {
		dir = "."
	}
	return &Logger{path: filepath.Join(dir, name), console: console}
}

// Log 写一行 "[2006-01-02 15:04:05] msg"：先打控制台，再追加到文件。
// 磁盘/权限等任何错误都静默吞掉（Python 的 except Exception: pass）。
func (l *Logger) Log(msg string) {
	if l == nil {
		return
	}
	defer func() { _ = recover() }()

	line := "[" + time.Now().Format("2006-01-02 15:04:05") + "] " + msg
	if l.console {
		fmt.Println(line)
	}
	if l.path == "" {
		return
	}
	if dir := filepath.Dir(l.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	fh, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer fh.Close()
	_, _ = fh.WriteString(line + "\n")
}

// Path 返回日志文件的完整路径（Panel.Logs 用它读日志尾巴）。
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// --------------------------------------------------------------- Gateway

// configWatchInterval 是 config.json 热重载 watcher 的轮询间隔（测试可改小）。
var configWatchInterval = 2 * time.Second

// Gateway 持有配置、账号池、模型目录与面板（对应 Python 的 Gateway）。
type Gateway struct {
	cfg    *Config
	logger *Logger

	// 字段名不能叫 pool：导出契约要一个 Pool() 方法，Go 不允许同名。
	acctPool     *Pool
	modelsClient *ModelGateway
	panel        *Panel

	modelsMu sync.RWMutex
	models   []map[string]any

	// 配置热重载：cfgMu 保护 ReloadConfig 的原地写回；watchStop 关闭即停 watcher；
	// watchOnce 保证 watcher goroutine 只启一次。
	cfgMu     sync.Mutex
	watchStop chan struct{}
	watchOnce sync.Once
}

// NewGateway 对应 Python Gateway(cfg, log=None)：构造账号池（解析失败上抛）并按
// 契约注入面板的 log / 日志路径 / 模型目录回调。
func NewGateway(cfg *Config, log *Logger) (*Gateway, error) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	if log == nil {
		logDir := cfg.LogDir
		if logDir == "" {
			logDir = "logs"
		}
		log = NewLogger(cfg.Path(logDir), "", cfg.LogConsole)
	}

	gw := &Gateway{cfg: cfg, logger: log, modelsClient: NewModelGateway(cfg), watchStop: make(chan struct{})}
	pool, err := NewPool(cfg, log.Log)
	if err != nil {
		return nil, err
	}
	gw.acctPool = pool
	gw.panel = NewPanel(cfg, pool, log.Log, log.Path(), gw.Catalogue)
	return gw, nil
}

// Start 对应 Python start：启动前先给没有会话的账号登录，再拉起 keeper，最后拉模型。
func (gw *Gateway) Start() {
	gw.acctPool.Bootstrap()
	gw.acctPool.Start()
	gw.RefreshModels()
	gw.startConfigWatcher()
}

// startConfigWatcher 用 watchOnce 启一个 goroutine，周期 stat config.json 并热载。
func (gw *Gateway) startConfigWatcher() {
	gw.watchOnce.Do(func() {
		gw.cfgMu.Lock()
		if gw.watchStop == nil {
			gw.watchStop = make(chan struct{})
		}
		stop := gw.watchStop
		gw.cfgMu.Unlock()
		go gw.watchConfigLoop(stop)
	})
}

// Stop 停掉配置 watcher（幂等）。Serve 的所有退出路径都应调用。
func (gw *Gateway) Stop() {
	gw.cfgMu.Lock()
	defer gw.cfgMu.Unlock()
	if gw.watchStop != nil {
		close(gw.watchStop)
		gw.watchStop = nil
	}
}

// watchConfigLoop 每 configWatchInterval stat 一次 config.json：文件不存在或
// mtime+size 未变则跳过；变化则直接 ReloadConfig。
func (gw *Gateway) watchConfigLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(configWatchInterval)
	defer ticker.Stop()

	var lastMod time.Time
	var lastSize int64
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}

		watchPath := gw.cfg.SourcePath
		if watchPath == "" {
			watchPath = filepath.Join(gw.cfg.Path(""), "config.json")
		}
		fi, err := os.Stat(watchPath)
		if err != nil {
			continue
		}
		if !fi.ModTime().After(lastMod) && fi.Size() == lastSize {
			continue
		}
		lastMod, lastSize = fi.ModTime(), fi.Size()
		gw.ReloadConfig()
	}
}

// ReloadConfig 对应 Python 侧的热重载：从 SourcePath 重读 config.json，并把新值
// 原地写回 *Config。Pool.Cfg / Panel cfg / ModelGateway / AccountClient 都是构造时
// 传入的同一个 *Config 指针，全字段原地拷贝（*gw.cfg = *fresh）对所有消费者立即生效
// —— 与 Python 改 dict 一样非事务化，逐字段赋值期间的读者看到的影响可忽略。
func (gw *Gateway) ReloadConfig() map[string]any {
	fresh, err := LoadConfig(gw.cfg.SourcePath)
	if err != nil {
		gw.Log("[config] 热载失败：" + err.Error())
		return map[string]any{"ok": false, "error": err.Error()}
	}

	gw.cfgMu.Lock()
	hostPortChanged := gw.cfg.Host != fresh.Host || gw.cfg.Port != fresh.Port
	root, src := gw.cfg.root, gw.cfg.SourcePath
	*gw.cfg = *fresh
	gw.cfg.root, gw.cfg.SourcePath = root, src
	gw.cfgMu.Unlock()

	gw.Log("[config] 已热载 " + gw.cfg.SourcePath)
	result := map[string]any{
		"ok":       true,
		"path":     gw.cfg.SourcePath,
		"strategy": gw.cfg.Strategy,
		"api_keys": len(gw.cfg.APIKeys),
	}
	if hostPortChanged {
		gw.Log("[config] host/port 已变更，需重启后生效")
		result["note"] = "host/port 的变更需重启后生效"
	}
	return result
}

// Handler 返回挂好本网关的 http.Handler（对应 Python 的 BoundHandler）。
func (gw *Gateway) Handler() http.Handler {
	return &apiHandler{gw: gw}
}

// Cfg 返回配置指针（对应 Python 的 gw.cfg）。
func (gw *Gateway) Cfg() *Config { return gw.cfg }

// Pool 返回账号池（对应 Python 的 gw.pool）。
func (gw *Gateway) Pool() *Pool { return gw.acctPool }

// Log 写一行网关日志（对应 Python 的 gw.log(msg)）。
func (gw *Gateway) Log(msg string) { gw.logger.Log(msg) }

// Catalogue 对应 Python catalogue：目录为空时先用内置清单填充，返回切片拷贝。
// 返回的 map 与内部共享（Python 的 list(self.models) 也是浅拷贝）。
func (gw *Gateway) Catalogue() []map[string]any {
	gw.modelsMu.Lock()
	defer gw.modelsMu.Unlock()
	if len(gw.models) == 0 {
		gw.models = svrFallbackCatalogue()
	}
	out := make([]map[string]any, len(gw.models))
	copy(out, gw.models)
	return out
}

// RefreshModels 对应 Python refresh_models：向上游要一次模型列表并缓存。
// 失败且当前为空时退回内置清单；失败但已有清单时保持沉默（与 Python 一致）。
func (gw *Gateway) RefreshModels() []map[string]any {
	data, err := gw.fetchModels()
	if err != nil {
		gw.modelsMu.RLock()
		have := len(gw.models) > 0
		gw.modelsMu.RUnlock()
		if !have {
			gw.modelsMu.Lock()
			gw.models = svrFallbackCatalogue()
			gw.modelsMu.Unlock()
			gw.Log(fmt.Sprintf("[warn] 拉取模型列表失败，使用内置清单：%v", err))
		}
		return gw.Catalogue()
	}
	gw.modelsMu.Lock()
	gw.models = data
	gw.modelsMu.Unlock()
	gw.Log(fmt.Sprintf("上游模型列表已刷新：%d 个", len(data)))
	return gw.Catalogue()
}

// fetchModels 借一个账号的会话拉 /models，并把 data 转成 map 列表。
func (gw *Gateway) fetchModels() ([]map[string]any, error) {
	acc, err := gw.acctPool.Acquire()
	if err != nil {
		return nil, err
	}
	payload, err := gw.modelsClient.Models(acc.Session)
	if err != nil {
		return nil, err
	}
	data := []map[string]any{}
	for _, item := range jlist(payload, "data") {
		if m, isMap := item.(map[string]any); isMap {
			data = append(data, m)
		}
	}
	return data, nil
}

// ResolveModel 对应 Python resolve_model：剥掉 provider 前缀（imodel/x → x），
// 空名回落默认模型。除此之外模型 id 原样透传 —— 没有别名表。
func (gw *Gateway) ResolveModel(requested string) string {
	name := strings.TrimSpace(requested)
	if gw.cfg.StripModelPrefix && strings.Contains(name, "/") {
		if idx := strings.Index(name, "/"); idx >= 0 {
			name = strings.TrimSpace(name[idx+1:])
		}
	}
	if name != "" {
		return name
	}
	if gw.cfg.DefaultModel != "" {
		return gw.cfg.DefaultModel
	}
	return DefaultModel
}

// maxAttempts 对应 Python 的 `int(cfg.get("max_retries") or 0) + 1`。
func (gw *Gateway) maxAttempts() int {
	attempts := gw.cfg.MaxRetries + 1
	if attempts < 0 {
		attempts = 0
	}
	return attempts
}

// svrFallbackCatalogue 对应 Python 的 `[dict(m, object="model", owned_by="loomy") ...]`。
func svrFallbackCatalogue() []map[string]any {
	out := make([]map[string]any, 0, len(FallbackModels))
	for _, model := range FallbackModels {
		item := make(map[string]any, len(model)+2)
		for k, v := range model {
			item[k] = v
		}
		item["object"] = "model"
		item["owned_by"] = "loomy"
		out = append(out, item)
	}
	return out
}

// ----------------------------------------------------------------- Serve

// Serve 监听 cfg.Host:Port 直到收到 SIGINT/SIGTERM，返回前停掉账号池并落盘。
// cfg 为 nil 时用 gw 自己的配置；gw 为 nil 时按 cfg 新建一个。
func Serve(cfg *Config, gw *Gateway) error {
	if gw == nil {
		var err error
		gw, err = NewGateway(cfg, nil)
		if err != nil {
			return err
		}
	}
	cfg = gw.Cfg()

	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.Port))

	gw.Start()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		gw.Stop()
		gw.acctPool.Stop()
		gw.acctPool.Flush()
		return fmt.Errorf("监听 %s 失败 / listen failed: %w", addr, err)
	}

	port := cfg.Port
	if cfg.Port == 0 {
		if tcp, isTCP := ln.Addr().(*net.TCPAddr); isTCP {
			port = tcp.Port
		}
	}

	srv := &http.Server{
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	gw.Log(fmt.Sprintf("loomy2api 已启动 / listening → http://%s:%d"+
		"  (OpenAI: /v1/chat/completions · Anthropic: /v1/messages)"+
		"  账号 %d 个，可用 %d 个",
		host, port, len(gw.acctPool.Accounts()), len(gw.acctPool.Usable())))

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	serveErr := srv.Serve(ln)
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		gw.Stop()
		gw.acctPool.Stop()
		gw.acctPool.Flush()
		return fmt.Errorf("HTTP 服务异常退出 / server error: %w", serveErr)
	}

	gw.Log("收到中断，退出 / interrupted, shutting down")
	gw.Stop()
	gw.acctPool.Stop()
	gw.acctPool.Flush()
	return nil
}

// ---------------------------------------------------------------- handler

// apiHandler 对应 Python 的 Handler（gateway 由 Gateway.Handler 注入）。
type apiHandler struct {
	gw *Gateway
}

// svrAccountFault：这些状态码说明是「账号的问题」（丢会话 / 进冷却）。
var svrAccountFault = []int{401, 403, 402, 429}

// svrFatalRequest：这些状态码说明请求本身有问题，换账号重试没有意义。
var svrFatalRequest = []int{400, 404, 422}

// svrHTTPError 是 handler 内部可控的响应：status + type + message。
type svrHTTPError struct {
	Status int
	Type   string
	Msg    string
}

func (e *svrHTTPError) Error() string { return e.Msg }

// svrBadJSON 表示请求体不是合法 JSON（对应 Python _read_json 抛的 ValueError）。
type svrBadJSON struct {
	Msg string
}

func (e *svrBadJSON) Error() string { return e.Msg }

// svrResponse 包一层 ResponseWriter：记下响应头是否已发出（流式开始后不能再改），
// 并把 Flush 透传出去。
type svrResponse struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *svrResponse) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *svrResponse) Write(data []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(data)
}

func (w *svrResponse) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// ServeHTTP 按方法分发；panic 一律降级成 502（Python 的 except Exception）。
func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &svrResponse{ResponseWriter: w}
	defer func() {
		if rec := recover(); rec != nil {
			h.gw.Log(fmt.Sprintf("[error] %s %s: panic: %v", r.Method, svrRequestURI(r), rec))
			if !rw.wroteHeader {
				svrWriteError(rw, r, 502,
					fmt.Sprintf("上游调用失败 / upstream failure: %v", rec), "upstream_error")
			}
		}
	}()

	switch r.Method {
	case http.MethodOptions:
		h.options(rw)
	case http.MethodGet, http.MethodHead:
		if err := h.get(rw, r); err != nil {
			h.finishError(rw, r, err, 503)
		}
	case http.MethodPost:
		if err := h.post(rw, r); err != nil {
			h.finishError(rw, r, err, 400)
		}
	default:
		svrWriteError(rw, r, 501,
			"不支持的方法 / unsupported method: "+r.Method, "invalid_request_error")
	}
}

// finishError 把 handler 返回的错误翻译成 JSON（对应 Python 的几个 except 分支）。
// poolStatus 是账号池层面错误的默认状态码：GET 503 / POST 400。
func (h *apiHandler) finishError(w *svrResponse, r *http.Request, err error, poolStatus int) {
	if w.wroteHeader {
		// 流式响应已经开出去，错误只能记日志（Python 里也没法再改状态码）。
		h.gw.Log(fmt.Sprintf("[error] %s %s: %v", r.Method, svrRequestURI(r), err))
		return
	}

	var httpErr *svrHTTPError
	if errors.As(err, &httpErr) {
		svrWriteError(w, r, httpErr.Status, httpErr.Msg, httpErr.Type)
		return
	}
	var badJSON *svrBadJSON
	if errors.As(err, &badJSON) {
		svrWriteError(w, r, 400, badJSON.Msg, "invalid_request_error")
		return
	}
	var poolErr *PoolError
	if errors.As(err, &poolErr) {
		svrWriteError(w, r, poolStatus, poolErr.Msg, "account_pool_error")
		return
	}

	h.gw.Log(fmt.Sprintf("[error] %s %s: %v", r.Method, svrRequestURI(r), err))
	svrWriteError(w, r, 502, "上游调用失败 / upstream failure: "+err.Error(), "upstream_error")
}

// options 对应 Python do_OPTIONS：204 + CORS 预检头。
func (h *apiHandler) options(w *svrResponse) {
	hdr := w.Header()
	hdr.Set("Access-Control-Allow-Origin", "*")
	hdr.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	hdr.Set("Access-Control-Allow-Headers", "*")
	hdr.Set("Content-Length", "0")
	w.WriteHeader(http.StatusNoContent)
}

// get 对应 Python do_GET。
func (h *apiHandler) get(w *svrResponse, r *http.Request) error {
	path := svrPath(svrRequestURI(r))

	if path == "/v1/health" || path == "/health" {
		return h.health(w)
	}
	if path == "/panel" || path == "/" || path == "/index.html" {
		return h.panelPage(w)
	}
	if !h.authorized(w, r) {
		return nil
	}

	switch {
	case path == "/v1/models":
		return h.models(w)
	case path == "/v1/points":
		return h.points(w)
	case path == "/v1/admin/accounts" || path == "/admin/accounts":
		return h.accounts(w)
	case path == "/api/panel/state":
		refresh := strings.Contains(svrRequestURI(r), "refresh=1")
		return svrWriteJSON(w, 200, h.gw.panel.State(refresh), nil)
	case path == "/api/panel/logs":
		return svrWriteJSON(w, 200, h.gw.panel.Logs(svrQueryInt(r, "lines", 200)), nil)
	case path == "/api/panel/models":
		return svrWriteJSON(w, 200, h.gw.panel.Models(), nil)
	case path == "/api/panel/docs":
		return svrWriteJSON(w, 200, h.gw.panel.Docs(), nil)
	case path == "/favicon.ico":
		return &svrHTTPError{Status: 404, Type: "invalid_request_error", Msg: "no favicon"}
	default:
		return &svrHTTPError{Status: 404, Type: "invalid_request_error",
			Msg: "未知路径 / unknown path: " + svrRequestURI(r)}
	}
}

// post 对应 Python do_POST。
func (h *apiHandler) post(w *svrResponse, r *http.Request) error {
	path := svrPath(svrRequestURI(r))

	if !h.authorized(w, r) {
		return nil
	}

	switch {
	case path == "/v1/chat/completions":
		return h.chat(w, r)
	case path == "/v1/messages":
		return h.messages(w, r)
	case path == "/v1/embeddings":
		return h.passthrough(w, r, "embeddings")
	case path == "/v1/images/generations":
		return h.passthrough(w, r, "images/generations")
	case path == "/v1/admin/accounts/reload" || path == "/admin/accounts/reload":
		if err := h.gw.acctPool.Load(); err != nil {
			return err
		}
		h.gw.RefreshModels()
		return svrWriteJSON(w, 200, h.gw.acctPool.Snapshot(), nil)
	case strings.HasPrefix(path, "/api/panel/"):
		if path == "/api/panel/config/reload" {
			return svrWriteJSON(w, 200, h.gw.ReloadConfig(), nil)
		}
		return h.panelAPI(w, r, path)
	default:
		return &svrHTTPError{Status: 404, Type: "invalid_request_error",
			Msg: "未知路径 / unknown path: " + svrRequestURI(r)}
	}
}

// ---- 简单路由

// health 对应 Python _health。
func (h *apiHandler) health(w *svrResponse) error {
	gw := h.gw
	usable := gw.acctPool.Usable()
	status := "degraded"
	if len(usable) > 0 {
		status = "ok"
	}
	return svrWriteJSON(w, 200, map[string]any{
		"status":          status,
		"name":            "loomy2api",
		"upstream":        gw.cfg.Upstream,
		"accounts":        len(gw.acctPool.Accounts()),
		"usable_accounts": len(usable),
		"models":          len(gw.Catalogue()),
		"auth_required":   len(gw.cfg.APIKeys) > 0,
		"strategy":        gw.cfg.Strategy,
		"panel":           fmt.Sprintf("http://%s:%d/panel", gw.cfg.Host, gw.cfg.Port),
	}, nil)
}

// models 对应 Python _models：目录为空时先拉一次上游。
func (h *apiHandler) models(w *svrResponse) error {
	catalogue := h.gw.Catalogue()
	if len(catalogue) == 0 {
		h.gw.RefreshModels()
		catalogue = h.gw.Catalogue()
	}
	return svrWriteJSON(w, 200, map[string]any{"object": "list", "data": catalogue}, nil)
}

// points 对应 Python _points：逐账号刷新额度并按 userid 去重合计。
func (h *apiHandler) points(w *svrResponse) error {
	gw := h.gw
	accounts := gw.acctPool.Accounts()
	out := make([]any, 0, len(accounts))
	for _, acc := range accounts {
		gw.acctPool.RefreshQuota(acc)
		out = append(out, map[string]any{
			"name":          acc.Name,
			"userid":        acc.Userid,
			"available":     accountOptInt(acc.Available),
			"balance":       accountOptInt(acc.Balance),
			"daily_balance": accountOptInt(acc.DailyBalance),
			"requests":      acc.Requests,
			"points_used":   acc.PointsUsed,
		})
	}
	gw.acctPool.Save(false)

	// 同一个账号可能以两条会话出现（自己的 + 客户端导入的），额度只算一次。
	seen := map[string]int64{}
	order := make([]string, 0, len(out))
	for _, item := range out {
		entry, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		key := jstr(entry, "userid")
		if key == "" {
			key = jstr(entry, "name")
		}
		available, isInt := entry["available"].(int64)
		if !isInt {
			continue
		}
		if _, ok := seen[key]; !ok {
			order = append(order, key)
		}
		if available > seen[key] {
			seen[key] = available
		}
	}
	total := int64(0)
	for _, key := range order {
		total += seen[key]
	}
	return svrWriteJSON(w, 200, map[string]any{
		"total_available": total,
		"unique_accounts": len(seen),
		"accounts":        out,
	}, nil)
}

// accounts 对应 Python _accounts。
func (h *apiHandler) accounts(w *svrResponse) error {
	return svrWriteJSON(w, 200, h.gw.acctPool.Snapshot(), nil)
}

// panelPage 对应 Python _panel_page：面板 HTML 不过 auth，也不带 CORS。
func (h *apiHandler) panelPage(w *svrResponse) error {
	body := h.gw.panel.HTML()
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	hdr.Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	_, err := w.Write(body)
	return err
}

// panelAPI 对应 Python _panel_api：POST /api/panel/* 分发到 Panel 的方法。
func (h *apiHandler) panelAPI(w *svrResponse, r *http.Request, path string) error {
	payload, err := svrReadJSON(r)
	if err != nil {
		return err
	}
	panel := h.gw.panel

	var (
		result map[string]any
		herr   error
	)
	switch path {
	case "/api/panel/accounts":
		result, herr = panel.AddAccount(payload)
	case "/api/panel/accounts/update":
		result, herr = panel.UpdateAccount(payload)
	case "/api/panel/accounts/remove":
		result, herr = panel.RemoveAccount(payload)
	case "/api/panel/accounts/renew":
		result, herr = panel.RenewAccount(payload)
	case "/api/panel/accounts/identity":
		result, herr = panel.Identity(payload)
	case "/api/panel/refresh":
		result, herr = panel.Refresh(payload)
	default:
		return &svrHTTPError{Status: 404, Type: "invalid_request_error",
			Msg: "未知面板接口 / unknown panel endpoint: " + path}
	}
	if herr != nil {
		return herr
	}
	if result == nil {
		// Python: 非 dict 的返回值包成 {"ok": True, "result": ...}
		result = map[string]any{"ok": true}
	}
	return svrWriteJSON(w, 200, result, nil)
}

// passthrough 对应 Python _passthrough：原样转发 embeddings / images/generations。
func (h *apiHandler) passthrough(w *svrResponse, r *http.Request, suffix string) error {
	gw := h.gw
	raw, err := svrReadBody(r)
	if err != nil {
		return err
	}
	payload := map[string]any{}
	if len(raw) > 0 {
		var obj map[string]any
		if jerr := json.Unmarshal(raw, &obj); jerr != nil {
			return &svrBadJSON{Msg: fmt.Sprintf(
				"请求体不是合法 JSON / body is not valid JSON: %v", jerr)}
		}
		if obj != nil {
			payload = obj
		}
	}

	acc, aerr := gw.acctPool.Acquire()
	if aerr != nil {
		return aerr
	}
	status, headers, data, cerr := gw.modelsClient.Call(acc.Session, suffix, payload, nil, "POST")
	if cerr != nil {
		return cerr
	}
	if status >= 400 {
		gw.acctPool.ReportFailure(acc, status, httpcText(data, 200))
	} else {
		gw.acctPool.ReportSuccess(acc, nil)
	}

	contentType := ""
	if headers != nil {
		contentType = headers.Get("Content-Type")
	}
	if contentType == "" {
		contentType = "application/json"
	}
	hdr := w.Header()
	hdr.Set("Content-Type", contentType)
	hdr.Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, werr := w.Write(data)
	return werr
}

// ------------------------------------------------------------ chat（核心）

// chat 对应 Python _chat。
func (h *apiHandler) chat(w *svrResponse, r *http.Request) error {
	gw := h.gw
	req, err := svrReadJSON(r)
	if err != nil {
		return err
	}
	model := gw.ResolveModel(jstr(req, "model"))
	stream := anthTruthy(req["stream"])

	extra := map[string]string{}
	for _, pair := range [][2]string{
		{"chat_id", "ChatId"}, {"msg_id", "MsgId"}, {"turn_id", "TurnId"},
	} {
		value, ok := req[pair[0]]
		delete(req, pair[0])
		if ok && anthTruthy(value) {
			extra[pair[1]] = anthStr(value)
		}
	}
	if purpose, ok := req["request_purpose"]; ok {
		delete(req, "request_purpose")
		if anthTruthy(purpose) {
			extra["X-Loomy-Request-Purpose"] = anthStr(purpose)
		}
	}
	req["model"] = model

	if stream {
		return h.chatStream(w, r, req, extra)
	}

	tried := []string{}
	lastStatus := 0
	var lastData []byte
	for i := 0; i < gw.maxAttempts(); i++ {
		acc, aerr := gw.acctPool.Acquire(tried...)
		if aerr != nil {
			gw.Log(fmt.Sprintf("[warn] %v", aerr))
			break
		}
		tried = append(tried, acc.Name)
		started := time.Now()
		status, _, data, cerr := gw.modelsClient.Call(acc.Session, "chat/completions", req, extra, "POST")
		if cerr != nil {
			return cerr
		}
		if status == 200 {
			var obj map[string]any
			if jerr := json.Unmarshal(data, &obj); jerr != nil {
				obj = nil // Python: except → obj = None
			}
			usage := ExtractUsage(obj)
			gw.acctPool.ReportSuccess(acc, usage)
			gw.Log(fmt.Sprintf("[call] %s %s %.1fs http=200 %s",
				acc.Name, model, time.Since(started).Seconds(), HumanUsage(usage)))
			return svrWriteJSON(w, 200, obj, nil)
		}
		lastStatus, lastData = status, data
		if svrStatusIn(status, svrAccountFault) {
			gw.Log(fmt.Sprintf("[call] %s %s http=%d → 账号问题，换账号重试",
				acc.Name, model, status))
			gw.acctPool.ReportFailure(acc, status, httpcText(data, 200))
		} else {
			// 上游抽风（502/504/500）不是账号的错：换一个重试，但不冷却它。
			gw.Log(fmt.Sprintf("[call] %s %s http=%d → 上游异常，换账号重试（不冷却该账号）",
				acc.Name, model, status))
		}
		if svrStatusIn(status, svrFatalRequest) {
			break
		}
	}

	if lastStatus == 0 {
		lastStatus = 502
	}
	var obj any
	if jerr := json.Unmarshal(lastData, &obj); jerr == nil {
		return svrWriteJSON(w, lastStatus, obj, nil)
	}
	return &svrHTTPError{Status: lastStatus, Type: "upstream_error",
		Msg: httpcText(lastData, 500)}
}

// chatStream 对应 Python _chat_stream：拿到 200 后逐块原样转发并聚合 usage。
func (h *apiHandler) chatStream(w *svrResponse, r *http.Request, req map[string]any, extra map[string]string) error {
	gw := h.gw
	model := jstr(req, "model")

	tried := []string{}
	var acc *Account
	var resp *http.Response

	for i := 0; i < gw.maxAttempts(); i++ {
		cur, aerr := gw.acctPool.Acquire(tried...)
		if aerr != nil {
			gw.Log(fmt.Sprintf("[warn] %v", aerr))
			break
		}
		acc = cur
		tried = append(tried, cur.Name)

		rs, serr := gw.modelsClient.Stream(r.Context(), cur.Session, "chat/completions", req, extra)
		if serr != nil {
			return serr
		}
		resp = rs
		if rs.StatusCode == 200 {
			break
		}
		// 非 200 必须把 body 读完再关，否则连接没法复用。
		data, _ := io.ReadAll(rs.Body)
		_ = rs.Body.Close()
		if svrStatusIn(rs.StatusCode, svrAccountFault) {
			gw.Log(fmt.Sprintf("[call] %s %s stream http=%d → 账号问题，换账号重试",
				cur.Name, model, rs.StatusCode))
			gw.acctPool.ReportFailure(cur, rs.StatusCode, httpcText(data, 200))
		} else {
			gw.Log(fmt.Sprintf("[call] %s %s stream http=%d → 上游异常，换账号重试（不冷却该账号）",
				cur.Name, model, rs.StatusCode))
		}
		resp = nil
	}

	if resp == nil {
		return &svrHTTPError{Status: 502, Type: "upstream_error",
			Msg: "所有账号都失败了 / all accounts failed"}
	}
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		var obj any
		if jerr := json.Unmarshal(data, &obj); jerr == nil {
			return svrWriteJSON(w, resp.StatusCode, obj, nil)
		}
		return &svrHTTPError{Status: resp.StatusCode, Type: "upstream_error",
			Msg: httpcText(data, 500)}
	}

	defer func() { _ = resp.Body.Close() }()

	started := time.Now()
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "close")
	hdr.Set("X-Accel-Buffering", "no")
	hdr.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(200)
	w.Flush()

	usage := map[string]any{}
	disconnected := func() error {
		gw.Log(fmt.Sprintf("[call] %s %s client disconnected", acc.Name, model))
		return nil
	}

	buffer := []byte{}
	chunk := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			if _, werr := w.Write(chunk[:n]); werr != nil {
				return disconnected()
			}
			w.Flush()
			buffer = append(buffer, chunk[:n]...)
			for {
				idx := bytes.IndexByte(buffer, '\n')
				if idx < 0 {
					break
				}
				SSEUsage(strings.TrimSpace(string(buffer[:idx])), usage)
				buffer = buffer[idx+1:]
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			if r.Context().Err() != nil {
				return disconnected()
			}
			return &svrHTTPError{Status: 502, Type: "upstream_error",
				Msg: "上游读取失败 / upstream read failed: " + rerr.Error()}
		}
		if n == 0 {
			break
		}
	}
	if len(buffer) > 0 {
		SSEUsage(strings.TrimSpace(string(buffer)), usage)
	}

	gw.acctPool.ReportSuccess(acc, usage)
	gw.Log(fmt.Sprintf("[call] %s %s %.1fs http=200 %s stream",
		acc.Name, model, time.Since(started).Seconds(), HumanUsage(usage)))
	return nil
}

// ---------------------------------------------------- anthropic messages

// messages 对应 Python _messages。
func (h *apiHandler) messages(w *svrResponse, r *http.Request) error {
	gw := h.gw
	req, err := svrReadJSON(r)
	if err != nil {
		return err
	}
	payload := AnthropicToOpenAI(req)
	model := gw.ResolveModel(jstr(payload, "model"))
	payload["model"] = model

	if anthTruthy(req["stream"]) {
		return h.messagesStream(w, r, payload, model)
	}

	tried := []string{}
	lastStatus := 0
	var lastData []byte
	for i := 0; i < gw.maxAttempts(); i++ {
		acc, aerr := gw.acctPool.Acquire(tried...)
		if aerr != nil {
			gw.Log(fmt.Sprintf("[warn] %v", aerr))
			break
		}
		tried = append(tried, acc.Name)
		started := time.Now()
		status, _, data, cerr := gw.modelsClient.Call(acc.Session, "chat/completions", payload, nil, "POST")
		if cerr != nil {
			return cerr
		}
		if status == 200 {
			var obj map[string]any
			if jerr := json.Unmarshal(data, &obj); jerr != nil {
				return fmt.Errorf("chat/completions 响应不是合法 JSON / invalid JSON: %w", jerr)
			}
			usage := ExtractUsage(obj)
			gw.acctPool.ReportSuccess(acc, usage)
			gw.Log(fmt.Sprintf("[messages] %s %s %.1fs http=200 %s",
				acc.Name, model, time.Since(started).Seconds(), HumanUsage(usage)))
			return svrWriteJSON(w, 200, OpenAIToAnthropic(obj, model), nil)
		}
		lastStatus, lastData = status, data
		if svrStatusIn(status, svrAccountFault) {
			gw.acctPool.ReportFailure(acc, status, httpcText(data, 200))
			gw.Log(fmt.Sprintf("[messages] %s %s http=%d → 账号问题，换账号重试",
				acc.Name, model, status))
		} else {
			gw.Log(fmt.Sprintf("[messages] %s %s http=%d → 上游异常，换账号重试（不冷却该账号）",
				acc.Name, model, status))
		}
		if svrStatusIn(status, svrFatalRequest) {
			break
		}
	}

	if lastStatus == 0 {
		lastStatus = 502
	}
	return svrWriteJSON(w, lastStatus, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": httpcText(lastData, 500),
		},
	}, nil)
}

// messagesStream 对应 Python _messages_stream：把上游 SSE 翻成 Anthropic 事件。
func (h *apiHandler) messagesStream(w *svrResponse, r *http.Request, payload map[string]any, model string) error {
	gw := h.gw
	tried := []string{}
	var acc *Account
	var resp *http.Response

	for i := 0; i < gw.maxAttempts(); i++ {
		cur, aerr := gw.acctPool.Acquire(tried...)
		if aerr != nil {
			gw.Log(fmt.Sprintf("[warn] %v", aerr))
			break
		}
		acc = cur
		tried = append(tried, cur.Name)

		rs, serr := gw.modelsClient.Stream(r.Context(), cur.Session, "chat/completions", payload, nil)
		if serr != nil {
			return serr
		}
		resp = rs
		if rs.StatusCode == 200 {
			break
		}
		data, _ := io.ReadAll(rs.Body)
		_ = rs.Body.Close()
		if svrStatusIn(rs.StatusCode, svrAccountFault) {
			gw.acctPool.ReportFailure(cur, rs.StatusCode, httpcText(data, 200))
			gw.Log(fmt.Sprintf("[messages] %s %s stream http=%d → 账号问题，换账号重试",
				cur.Name, model, rs.StatusCode))
		} else {
			gw.Log(fmt.Sprintf("[messages] %s %s stream http=%d → 上游异常，换账号重试（不冷却该账号）",
				cur.Name, model, rs.StatusCode))
		}
		resp = nil
	}

	if resp == nil {
		return svrWriteJSON(w, 502, map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": "all accounts failed",
			},
		}, nil)
	}
	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return svrWriteJSON(w, resp.StatusCode, map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": httpcText(data, 500),
			},
		}, nil)
	}

	defer func() { _ = resp.Body.Close() }()

	translator := NewStreamTranslator(model)
	started := time.Now()
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream; charset=utf-8")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "close")
	hdr.Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(200)
	w.Flush()

	emit := func(ev AnthropicEvent) error {
		text, jerr := jsonDumps(ev.Data)
		if jerr != nil {
			text = "{}"
		}
		if _, werr := io.WriteString(w, "event: "+ev.Name+"\ndata: "+text+"\n\n"); werr != nil {
			return werr
		}
		w.Flush()
		return nil
	}
	emitAll := func(events []AnthropicEvent) error {
		for _, ev := range events {
			if err := emit(ev); err != nil {
				return err
			}
		}
		return nil
	}
	disconnected := func() error {
		gw.Log(fmt.Sprintf("[messages] %s %s client disconnected", acc.Name, model))
		return nil
	}

	if err := emitAll(translator.Start()); err != nil {
		return disconnected()
	}

	buffer := []byte{}
	chunk := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(chunk)
		if n > 0 {
			buffer = append(buffer, chunk[:n]...)
			for {
				idx := bytes.IndexByte(buffer, '\n')
				if idx < 0 {
					break
				}
				line := strings.TrimSpace(string(buffer[:idx]))
				buffer = buffer[idx+1:]
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				body := strings.TrimSpace(line[len("data:"):])
				if body == "" || body == "[DONE]" {
					continue
				}
				var obj map[string]any
				if jerr := json.Unmarshal([]byte(body), &obj); jerr != nil {
					continue
				}
				if err := emitAll(translator.Feed(obj)); err != nil {
					return disconnected()
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			if r.Context().Err() != nil {
				return disconnected()
			}
			return &svrHTTPError{Status: 502, Type: "upstream_error",
				Msg: "上游读取失败 / upstream read failed: " + rerr.Error()}
		}
		if n == 0 {
			break
		}
	}

	if err := emitAll(translator.Finish()); err != nil {
		return disconnected()
	}
	usage := translator.Usage()
	gw.acctPool.ReportSuccess(acc, usage)
	gw.Log(fmt.Sprintf("[messages] %s %s %.1fs http=200 %s stream",
		acc.Name, model, time.Since(started).Seconds(), HumanUsage(usage)))
	return nil
}

// ------------------------------------------------------------------ auth

// authorized 对应 Python _authorized：没有配置 api_keys 时全部放行。
func (h *apiHandler) authorized(w *svrResponse, r *http.Request) bool {
	keys := h.gw.cfg.APIKeys
	if len(keys) == 0 {
		return true
	}
	token, source := svrPresentedKey(r)
	token = svrTrimKey(token)
	if token != "" {
		for _, key := range keys {
			if token == key {
				return true
			}
		}
	}
	// 绝不记录密钥本身 —— 只留够看出「来的是什么」的信息。
	h.gw.Log(fmt.Sprintf("[auth] 拒绝 %s %s ← %s token=%s",
		r.Method, svrRequestURI(r), source, svrMask(token)))
	svrWriteError(w, r, 401, "无效的 API Key / invalid API key", "authentication_error")
	return false
}

// svrPresentedKey 对应 Python _presented_key：从 Authorization / 各种 api-key 头 /
// 查询参数里把调用方的 key 挖出来，返回 (值, 来源)。
func svrPresentedKey(r *http.Request) (string, string) {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if raw != "" {
		lower := strings.ToLower(raw)
		if strings.HasPrefix(lower, "bearer ") {
			return strings.TrimSpace(raw[len("bearer "):]), "authorization:bearer"
		}
		if strings.HasPrefix(lower, "token ") {
			return strings.TrimSpace(raw[len("token "):]), "authorization:token"
		}
		return raw, "authorization:raw"
	}
	for _, header := range []string{"x-api-key", "api-key", "apikey", "x-goog-api-key"} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value, "header:" + header
		}
	}
	query := r.URL.Query()
	for _, name := range []string{"api_key", "apikey", "key", "access_token"} {
		if values := query[name]; len(values) > 0 && values[0] != "" {
			return strings.TrimSpace(values[0]), "query:" + name
		}
	}
	return "", "none"
}

// svrTrimKey 对应 Python 的 token.strip().strip('"').strip("'")。
func svrTrimKey(token string) string {
	token = strings.TrimSpace(token)
	token = strings.Trim(token, `"`)
	token = strings.Trim(token, "'")
	return token
}

// svrMask 对应 Python _mask：空 → (empty)；≤12 → 前4…；否则 前8…后4(len=N)。
func svrMask(value string) string {
	if value == "" {
		return "(empty)"
	}
	runes := []rune(value)
	if len(runes) <= 12 {
		return string(runes[:4]) + "…"
	}
	return string(runes[:8]) + "…" + string(runes[len(runes)-4:]) +
		fmt.Sprintf("(len=%d)", len(runes))
}

// ------------------------------------------------------------- 工具函数

// svrPath 对应 Python _path：去掉 query、折叠 /v1/v1、根路径指向面板，
// 非 /v1 前缀补上 /v1（面板 / 面板 API / health / admin / favicon 豁免）。
func svrPath(rawURI string) string {
	path := rawURI
	if idx := strings.Index(path, "?"); idx >= 0 {
		path = path[:idx]
	}
	for strings.Contains(path, "/v1/v1") {
		path = strings.ReplaceAll(path, "/v1/v1", "/v1")
	}
	if path == "" || path == "/" || path == "/index.html" {
		return "/panel" // 面板就是首页
	}
	exempt := []string{"/panel", "/api/", "/health", "/admin", "/favicon"}
	exempted := false
	for _, prefix := range exempt {
		if strings.HasPrefix(path, prefix) {
			exempted = true
			break
		}
	}
	if !exempted && path != "/v1" && !strings.HasPrefix(path, "/v1/") {
		path = "/v1" + path
	}
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}
	return path
}

// svrRequestURI 返回客户端发来的原始 path?query（对应 Python 的 self.path）。
func svrRequestURI(r *http.Request) string {
	if r == nil {
		return ""
	}
	if r.RequestURI != "" {
		return r.RequestURI
	}
	return r.URL.RequestURI()
}

// svrQueryInt 对应 Python _query_int：解析失败用默认值。
func svrQueryInt(r *http.Request, name string, def int) int {
	values := r.URL.Query()[name]
	if len(values) == 0 {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(values[0]))
	if err != nil {
		return def
	}
	return n
}

// svrReadBody 对应 Python _read_body（body 只读一次，这里由 handler 自身保证）。
func svrReadBody(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil {
		return nil, nil
	}
	return io.ReadAll(r.Body)
}

// svrReadJSON 对应 Python _read_json：空 body → {}，解析失败 → svrBadJSON。
func svrReadJSON(r *http.Request) (map[string]any, error) {
	raw, err := svrReadBody(r)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var obj map[string]any
	if jerr := json.Unmarshal(raw, &obj); jerr != nil {
		return nil, &svrBadJSON{Msg: fmt.Sprintf(
			"请求体不是合法 JSON / body is not valid JSON: %v", jerr)}
	}
	if obj == nil {
		obj = map[string]any{}
	}
	return obj, nil
}

// svrWriteJSON 对应 Python _json：JSON + Content-Length + CORS，可选额外响应头。
func svrWriteJSON(w *svrResponse, status int, obj any, extra map[string]string) error {
	text, err := jsonDumps(obj)
	if err != nil {
		text = `{"error":{"message":"序列化失败 / encode error","type":"upstream_error","code":500}}`
		status = 500
	}
	body := []byte(text)

	hdr := w.Header()
	hdr.Set("Content-Type", "application/json; charset=utf-8")
	hdr.Set("Content-Length", strconv.Itoa(len(body)))
	hdr.Set("Access-Control-Allow-Origin", "*")
	for key, value := range extra {
		hdr.Set(key, value)
	}
	w.WriteHeader(status)
	_, werr := w.Write(body)
	return werr
}

// svrWriteError 对应 Python _error：把未读的请求体读完再关连接（Windows 上
// 提前拒绝会变成 RST），响应形状 {"error":{"message","type","code"}}。
func svrWriteError(w *svrResponse, r *http.Request, status int, message, kind string) {
	if r != nil && r.Body != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
	}
	w.Header().Set("Connection", "close")
	_ = svrWriteJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"code":    status,
		},
	}, nil)
}

// svrStatusIn 判断 status 是否落在给定集合里。
func svrStatusIn(status int, set []int) bool {
	for _, item := range set {
		if item == status {
			return true
		}
	}
	return false
}
