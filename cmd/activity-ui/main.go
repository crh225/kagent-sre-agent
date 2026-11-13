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
)

//go:embed static/index.html
var indexHTML []byte

type app struct {
	a2aURL     string
	mcpURL     string
	httpClient *http.Client
}

type chatRequest struct {
	Message   string `json:"message"`
	ContextID string `json:"contextId"`
}

func main() {
	a := &app{
		a2aURL:     valueOrDefault("KAGENT_A2A_URL", defaultA2AURL),
		mcpURL:     valueOrDefault("MCP_ACTIVITY_URL", defaultMCPURL),
		httpClient: &http.Client{Timeout: 6 * time.Minute},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", a.handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/chat", a.handleChat)
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

	payload, err := sendMessagePayload(input)
	if err != nil {
		http.Error(w, "create A2A request", http.StatusInternalServerError)
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.a2aURL, bytes.NewReader(payload))
	if err != nil {
		http.Error(w, "create upstream request", http.StatusInternalServerError)
		return
	}
	request.Header.Set("A2A-Version", "1.0")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-User-Id", "vision-demo-ui")

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
		if _, err := io.WriteString(w, scanner.Text()+"\n"); err != nil {
			return err
		}
		flush.Flush()
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	return scanner.Err()
}
