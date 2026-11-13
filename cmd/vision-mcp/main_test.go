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

func TestToolsListAdvertisesExpectedTools(t *testing.T) {
	server := &server{imageClient: http.DefaultClient}
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
	found := map[string]bool{}
	for _, tool := range body.Result.Tools {
		found[tool.Name] = true
	}
	for _, name := range []string{toolName, "sre_get_recent_failures", "github_list_files", "github_get_file", "github_create_draft_pr"} {
		if !found[name] {
			t.Errorf("tools = %#v, want %q", body.Result.Tools, name)
		}
	}
	if found["github_propose_draft_pr"] {
		t.Fatal("tools expose the removed custom proposal approval path")
	}
}

func TestRepositoryPathRejectsTraversal(t *testing.T) {
	for _, value := range []string{"../go.mod", "cmd/vision mcp/main.go", "cmd/vision-mcp/main.go?ref=other"} {
		if _, err := repositoryPath(value); err == nil {
			t.Errorf("repositoryPath accepted %q", value)
		}
	}
	path, err := repositoryPath("cmd/vision-mcp/main.go")
	if err != nil {
		t.Fatalf("repositoryPath: %v", err)
	}
	if path != "cmd/vision-mcp/main.go" {
		t.Fatalf("path = %q", path)
	}
}

func TestRepositoryDirectoryAcceptsRootOnlyExplicitly(t *testing.T) {
	path, err := repositoryDirectory(".")
	if err != nil {
		t.Fatalf("repositoryDirectory root: %v", err)
	}
	if path != "." {
		t.Fatalf("path = %q, want .", path)
	}
	if _, err := repositoryDirectory("../"); err == nil {
		t.Fatal("repositoryDirectory accepted traversal")
	}
}

func TestDraftPRArgumentsRequireOneSafeCompleteReplacement(t *testing.T) {
	valid, err := draftPRFromArguments(json.RawMessage(`{
		"title":"Fix timeout handling",
		"body":"Concise summary.",
		"path":"cmd/vision-mcp/main.go",
		"content":"package main\n"
	}`))
	if err != nil {
		t.Fatalf("draftPRFromArguments valid input: %v", err)
	}
	if valid.Path != "cmd/vision-mcp/main.go" {
		t.Fatalf("path = %q", valid.Path)
	}
	for _, raw := range []string{
		`{"title":"","body":"body","path":"go.mod","content":"module example"}`,
		`{"title":"title","body":"body","path":"../go.mod","content":"module example"}`,
		`{"title":"title","body":"body","path":"go.mod","content":""}`,
	} {
		if _, err := draftPRFromArguments(json.RawMessage(raw)); err == nil {
			t.Errorf("draftPRFromArguments accepted %s", raw)
		}
	}
}

func TestCreateDraftPRDoesNotWriteWithoutToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	result := (&server{}).createDraftPRTool(context.Background(), json.RawMessage(`{
		"title":"title",
		"body":"body",
		"path":"go.mod",
		"content":"module example"
	}`))
	body, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("result type = %T, want map", result)
	}
	if body["isError"] != true {
		t.Fatalf("result = %#v, want no-token error", result)
	}
}

func TestGitHubTokenTrimsSecretNewline(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "token-value\n")
	if token := githubToken(); token != "token-value" {
		t.Fatalf("githubToken() = %q, want token-value", token)
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
		ollamaClient: ollama.Client(),
		ollamaHost:   ollama.URL,
		ollamaModel:  "qwen3.6:27b",
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
	server := &server{imageClient: http.DefaultClient}
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
