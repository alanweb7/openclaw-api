package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
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
}

type chatRequest struct {
	UserID         string             `json:"user_id"`
	AgentID        string             `json:"agent_id"`
	SessionKey     string             `json:"session_key"`
	Message        string             `json:"message"`
	Locale         string             `json:"locale,omitempty"`
	Temperature    *float64           `json:"temperature,omitempty"`
	MaxTokens      *int               `json:"max_tokens,omitempty"`
	Model          string             `json:"model,omitempty"`
	BackendModel   string             `json:"backend_model,omitempty"`
	MessageChannel string             `json:"message_channel,omitempty"`
	Metadata       map[string]any     `json:"metadata,omitempty"`
	ExtraHeaders   map[string]string  `json:"extra_headers,omitempty"`
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
		return "", fmt.Errorf("upstream returned %d: %s", resp.StatusCode, truncate(raw, 400))
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
