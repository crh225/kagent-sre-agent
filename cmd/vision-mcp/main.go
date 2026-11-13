package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
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
	"sync"
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
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type visionArguments struct {
	ImageURL string `json:"image_url"`
	Question string `json:"question"`
}

type githubFileArguments struct {
	Path string `json:"path"`
}

type githubDirectoryArguments struct {
	Path string `json:"path"`
}

type draftPRArguments struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Path    string `json:"path"`
	Content string `json:"content"`
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
	imageClient   *http.Client
	ollamaClient  *http.Client
	ollamaHost    string
	ollamaModel   string
	githubBaseURL string
	activityMu    sync.RWMutex
	activity      []toolActivity
}

type toolActivity struct {
	At     time.Time `json:"at"`
	Tool   string    `json:"tool"`
	Phase  string    `json:"phase"`
	Detail string    `json:"detail"`
}

type draftPRProposal struct {
	ID      string
	Title   string
	Body    string
	Path    string
	Content string
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
	mux.HandleFunc("/activity", s.handleActivity)
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
		response.Result = map[string]any{"tools": toolDefinitions()}
	case "tools/call":
		response.Result, response.Error = s.callTool(r.Context(), request.Params)
	default:
		response.Error = &rpcError{Code: -32601, Message: "method not found"}
	}
	s.writeResponse(w, response)
}

func (s *server) handleActivity(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method must be GET", http.StatusMethodNotAllowed)
		return
	}
	s.activityMu.RLock()
	events := append([]toolActivity(nil), s.activity...)
	s.activityMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"events": events}); err != nil {
		log.Printf("write activity response: %v", err)
	}
}

func toolDefinitions() []any {
	return []any{
		map[string]any{
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
		},
		map[string]any{
			"name":        "sre_get_recent_failures",
			"description": "Read recent failed MCP tool calls from this local demo.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		map[string]any{
			"name":        "github_get_file",
			"description": "Read a file from the public crh225/kagent-sre-agent main branch.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"path"},
				"properties": map[string]any{
					"path": map[string]string{"type": "string", "description": "Repository-relative file path."},
				},
			},
		},
		map[string]any{
			"name":        "github_list_files",
			"description": "List up to 100 direct entries in a directory of the public crh225/kagent-sre-agent main branch. Use path \".\" for the repository root.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"path"},
				"properties": map[string]any{
					"path": map[string]string{"type": "string", "description": "Repository-relative directory path, or \".\" for the root."},
				},
			},
		},
		map[string]any{
			"name":        "github_create_draft_pr",
			"description": "Create a draft PR for a complete file replacement in crh225/kagent-sre-agent. This tool is approval-gated by the SRE agent configuration.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"title", "body", "path", "content"},
				"properties": map[string]any{
					"title":   map[string]string{"type": "string"},
					"body":    map[string]string{"type": "string"},
					"path":    map[string]string{"type": "string"},
					"content": map[string]string{"type": "string", "description": "Complete replacement content for the file."},
				},
			},
		},
	}
}

func (s *server) callTool(ctx context.Context, rawParams json.RawMessage) (any, *rpcError) {
	var params toolCallParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid tool arguments"}
	}
	switch params.Name {
	case toolName:
		return s.describeImageTool(ctx, params.Arguments)
	case "sre_get_recent_failures":
		return s.recentFailures(), nil
	case "github_get_file":
		return s.githubGetFile(ctx, params.Arguments), nil
	case "github_list_files":
		return s.githubListFiles(ctx, params.Arguments), nil
	case "github_create_draft_pr":
		return s.createDraftPRTool(ctx, params.Arguments), nil
	default:
		return nil, &rpcError{Code: -32602, Message: "unknown tool"}
	}
}

func (s *server) createDraftPRTool(ctx context.Context, rawArguments json.RawMessage) any {
	if githubToken() == "" {
		return toolError(errors.New("GITHUB_TOKEN is not configured; no GitHub write was attempted"))
	}
	proposal, err := draftPRFromArguments(rawArguments)
	if err != nil {
		return toolError(err)
	}
	proposal.ID, err = randomID()
	if err != nil {
		return toolError(fmt.Errorf("create draft PR ID: %w", err))
	}
	prURL, err := s.createDraftPR(ctx, proposal)
	if err != nil {
		return toolError(err)
	}
	return toolText(map[string]any{"state": "created", "pullRequestURL": prURL})
}

