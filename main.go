// anthropic-proxy: Anthropic API uyumlu reverse proxy.
// Client'lara saf Anthropic /v1/messages endpoint'i sunar,
// arkada OpenAI uyumlu bir backend'e (OpenAI, vLLM, Ollama, LM Studio,
// Groq, DeepSeek, Together, OpenRouter vs.) konuşur.
//
// Çalışma:
//   UPSTREAM_URL=https://api.openai.com/v1/chat/completions \
//   UPSTREAM_API_KEY=sk-... \
//   MODEL_MAP='{"claude-opus-4-7":"gpt-5","claude-sonnet-4-6":"gpt-5","claude-haiku-4-5":"gpt-5-mini"}' \
//   ./anthropic-proxy
//
// Claude Code ile kullanım:
//   ANTHROPIC_BASE_URL=http://localhost:8787 \
//   ANTHROPIC_API_KEY=anything \
//   claude
//
// Single binary, stdlib-only.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// =============================================================================
// CONFIG
// =============================================================================

type Config struct {
	ListenAddr   string
	UpstreamURL  string
	UpstreamKey  string
	DefaultModel string
	ModelMap     map[string]string
	Debug        bool
	// Opsiyonel: client'ın göndereceği API key. Boşsa auth kontrolü yapılmaz.
	ExpectedClientKey string
	RequestTimeout    time.Duration
}

func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}

		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func loadConfig() (*Config, error) {
	c := &Config{
		ListenAddr:        envOr("LISTEN_ADDR", ":8787"),
		UpstreamURL:       envOr("UPSTREAM_URL", "https://api.openai.com/v1/chat/completions"),
		UpstreamKey:       os.Getenv("UPSTREAM_API_KEY"),
		DefaultModel:      os.Getenv("DEFAULT_MODEL"),
		Debug:             os.Getenv("DEBUG") == "1" || strings.EqualFold(os.Getenv("DEBUG"), "true"),
		ExpectedClientKey: os.Getenv("PROXY_CLIENT_KEY"),
		RequestTimeout:    10 * time.Minute,
	}
	if c.UpstreamKey == "" {
		return nil, errors.New("UPSTREAM_API_KEY zorunlu")
	}
	if raw := os.Getenv("MODEL_MAP"); raw != "" {
		m := map[string]string{}
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, fmt.Errorf("MODEL_MAP parse: %w", err)
		}
		c.ModelMap = m
	} else {
		c.ModelMap = map[string]string{}
	}
	if c.DefaultModel == "" && len(c.ModelMap) == 0 {
		return nil, errors.New("en az DEFAULT_MODEL ya da MODEL_MAP tanımlanmalı")
	}
	if t := os.Getenv("REQUEST_TIMEOUT_SEC"); t != "" {
		var n int
		fmt.Sscanf(t, "%d", &n)
		if n > 0 {
			c.RequestTimeout = time.Duration(n) * time.Second
		}
	}
	return c, nil
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// =============================================================================
// ANTHROPIC TYPES (client <-> proxy)
// =============================================================================

type AnthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int                  `json:"max_tokens"`
	Messages      []AnthropicMessage   `json:"messages"`
	System        json.RawMessage      `json:"system,omitempty"` // string | []Block
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	TopK          *int                 `json:"top_k,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Metadata      json.RawMessage      `json:"metadata,omitempty"`
}

type AnthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type AnthropicBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`
	Source       *ImageSource    `json:"source,omitempty"`
	ID           string          `json:"id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ToolUseID    string          `json:"tool_use_id,omitempty"`
	Content      json.RawMessage `json:"content,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type AnthropicTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl json.RawMessage `json:"cache_control,omitempty"`
}

type AnthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

type AnthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []AnthropicBlock `json:"content"`
	StopReason   string           `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        AnthropicUsage   `json:"usage"`
}

type AnthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// =============================================================================
// OPENAI TYPES (proxy <-> upstream)
// =============================================================================

type OpenAIRequest struct {
	Model       string          `json:"model"`
	Messages    []OpenAIMessage `json:"messages"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	Stop        []string        `json:"stop,omitempty"`
	Tools       []OpenAITool    `json:"tools,omitempty"`
	ToolChoice  json.RawMessage `json:"tool_choice,omitempty"`
	Stream      bool            `json:"stream,omitempty"`

	// Stream açıkken usage bilgisini almak için (OpenAI + uyumlu çoğu sunucu)
	StreamOptions *OpenAIStreamOptions `json:"stream_options,omitempty"`
}

type OpenAIStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type OpenAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content,omitempty"`
	Name       string           `json:"name,omitempty"`
	ToolCalls  []OpenAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

type OpenAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function OpenAIFunctionCall `json:"function"`
}

type OpenAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // JSON string
}

type OpenAITool struct {
	Type     string         `json:"type"`
	Function OpenAIFunction `json:"function"`
}

type OpenAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

type OpenAIResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []OpenAIChoice `json:"choices"`
	Usage   OpenAIUsage    `json:"usage"`
}

type OpenAIChoice struct {
	Index        int           `json:"index"`
	Message      OpenAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

type OpenAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Streaming chunk
type OpenAIStreamChunk struct {
	ID      string               `json:"id"`
	Model   string               `json:"model"`
	Choices []OpenAIStreamChoice `json:"choices"`
	Usage   *OpenAIUsage         `json:"usage,omitempty"`
}

type OpenAIStreamChoice struct {
	Index        int                `json:"index"`
	Delta        OpenAIStreamDelta  `json:"delta"`
	FinishReason *string            `json:"finish_reason"`
}

type OpenAIStreamDelta struct {
	Role      string                 `json:"role,omitempty"`
	Content   string                 `json:"content,omitempty"`
	ToolCalls []OpenAIStreamToolCall `json:"tool_calls,omitempty"`
}

type OpenAIStreamToolCall struct {
	Index    int                     `json:"index"`
	ID       string                  `json:"id,omitempty"`
	Type     string                  `json:"type,omitempty"`
	Function *OpenAIStreamFunction   `json:"function,omitempty"`
}

type OpenAIStreamFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// =============================================================================
// REQUEST CONVERTER (Anthropic -> OpenAI)
// =============================================================================

func (p *Proxy) convertRequest(a *AnthropicRequest) (*OpenAIRequest, error) {
	o := &OpenAIRequest{
		Model:       p.mapModel(a.Model),
		MaxTokens:   a.MaxTokens,
		Temperature: a.Temperature,
		TopP:        a.TopP,
		Stop:        a.StopSequences,
		Stream:      a.Stream,
	}
	if a.Stream {
		o.StreamOptions = &OpenAIStreamOptions{IncludeUsage: true}
	}

	// System mesajı: Anthropic'te top-level, OpenAI'de ilk message
	if len(a.System) > 0 {
		sys, err := flattenSystem(a.System)
		if err != nil {
			return nil, fmt.Errorf("system parse: %w", err)
		}
		if sys != "" {
			c, _ := json.Marshal(sys)
			o.Messages = append(o.Messages, OpenAIMessage{Role: "system", Content: c})
		}
	}

	for i, m := range a.Messages {
		msgs, err := convertMessage(m)
		if err != nil {
			return nil, fmt.Errorf("message[%d]: %w", i, err)
		}
		o.Messages = append(o.Messages, msgs...)
	}

	for _, t := range a.Tools {
		params := t.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		o.Tools = append(o.Tools, OpenAITool{
			Type: "function",
			Function: OpenAIFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}

	if a.ToolChoice != nil {
		switch a.ToolChoice.Type {
		case "auto":
			o.ToolChoice = json.RawMessage(`"auto"`)
		case "any":
			o.ToolChoice = json.RawMessage(`"required"`)
		case "none":
			o.ToolChoice = json.RawMessage(`"none"`)
		case "tool":
			b, _ := json.Marshal(map[string]any{
				"type":     "function",
				"function": map[string]string{"name": a.ToolChoice.Name},
			})
			o.ToolChoice = b
		}
	}

	return o, nil
}

func flattenSystem(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var blocks []AnthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, b := range blocks {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String(), nil
}

// Anthropic bir message -> OpenAI 1+ message.
// tool_result'lar OpenAI'de ayrı role:"tool" mesajları olmak zorunda.
func convertMessage(m AnthropicMessage) ([]OpenAIMessage, error) {
	// Content string olabilir
	var asString string
	if err := json.Unmarshal(m.Content, &asString); err == nil {
		c, _ := json.Marshal(asString)
		return []OpenAIMessage{{Role: m.Role, Content: c}}, nil
	}

	var blocks []AnthropicBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}

	if m.Role == "assistant" {
		msg := OpenAIMessage{Role: "assistant"}
		var textBuf strings.Builder
		for _, b := range blocks {
			switch b.Type {
			case "text":
				textBuf.WriteString(b.Text)
			case "tool_use":
				args := string(b.Input)
				if args == "" || !json.Valid([]byte(args)) {
					args = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, OpenAIToolCall{
					ID:   b.ID,
					Type: "function",
					Function: OpenAIFunctionCall{
						Name:      b.Name,
						Arguments: args,
					},
				})
			}
		}
		if textBuf.Len() > 0 {
			c, _ := json.Marshal(textBuf.String())
			msg.Content = c
		}
		// OpenAI: assistant message'da content veya tool_calls olmalı
		if msg.Content == nil && len(msg.ToolCalls) == 0 {
			msg.Content = json.RawMessage(`""`)
		}
		return []OpenAIMessage{msg}, nil
	}

	// user turn
	var out []OpenAIMessage
	var userParts []map[string]any
	var userPlainText strings.Builder
	hasImage := false
	for _, b := range blocks {
		if b.Type == "image" {
			hasImage = true
			break
		}
	}

	flushUser := func() {
		if len(userParts) > 0 {
			c, _ := json.Marshal(userParts)
			out = append(out, OpenAIMessage{Role: "user", Content: c})
			userParts = nil
		} else if userPlainText.Len() > 0 {
			c, _ := json.Marshal(userPlainText.String())
			out = append(out, OpenAIMessage{Role: "user", Content: c})
			userPlainText.Reset()
		}
	}

	for _, b := range blocks {
		switch b.Type {
		case "text":
			if hasImage {
				userParts = append(userParts, map[string]any{"type": "text", "text": b.Text})
			} else {
				userPlainText.WriteString(b.Text)
			}
		case "image":
			if b.Source != nil {
				dataURL := fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
				userParts = append(userParts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]string{"url": dataURL},
				})
			}
		case "tool_result":
			flushUser()
			var txt string
			if err := json.Unmarshal(b.Content, &txt); err != nil {
				var rblocks []AnthropicBlock
				if err := json.Unmarshal(b.Content, &rblocks); err == nil {
					var sb strings.Builder
					for _, rb := range rblocks {
						if rb.Type == "text" {
							sb.WriteString(rb.Text)
						}
					}
					txt = sb.String()
				}
			}
			if txt == "" {
				txt = "(empty)"
			}
			c, _ := json.Marshal(txt)
			out = append(out, OpenAIMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    c,
			})
		}
	}
	flushUser()
	return out, nil
}

