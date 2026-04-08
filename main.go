package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	ListenAddr  string
	UpstreamURL string
	Token       string
	Scopes      string
}

type chatRequest struct {
	UserID         string            `json:"user_id"`
	AgentID        string            `json:"agent_id"`
	SessionKey     string            `json:"session_key"`
	Message        string            `json:"message"`
	Locale         string            `json:"locale,omitempty"`
	Temperature    *float64          `json:"temperature,omitempty"`
	MaxTokens      *int              `json:"max_tokens,omitempty"`
	Model          string            `json:"model,omitempty"`
	BackendModel   string            `json:"backend_model,omitempty"`
	MessageChannel string            `json:"message_channel,omitempty"`
	Metadata       map[string]any    `json:"metadata,omitempty"`
	ExtraHeaders   map[string]string `json:"extra_headers,omitempty"`
}

type chatResponse struct {
	SessionKey string `json:"session_key"`
	Content    string `json:"content"`
}

type openAIRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Stream      bool            `json:"stream"`
	Temperature *float64        `json:"temperature,omitempty"`
	MaxTokens   *int            `json:"max_tokens,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type wsRequest struct {
	Type   string `json:"type"`
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type wsFrame struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	OK      bool            `json:"ok,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Error   *struct {
		Code    string `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

type upstreamStatusError struct {
	Status int
	Body   string
}

func (e upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream returned %d: %s", e.Status, e.Body)
}

type createJobRequest struct {
	UserID          string            `json:"user_id"`
	AgentID         string            `json:"agent_id"`
	SessionKey      string            `json:"session_key"`
	Message         string            `json:"message"`
	Locale          string            `json:"locale,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	MaxTokens       *int              `json:"max_tokens,omitempty"`
	Model           string            `json:"model,omitempty"`
	BackendModel    string            `json:"backend_model,omitempty"`
	MessageChannel  string            `json:"message_channel,omitempty"`
	Metadata        map[string]any    `json:"metadata,omitempty"`
	CallbackURL     string            `json:"callback_url,omitempty"`
	CallbackHeaders map[string]string `json:"callback_headers,omitempty"`
}

type jobResponse struct {
	ID              string            `json:"id"`
	Status          string            `json:"status"`
	UserID          string            `json:"user_id"`
	AgentID         string            `json:"agent_id"`
	SessionKey      string            `json:"session_key"`
	Message         string            `json:"message"`
	Content         string            `json:"content,omitempty"`
	Error           string            `json:"error,omitempty"`
	CallbackURL     string            `json:"callback_url,omitempty"`
	CallbackHeaders map[string]string `json:"callback_headers,omitempty"`
	CreatedAt       time.Time         `json:"created_at"`
	UpdatedAt       time.Time         `json:"updated_at"`
}

type jobStore struct {
	mu   sync.RWMutex
	jobs map[string]*jobResponse
}

var jobCounter uint64

const (
	jobStatusQueued    = "queued"
	jobStatusRunning   = "running"
	jobStatusSucceeded = "succeeded"
	jobStatusFailed    = "failed"
)

func main() {
	token, err := loadToken()
	if err != nil {
		log.Fatal(err)
	}

	cfg := config{
		ListenAddr:  getenv("PORT", "8080"),
		UpstreamURL: strings.TrimRight(getenv("OPENCLAW_UPSTREAM_URL", "http://127.0.0.1:18789"), "/"),
		Token:       token,
		Scopes:      getenv("OPENCLAW_SCOPES", "operator.write,operator.read"),
	}

	jobs := &jobStore{jobs: make(map[string]*jobResponse)}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleChat(w, r, cfg)
	})
	mux.HandleFunc("/v1/jobs", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleCreateJob(w, r, cfg, jobs)
	})
	mux.HandleFunc("/v1/jobs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		handleGetJob(w, r, jobs)
	})

	srv := &http.Server{
		Addr:              ":" + strings.TrimPrefix(cfg.ListenAddr, ":"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("openclaw-api listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func handleChat(w http.ResponseWriter, r *http.Request, cfg config) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()

	req, err := decodeAndValidateChatRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	content, err := callOpenClaw(ctx, cfg, req)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, chatResponse{
		SessionKey: req.SessionKey,
		Content:    content,
	})
}

func decodeAndValidateChatRequest(r *http.Request) (chatRequest, error) {
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return chatRequest{}, errors.New("invalid JSON body")
	}
	if strings.TrimSpace(req.UserID) == "" {
		return chatRequest{}, errors.New("user_id is required")
	}
	if strings.TrimSpace(req.AgentID) == "" {
		return chatRequest{}, errors.New("agent_id is required")
	}
	if strings.TrimSpace(req.SessionKey) == "" {
		return chatRequest{}, errors.New("session_key is required")
	}
	if strings.TrimSpace(req.Message) == "" {
		return chatRequest{}, errors.New("message is required")
	}
	if req.Locale == "" {
		req.Locale = "pt-BR"
	}
	return req, nil
}

