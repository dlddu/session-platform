#!/bin/sh
set -eu

agent_bin="${DATA_PLANE_AGENT_BIN:-/agent}"
workload="${DATA_PLANE_WORKLOAD:-shell}"

if [ "$workload" = "claude-code" ]; then
  : "${K3S_MCP_TOKEN:?K3S_MCP_TOKEN is required for Claude plugin bootstrap}"

  k3s_mcp_url="${K3S_MCP_URL:-https://homelab-k3s-mcp.llkm.nl/mcp}"
  # The marketplace is a plain git remote, so the platform can point this at any
  # host that serves the same repository shape — production keeps github.com and
  # the kind e2e SUT points it at an in-cluster remote (deploy/). Both run the
  # identical `claude plugin marketplace add` code path.
  marketplace_url="${CLAUDE_CODE_PLUGIN_MARKETPLACE_URL:-https://github.com/dlddu/plugin-marketplace.git}"
  plugin_cache_dir="${CLAUDE_CODE_PLUGIN_CACHE_DIR:-/tmp/session-platform-claude-plugin-seed}"
  bootstrap_home="${CLAUDE_CODE_PLUGIN_BOOTSTRAP_HOME:-/tmp/session-platform-claude-plugin-home}"

  mkdir -p "$plugin_cache_dir" "$bootstrap_home"
  chmod 700 "$plugin_cache_dir" "$bootstrap_home"

  mcp_request='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_app_installation_token","arguments":{"repositories":["plugin-marketplace"],"permissions":{"contents":"read"}}}}'
  mcp_response="$(
    curl --fail --silent --show-error \
      --connect-timeout 10 --max-time 30 \
      --header "Authorization: Bearer ${K3S_MCP_TOKEN}" \
      --header "Content-Type: application/json" \
      --header "Accept: application/json" \
      --data "$mcp_request" \
      "$k3s_mcp_url"
  )"
  github_token="$(
    printf '%s' "$mcp_response" | jq -er '
      if .error != null then
        error("K3s MCP GitHub token request failed")
      elif .result.isError == true then
        error("K3s MCP GitHub token tool returned an error")
      else
        first(
          .result.content[]?
          | select(.type == "resource")
          | .resource.text
          | split("\n")[]
          | select(startswith("GITHUB_TOKEN="))
          | ltrimstr("GITHUB_TOKEN=")
          | select(length > 0)
        ) // error("K3s MCP response omitted GITHUB_TOKEN")
      end
    '
  )"
  github_auth_header="Basic $(
    printf '%s' "x-access-token:${github_token}" | base64 | tr -d '\n'
  )"

  echo "Bootstrapping Claude plugin marketplace" >&2
  # Claude Code clears GIT_ASKPASS before it spawns Git. A process-scoped Git
  # config survives that sanitization without persisting the short-lived token.
  # The extraheader is scoped to the marketplace URL itself, so pointing the URL
  # elsewhere moves the scope with it and never widens it.
  marketplace_auth_key="http.${marketplace_url}.extraheader"
  HOME="$bootstrap_home" \
  CLAUDE_CODE_PLUGIN_CACHE_DIR="$plugin_cache_dir" \
  GIT_CONFIG_COUNT=1 \
  GIT_CONFIG_KEY_0="$marketplace_auth_key" \
  GIT_CONFIG_VALUE_0="Authorization: ${github_auth_header}" \
  GIT_TERMINAL_PROMPT=0 \
    claude plugin marketplace add "$marketplace_url"
  HOME="$bootstrap_home" \
  CLAUDE_CODE_PLUGIN_CACHE_DIR="$plugin_cache_dir" \
  GIT_CONFIG_COUNT=1 \
  GIT_CONFIG_KEY_0="$marketplace_auth_key" \
  GIT_CONFIG_VALUE_0="Authorization: ${github_auth_header}" \
  GIT_TERMINAL_PROMPT=0 \
    claude plugin install session-platform@dlddu-plugins

  unset github_auth_header github_token mcp_response mcp_request
  # The agent must resolve the plugin from the very same cache the bootstrap
  # populated. A seed dir only syncs marketplace registrations, and only after
  # plugin loading has already run, so the installed plugin never resolves.
  export CLAUDE_CODE_PLUGIN_CACHE_DIR="$plugin_cache_dir"
fi

exec "$agent_bin" "$@"
