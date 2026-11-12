package main

import (
	"context"
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
