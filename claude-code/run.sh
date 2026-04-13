#!/bin/bash
set -e

# ==============================================================================
# Claude Code Add-on v2.0: Entrypoint
# Launches claude-bridge (chat UI + subprocess manager + auth keepalive)
# ==============================================================================

# Read addon options
OPTIONS_FILE="/data/options.json"
API_KEY=""
SYSTEM_PROMPT=""
SKIP_PERMISSIONS=false
AUTH_KEEPALIVE_HOURS=4
if [ -f "${OPTIONS_FILE}" ]; then
    API_KEY=$(jq -r '.anthropic_api_key // empty' "${OPTIONS_FILE}")
    SYSTEM_PROMPT=$(jq -r '.system_prompt // empty' "${OPTIONS_FILE}")
    SKIP_PERMISSIONS=$(jq -r '.dangerously_skip_permissions // false' "${OPTIONS_FILE}")
    AUTH_KEEPALIVE_HOURS=$(jq -r '.auth_keepalive_hours // 4' "${OPTIONS_FILE}")
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
# Ensure .claude.json exists (Claude Code errors out if it's missing after an update)
if [ ! -f "${HOME}/.claude.json" ]; then
    echo '{}' > "${HOME}/.claude.json"
fi

# Ensure claude binary is on PATH
export PATH="/root/.local/bin:$PATH"
export USE_BUILTIN_RIPGREP=0

# Expose Supervisor API token for HA API calls
export SUPERVISOR_TOKEN="${SUPERVISOR_TOKEN}"

# Working directory
cd /config || exit 1

# Read ingress port and entry from supervisor
export INGRESS_PORT="${INGRESS_PORT:-7681}"
export INGRESS_ENTRY="${INGRESS_ENTRY:-/}"

echo "Starting Claude Code v2.0 on port ${INGRESS_PORT} with base path ${INGRESS_ENTRY}..."

# Export bridge config via environment
export CLAUDE_BIN="claude"
export CLAUDE_WORKDIR="/config"
export AUTH_KEEPALIVE_HOURS="${AUTH_KEEPALIVE_HOURS}"

# System prompt
if [ -n "${SYSTEM_PROMPT}" ]; then
    export CLAUDE_SYSTEM_PROMPT="${SYSTEM_PROMPT}"
    echo "System prompt configured from addon options."
fi

if [ "${SKIP_PERMISSIONS}" = "true" ]; then
    # Pre-accept the dangerous mode prompt
    SETTINGS_FILE="${HOME}/.claude/settings.json"
    if [ -f "${SETTINGS_FILE}" ]; then
        jq '.skipDangerousModePermissionPrompt = true' "${SETTINGS_FILE}" > "${SETTINGS_FILE}.tmp" \
            && mv "${SETTINGS_FILE}.tmp" "${SETTINGS_FILE}"
    else
        echo '{"skipDangerousModePermissionPrompt":true}' > "${SETTINGS_FILE}"
    fi
    echo "WARNING: Running Claude with --dangerously-skip-permissions. All permission prompts are disabled."

    # Drop to non-root user via gosu (--dangerously-skip-permissions is blocked as root)
    # Point claude user's home to /data; symlink .claude/.ssh back to /data for persistence
    usermod -d /data claude 2>/dev/null || true
    for dir in .claude .ssh; do
        mkdir -p "/data/${dir}"
        if [ -d "/home/claude/${dir}" ] && [ ! -L "/home/claude/${dir}" ]; then
            cp -a "/home/claude/${dir}/." "/data/${dir}/" 2>/dev/null || true
            rm -rf "/home/claude/${dir}"
        fi
        ln -sfn "/data/${dir}" "/home/claude/${dir}"
        chown -R claude:claude "/data/${dir}"
    done
    # Symlink .claude.json so Claude Code finds it regardless of home resolution
    ln -sf /data/.claude.json /home/claude/.claude.json 2>/dev/null || true
    chown claude:claude /data/.claude.json
    chown -R claude:claude /home/claude

    export CLAUDE_USE_GOSU=1
    export CLAUDE_SKIP_PERMS=1
else
    export CLAUDE_USE_GOSU=0
    export CLAUDE_SKIP_PERMS=0
fi

# Prevent nested invocation detection
unset CLAUDECODE
unset CLAUDE_CODE_ENTRYPOINT

# Launch claude-bridge
exec claude-bridge
