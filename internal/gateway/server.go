package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	Identity          = "codex-gateway"
	maxRequestBytes   = 64 << 20
	maxErrorBytes     = 1 << 20
	maxCompatRetries  = 16
	requestTimeout    = 30 * time.Second
	upstreamTimeout   = 300 * time.Second
	authRotationGrace = 120 * time.Second
	shutdownTimeout   = 5 * time.Second
)

var hopHeaders = headerSet("Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade", "Host", "Content-Length")
var authHeaders = headerSet("Authorization", "Cookie", "Cookie2", "X-API-Key", "API-Key", "X-Codex-Gateway-Token", "X-Codex-Router-Token")
var apiHeaders = headerSet("Accept", "Accept-Language", "Content-Type", "User-Agent", "OpenAI-Beta", "X-Codex-Beta-Features", "X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-ID", "X-Request-ID", "Idempotency-Key")

type gatewayServer struct {
	config             *Config
	catalog            Catalog
	fingerprint        string
	adminToken         string
	clientToken        string
	official           *officialAuth
	authPath           string
	clients            map[string]*http.Client
	authMu             sync.Mutex
	authHashes         map[[sha256.Size]byte]time.Time
	now                func() time.Time
	requestReadTimeout time.Duration
	activityMu         sync.Mutex
	active             int
	stopping           bool
	stop               chan struct{}
	stopOnce           sync.Once
}