func callOpenClaw(ctx context.Context, cfg config, req chatRequest) (string, error) {
	content, err := callOpenClawHTTP(ctx, cfg, req)
	if err == nil {
		return content, nil
	}

	var statusErr upstreamStatusError
	if errors.As(err, &statusErr) && statusErr.Status == http.StatusNotFound {
		return callOpenClawWS(ctx, cfg, req)
	}
	return "", err
}

func callOpenClawHTTP(ctx context.Context, cfg config, req chatRequest) (string, error) {
	model := req.Model
	if strings.TrimSpace(model) == "" {
		model = "openclaw/" + req.AgentID
	}

	body := openAIRequest{
		Model:       model,
		Messages:    []openAIMessage{{Role: "user", Content: req.Message}},
		Stream:      false,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("failed to marshal upstream payload: %w", err)
	}

	u := cfg.UpstreamURL + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("failed creating upstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.Token)
	httpReq.Header.Set("x-openclaw-agent-id", req.AgentID)
	httpReq.Header.Set("x-openclaw-session-key", req.SessionKey)
	httpReq.Header.Set("x-openclaw-user-id", req.UserID)
	httpReq.Header.Set("Accept-Language", req.Locale)
	if strings.TrimSpace(cfg.Scopes) != "" {
		httpReq.Header.Set("x-openclaw-scopes", cfg.Scopes)
	}
	if strings.TrimSpace(req.BackendModel) != "" {
		httpReq.Header.Set("x-openclaw-model", req.BackendModel)
	}
	if strings.TrimSpace(req.MessageChannel) != "" {
		httpReq.Header.Set("x-openclaw-message-channel", req.MessageChannel)
	}
	for k, v := range req.ExtraHeaders {
		if strings.TrimSpace(k) != "" {
			httpReq.Header.Set(k, v)
		}
	}

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("upstream request failed: %w", err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", upstreamStatusError{Status: resp.StatusCode, Body: truncate(raw, 400)}
	}

	var out openAIResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("invalid upstream JSON: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", errors.New(out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("upstream returned no choices")
	}
	content := strings.TrimSpace(out.Choices[0].Message.Content)
	if content == "" {
		return "", errors.New("upstream returned empty content")
	}
	return content, nil
}

func callOpenClawWS(ctx context.Context, cfg config, req chatRequest) (string, error) {
	wsURL, origin, err := wsEndpointFromUpstream(cfg.UpstreamURL)
	if err != nil {
		return "", fmt.Errorf("invalid OPENCLAW_UPSTREAM_URL for websocket: %w", err)
	}

	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 15 * time.Second,
	}
	conn, _, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return "", fmt.Errorf("websocket dial failed: %w", err)
	}
	defer conn.Close()

	scopes := parseScopes(cfg.Scopes)
	connectParams := map[string]any{
		"minProtocol": 3,
		"maxProtocol": 3,
		"client": map[string]any{
			// Gateway validates client.id/mode against an allowlist.
			// "openclaw-control-ui"+"webchat" is accepted in current OpenClaw builds.
			"id":         "openclaw-control-ui",
			"version":    "control-ui",
			"platform":   "linux",
			"mode":       "webchat",
			"instanceId": "openclaw-api",
		},
		"role":      "operator",
		"scopes":    scopes,
		"caps":      []string{"tool-events"},
		"auth":      map[string]any{"token": cfg.Token},
		"userAgent": "openclaw-api/1.0",
		"locale":    req.Locale,
	}

	if _, err := wsRequestCall(ctx, conn, "connect", connectParams); err != nil {
		return "", fmt.Errorf("websocket connect failed: %w", err)
	}

	baselineCount, baselineText, err := wsAssistantSnapshot(ctx, conn, req.SessionKey)
	if err != nil {
		return "", fmt.Errorf("websocket chat.history baseline failed: %w", err)
	}

	sendParams := map[string]any{
		"sessionKey":     req.SessionKey,
		"idempotencyKey": newWSID(),
		"message":        req.Message,
	}
	if strings.TrimSpace(req.MessageChannel) != "" {
		sendParams["messageChannel"] = req.MessageChannel
	}
	if strings.TrimSpace(req.BackendModel) != "" {
		sendParams["model"] = req.BackendModel
	}
	if _, err := wsRequestCall(ctx, conn, "chat.send", sendParams); err != nil {
		return "", fmt.Errorf("websocket chat.send failed: %w", err)
	}

	ticker := time.NewTicker(1200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("timeout waiting chat response: %w", ctx.Err())
		case <-ticker.C:
			count, lastText, err := wsAssistantSnapshot(ctx, conn, req.SessionKey)
			if err != nil {
				continue
			}
			if strings.TrimSpace(lastText) == "" {
				continue
			}
			if count > baselineCount || lastText != baselineText {
				return strings.TrimSpace(lastText), nil
			}
		}
	}
}

