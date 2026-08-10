#!/bin/sh
set -eu

agent_bin="${DATA_PLANE_AGENT_BIN:-/agent}"
workload="${DATA_PLANE_WORKLOAD:-shell}"

if [ "$workload" = "claude-code" ]; then
  : "${K3S_MCP_TOKEN:?K3S_MCP_TOKEN is required for Claude plugin bootstrap}"

  k3s_mcp_url="${K3S_MCP_URL:-https://homelab-k3s-mcp.llkm.nl/mcp}"
  plugin_cache_dir="${CLAUDE_CODE_PLUGIN_CACHE_DIR:-/tmp/session-platform-claude-plugin-seed}"
  bootstrap_home="${CLAUDE_CODE_PLUGIN_BOOTSTRAP_HOME:-/tmp/session-platform-claude-plugin-home}"
  git_askpass="${SESSION_PLATFORM_GIT_ASKPASS_BIN:-/usr/local/bin/session-platform-git-askpass}"

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

  echo "Bootstrapping Claude plugin marketplace" >&2
  HOME="$bootstrap_home" \
  CLAUDE_CODE_PLUGIN_CACHE_DIR="$plugin_cache_dir" \
  GIT_ASKPASS="$git_askpass" \
  GIT_TERMINAL_PROMPT=0 \
  SESSION_PLATFORM_GITHUB_TOKEN="$github_token" \
    claude plugin marketplace add https://github.com/dlddu/plugin-marketplace.git
  HOME="$bootstrap_home" \
  CLAUDE_CODE_PLUGIN_CACHE_DIR="$plugin_cache_dir" \
  GIT_ASKPASS="$git_askpass" \
  GIT_TERMINAL_PROMPT=0 \
  SESSION_PLATFORM_GITHUB_TOKEN="$github_token" \
    claude plugin install session-platform@dlddu-plugins

  unset github_token mcp_response mcp_request
  export CLAUDE_CODE_PLUGIN_SEED_DIR="$plugin_cache_dir"
fi

exec "$agent_bin" "$@"