// Serve runs the authenticated loopback gateway until its control endpoint or a
// process signal requests shutdown. Only this server's listeners are closed.
func Serve(home string) error {
	g, err := newGatewayServer(home)
	if err != nil {
		return err
	}
	defer g.closeIdleConnections()
	if g.config.Listen.Host != "127.0.0.1" {
		return errors.New("gateway must listen on 127.0.0.1")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(g.config.Listen.Host, strconv.Itoa(g.config.Listen.Port)))
	if err != nil {
		return fmt.Errorf("cannot listen on the configured gateway address: %w", err)
	}
	server := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: requestTimeout,
		ReadTimeout:       requestTimeout,
		IdleTimeout:       requestTimeout,
		MaxHeaderBytes:    1 << 20,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()
	select {
	case err = <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signals.Done():
		g.activityMu.Lock()
		g.stopping = true
		g.activityMu.Unlock()
	case <-g.stop:
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		_ = server.Close()
	}
	err = <-finished
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func newGatewayServer(home string) (*gatewayServer, error) {
	cfg, err := LoadConfig(home)
	if err != nil {
		return nil, err
	}
	catalog, err := LoadCatalog(home)
	if err != nil {
		return nil, err
	}
	fingerprint, err := Fingerprint(home)
	if err != nil {
		return nil, err
	}
	admin, err := ReadToken(home, "admin")
	if err != nil {
		return nil, err
	}
	client := ""
	if cfg.ClientAuth == "token" {
		client, err = ReadToken(home, "client")
		if err != nil {
			return nil, err
		}
	}
	g := &gatewayServer{
		config: cfg, catalog: catalog, fingerprint: fingerprint,
		adminToken: admin, clientToken: client,
		authPath:   filepath.Join(cfg.CodexHome, "auth.json"),
		clients:    make(map[string]*http.Client),
		authHashes: make(map[[sha256.Size]byte]time.Time),
		now:        time.Now, requestReadTimeout: requestTimeout, stop: make(chan struct{}),
	}
	if cfg.ClientAuth == "token" {
		g.official = newOfficialAuth(home, cfg)
	}
	for name, provider := range cfg.Providers {
		client, err := newUpstreamClient(provider.Proxy)
		if err != nil {
			g.closeIdleConnections()
			return nil, errors.New("invalid configured upstream proxy")
		}
		g.clients[name] = client
	}
	return g, nil
}

// A deadline on each socket operation bounds stalled streams without imposing
// an overall duration limit on a healthy long-running SSE response.
type upstreamConn struct{ net.Conn }

func (c *upstreamConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(upstreamTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}
func (c *upstreamConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(upstreamTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func newUpstreamClient(proxy string) (*http.Client, error) {
	dialer := &net.Dialer{Timeout: requestTimeout, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			return &upstreamConn{conn}, nil
		},
		TLSHandshakeTimeout:   requestTimeout,
		ResponseHeaderTimeout: upstreamTimeout,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		DisableCompression:    true,
	}
	// A nil Proxy is deliberately direct. ProxyURL does not consult NO_PROXY or
	// any process environment setting, including for HTTPS CONNECT requests.
	if proxy != "" {
		parsed, err := url.Parse(proxy)
		if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return nil, errors.New("unsupported upstream proxy")
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, nil
}

func (g *gatewayServer) closeIdleConnections() {
	for _, client := range g.clients {
		client.CloseIdleConnections()
	}
}

func (g *gatewayServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.RequestURI, "/") || strings.HasPrefix(r.RequestURI, "//") ||
		r.URL.IsAbs() || r.URL.Host != "" || strings.Contains(r.RequestURI, "#") {
		gatewayError(w, 400, "An origin-relative request target is required")
		return
	}
	path := r.URL.EscapedPath()
	if r.Method == http.MethodGet && path == "/_gateway/health" {
		if !g.administrative(r.Header) {
			gatewayError(w, 403, "Gateway administration token required")
			return
		}
		g.activityMu.Lock()
		active := g.active
		g.activityMu.Unlock()
		replyJSON(w, 200, map[string]any{"service": Identity, "fingerprint": g.fingerprint, "active_requests": active, "pid": os.Getpid()})
		return
	}
	if r.Method == http.MethodPost && path == "/_gateway/shutdown" {
		if !g.administrative(r.Header) {
			gatewayError(w, 403, "Gateway administration token required")
			return
		}
		g.activityMu.Lock()
		if g.active != 0 {
			g.activityMu.Unlock()
			gatewayError(w, 409, "Gateway has active requests; wait for them to finish")
			return
		}
		g.stopping = true
		g.activityMu.Unlock()
		w.Header().Set("Connection", "close")
		replyJSON(w, 200, map[string]any{"stopping": true})
		g.stopOnce.Do(func() { close(g.stop) })
		return
	}
	if !g.authenticated(r.Header) {
		gatewayError(w, 401, "Gateway client credential required")
		return
	}
	if (path == "/v1/responses" || path == "/responses") && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		gatewayError(w, 426, "Use the Responses HTTP/SSE transport")
		return
	}
	if r.Method == http.MethodGet {
		if path == "/v1/models" || path == "/models" {
			data := make([]map[string]any, 0, len(g.catalog.Models))
			for _, model := range g.catalog.Models {
				data = append(data, map[string]any{"id": model["slug"], "object": "model"})
			}
			replyJSON(w, 200, map[string]any{"models": g.catalog.Models, "object": "list", "data": data})
			return
		}
		gatewayError(w, 404, "Unknown gateway endpoint")
		return
	}
	if r.Method != http.MethodPost {
		gatewayError(w, 405, "Unsupported gateway method")
		return
	}
	g.activityMu.Lock()
	if g.stopping {
		g.activityMu.Unlock()
		gatewayError(w, 503, "Gateway is stopping; retry after it restarts")
		return
	}
	g.active++
	g.activityMu.Unlock()
	defer func() { g.activityMu.Lock(); g.active--; g.activityMu.Unlock() }()
	g.forward(w, r, path)
}

func replyJSON(w http.ResponseWriter, code int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		data = []byte(`{"error":{"type":"local_gateway_error","message":"Cannot encode gateway response"}}`)
		code = 500
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(code)
	_, _ = w.Write(data)
}

func gatewayError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Connection", "close")
	replyJSON(w, code, map[string]any{"error": map[string]string{"type": "local_gateway_error", "message": message}})
}

func singleHeader(h http.Header, name string) (string, bool) {
	values := h.Values(name)
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

func equalCredential(a, b string) bool {
	x, y := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(x[:], y[:]) == 1
}

func (g *gatewayServer) administrative(h http.Header) bool {
	value, ok := singleHeader(h, "X-Codex-Gateway-Token")
	return ok && equalCredential(value, g.adminToken)
}

func (g *gatewayServer) authenticated(h http.Header) bool {
	value, ok := singleHeader(h, "Authorization")
	if !ok {
		return false
	}
	scheme, token, found := strings.Cut(value, " ")
	if !found || !strings.EqualFold(scheme, "bearer") || token == "" || strings.IndexFunc(token, unicode.IsSpace) >= 0 {
		return false
	}
	if g.config.ClientAuth == "token" {
		return equalCredential(token, g.clientToken)
	}
	g.authMu.Lock()
	defer g.authMu.Unlock()
	g.refreshAuth()
	supplied := sha256.Sum256([]byte(token))
	matched := 0
	for digest := range g.authHashes {
		matched |= subtle.ConstantTimeCompare(supplied[:], digest[:])
	}
	return matched == 1
}

func (g *gatewayServer) refreshAuth() {
	now := g.now()
	if file, err := os.Open(g.authPath); err == nil {
		raw, err := io.ReadAll(io.LimitReader(file, maxErrorBytes+1))
		_ = file.Close()
		if err == nil && len(raw) <= maxErrorBytes {
			if value, err := decodeGatewayJSON(raw); err == nil {
				if auth, ok := value.(map[string]any); ok {
					tokens, _ := auth["tokens"].(map[string]any)
					for _, candidate := range []any{auth["OPENAI_API_KEY"], tokens["access_token"]} {
						if token, ok := candidate.(string); ok && token != "" {
							g.authHashes[sha256.Sum256([]byte(token))] = now
						}
					}
				}
			}
		}
	}
	for digest, seen := range g.authHashes {
		if now.Sub(seen) > authRotationGrace {
			delete(g.authHashes, digest)
		}
	}
	for len(g.authHashes) > 4 {
		var oldest [sha256.Size]byte
		var oldestTime time.Time
		for digest, seen := range g.authHashes {
			if oldestTime.IsZero() || seen.Before(oldestTime) {
				oldest, oldestTime = digest, seen
			}
		}
		delete(g.authHashes, oldest)
	}
}

func readGatewayBody(w http.ResponseWriter, r *http.Request, timeout time.Duration) (map[string]any, bool) {
	if len(r.TransferEncoding) != 0 || len(r.Header.Values("Transfer-Encoding")) != 0 {
		gatewayError(w, 411, "Content-Length is required; transfer-encoded requests are unsupported")
		return nil, false
	}
	if encoding, ok := singleHeader(r.Header, "Content-Encoding"); len(r.Header.Values("Content-Encoding")) > 0 && (!ok || !strings.EqualFold(encoding, "identity")) {
		gatewayError(w, 415, "Disable Codex request compression for the gateway")
		return nil, false
	}
	length, present := singleHeader(r.Header, "Content-Length")
	if !present {
		code := 411
		if len(r.Header.Values("Content-Length")) != 0 {
			code = 400
		}
		gatewayError(w, code, "Exactly one Content-Length header is required")
		return nil, false
	}
	if len(length) > 10 || r.ContentLength < 1 || r.ContentLength > maxRequestBytes {
		gatewayError(w, 413, "Request size must be between 1 byte and 64 MiB")
		return nil, false
	}
	for _, c := range length {
		if c < '0' || c > '9' {
			gatewayError(w, 400, "Invalid Content-Length")
			return nil, false
		}
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(timeout))
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	_ = controller.SetReadDeadline(time.Time{})
	if err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			gatewayError(w, 408, "Timed out while reading the request body")
		} else {
			gatewayError(w, 400, "Incomplete request body")
		}
		return nil, false
	}
	if int64(len(raw)) != r.ContentLength {
		gatewayError(w, 400, "Incomplete request body")
		return nil, false
	}
	value, err := decodeGatewayJSON(raw)
	if err != nil {
		gatewayError(w, 400, "Invalid JSON request")
		return nil, false
	}
	body, ok := value.(map[string]any)
	model, _ := body["model"].(string)
	if !ok || model == "" {
		gatewayError(w, 400, "A model name is required")
		return nil, false
	}
	return body, true
}

