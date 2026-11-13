#!/bin/sh
set -eu

agent_bin="${DATA_PLANE_AGENT_BIN:-/agent}"

# Every workload starts the agent directly. A claude-code session's plugins
# come from its loopback k3s-mcp-proxy sidecar, which holds the K3s MCP token
# and clones the marketplace; the agent installs them (claude_plugins.go).
exec "$agent_bin" "$@"
