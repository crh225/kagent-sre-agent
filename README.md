# Kagent Vision on kind

This is a small, current-kagent port of
[`automateyournetwork/kagent_vision`](https://github.com/automateyournetwork/kagent_vision).
It replaces the original long-running ADK agent with a native kagent `Agent`
that runs on the Agent Substrate-backed `kagent-default` Harness.

The vision agent has one MCP tool: it fetches a public image URL and asks a
local Ollama vision model to describe it. A local kind cluster cannot reliably
access a macOS webcam, so camera capture, image generation, video generation,
and ASL recording are intentionally out of this first local demo.

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

## Activity demo UI

The optional activity UI shows the real A2A task lifecycle, streaming response,
token usage, and actual MCP tool events from this demo:

```bash
kubectl --context kind-kagent -n kagent port-forward service/kagent-vision-ui 8090:8090
```

Open [http://localhost:8090](http://localhost:8090), paste a public HTTPS image
URL, and watch the agent Session and `vision_describe_image` activity.

Create a session for **kagent-vision** and paste a public HTTPS image URL,
then ask what is in it. The session actor is managed, sandboxed, suspended
when idle, and resumed by Agent Substrate through kagent.
Image analysis with the default local 27B model can take several minutes.

## SRE handoff and approved draft PRs

The activity UI automatically sends new MCP failures to `kagent-sre`. The SRE
agent can read recent failures and public files from this repository. It can
propose a complete replacement for one file, but `github_create_draft_pr` is
configured with `requireApproval: true`: the kagent dashboard presents its
arguments and must be explicitly approved before any GitHub write occurs.
The local UI uses the dashboard's default `admin@kagent.dev` identity, so new
Vision and SRE conversations appear in the same dashboard session list.

To enable that optional local capability, create an in-cluster Secret from
your existing GitHub CLI login. The token is not stored in this repository,
the manifest, the browser, or the container image:

```bash
gh auth token | kubectl --context kind-kagent -n kagent create secret generic kagent-vision-github \
  --from-file=GITHUB_TOKEN=/dev/stdin
kubectl --context kind-kagent -n kagent rollout restart deployment/kagent-vision-mcp
```

Without that Secret, the tool returns an error before attempting a GitHub
write. The SRE agent cannot merge a pull request or modify Kubernetes.

## Cleanup

```bash
kubectl --context kind-kagent delete -k k8s/
kind delete cluster --name kagent
```