func draftPRFromArguments(rawArguments json.RawMessage) (draftPRProposal, error) {
	var arguments draftPRArguments
	if err := json.Unmarshal(rawArguments, &arguments); err != nil {
		return draftPRProposal{}, fmt.Errorf("invalid draft PR proposal: %w", err)
	}
	path, err := repositoryPath(arguments.Path)
	if err != nil {
		return draftPRProposal{}, err
	}
	arguments.Title = strings.TrimSpace(arguments.Title)
	arguments.Body = strings.TrimSpace(arguments.Body)
	if arguments.Title == "" || arguments.Body == "" || arguments.Content == "" {
		return draftPRProposal{}, errors.New("title, body, and complete replacement content are required")
	}
	if len(arguments.Content) > 32<<10 {
		return draftPRProposal{}, errors.New("proposal content exceeds the 32 KiB limit")
	}
	return draftPRProposal{
		Title:   arguments.Title,
		Body:    arguments.Body,
		Path:    path,
		Content: arguments.Content,
	}, nil
}

func (s *server) createDraftPR(ctx context.Context, proposal draftPRProposal) (string, error) {
	var mainRef struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := s.githubRequest(ctx, http.MethodGet, "/repos/crh225/kagent-sre-agent/git/ref/heads/main", nil, &mainRef); err != nil {
		return "", err
	}
	var currentFile struct {
		SHA string `json:"sha"`
	}
	if err := s.githubRequest(ctx, http.MethodGet, "/repos/crh225/kagent-sre-agent/contents/"+proposal.Path+"?ref=main", nil, &currentFile); err != nil {
		return "", err
	}
	if currentFile.SHA == "" {
		return "", errors.New("GitHub did not return the current file SHA")
	}
	branch := "sre/" + proposal.ID[:12]
	if err := s.githubRequest(ctx, http.MethodPost, "/repos/crh225/kagent-sre-agent/git/refs", map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": mainRef.Object.SHA,
	}, nil); err != nil {
		return "", err
	}
	if err := s.githubRequest(ctx, http.MethodPut, "/repos/crh225/kagent-sre-agent/contents/"+proposal.Path, map[string]string{
		"message": proposal.Title,
		"content": base64.StdEncoding.EncodeToString([]byte(proposal.Content)),
		"branch":  branch,
		"sha":     currentFile.SHA,
	}, nil); err != nil {
		return "", err
	}
	var pull struct {
		HTMLURL string `json:"html_url"`
	}
	if err := s.githubRequest(ctx, http.MethodPost, "/repos/crh225/kagent-sre-agent/pulls", map[string]any{
		"title": proposal.Title,
		"head":  branch,
		"base":  "main",
		"body":  proposal.Body,
		"draft": true,
	}, &pull); err != nil {
		return "", err
	}
	return pull.HTMLURL, nil
}

func (s *server) githubRequest(ctx context.Context, method, path string, body any, output any) error {
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode GitHub request: %w", err)
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequestWithContext(ctx, method, s.githubAPIURL(path), reader)
	if err != nil {
		return fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+githubToken())
	request.Header.Set("User-Agent", "kagent-vision-mcp")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := s.ollamaClient.Do(request)
	if err != nil {
		return fmt.Errorf("call GitHub: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		return fmt.Errorf("GitHub returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if output != nil {
		if err := json.NewDecoder(response.Body).Decode(output); err != nil {
			return fmt.Errorf("decode GitHub response: %w", err)
		}
	}
	return nil
}

func (s *server) githubAPIURL(path string) string {
	baseURL := s.githubBaseURL
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return strings.TrimRight(baseURL, "/") + path
}

func (s *server) describeImageTool(ctx context.Context, rawArguments json.RawMessage) (any, *rpcError) {
	var arguments visionArguments
	if err := json.Unmarshal(rawArguments, &arguments); err != nil {
		return nil, &rpcError{Code: -32602, Message: "invalid image tool arguments"}
	}
	s.recordActivity("started", "Fetching the requested image.")
	image, mimeType, err := s.fetchImage(ctx, arguments.ImageURL)
	if err != nil {
		s.recordActivity("failed", err.Error())
		return toolError(err), nil
	}
	answer, err := s.describeImage(ctx, image, mimeType, arguments.Question)
	if err != nil {
		s.recordActivity("failed", err.Error())
		return toolError(err), nil
	}
	s.recordActivity("completed", "Image analysis returned successfully.")
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": answer}},
	}, nil
}

func (s *server) recordActivity(phase, detail string) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	s.activity = append(s.activity, toolActivity{
		At:     time.Now().UTC(),
		Tool:   toolName,
		Phase:  phase,
		Detail: detail,
	})
	if len(s.activity) > 50 {
		s.activity = s.activity[len(s.activity)-50:]
	}
}

func randomID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes[:]), nil
}

