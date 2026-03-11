#!/usr/bin/with-contenv bashio
# shellcheck shell=bash

# ==============================================================================
# Claude Code Add-on: Entrypoint
# Launches ttyd serving Claude CLI on the ingress port
# ==============================================================================

declare INGRESS_PORT
declare API_KEY

# Read ingress port from supervisor
INGRESS_PORT=$(bashio::addon.ingress_port)
bashio::log.info "Starting Claude Code on ingress port ${INGRESS_PORT}..."

# Set API key from addon options if provided
API_KEY=$(bashio::config 'anthropic_api_key' '')
if bashio::var.has_value "${API_KEY}"; then
    export ANTHROPIC_API_KEY="${API_KEY}"
    bashio::log.info "Anthropic API key configured from addon options."
else
    bashio::log.info "No API key set in options. Claude will use OAuth or prompt for authentication."
fi

# Set HOME so Claude CLI can persist config/auth
export HOME="/data"

# Ensure Claude config directory exists
mkdir -p "${HOME}/.claude"

# Expose Supervisor API token so Claude can call HA/Supervisor REST APIs
# e.g. curl -H "Authorization: Bearer ${SUPERVISOR_TOKEN}" http://supervisor/core/api/services
export SUPERVISOR_TOKEN="${SUPERVISOR_TOKEN}"

# Set the working directory to HA config
cd /config || exit 1

# Launch ttyd with Claude CLI
# --writable: allow input
# --port: bind to ingress port
# --base-path: set base path for ingress compatibility
# --credential: no auth needed, HA ingress handles it
exec ttyd \
    --writable \
    --port "${INGRESS_PORT}" \
    --base-path "$(bashio::addon.ingress_entry)" \
    --ping-interval 30 \
    --max-clients 3 \
    --title-fixed "Claude Code" \
    bash -c 'echo "Welcome to Claude Code for Home Assistant"; echo "Working directory: $(pwd)"; echo "---"; exec claude'
