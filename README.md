# Kagent Vision on kind

This is a small, current-kagent port of
[`automateyournetwork/kagent_vision`](https://github.com/automateyournetwork/kagent_vision).
It replaces the original long-running ADK agent with a native kagent `Agent`
that runs on the Agent Substrate-backed `kagent-default` Harness.

The demo has one MCP tool: it fetches a public image URL and asks a local
Ollama vision model to describe it. A local kind cluster cannot reliably access a macOS webcam, so
camera capture, image generation, video generation, and ASL recording are
intentionally out of this first local demo.

## Prerequisites

- Docker, kind, kubectl, and the [kagent CLI](https://kagent.dev/)
- Ollama running locally with a multimodal model; this demo defaults to
  `qwen3.6:27b`

## Run locally

Create a kind cluster and install kagent plus Agent Substrate. The kagent
installer creates the `kagent-default` Harness, its WorkerPool, and local
snapshot storage.

```bash
kind create cluster --name kagent --config kind/kagent.yaml
export OLLAMA_API_KEY=local-ollama # Required by the installer; Ollama ignores it.
kagent --namespace kagent install --provider Ollama
```

Build and load the MCP image, then apply the demo:

```bash
docker build -t kagent-vision-mcp:dev .
kind load docker-image --name kagent kagent-vision-mcp:dev
kubectl --context kind-kagent apply -k k8s/
kubectl --context kind-kagent rollout status deployment/kagent-vision-mcp -n kagent
kubectl --context kind-kagent wait --for=condition=Ready agent/kagent-vision -n kagent --timeout=120s
```

If a public image host uses a locally trusted corporate or gateway CA, mount
that CA into the MCP pod without disabling TLS:

```bash
kubectl --context kind-kagent -n kagent create configmap kagent-vision-extra-ca \
  --from-file=extra-ca.pem=/path/to/trusted-ca.pem
kubectl --context kind-kagent -n kagent rollout restart deployment/kagent-vision-mcp
```

Open the kagent dashboard:

```bash
kagent dashboard
```

Create a session for **kagent-vision** and paste a public HTTPS image URL,
then ask what is in it. The session actor is managed, sandboxed, suspended
when idle, and resumed by Agent Substrate through kagent.
Image analysis with the default local 27B model can take several minutes.

## Cleanup

```bash
kubectl --context kind-kagent delete -k k8s/
kind delete cluster --name kagent
```