func (s *server) recentFailures() any {
	s.activityMu.RLock()
	defer s.activityMu.RUnlock()
	failures := make([]toolActivity, 0)
	for _, event := range s.activity {
		if event.Phase == "failed" {
			failures = append(failures, event)
		}
	}
	return toolText(map[string]any{"failures": failures})
}

func (s *server) githubGetFile(ctx context.Context, rawArguments json.RawMessage) any {
	var arguments githubFileArguments
	if err := json.Unmarshal(rawArguments, &arguments); err != nil {
		return toolError(fmt.Errorf("invalid GitHub tool arguments: %w", err))
	}
	path, err := repositoryPath(arguments.Path)
	if err != nil {
		return toolError(err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/crh225/kagent-sre-agent/contents/"+path+"?ref=main", nil)
	if err != nil {
		return toolError(fmt.Errorf("create GitHub request: %w", err))
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "kagent-vision-mcp")
	response, err := s.imageClient.Do(request)
	if err != nil {
		return toolError(fmt.Errorf("read GitHub file: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return toolError(fmt.Errorf("read GitHub file: received HTTP %d", response.StatusCode))
	}
	var rawResponse json.RawMessage
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&rawResponse); err != nil {
		return toolError(fmt.Errorf("decode GitHub response: %w", err))
	}
	if len(rawResponse) > 0 && rawResponse[0] == '[' {
		return toolError(errors.New("path is a directory; use github_list_files to discover its files"))
	}
	var file struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Size     int    `json:"size"`
	}
	if err := json.Unmarshal(rawResponse, &file); err != nil {
		return toolError(fmt.Errorf("decode GitHub file: %w", err))
	}
	if file.Encoding != "base64" {
		return toolError(errors.New("GitHub file did not use base64 encoding"))
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return toolError(fmt.Errorf("decode GitHub file content: %w", err))
	}
	const maxFileBytes = 32 << 10
	if len(content) > maxFileBytes {
		return toolError(fmt.Errorf("GitHub file is %d bytes; maximum supported size is %d", len(content), maxFileBytes))
	}
	return toolText(map[string]any{
		"repository": "crh225/kagent-sre-agent",
		"path":       path,
		"size":       file.Size,
		"content":    string(content),
	})
}

func (s *server) githubListFiles(ctx context.Context, rawArguments json.RawMessage) any {
	var arguments githubDirectoryArguments
	if err := json.Unmarshal(rawArguments, &arguments); err != nil {
		return toolError(fmt.Errorf("invalid GitHub tool arguments: %w", err))
	}
	path, err := repositoryDirectory(arguments.Path)
	if err != nil {
		return toolError(err)
	}
	endpoint := "https://api.github.com/repos/crh225/kagent-sre-agent/contents"
	if path != "." {
		endpoint += "/" + path
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?ref=main", nil)
	if err != nil {
		return toolError(fmt.Errorf("create GitHub request: %w", err))
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "kagent-vision-mcp")
	response, err := s.imageClient.Do(request)
	if err != nil {
		return toolError(fmt.Errorf("list GitHub files: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return toolError(fmt.Errorf("list GitHub files: received HTTP %d", response.StatusCode))
	}
	var entries []struct {
		Name string `json:"name"`
		Path string `json:"path"`
		Type string `json:"type"`
		Size int    `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&entries); err != nil {
		return toolError(errors.New("path is a file; use github_get_file to read it"))
	}
	if len(entries) > 100 {
		entries = entries[:100]
	}
	return toolText(map[string]any{
		"repository": "crh225/kagent-sre-agent",
		"path":       path,
		"entries":    entries,
	})
}

func repositoryPath(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(value), "/")
	if value == "" || strings.Contains(value, "..") || strings.Contains(value, "\\") {
		return "", errors.New("path must be a non-empty repository-relative path")
	}
	sanitized := strings.Map(func(r rune) rune {
		if r == '/' || r == '-' || r == '_' || r == '.' || ('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9') {
			return r
		}
		return -1
	}, value)
	if sanitized != value {
		return "", errors.New("path contains unsupported characters")
	}
	return value, nil
}

func repositoryDirectory(value string) (string, error) {
	value = strings.Trim(strings.TrimSpace(value), "/")
	if value == "." {
		return value, nil
	}
	return repositoryPath(value)
}

func githubToken() string {
	return strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
}

func toolError(err error) map[string]any {
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": err.Error()}},
		"isError": true,
	}
}

func toolText(value any) map[string]any {
	content, err := json.Marshal(value)
	if err != nil {
		return toolError(fmt.Errorf("encode tool response: %w", err))
	}
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": string(content)}},
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