// =============================================================================
// RESPONSE CONVERTER (OpenAI -> Anthropic)
// =============================================================================

func convertResponse(o *OpenAIResponse, originalModel string) *AnthropicResponse {
	ares := &AnthropicResponse{
		ID:    "msg_" + sanitizeID(o.ID),
		Type:  "message",
		Role:  "assistant",
		Model: originalModel,
		Usage: AnthropicUsage{
			InputTokens:  o.Usage.PromptTokens,
			OutputTokens: o.Usage.CompletionTokens,
		},
	}
	if len(o.Choices) == 0 {
		ares.StopReason = "end_turn"
		ares.Content = []AnthropicBlock{}
		return ares
	}
	ch := o.Choices[0]

	var textContent string
	if len(ch.Message.Content) > 0 {
		_ = json.Unmarshal(ch.Message.Content, &textContent)
	}
	if textContent != "" {
		ares.Content = append(ares.Content, AnthropicBlock{Type: "text", Text: textContent})
	}

	for _, tc := range ch.Message.ToolCalls {
		var input json.RawMessage
		if tc.Function.Arguments == "" || !json.Valid([]byte(tc.Function.Arguments)) {
			input = json.RawMessage(`{}`)
		} else {
			input = json.RawMessage(tc.Function.Arguments)
		}
		ares.Content = append(ares.Content, AnthropicBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	if len(ares.Content) == 0 {
		ares.Content = []AnthropicBlock{{Type: "text", Text: ""}}
	}

	ares.StopReason = mapFinishReason(ch.FinishReason, len(ch.Message.ToolCalls) > 0)
	return ares
}

func mapFinishReason(r string, hasToolCalls bool) string {
	switch r {
	case "stop":
		if hasToolCalls {
			return "tool_use"
		}
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "end_turn"
	default:
		if hasToolCalls {
			return "tool_use"
		}
		return "end_turn"
	}
}

func sanitizeID(id string) string {
	if id == "" {
		return randomID()
	}
	return strings.TrimPrefix(id, "chatcmpl-")
}

func randomID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// =============================================================================
// PROXY + HANDLERS
// =============================================================================

type Proxy struct {
	cfg     *Config
	client  *http.Client
	reqNum  atomic.Uint64
}

func NewProxy(cfg *Config) *Proxy {
	return &Proxy{
		cfg: cfg,
		client: &http.Client{
			Timeout: cfg.RequestTimeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

func (p *Proxy) mapModel(anthropicModel string) string {
	if v, ok := p.cfg.ModelMap[anthropicModel]; ok {
		return v
	}
	// Prefix match (ör. claude-opus-4-7-20250xxx → claude-opus-4-7)
	for k, v := range p.cfg.ModelMap {
		if strings.HasPrefix(anthropicModel, k) {
			return v
		}
	}
	if p.cfg.DefaultModel != "" {
		return p.cfg.DefaultModel
	}
	return anthropicModel
}

func (p *Proxy) checkAuth(r *http.Request) bool {
	if p.cfg.ExpectedClientKey == "" {
		return true
	}
	if k := r.Header.Get("x-api-key"); k == p.cfg.ExpectedClientKey {
		return true
	}
	if auth := r.Header.Get("Authorization"); auth != "" {
		if strings.TrimPrefix(auth, "Bearer ") == p.cfg.ExpectedClientKey {
			return true
		}
	}
	return false
}

func (p *Proxy) handleMessages(w http.ResponseWriter, r *http.Request) {
	reqID := p.reqNum.Add(1)
	if !p.checkAuth(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid x-api-key")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	defer r.Body.Close()

	var areq AnthropicRequest
	if err := json.Unmarshal(body, &areq); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "json: "+err.Error())
		return
	}
	if areq.Model == "" || areq.MaxTokens == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "model ve max_tokens zorunlu")
		return
	}

	p.debugf("[#%d] %s model=%s stream=%v tools=%d msgs=%d",
		reqID, r.URL.Path, areq.Model, areq.Stream, len(areq.Tools), len(areq.Messages))

	oreq, err := p.convertRequest(&areq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if areq.Stream {
		p.handleStream(w, r.Context(), &areq, oreq, reqID)
		return
	}
	p.handleSync(w, r.Context(), &areq, oreq, reqID)
}

func (p *Proxy) handleSync(w http.ResponseWriter, ctx context.Context, areq *AnthropicRequest, oreq *OpenAIRequest, reqID uint64) {
	reqBytes, _ := json.Marshal(oreq)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.cfg.UpstreamURL, bytes.NewReader(reqBytes))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.UpstreamKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		p.debugf("[#%d] upstream %d: %s", reqID, resp.StatusCode, truncate(string(respBody), 500))
		writeAnthropicError(w, resp.StatusCode, mapErrorType(resp.StatusCode), extractUpstreamError(respBody))
		return
	}

	var ores OpenAIResponse
	if err := json.Unmarshal(respBody, &ores); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "decode: "+err.Error())
		return
	}
	ares := convertResponse(&ores, areq.Model)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(ares)
	p.debugf("[#%d] sync ok stop=%s in=%d out=%d", reqID, ares.StopReason, ares.Usage.InputTokens, ares.Usage.OutputTokens)
}