func wsAssistantSnapshot(ctx context.Context, conn *websocket.Conn, sessionKey string) (int, string, error) {
	raw, err := wsRequestCall(ctx, conn, "chat.history", map[string]any{
		"sessionKey": sessionKey,
		"limit":      60,
	})
	if err != nil {
		return 0, "", err
	}

	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return 0, "", err
	}

	assistantCount := 0
	lastText := ""
	for _, m := range payload.Messages {
		if m.Role != "assistant" {
			continue
		}
		assistantCount++
		var parts []string
		for _, c := range m.Content {
			if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
				parts = append(parts, c.Text)
			}
		}
		if len(parts) > 0 {
			lastText = strings.Join(parts, "\n")
		}
	}
	return assistantCount, strings.TrimSpace(lastText), nil
}

func wsRequestCall(ctx context.Context, conn *websocket.Conn, method string, params any) (json.RawMessage, error) {
	reqID := newWSID()
	req := wsRequest{
		Type:   "req",
		ID:     reqID,
		Method: method,
		Params: params,
	}

	if err := wsWriteJSON(ctx, conn, req); err != nil {
		return nil, err
	}

	for {
		frame, err := wsReadFrame(ctx, conn)
		if err != nil {
			return nil, err
		}
		if frame.Type != "res" || frame.ID != reqID {
			continue
		}
		if frame.OK {
			return frame.Payload, nil
		}

		msg := "request failed"
		if frame.Error != nil && strings.TrimSpace(frame.Error.Message) != "" {
			msg = frame.Error.Message
		}
		return nil, errors.New(msg)
	}
}

func wsWriteJSON(ctx context.Context, conn *websocket.Conn, v any) error {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetWriteDeadline(dl)
	} else {
		_ = conn.SetWriteDeadline(time.Now().Add(20 * time.Second))
	}
	if err := conn.WriteJSON(v); err != nil {
		return fmt.Errorf("write websocket frame: %w", err)
	}
	return nil
}

func wsReadFrame(ctx context.Context, conn *websocket.Conn) (wsFrame, error) {
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(dl)
	} else {
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return wsFrame{}, fmt.Errorf("read websocket frame: %w", err)
	}
	var frame wsFrame
	if err := json.Unmarshal(raw, &frame); err != nil {
		return wsFrame{}, fmt.Errorf("invalid websocket JSON frame: %w", err)
	}
	return frame, nil
}

