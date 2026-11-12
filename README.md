# kagent SRE agent

A minimal, read-only Flux investigator built with kagent and Agent Substrate.

The agent runs through a kagent `Harness` backed by an Agent Substrate
WorkerPool. Its only tool is the local `flux-mcp` service, which can read one
Flux `Kustomization` or `HelmRelease`. It has no Kubernetes write permissions,
GitHub access, Datadog, Port, or Secret access.

## Prerequisites

- kagent installed with Agent Substrate and a ready `kagent-default` WorkerPool
- a snapshot location configured by the Agent Substrate installation
- a `ModelConfig` in `kagent-sre`
- a published `ghcr.io/crh225/kagent-sre-agent` image tag

Set the `AgentTemplate.spec.modelConfig.name` and MCP image tag before applying:

```bash
kubectl apply -k k8s/
```

Create a session after the Agent is Ready:

```bash
kagent agent session create --agent flux-investigator --namespace kagent-sre
```

## Use the kagent UI

From a machine whose kubeconfig targets the cluster running kagent, open the
dashboard with:

```bash
kagent dashboard
```

The kagent CLI connects to the controller using the selected kubeconfig and
starts any required local connection. Use the dashboard to find the
`flux-investigator` agent, create a session, and inspect its conversations.

