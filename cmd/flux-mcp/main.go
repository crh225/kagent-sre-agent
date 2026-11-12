package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func main() {
	http.HandleFunc("/mcp", handle)
	if err := http.ListenAndServe(":8080", nil); err != nil {
		panic(err)
	}
}

func handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	var request request
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request); err != nil {
		write(w, nil, nil, "invalid request")
		return
	}
	switch request.Method {
	case "initialize":
		write(w, request.ID, map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "flux-mcp", "version": "0.1.0"},
		}, "")
	case "tools/list":
		write(w, request.ID, map[string]any{"tools": []any{map[string]any{
			"name": "flux_get_status", "description": "Get the Ready conditions and status for one Flux Kustomization or HelmRelease.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"kind", "namespace", "name"}, "properties": map[string]any{
				"kind":      map[string]any{"type": "string", "enum": []string{"Kustomization", "HelmRelease"}},
				"namespace": map[string]any{"type": "string"}, "name": map[string]any{"type": "string"},
			}},
		}}}, "")
	case "tools/call":
		var params struct {
			Name      string `json:"name"`
			Arguments struct {
				Kind      string `json:"kind"`
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"arguments"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil || params.Name != "flux_get_status" {
			write(w, request.ID, nil, "unsupported tool")
			return
		}
		status, err := getFluxStatus(r, params.Arguments.Kind, params.Arguments.Namespace, params.Arguments.Name)
		if err != nil {
			write(w, request.ID, nil, err.Error())
			return
		}
		write(w, request.ID, map[string]any{"content": []any{map[string]string{"type": "text", "text": status}}}, "")
	default:
		write(w, request.ID, nil, "unsupported method")
	}
}

func getFluxStatus(request *http.Request, kind, namespace, name string) (string, error) {
	paths := map[string]string{"Kustomization": "kustomize.toolkit.fluxcd.io/v1", "HelmRelease": "helm.toolkit.fluxcd.io/v2"}
	group, ok := paths[kind]
	if !ok || namespace == "" || name == "" || strings.ContainsAny(namespace+name, "/\\") {
		return "", fmt.Errorf("invalid Flux resource")
	}
	token, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return "", fmt.Errorf("read service account token: %w", err)
	}
	ca, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt")
	if err != nil {
		return "", fmt.Errorf("read Kubernetes CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return "", fmt.Errorf("parse Kubernetes CA")
	}
	url := fmt.Sprintf("https://%s:%s/apis/%s/namespaces/%s/%ss/%s",
		os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT_HTTPS"), group, namespace, strings.ToLower(kind), name)
	apiRequest, err := http.NewRequestWithContext(request.Context(), http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	apiRequest.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}}
	response, err := client.Do(apiRequest)
	if err != nil {
		return "", fmt.Errorf("get Flux resource: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("get Flux resource: %s", response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 256<<10))
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func write(w http.ResponseWriter, id json.RawMessage, result any, message string) {
	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{"jsonrpc": "2.0", "id": id}
	if message != "" {
		response["error"] = map[string]any{"code": -32602, "message": message}
	} else {
		response["result"] = result
	}
	_ = json.NewEncoder(w).Encode(response)
}
