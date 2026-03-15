#!/bin/bash
set -e

# ==============================================================================
# Claude Code Add-on: Entrypoint
# Launches ttyd serving Claude CLI via tmux for session persistence
# ==============================================================================

# Read addon options
OPTIONS_FILE="/data/options.json"
API_KEY=""
SKIP_PERMISSIONS=false
if [ -f "${OPTIONS_FILE}" ]; then
    API_KEY=$(jq -r '.anthropic_api_key // empty' "${OPTIONS_FILE}")
    SKIP_PERMISSIONS=$(jq -r '.dangerously_skip_permissions // false' "${OPTIONS_FILE}")
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

# Ensure claude binary is on PATH
export PATH="/root/.local/bin:$PATH"
export USE_BUILTIN_RIPGREP=0

# Expose Supervisor API token for HA API calls
export SUPERVISOR_TOKEN="${SUPERVISOR_TOKEN}"

# Working directory
cd /config || exit 1

# Read ingress port and entry from supervisor
INGRESS_PORT="${INGRESS_PORT:-7681}"
INGRESS_ENTRY="${INGRESS_ENTRY:-/}"

echo "Starting Claude Code on port ${INGRESS_PORT} with base path ${INGRESS_ENTRY}..."

# Build claude command
CLAUDE_CMD="claude"
if [ "${SKIP_PERMISSIONS}" = "true" ]; then
    CLAUDE_CMD="claude --dangerously-skip-permissions"
    # Pre-accept the dangerous mode prompt so claude doesn't exit waiting for input
    SETTINGS_FILE="${HOME}/.claude/settings.json"
    if [ -f "${SETTINGS_FILE}" ]; then
        jq '.skipDangerousModePermissionPrompt = true' "${SETTINGS_FILE}" > "${SETTINGS_FILE}.tmp" \
            && mv "${SETTINGS_FILE}.tmp" "${SETTINGS_FILE}"
    else
        echo '{"skipDangerousModePermissionPrompt":true}' > "${SETTINGS_FILE}"
    fi
    echo "WARNING: Running Claude with --dangerously-skip-permissions. All permission prompts are disabled."

    # --dangerously-skip-permissions is blocked as root; drop to non-root user via gosu
    # Grant claude user access to necessary directories
    chown -R claude:claude /data/.claude
    chown -R claude:claude /home/claude

    export CLAUDE_USE_GOSU=1
else
    export CLAUDE_USE_GOSU=0
fi

# Kill stale tmux sessions to ensure a clean process tree
tmux kill-server 2>/dev/null || true

# Prevent Claude Code from detecting a "nested" invocation
unset CLAUDECODE
unset CLAUDE_CODE_ENTRYPOINT

# Export the claude command for use in the wrapper
export CLAUDE_CMD

# Create a wrapper script that attaches to or creates a tmux session
cat > /tmp/claude-tmux.sh << 'WRAPPER'
#!/bin/bash
export HOME="/data"
export PATH="/root/.local/bin:$PATH"
export USE_BUILTIN_RIPGREP=0
export TERM=xterm-256color

# Prevent Claude Code from detecting a "nested" invocation
unset CLAUDECODE
unset CLAUDE_CODE_ENTRYPOINT

cd /config

SESSION="claude"

if tmux has-session -t "$SESSION" 2>/dev/null; then
    exec tmux attach-session -t "$SESSION"
else
    if [ "$CLAUDE_USE_GOSU" = "1" ]; then
        # Drop privileges for --dangerously-skip-permissions
        exec tmux new-session -s "$SESSION" "gosu claude $CLAUDE_CMD"
    else
        exec tmux new-session -s "$SESSION" "$CLAUDE_CMD"
    fi
fi
WRAPPER
chmod +x /tmp/claude-tmux.sh

# Launch ttyd with the tmux wrapper
exec ttyd \
    --writable \
    --port "${INGRESS_PORT}" \
    --base-path "${INGRESS_ENTRY}" \
    --ping-interval 30 \
    --max-clients 0 \
    /tmp/claude-tmux.sh
