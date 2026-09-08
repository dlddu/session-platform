#!/bin/sh
set -eu

agent_bin="${DATA_PLANE_AGENT_BIN:-/agent}"
workload="${DATA_PLANE_WORKLOAD:-shell}"

plugin_slug="session-platform@dlddu-plugins"
plugin_cache_dir="${CLAUDE_CODE_PLUGIN_CACHE_DIR:-/tmp/session-platform-claude-plugin-seed}"
bootstrap_home="${CLAUDE_CODE_PLUGIN_BOOTSTRAP_HOME:-/tmp/session-platform-claude-plugin-home}"
k3s_mcp_url="${K3S_MCP_URL:-https://homelab-k3s-mcp.llkm.nl/mcp}"
# The marketplace is a plain git remote, so the platform can point this at any
# host that serves the same repository shape — production keeps github.com and
# the kind e2e SUT points it at an in-cluster remote (deploy/). Both run the
# identical `claude plugin marketplace add` code path.
marketplace_url="${CLAUDE_CODE_PLUGIN_MARKETPLACE_URL:-https://github.com/dlddu/plugin-marketplace.git}"
shared_dir="${SESSION_SHARED_DIR:-/shared}"
# The seeded marketplace an approval-gated workload installs from: a git working
# tree on AC-F5's volume. Non-bare so that it serves both readings of a local
# marketplace argument — cloned as a remote, or read in place as a directory.
seed_marketplace="$shared_dir/plugin-marketplace"
seed_staging="$shared_dir/.plugin-marketplace.staging"

# mint_github_token exchanges the platform's K3s MCP token for a short-lived,
# marketplace-scoped GitHub token and prints it.
mint_github_token() {
  : "${K3S_MCP_TOKEN:?K3S_MCP_TOKEN is required for Claude plugin bootstrap}"
  mcp_request='{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"github_app_installation_token","arguments":{"repositories":["plugin-marketplace"],"permissions":{"contents":"read"}}}}'
  curl --fail --silent --show-error \
    --connect-timeout 10 --max-time 30 \
    --header "Authorization: Bearer ${K3S_MCP_TOKEN}" \
    --header "Content-Type: application/json" \
    --header "Accept: application/json" \
    --data "$mcp_request" \
    "$k3s_mcp_url" |
    jq -er '
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
}

# install_plugin runs the marketplace registration and install against the
# remote in $1, authenticating with the Git header in $2 when it is non-empty.
#
# Claude Code clears GIT_ASKPASS before it spawns Git. A process-scoped Git
# config survives that sanitization without persisting the short-lived token.
# The extraheader is scoped to the marketplace URL itself, so pointing the URL
# elsewhere moves the scope with it and never widens it.
install_plugin() {
  install_url="$1"
  install_auth="${2:-}"

  mkdir -p "$plugin_cache_dir" "$bootstrap_home"
  chmod 700 "$plugin_cache_dir" "$bootstrap_home"

  if [ -n "$install_auth" ]; then
    set -- \
      GIT_CONFIG_COUNT=1 \
      "GIT_CONFIG_KEY_0=http.${install_url}.extraheader" \
      "GIT_CONFIG_VALUE_0=Authorization: ${install_auth}"
  else
    set -- GIT_CONFIG_COUNT=0
  fi

  env HOME="$bootstrap_home" \
    CLAUDE_CODE_PLUGIN_CACHE_DIR="$plugin_cache_dir" \
    GIT_TERMINAL_PROMPT=0 \
    "$@" \
    claude plugin marketplace add "$install_url"
  env HOME="$bootstrap_home" \
    CLAUDE_CODE_PLUGIN_CACHE_DIR="$plugin_cache_dir" \
    GIT_TERMINAL_PROMPT=0 \
    "$@" \
    claude plugin install "$plugin_slug"

  # The agent must resolve the plugin from the very same cache the bootstrap
  # populated. A seed dir only syncs marketplace registrations, and only after
  # plugin loading has already run, so the installed plugin never resolves.
  export CLAUDE_CODE_PLUGIN_CACHE_DIR="$plugin_cache_dir"
}

# seed_plugin_marketplace publishes a skills-only copy of the marketplace onto
# the shared volume for this session's workload pod to install from (AC-F6).
#
# It runs in the helper pod's MCP container *before* that container opens its
# port, and the control plane creates the workload pod only after the helper
# reports Ready — so the seed is complete before anything can read it, without
# the two pods having to coordinate.
#
# Stripping the MCP servers is the point, not a tidy-up: a plugin-declared MCP
# server is a tool surface that never passes the approval gate (AC-F3), so the
# seed carries the plugin's skills and nothing that can leave the pod.
seed_plugin_marketplace() {
  github_token="$(mint_github_token)"

  rm -rf "$seed_staging"
  git -c "http.${marketplace_url}.extraheader=Authorization: Basic $(
    printf '%s' "x-access-token:${github_token}" | base64 | tr -d '\n'
  )" \
    -c credential.helper= \
    clone --quiet --depth 1 "$marketplace_url" "$seed_staging"
  unset github_token

  rm -rf "$seed_staging/mcp"
  jq 'del(.plugins[].mcpServers)' \
    "$seed_staging/.claude-plugin/marketplace.json" >"$seed_staging/.claude-plugin/marketplace.json.stripped"
  mv "$seed_staging/.claude-plugin/marketplace.json.stripped" \
    "$seed_staging/.claude-plugin/marketplace.json"

  # The seed is only worth publishing if the strip actually held. Failing here
  # leaves no marketplace on the volume at all, which the workload reads as
  # "no plugin" rather than as an ungated one.
  if jq -e '[.plugins[].mcpServers] | any' \
    "$seed_staging/.claude-plugin/marketplace.json" >/dev/null; then
    echo "seeded marketplace still declares an MCP server; refusing to publish" >&2
    rm -rf "$seed_staging"
    exit 1
  fi

  git -C "$seed_staging" -c user.name='session-platform' \
    -c user.email='session-platform@invalid' \
    -c commit.gpgsign=false \
    commit --quiet --all --message 'strip MCP servers for the approval gate'

  rm -rf "$seed_marketplace"
  mv "$seed_staging" "$seed_marketplace"
}

case "$workload" in
claude-code)
  echo "Bootstrapping Claude plugin marketplace" >&2
  github_token="$(mint_github_token)"
  install_plugin "$marketplace_url" "Basic $(
    printf '%s' "x-access-token:${github_token}" | base64 | tr -d '\n'
  )"
  unset github_token
  ;;
approval-gated)
  # No plugin without AC-F5's volume: the seed has nowhere to live, and this
  # type has no egress that could reach the marketplace itself (AC-F2).
  if [ -d "$seed_marketplace" ]; then
    echo "Installing the seeded Claude plugin marketplace" >&2
    install_plugin "$seed_marketplace"
    export CLAUDE_CODE_PLUGIN_ENABLED=1
  else
    echo "No seeded plugin marketplace at $seed_marketplace; continuing without the plugin" >&2
  fi
  ;;
mcp)
  # Both conditions are opt-ins a deployment can leave off, and either one off
  # means no seed and an approval-gated session shaped exactly as before.
  if [ -d "$shared_dir" ] && [ -n "${K3S_MCP_TOKEN:-}" ]; then
    echo "Seeding the Claude plugin marketplace for this session" >&2
    seed_plugin_marketplace
  fi
  ;;
esac

exec "$agent_bin" "$@"