// Token-level decoding detects duplicate names at every nesting level, while
// UseNumber preserves integers and decimal literals verbatim during forwarding.
func decodeGatewayJSON(raw []byte) (any, error) {
	if !utf8.Valid(raw) {
		return nil, errors.New("invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := decodeGatewayValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err = decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON content")
	}
	return value, nil
}

func decodeGatewayValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 256 {
		return nil, errors.New("JSON nesting is too deep")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			object := make(map[string]any)
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("invalid JSON object name")
				}
				if _, exists := object[key]; exists {
					return nil, errors.New("duplicate JSON member")
				}
				value, err := decodeGatewayValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim('}') {
				return nil, errors.New("invalid JSON object")
			}
			return object, nil
		case '[':
			array := make([]any, 0)
			for decoder.More() {
				value, err := decodeGatewayValue(decoder, depth+1)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			end, err := decoder.Token()
			if err != nil || end != json.Delim(']') {
				return nil, errors.New("invalid JSON array")
			}
			return array, nil
		default:
			return nil, errors.New("invalid JSON delimiter")
		}
	case json.Number:
		if strings.ContainsAny(string(token), ".eE") {
			number, err := strconv.ParseFloat(string(token), 64)
			if err != nil || math.IsInf(number, 0) || math.IsNaN(number) {
				return nil, errors.New("nonfinite JSON number")
			}
		}
	}
	return token, nil
}

func headerSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[strings.ToLower(name)] = true
	}
	return set
}

func connectionHeaders(h http.Header) map[string]bool {
	tokens := make(map[string]bool)
	for _, line := range h.Values("Connection") {
		for _, token := range strings.Split(line, ",") {
			tokens[strings.ToLower(strings.TrimSpace(token))] = true
		}
	}
	return tokens
}

func upstreamHeaders(h http.Header, codex bool) http.Header {
	headers := make(http.Header)
	connection := connectionHeaders(h)
	for key, values := range h {
		name := strings.ToLower(key)
		if hopHeaders[name] || authHeaders[name] || connection[name] || name == "accept-encoding" || name == "expect" {
			continue
		}
		if !codex && !apiHeaders[name] {
			continue
		}
		headers[key] = append([]string(nil), values...)
	}
	headers.Set("Accept-Encoding", "identity")
	headers.Set("Content-Type", "application/json")
	return headers
}

func (g *gatewayServer) forward(w http.ResponseWriter, r *http.Request, path string) {
	suffix := strings.TrimPrefix(path, "/v1")
	if suffix != "/responses" && suffix != "/responses/compact" && suffix != "/responses/input_tokens" {
		gatewayError(w, 404, "Unsupported Responses endpoint")
		return
	}
	if r.URL.RawQuery != "" {
		gatewayError(w, 400, "Responses query parameters are unsupported; use the JSON body")
		return
	}
	body, ok := readGatewayBody(w, r, g.requestReadTimeout)
	if !ok {
		return
	}
	route, ok := g.config.Models[body["model"].(string)]
	if !ok {
		gatewayError(w, 400, "Unknown model alias")
		return
	}
	provider := g.config.Providers[route.Provider]
	body["model"] = route.Model
	headers := upstreamHeaders(r.Header, provider.Auth == "codex" && g.config.ClientAuth == "codex")
	var officialToken string
	if provider.Auth == "codex" {
		if g.config.ClientAuth == "codex" {
			headers.Set("Authorization", r.Header.Get("Authorization"))
		} else {
			if strings.TrimRight(provider.BaseURL, "/") != officialBaseURL || g.official == nil {
				gatewayError(w, 500, "Native ChatGPT credentials require the official endpoint")
				return
			}
			credential, err := g.official.credential(r.Context(), false, "")
			if err != nil {
				gatewayError(w, 503, err.Error())
				return
			}
			credential.apply(headers)
			officialToken = credential.token
			headers.Set("Originator", "codex_cli_rs")
		}
	} else {
		key, err := ResolveAPIKey(provider)
		if err != nil || key == "" || strings.ContainsAny(key, "\r\n") {
			gatewayError(w, 503, "Upstream API credential is unavailable")
			return
		}
		headers.Set("Authorization", "Bearer "+key)
	}
	client := g.clients[route.Provider]
	if client == nil {
		gatewayError(w, 502, "Cannot complete the upstream request")
		return
	}
	var response *http.Response
	var buffered []byte
	refreshedOfficial := false
	for attempt := 0; attempt <= maxCompatRetries; attempt++ {
		data, err := json.Marshal(body)
		if err != nil {
			gatewayError(w, 400, "Invalid JSON request")
			return
		}
		request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, strings.TrimRight(provider.BaseURL, "/")+suffix, bytes.NewReader(data))
		if err != nil {
			gatewayError(w, 502, "Cannot complete the upstream request")
			return
		}
		request.Header = headers.Clone()
		response, err = client.Do(request)
		if err != nil {
			gatewayError(w, 502, "Cannot complete the upstream request")
			return
		}
		if response.StatusCode == http.StatusUnauthorized && officialToken != "" && !refreshedOfficial {
			_ = response.Body.Close()
			refreshedOfficial = true
			credential, refreshErr := g.official.credential(r.Context(), true, officialToken)
			if refreshErr != nil {
				gatewayError(w, 503, refreshErr.Error())
				return
			}
			credential.apply(headers)
			officialToken = credential.token
			attempt-- // Authentication retry is independent of compatibility retries.
			continue
		}
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			_ = response.Body.Close()
			gatewayError(w, 502, "Upstream redirects are unsupported")
			return
		}
		encoding, one := singleHeader(response.Header, "Content-Encoding")
		if len(response.Header.Values("Content-Encoding")) > 0 && (!one || !strings.EqualFold(encoding, "identity")) {
			_ = response.Body.Close()
			gatewayError(w, 502, "Upstream returned an unsupported compressed response")
			return
		}
		buffered = nil
		input, hasInput := body["input"].([]any)
		if !g.config.RetryInvalidEncryptedReasoning || response.StatusCode != 400 || !hasInput {
			break
		}
		buffered, err = io.ReadAll(io.LimitReader(response.Body, maxErrorBytes+1))
		if err != nil {
			_ = response.Body.Close()
			gatewayError(w, 502, "Cannot complete the upstream request")
			return
		}
		if len(buffered) > maxErrorBytes || attempt == maxCompatRetries {
			break
		}
		rejected := rejectedReasoningID(buffered)
		if rejected == "" {
			break
		}
		filtered := make([]any, 0, len(input))
		for _, item := range input {
			if !isRejectedReasoning(item, rejected) {
				filtered = append(filtered, item)
			}
		}
		if len(filtered) == len(input) {
			break
		}
		body["input"] = filtered
		_ = response.Body.Close()
	}
	defer response.Body.Close()
	streamGatewayResponse(w, response, buffered)
}

