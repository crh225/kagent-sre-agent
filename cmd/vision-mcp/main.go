package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	maxImageBytes = 10 << 20
	toolName      = "vision_describe_image"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolCallParams struct {
	Name      string `json:"name"`
	Arguments struct {
		ImageURL string `json:"image_url"`
		Question string `json:"question"`
	} `json:"arguments"`
}

type ollamaRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string   `json:"role"`
		Content string   `json:"content"`
		Images  []string `json:"images"`
	} `json:"messages"`
}

type ollamaResponse struct {
	Message struct {
		Content string `json:"content"`
	} `json:"message"`
	Error string `json:"error"`
}

type server struct {
	imageClient  *http.Client
	ollamaClient *http.Client
	ollamaHost   string
	ollamaModel  string
}

func main() {
	ollamaHost := strings.TrimRight(os.Getenv("OLLAMA_HOST"), "/")
	if ollamaHost == "" {
		ollamaHost = "http://host.docker.internal:11434"
	}
	ollamaModel := os.Getenv("OLLAMA_MODEL")
	if ollamaModel == "" {
		ollamaModel = "qwen3.6:27b"
	}

	s := &server{
		ollamaHost:  ollamaHost,
		ollamaModel: ollamaModel,
		imageClient: &http.Client{
			Timeout: 45 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		ollamaClient: &http.Client{Timeout: 5 * time.Minute},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handleMCP)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Println("vision MCP server listening on :8080")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatal(err)
	}
}

func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method must be POST", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()

	var request rpcRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
		s.writeResponse(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "invalid JSON-RPC request"}})
		return
	}

	response := rpcResponse{JSONRPC: "2.0", ID: request.ID}
	switch request.Method {
	case "initialize":
		response.Result = map[string]any{
			"protocolVersion": "2025-03-26",
			"serverInfo":      map[string]string{"name": "kagent-vision-mcp", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}
	case "tools/list":
		response.Result = map[string]any{"tools": []any{toolDefinition()}}
	case "tools/call":
		response.Result, response.Error = s.callTool(r.Context(), request.Params)
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	s.writeResponse(w, response)
}

func toolDefinition() map[string]any {
	return map[string]any{
		"name":        toolName,
		"description": "Describe a publicly reachable image URL using local Ollama Vision.",
		"inputSchema": map[string]any{
			"type":     "object",
			"required": []string{"image_url"},
			"properties": map[string]any{
				"image_url": map[string]string{"type": "string", "description": "Public HTTPS URL of a JPEG, PNG, GIF, or WebP image."},
				"question":  map[string]string{"type": "string", "description": "Optional question about the image."},
			},
		},
	}
}

func (s *server) callTool(ctx context.Context, rawParams json.RawMessage) (any, *rpcError) {
	var params toolCallParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid tool arguments"}
	}
	if params.Name != toolName {
		return nil, &rpcError{Code: -32602, Message: "unknown tool"}
	}

	image, mimeType, err := s.fetchImage(ctx, params.Arguments.ImageURL)
	if err != nil {
		return toolError(err), nil
	}
	answer, err := s.describeImage(ctx, image, mimeType, params.Arguments.Question)
	if err != nil {
		return toolError(err), nil
	}
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": answer}},
	}, nil
}

func toolError(err error) map[string]any {
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": err.Error()}},
		"isError": true,
	}
}

func (s *server) fetchImage(ctx context.Context, rawURL string) ([]byte, string, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, "", errors.New("image_url must be a public HTTPS URL")
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && !ip.IsGlobalUnicast() {
		return nil, "", errors.New("image_url must not use a private IP address")
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("create image request: %w", err)
	}
	response, err := s.imageClient.Do(request)
	if err != nil {
		return nil, "", fmt.Errorf("fetch image: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fetch image: received HTTP %d", response.StatusCode)
	}

	mimeType := strings.Split(response.Header.Get("Content-Type"), ";")[0]
	switch mimeType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
	default:
		return nil, "", errors.New("image_url must return JPEG, PNG, GIF, or WebP content")
	}
	image, err := io.ReadAll(io.LimitReader(response.Body, maxImageBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read image: %w", err)
	}
	if len(image) > maxImageBytes {
		return nil, "", errors.New("image is larger than 10 MiB")
	}
	return image, mimeType, nil
}

func (s *server) describeImage(ctx context.Context, image []byte, _ string, question string) (string, error) {
	if question == "" {
		question = "Describe this image accurately and concisely."
	}
	var body ollamaRequest
	body.Model = s.ollamaModel
	body.Stream = false
	body.Messages = append(body.Messages, struct {
		Role    string   `json:"role"`
		Content string   `json:"content"`
		Images  []string `json:"images"`
	}{
		Role:    "user",
		Content: question,
		Images:  []string{base64.StdEncoding.EncodeToString(image)},
	})
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode Ollama request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		s.ollamaHost+"/api/chat",
		bytes.NewReader(payload),
	)
	if err != nil {
		return "", fmt.Errorf("create Ollama request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.ollamaClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("call Ollama: %w", err)
	}
	defer response.Body.Close()

	var decoded ollamaResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode Ollama response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if decoded.Error != "" {
			return "", fmt.Errorf("Ollama request failed: %s", decoded.Error)
		}
		return "", fmt.Errorf("Ollama request failed with HTTP %d", response.StatusCode)
	}
	if decoded.Message.Content == "" {
		return "", errors.New("Ollama returned no description")
	}
	return decoded.Message.Content, nil
}

func (s *server) writeResponse(w http.ResponseWriter, response rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("write response: %v", err)
	}
}