// =============================================================================
// STREAMING
// =============================================================================

// Anthropic SSE state machine. OpenAI chunk'larını Anthropic event'larına çevirir.
type streamState struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	messageID string
	model     string

	messageStarted bool

	// Aktif block (0 veya 1 tane açık olabilir)
	currentBlockIndex int
	currentBlockType  string // "" | "text" | "tool_use"
	nextBlockIndex    int

	// OpenAI tool_call index -> Anthropic block index
	toolBlockByOAIIdx map[int]int

	inputTokens  int
	outputTokens int
	finishReason string

	stopped bool
}

func newStreamState(w http.ResponseWriter, messageID, model string) (*streamState, error) {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil, errors.New("streaming unsupported")
	}
	return &streamState{
		w:                 w,
		flusher:           f,
		messageID:         messageID,
		model:             model,
		toolBlockByOAIIdx: map[int]int{},
		currentBlockType:  "",
	}, nil
}

func (s *streamState) emit(event string, data any) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, b); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}

func (s *streamState) emitMessageStart() error {
	if s.messageStarted {
		return nil
	}
	s.messageStarted = true
	return s.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            s.messageID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         s.model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  s.inputTokens,
				"output_tokens": 0,
			},
		},
	})
}

func (s *streamState) closeCurrentBlock() error {
	if s.currentBlockType == "" {
		return nil
	}
	idx := s.currentBlockIndex
	s.currentBlockType = ""
	return s.emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

func (s *streamState) openTextBlock() error {
	if s.currentBlockType == "text" {
		return nil
	}
	if err := s.closeCurrentBlock(); err != nil {
		return err
	}
	s.currentBlockIndex = s.nextBlockIndex
	s.nextBlockIndex++
	s.currentBlockType = "text"
	return s.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.currentBlockIndex,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	})
}