var reasoningIDPattern = regexp.MustCompile(`(?i)\bitem\s+['"]?(rs_[A-Za-z0-9_-]+)`)

func rejectedReasoningID(raw []byte) string {
	return visitReasoningError(strings.TrimSpace(string(raw)), 0)
}

func visitReasoningError(value any, depth int) string {
	if depth > 8 {
		return ""
	}
	switch value := value.(type) {
	case string:
		raw := strings.TrimSpace(strings.TrimPrefix(value, "data:"))
		decoded, err := decodeGatewayJSON([]byte(raw))
		if err == nil {
			return visitReasoningError(decoded, depth+1)
		}
	case map[string]any:
		if value["code"] == "invalid_encrypted_content" {
			message, _ := value["message"].(string)
			match := reasoningIDPattern.FindStringSubmatch(message)
			if len(match) == 2 {
				return match[1]
			}
			return ""
		}
		for _, child := range value {
			if id := visitReasoningError(child, depth+1); id != "" {
				return id
			}
		}
	}
	return ""
}

func isRejectedReasoning(value any, id string) bool {
	item, ok := value.(map[string]any)
	if !ok {
		return false
	}
	encrypted, _ := item["encrypted_content"].(string)
	return item["type"] == "reasoning" && item["id"] == id && encrypted != ""
}

func streamGatewayResponse(w http.ResponseWriter, upstream *http.Response, buffered []byte) {
	connection := connectionHeaders(upstream.Header)
	for key, values := range upstream.Header {
		name := strings.ToLower(key)
		if hopHeaders[name] || authHeaders[name] || connection[name] || name == "set-cookie" || name == "set-cookie2" || name == "server" || name == "date" {
			continue
		}
		w.Header()[key] = append([]string(nil), values...)
	}
	if upstream.StatusCode == 204 || upstream.StatusCode == 205 {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(upstream.StatusCode)
		return
	}
	w.Header().Set("X-Accel-Buffering", "no")
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Now().Add(upstreamTimeout))
	w.WriteHeader(upstream.StatusCode)
	if err := controller.Flush(); err != nil {
		panic(http.ErrAbortHandler)
	}
	write := func(chunk []byte) {
		_ = controller.SetWriteDeadline(time.Now().Add(upstreamTimeout))
		if _, err := w.Write(chunk); err != nil {
			panic(http.ErrAbortHandler)
		}
		if err := controller.Flush(); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
	if len(buffered) != 0 {
		write(buffered)
	}
	buf := make([]byte, 64<<10)
	for {
		n, err := upstream.Body.Read(buf)
		if n != 0 {
			write(buf[:n])
		}
		if errors.Is(err, io.EOF) {
			return
		}
		// Let net/http abort framing so a truncated upstream response never looks
		// like a successfully completed chunked response to Codex.
		if err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}
