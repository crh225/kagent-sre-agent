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

type geminiRequest struct {
	Contents []struct {
		Parts []struct {
			Text       string `json:"text,omitempty"`
			InlineData *struct {
				MIMEType string `json:"mime_type"`
				Data     string `json:"data"`
			} `json:"inline_data,omitempty"`
		} `json:"parts"`
	} `json:"contents"`
}

type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type server struct {
	httpClient *http.Client
	apiKey     string
}

func main() {
	apiKey := os.Getenv("GEMINI_API_KEY")
	if apiKey == "" {
		log.Fatal("GEMINI_API_KEY must be configured")
	}

	s := &server{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: 45 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
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
		"description": "Describe a publicly reachable image URL using Gemini Vision.",
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
	response, err := s.httpClient.Do(request)
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

func (s *server) describeImage(ctx context.Context, image []byte, mimeType, question string) (string, error) {
	if question == "" {
		question = "Describe this image accurately and concisely."
	}
	var body geminiRequest
	body.Contents = append(body.Contents, struct {
		Parts []struct {
			Text       string `json:"text,omitempty"`
			InlineData *struct {
				MIMEType string `json:"mime_type"`
				Data     string `json:"data"`
			} `json:"inline_data,omitempty"`
		} `json:"parts"`
	}{
		Parts: []struct {
			Text       string `json:"text,omitempty"`
			InlineData *struct {
				MIMEType string `json:"mime_type"`
				Data     string `json:"data"`
			} `json:"inline_data,omitempty"`
		}{
			{Text: question},
			{InlineData: &struct {
				MIMEType string `json:"mime_type"`
				Data     string `json:"data"`
			}{MIMEType: mimeType, Data: base64.StdEncoding.EncodeToString(image)}},
		},
	})
	payload, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("encode Gemini request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://generativelanguage.googleapis.com/v1beta/models/gemini-2.0-flash:generateContent?key="+url.QueryEscape(s.apiKey),
		bytes.NewReader(payload),
	)
	if err != nil {
		return "", fmt.Errorf("create Gemini request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.httpClient.Do(request)
	if err != nil {
		return "", fmt.Errorf("call Gemini: %w", err)
	}
	defer response.Body.Close()

	var decoded geminiResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decode Gemini response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		if decoded.Error != nil && decoded.Error.Message != "" {
			return "", fmt.Errorf("Gemini request failed: %s", decoded.Error.Message)
		}
		return "", fmt.Errorf("Gemini request failed with HTTP %d", response.StatusCode)
	}
	if len(decoded.Candidates) == 0 || len(decoded.Candidates[0].Content.Parts) == 0 {
		return "", errors.New("Gemini returned no description")
	}
	return decoded.Candidates[0].Content.Parts[0].Text, nil
}

func (s *server) writeResponse(w http.ResponseWriter, response rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("write response: %v", err)
	}
}
