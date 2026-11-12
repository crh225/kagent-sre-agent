package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToolsListAdvertisesVisionTool(t *testing.T) {
	server := &server{httpClient: http.DefaultClient}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	response := httptest.NewRecorder()

	server.handleMCP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	var body struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Result.Tools) != 1 || body.Result.Tools[0].Name != toolName {
		t.Fatalf("tools = %#v, want only %q", body.Result.Tools, toolName)
	}
}

func TestDescribeImageCallsLocalOllama(t *testing.T) {
	var received struct {
		Model    string `json:"model"`
		Messages []struct {
			Images []string `json:"images"`
		} `json:"messages"`
	}
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			t.Errorf("path = %q, want /api/chat", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"message":{"content":"A blue square."}}`))
	}))
	defer ollama.Close()

	server := &server{
		httpClient:  ollama.Client(),
		ollamaHost:  ollama.URL,
		ollamaModel: "qwen3.6:27b",
	}
	answer, err := server.describeImage(context.Background(), []byte("image"), "image/png", "What is shown?")
	if err != nil {
		t.Fatalf("describeImage: %v", err)
	}
	if answer != "A blue square." {
		t.Fatalf("answer = %q, want %q", answer, "A blue square.")
	}
	if received.Model != "qwen3.6:27b" {
		t.Fatalf("model = %q, want qwen3.6:27b", received.Model)
	}
	if len(received.Messages) != 1 || len(received.Messages[0].Images) != 1 {
		t.Fatalf("images = %#v, want one image", received.Messages)
	}
	if received.Messages[0].Images[0] != base64.StdEncoding.EncodeToString([]byte("image")) {
		t.Fatalf("image = %q, want encoded input", received.Messages[0].Images[0])
	}
}

func TestFetchImageRejectsUnsafeURLs(t *testing.T) {
	server := &server{httpClient: http.DefaultClient}
	for _, rawURL := range []string{
		"http://example.com/image.jpg",
		"https://127.0.0.1/image.jpg",
		"https://[::1]/image.jpg",
	} {
		t.Run(rawURL, func(t *testing.T) {
			_, _, err := server.fetchImage(context.Background(), rawURL)
			if err == nil {
				t.Fatal("fetchImage unexpectedly accepted an unsafe URL")
			}
		})
	}
}
