package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultA2AURL = "http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/kagent-vision"
	defaultMCPURL = "http://kagent-vision-mcp.kagent.svc.cluster.local:8080/activity"
	defaultUserID = "admin@kagent.dev"
	hitlExtension  = "https://kagent.dev/extensions/hitl/v1"
)

//go:embed static/index.html
var indexHTML []byte

type app struct {
	a2aURL     string
	sreA2AURL  string
	mcpURL     string
	userID     string
	httpClient *http.Client
}

type chatRequest struct {
	Message      string         `json:"message"`
	ContextID    string         `json:"contextId"`
	TaskID       string         `json:"taskId,omitempty"`
	HITLResponse map[string]any `json:"-"`
}

type approvalRequest struct {
	ContextID string `json:"contextId"`
	TaskID    string `json:"taskId"`
	Approvals []struct {
		ID              string `json:"id"`
		Approved        bool   `json:"approved"`
		RejectionReason string `json:"rejectionReason,omitempty"`
	} `json:"approvals"`
}

func main() {
	a := &app{
		a2aURL:     valueOrDefault("KAGENT_A2A_URL", defaultA2AURL),
		sreA2AURL:  valueOrDefault("KAGENT_SRE_A2A_URL", "http://kagent-controller.kagent.svc.cluster.local:8083/agents/kagent/kagent-sre"),
		mcpURL:     valueOrDefault("MCP_ACTIVITY_URL", defaultMCPURL),
		userID:     valueOrDefault("KAGENT_USER_ID", defaultUserID),
		httpClient: &http.Client{Timeout: 6 * time.Minute},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/chat", a.handleChat)
	mux.HandleFunc("/api/sre", a.handleSRE)
	mux.HandleFunc("/api/approval", a.handleApproval)
	mux.HandleFunc("/api/activity", a.handleActivity)

	log.Println("activity UI listening on :8090")
	if err := http.ListenAndServe(":8090", mux); err != nil {
		log.Fatal(err)
	}
}

func valueOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func (a *app) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

func (a *app) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var input chatRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&input); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	input.Message = strings.TrimSpace(input.Message)
	if input.Message == "" {
		http.Error(w, "message is required", http.StatusBadRequest)
		return
	}

	a.proxyA2A(w, r, a.a2aURL, input)
}

func (a *app) handleSRE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var failure struct {
		Tool      string `json:"tool"`
		Detail    string `json:"detail"`
		ContextID string `json:"contextId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&failure); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	input := chatRequest{
		ContextID: failure.ContextID,
		Message: fmt.Sprintf(
			"Investigate this local MCP failure: tool=%q detail=%q. First call sre_get_recent_failures. Then use github_get_file only if repository context is needed. Explain the likely cause and a minimal safe fix. Do not claim a PR was created; any GitHub write needs explicit human approval.",
			failure.Tool,
			failure.Detail,
		),
	}
	a.proxyA2A(w, r, a.sreA2AURL, input)
}

func (a *app) handleApproval(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var approval approvalRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&approval); err != nil {
		http.Error(w, "invalid approval body", http.StatusBadRequest)
		return
	}
	approval.ContextID = strings.TrimSpace(approval.ContextID)
	approval.TaskID = strings.TrimSpace(approval.TaskID)
	if approval.ContextID == "" || approval.TaskID == "" || len(approval.Approvals) == 0 {
		http.Error(w, "contextId, taskId, and approvals are required", http.StatusBadRequest)
		return
	}
	for _, decision := range approval.Approvals {
		if strings.TrimSpace(decision.ID) == "" {
			http.Error(w, "approval id is required", http.StatusBadRequest)
			return
		}
	}

	input := chatRequest{
		Message:   "The requested tool approval decision was submitted.",
		ContextID: approval.ContextID,
		TaskID:    approval.TaskID,
		HITLResponse: map[string]any{
			"type":      "tool_approval_response",
			"approvals": approval.Approvals,
		},
	}
	a.proxyA2A(w, r, a.sreA2AURL, input)
}

func (a *app) proxyA2A(w http.ResponseWriter, r *http.Request, endpoint string, input chatRequest) {
	payload, err := sendMessagePayload(input)
	if err != nil {
		http.Error(w, "create A2A request", http.StatusInternalServerError)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		http.Error(w, "create upstream request", http.StatusInternalServerError)
		return
	}
	request.Header.Set("A2A-Version", "1.0")
	request.Header.Set("A2A-Extensions", hitlExtension)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-User-Id", a.userID)

	response, err := a.httpClient.Do(request)
	if err != nil {
		http.Error(w, fmt.Sprintf("call kagent: %v", err), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		http.Error(w, fmt.Sprintf("kagent returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(body))), http.StatusBadGateway)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("X-Accel-Buffering", "no")
	flush, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is unavailable", http.StatusInternalServerError)
		return
	}
	if err := copySSE(r.Context(), w, flush, response.Body); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("proxy A2A stream: %v", err)
	}
}

func (a *app) handleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method must be GET", http.StatusMethodNotAllowed)
		return
	}

	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, a.mcpURL, nil)
	if err != nil {
		http.Error(w, "create MCP activity request", http.StatusInternalServerError)
		return
	}
	response, err := a.httpClient.Do(request)
	if err != nil {
		http.Error(w, fmt.Sprintf("read MCP activity: %v", err), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(response.Body, 128<<10))
}

func sendMessagePayload(input chatRequest) ([]byte, error) {
	messageID, err := randomID()
	if err != nil {
		return nil, err
	}
	message := map[string]any{
		"messageId": messageID,
		"role":      "ROLE_USER",
		"parts":     []map[string]string{{"text": input.Message}},
	}
	if input.ContextID != "" {
		message["contextId"] = input.ContextID
	}
	if input.TaskID != "" {
		message["taskId"] = input.TaskID
	}
	if input.HITLResponse != nil {
		message["extensions"] = []string{hitlExtension}
		message["metadata"] = map[string]any{hitlExtension: input.HITLResponse}
	}
	return json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      messageID,
		"method":  "SendStreamingMessage",
		"params":  map[string]any{"message": message},
	})
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func copySSE(ctx context.Context, w io.Writer, flush http.Flusher, reader io.Reader) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			return err
		}
		flush.Flush()
		if strings.Contains(line, `"state":"TASK_STATE_COMPLETED"`) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return scanner.Err()
}
