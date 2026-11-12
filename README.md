# Kagent Vision on kind

This is a small, current-kagent port of
[`automateyournetwork/kagent_vision`](https://github.com/automateyournetwork/kagent_vision).
It replaces the original long-running ADK agent with a native kagent `Agent`
that runs on the Agent Substrate-backed `kagent-default` Harness.

The demo has one MCP tool: it fetches a public image URL and asks Gemini Vision
to describe it. A local kind cluster cannot reliably access a macOS webcam, so
camera capture, image generation, video generation, and ASL recording are
intentionally out of this first local demo.

## Prerequisites

- Docker, kind, kubectl, and the [kagent CLI](https://kagent.dev/)
- A Gemini API key with access to `gemini-2.0-flash`

## Run locally

Create a kind cluster and install kagent plus Agent Substrate. The kagent
installer creates the `kagent-default` Harness, its WorkerPool, and local
snapshot storage.

```bash
kind create cluster --name kagent
export GEMINI_API_KEY='replace-me'
cat > kagent-config.yaml <<'EOF'
install:
  defaultModelProvider: Gemini
EOF
kagent --config kagent-config.yaml install
```

Build and load the MCP image, then create the shared Gemini Secret and apply
the demo:

```bash
docker build -t kagent-vision-mcp:dev .
kind load docker-image --name kagent kagent-vision-mcp:dev
kubectl --context kind-kagent -n kagent create secret generic kagent-vision-gemini \
  --from-literal=GEMINI_API_KEY="$GEMINI_API_KEY"
kubectl --context kind-kagent apply -k k8s/
kubectl --context kind-kagent rollout status deployment/kagent-vision-mcp -n kagent
kubectl --context kind-kagent wait --for=condition=Ready agent/kagent-vision -n kagent --timeout=120s
```

Open the kagent dashboard:

```bash
kagent dashboard
```

Create a session for **kagent-vision** and paste a public HTTPS image URL,
then ask what is in it. The session actor is managed, sandboxed, suspended
when idle, and resumed by Agent Substrate through kagent.

## Cleanup

```bash
kubectl --context kind-kagent delete -k k8s/
kubectl --context kind-kagent -n kagent delete secret kagent-vision-gemini
kind delete cluster --name kagent
```