func (s *streamState) openToolBlock(oaiIdx int, id, name string) error {
	if err := s.closeCurrentBlock(); err != nil {
		return err
	}
	s.currentBlockIndex = s.nextBlockIndex
	s.nextBlockIndex++
	s.currentBlockType = "tool_use"
	s.toolBlockByOAIIdx[oaiIdx] = s.currentBlockIndex
	if id == "" {
		id = "toolu_" + randomID()
	}
	return s.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": s.currentBlockIndex,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    id,
			"name":  name,
			"input": map[string]any{},
		},
	})
}

func (s *streamState) emitTextDelta(text string) error {
	return s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": s.currentBlockIndex,
		"delta": map[string]any{
			"type": "text_delta",
			"text": text,
		},
	})
}

func (s *streamState) emitToolArgsDelta(idx int, partial string) error {
	return s.emit("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": idx,
		"delta": map[string]any{
			"type":         "input_json_delta",
			"partial_json": partial,
		},
	})
}

func (s *streamState) finish() error {
	if s.stopped {
		return nil
	}
	s.stopped = true
	if err := s.closeCurrentBlock(); err != nil {
		return err
	}
	stopReason := mapFinishReason(s.finishReason, len(s.toolBlockByOAIIdx) > 0)
	if err := s.emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": s.outputTokens,
		},
	}); err != nil {
		return err
	}
	return s.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (p *Proxy) handleStream(w http.ResponseWriter, ctx context.Context, areq *AnthropicRequest, oreq *OpenAIRequest, reqID uint64) {
	reqBytes, _ := json.Marshal(oreq)
	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.cfg.UpstreamURL, bytes.NewReader(reqBytes))
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", err.Error())
		return
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.cfg.UpstreamKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.client.Do(httpReq)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream: "+err.Error())
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		p.debugf("[#%d] upstream stream %d: %s", reqID, resp.StatusCode, truncate(string(body), 500))
		writeAnthropicError(w, resp.StatusCode, mapErrorType(resp.StatusCode), extractUpstreamError(body))
		return
	}

	// SSE header'ları client'a göre ayarla
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	messageID := "msg_" + randomID()
	state, err := newStreamState(w, messageID, areq.Model)
	if err != nil {
		p.debugf("[#%d] stream init: %v", reqID, err)
		return
	}

	scanner := bufio.NewScanner(resp.Body)
	// SSE chunk'ları uzun olabilir
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := scanner.Text()
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}

		var chunk OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			p.debugf("[#%d] chunk parse: %v / %s", reqID, err, truncate(data, 200))
			continue
		}

		// message_start'ı ilk chunk'ta yolla
		if !state.messageStarted {
			if err := state.emitMessageStart(); err != nil {
				p.debugf("[#%d] emit start: %v", reqID, err)
				return
			}
		}

		if chunk.Usage != nil {
			if chunk.Usage.PromptTokens > 0 {
				state.inputTokens = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				state.outputTokens = chunk.Usage.CompletionTokens
			}
		}

		for _, ch := range chunk.Choices {
			// Text delta
			if ch.Delta.Content != "" {
				if err := state.openTextBlock(); err != nil {
					return
				}
				if err := state.emitTextDelta(ch.Delta.Content); err != nil {
					return
				}
			}

			// Tool call deltas
			for _, tc := range ch.Delta.ToolCalls {
				blockIdx, seen := state.toolBlockByOAIIdx[tc.Index]
				if !seen {
					name := ""
					id := tc.ID
					if tc.Function != nil {
						name = tc.Function.Name
					}
					if err := state.openToolBlock(tc.Index, id, name); err != nil {
						return
					}
					blockIdx = state.toolBlockByOAIIdx[tc.Index]
				}
				if tc.Function != nil && tc.Function.Arguments != "" {
					if err := state.emitToolArgsDelta(blockIdx, tc.Function.Arguments); err != nil {
						return
					}
				}
			}

			if ch.FinishReason != nil && *ch.FinishReason != "" {
				state.finishReason = *ch.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		p.debugf("[#%d] scanner: %v", reqID, err)
	}

	if !state.messageStarted {
		// Hiç chunk gelmediyse de protokolü kapat
		_ = state.emitMessageStart()
	}
	_ = state.finish()
	p.debugf("[#%d] stream ok stop=%s in=%d out=%d", reqID, state.finishReason, state.inputTokens, state.outputTokens)
}

