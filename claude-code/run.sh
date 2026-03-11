#!/bin/bash
set -e

# ==============================================================================
# Claude Code Add-on: Entrypoint
# Launches ttyd serving Claude CLI on the ingress port
# ==============================================================================

# Read addon options
OPTIONS_FILE="/data/options.json"
API_KEY=""
if [ -f "${OPTIONS_FILE}" ]; then
    API_KEY=$(jq -r '.anthropic_api_key // empty' "${OPTIONS_FILE}")
fi

# Set API key if provided
if [ -n "${API_KEY}" ]; then
    export ANTHROPIC_API_KEY="${API_KEY}"
    echo "Anthropic API key configured from addon options."
else
    echo "No API key set. Claude will use OAuth or prompt for authentication."
fi

# Set HOME for Claude CLI config persistence
export HOME="/data"
mkdir -p "${HOME}/.claude"

# Expose Supervisor API token for HA API calls
export SUPERVISOR_TOKEN="${SUPERVISOR_TOKEN}"

# Working directory
cd /config || exit 1

# Read ingress port and entry from supervisor
INGRESS_PORT="${INGRESS_PORT:-7681}"
INGRESS_ENTRY="${INGRESS_ENTRY:-/}"

echo "Starting Claude Code on port ${INGRESS_PORT} with base path ${INGRESS_ENTRY}..."

# Launch ttyd with Claude CLI
exec ttyd \
    --writable \
    --port "${INGRESS_PORT}" \
    --base-path "${INGRESS_ENTRY}" \
    --ping-interval 30 \
    --max-clients 3 \
    --title-fixed "Claude Code" \
    bash -c 'echo "Welcome to Claude Code for Home Assistant"; echo "Working directory: $(pwd)"; echo "---"; exec claude'
