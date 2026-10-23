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

if [ "$workload" = "minimax-code" ]; then
  # minimax-code never holds the K3s MCP token (AC-G3). Its sidecar does, and
  # every K3s MCP call here goes through that loopback proxy (AC-G5).
  k3s_proxy_url="${K3S_MCP_PROXY_URL:-http://127.0.0.1:8093}"
  k3s_mcp_url="${K3S_MCP_URL:-https://homelab-k3s-mcp.llkm.nl/mcp}"
  marketplace_url="${MINIMAX_CODE_PLUGIN_MARKETPLACE_URL:-https://github.com/dlddu/plugin-marketplace.git}"
  plugin_name="${MINIMAX_CODE_PLUGIN:-mcp-servers}"
  plugin_seed_dir="${MINIMAX_CODE_PLUGIN_SEED_DIR:-/tmp/session-platform-minimax-plugin-seed}"

  # The sidecar may still be starting; a refused connection is retried.
  proxy_health="$(
    curl --fail --silent --show-error \
      --retry 30 --retry-delay 1 --retry-connrefused \
      --connect-timeout 2 --max-time 5 \
      "${k3s_proxy_url}/healthz"
  )"
  if [ "$(printf '%s' "$proxy_health" | jq -r '.configured')" != "true" ]; then
    echo "K3s MCP proxy has no token; skipping MiniMax plugin bootstrap" >&2
    unset MINIMAX_CODE_PLUGIN_SEED_DIR
  else
    k3s_mcp_path="$(printf '%s' "$k3s_mcp_url" | sed -E 's#^[A-Za-z][A-Za-z0-9+.-]*://[^/]*##')"
    loopback_mcp_url="${k3s_proxy_url}${k3s_mcp_path}"

    mcp_request='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_app_installation_token","arguments":{"repositories":["plugin-marketplace"],"permissions":{"contents":"read"}}}}'
    mcp_response="$(
      curl --fail --silent --show-error \
        --connect-timeout 10 --max-time 30 \
        --header "Content-Type: application/json" \
        --header "Accept: application/json" \
        --data "$mcp_request" \
        "$loopback_mcp_url"
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

    echo "Bootstrapping MiniMax plugin ${plugin_name}" >&2
    marketplace_clone="$(mktemp -d)"
    GIT_CONFIG_COUNT=1 \
    GIT_CONFIG_KEY_0="http.${marketplace_url}.extraheader" \
    GIT_CONFIG_VALUE_0="Authorization: ${github_auth_header}" \
    GIT_TERMINAL_PROMPT=0 \
      git clone --quiet --depth 1 "$marketplace_url" "$marketplace_clone"
    unset github_auth_header github_token mcp_response mcp_request

    # mcode reads Claude-format plugins from ~/.minimax/plugins, but rejects
    # MCP OAuth fields and does not expand ${VAR} in headers. The seed is the
    # marketplace plugin rewritten for that: its K3s MCP server points at the
    # loopback proxy and carries no auth of its own. Any other server is
    # refused rather than silently routed with a token that is not its own.
    plugin_entry="$(
      jq -ec --arg name "$plugin_name" '
        [.plugins[]? | select(.name == $name)]
        | if length == 1 then .[0] else error("marketplace has no single plugin named " + $name) end
      ' "$marketplace_clone/.claude-plugin/marketplace.json"
    )"
    plugin_source="$(printf '%s' "$plugin_entry" | jq -er '.source | strings')"
    plugin_servers="$(
      printf '%s' "$plugin_entry" | jq -ec '
        .mcpServers
        | if type == "string" then [.] elif type == "array" then . else error("mcpServers must be file paths") end
        | if length > 0 and all(.[]; type == "string") then . else error("mcpServers must be file paths") end
      '
    )"
    rm -rf "$plugin_seed_dir"
    mkdir -p "$plugin_seed_dir/$plugin_name/.claude-plugin"
    chmod 700 "$plugin_seed_dir"
    printf '%s' "$plugin_servers" | jq -r '.[]' | while IFS= read -r server_file; do
      case "$plugin_source/$server_file" in
        /* | *..*)
          echo "MiniMax plugin MCP path escapes the marketplace: $server_file" >&2
          exit 1
          ;;
      esac
      mkdir -p "$(dirname "$plugin_seed_dir/$plugin_name/$server_file")"
      jq --arg upstream "$k3s_mcp_url" --arg loopback "$loopback_mcp_url" '
        .mcpServers |= with_entries(
          if (.value.url // "") == $upstream then
            .value |= (
              del(.oauthClientId)
              | .url = $loopback
              | if .headers then .headers |= with_entries(select((.key | ascii_downcase) != "authorization")) else . end
              | if (.headers // {}) == {} then del(.headers) else . end
            )
          else
            error("MCP server " + .key + " is not the K3s MCP the proxy fronts")
          end
        )
      ' "$marketplace_clone/$plugin_source/$server_file" > "$plugin_seed_dir/$plugin_name/$server_file"
    done
    printf '%s' "$plugin_entry" | jq --argjson servers "$plugin_servers" '{
      name,
      description: (.description // ""),
      version: (.version // "1.0.0"),
      author: (.author // {}),
      mcpServers: $servers
    }' > "$plugin_seed_dir/$plugin_name/.claude-plugin/plugin.json"
    rm -rf "$marketplace_clone"
    export MINIMAX_CODE_PLUGIN_SEED_DIR="$plugin_seed_dir"
  fi
fi

exec "$agent_bin" "$@"
