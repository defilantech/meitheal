---
name: Bug Report
about: Report a bug or unexpected behavior
title: '[BUG] '
labels: bug
assignees: ''
---

## Bug Description

A clear and concise description of what the bug is.

## Steps to Reproduce

1. Apply '...'
2. Run command '...'
3. Observe error '...'

## Expected Behavior

What you expected to happen.

## Actual Behavior

What actually happened.

## Environment

**Meitheal Version:**
```bash
meitheal version
# Output:
# Or the chart version and image tags you installed:
helm list -n meitheal-system
```

**Kubernetes Version:**
```bash
kubectl version
# Output:
```

**Where the agent runs:**
- [ ] In-cluster (meitheal-agent Deployment)
- [ ] Native on a Linux host
- [ ] Native on a macOS host
- [ ] Other:

**Agent execution mode:**
- [ ] InProcess
- [ ] Job (coder Job pods)

**Model backend:**
- [ ] LLMKube InferenceService
- [ ] OpenAI-compatible endpoint (`provider: cloud-proxy`)
- [ ] Other:

## Logs

**Operator Logs:**
```bash
kubectl logs -n meitheal-system deploy/meitheal-operator --tail=50
# Paste logs here
```

**Agent Logs:**
```bash
kubectl logs -n meitheal-system deploy/meitheal-agent --tail=50
# Or, for a native agent, its log file or journal
# Paste logs here
```

## YAML Manifests

Remove tokens, keys and other secrets before pasting.

**Agent:**
```yaml
# Paste your Agent YAML here
```

**AgenticTask:**
```yaml
# Paste the AgenticTask YAML here (kubectl get agentictask <name> -o yaml)
```

**Workload (if any):**
```yaml
# Paste your Workload YAML here
```

## Additional Context

Add any other context about the problem here (screenshots, error messages, etc.).