// =============================================================================
// count_tokens (yaklaşık)
// =============================================================================

func (p *Proxy) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if !p.checkAuth(r) {
		writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid x-api-key")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	defer r.Body.Close()
	// Basit yaklaşım: ~4 char/token
	n := len(body) / 4
	if n < 1 {
		n = 1
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"input_tokens": n})
}

// =============================================================================
// ERROR HELPERS
// =============================================================================

func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": msg,
		},
	})
}

func mapErrorType(status int) string {
	switch {
	case status == 401 || status == 403:
		return "authentication_error"
	case status == 429:
		return "rate_limit_error"
	case status == 404:
		return "not_found_error"
	case status >= 400 && status < 500:
		return "invalid_request_error"
	default:
		return "api_error"
	}
}

func extractUpstreamError(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil && e.Error.Message != "" {
		return "upstream: " + e.Error.Message
	}
	return "upstream: " + truncate(string(body), 300)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// =============================================================================
// MAIN
// =============================================================================

func (p *Proxy) debugf(format string, args ...any) {
	if p.cfg.Debug {
		log.Printf(format, args...)
	}
}

func (p *Proxy) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", p.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", p.handleCountTokens)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"service":  "anthropic-proxy",
			"upstream": p.cfg.UpstreamURL,
			"models":   p.cfg.ModelMap,
		})
	})
	return mux
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if err := loadDotEnv(".env"); err != nil {
		log.Fatalf("dotenv: %v", err)
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	p := NewProxy(cfg)

	log.Printf("anthropic-proxy")
	log.Printf("  listen   : %s", cfg.ListenAddr)
	log.Printf("  upstream : %s", cfg.UpstreamURL)
	log.Printf("  default  : %s", cfg.DefaultModel)
	log.Printf("  models   : %d mapped", len(cfg.ModelMap))
	for k, v := range cfg.ModelMap {
		log.Printf("    %s  ->  %s", k, v)
	}
	if cfg.Debug {
		log.Printf("  debug    : on")
	}

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           p.routes(),
		ReadHeaderTimeout: 30 * time.Second,
		// Stream yanıtlar uzun sürebilir, WriteTimeout koymuyoruz
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