func wsEndpointFromUpstream(upstream string) (string, string, error) {
	u, err := url.Parse(strings.TrimSpace(upstream))
	if err != nil {
		return "", "", err
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	if strings.TrimSpace(u.Path) == "" || u.Path == "/" {
		u.Path = "/v1/realtime"
	}

	originURL := *u
	switch originURL.Scheme {
	case "ws":
		originURL.Scheme = "http"
	case "wss":
		originURL.Scheme = "https"
	}
	originURL.Path = ""
	originURL.RawPath = ""
	originURL.RawQuery = ""
	originURL.Fragment = ""

	return u.String(), originURL.String(), nil
}

func parseScopes(raw string) []string {
	out := make([]string, 0, 4)
	seen := map[string]struct{}{}
	for _, s := range strings.Split(raw, ",") {
		scope := strings.TrimSpace(s)
		if scope == "" {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	if len(out) == 0 {
		return []string{"operator.write", "operator.read"}
	}
	return out
}

func newWSID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b[:])
}

func handleCreateJob(w http.ResponseWriter, r *http.Request, cfg config, jobs *jobStore) {
	var in createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := validateCallbackURL(in.CallbackURL); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	req := chatRequest{
		UserID:         in.UserID,
		AgentID:        in.AgentID,
		SessionKey:     in.SessionKey,
		Message:        in.Message,
		Locale:         in.Locale,
		Temperature:    in.Temperature,
		MaxTokens:      in.MaxTokens,
		Model:          in.Model,
		BackendModel:   in.BackendModel,
		MessageChannel: in.MessageChannel,
		Metadata:       in.Metadata,
	}
	if req.Locale == "" {
		req.Locale = "pt-BR"
	}
	if strings.TrimSpace(req.UserID) == "" || strings.TrimSpace(req.AgentID) == "" || strings.TrimSpace(req.SessionKey) == "" || strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "user_id, agent_id, session_key and message are required")
		return
	}

	jobID := newJobID()
	now := time.Now().UTC()
	job := &jobResponse{
		ID:              jobID,
		Status:          jobStatusQueued,
		UserID:          req.UserID,
		AgentID:         req.AgentID,
		SessionKey:      req.SessionKey,
		Message:         req.Message,
		CallbackURL:     in.CallbackURL,
		CallbackHeaders: in.CallbackHeaders,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	jobs.put(job)
	go processJob(cfg, jobs, jobID, req)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"id":     jobID,
		"status": jobStatusQueued,
	})
}

func handleGetJob(w http.ResponseWriter, r *http.Request, jobs *jobStore) {
	id := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, "/v1/jobs/"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "job id is required")
		return
	}
	job, ok := jobs.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func processJob(cfg config, jobs *jobStore, jobID string, req chatRequest) {
	jobs.update(jobID, func(j *jobResponse) {
		j.Status = jobStatusRunning
		j.UpdatedAt = time.Now().UTC()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	content, err := callOpenClaw(ctx, cfg, req)
	if err != nil {
		jobs.update(jobID, func(j *jobResponse) {
			j.Status = jobStatusFailed
			j.Error = err.Error()
			j.UpdatedAt = time.Now().UTC()
		})
		publishCallback(jobs, jobID)
		return
	}

	jobs.update(jobID, func(j *jobResponse) {
		j.Status = jobStatusSucceeded
		j.Content = content
		j.UpdatedAt = time.Now().UTC()
	})
	publishCallback(jobs, jobID)
}

func publishCallback(jobs *jobStore, jobID string) {
	job, ok := jobs.get(jobID)
	if !ok || strings.TrimSpace(job.CallbackURL) == "" {
		return
	}

	body, err := json.Marshal(job)
	if err != nil {
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for i := 0; i < 3; i++ {
		req, err := http.NewRequest(http.MethodPost, job.CallbackURL, bytes.NewReader(body))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range job.CallbackHeaders {
			if strings.TrimSpace(k) != "" {
				req.Header.Set(k, v)
			}
		}

		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
		}
		time.Sleep(time.Duration(i+1) * time.Second)
	}
}

func validateCallbackURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return errors.New("callback_url is invalid")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("callback_url must start with http:// or https://")
	}
	if u.Host == "" {
		return errors.New("callback_url host is required")
	}
	return nil
}

func newJobID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	n := atomic.AddUint64(&jobCounter, 1)
	return "job_" + strconv.FormatInt(time.Now().Unix(), 36) + "_" + fmt.Sprintf("%x", b[:]) + "_" + strconv.FormatUint(n, 36)
}

func (s *jobStore) put(job *jobResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
}

func (s *jobStore) get(id string) (jobResponse, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	if !ok {
		return jobResponse{}, false
	}
	out := *job
	return out, true
}

func (s *jobStore) update(id string, fn func(*jobResponse)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[id]
	if !ok {
		return
	}
	fn(job)
}

func loadToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv("OPENCLAW_TOKEN")); token != "" {
		return token, nil
	}
	if p := strings.TrimSpace(os.Getenv("OPENCLAW_TOKEN_FILE")); p != "" {
		data, err := os.ReadFile(filepath.Clean(p))
		if err != nil {
			return "", fmt.Errorf("failed reading OPENCLAW_TOKEN_FILE: %w", err)
		}
		token := strings.TrimSpace(string(data))
		if token == "" {
			return "", errors.New("OPENCLAW_TOKEN_FILE is empty")
		}
		return token, nil
	}
	return "", errors.New("missing OPENCLAW_TOKEN or OPENCLAW_TOKEN_FILE")
}

func getenv(k, fallback string) string {
	v := os.Getenv(k)
	if v == "" {
		return fallback
	}
	return v
}

func truncate(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
