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

# Build claude flags
export CLAUDE_EXTRA_FLAGS=""
if [ "${SKIP_PERMISSIONS}" = "true" ]; then
    export CLAUDE_EXTRA_FLAGS="--dangerously-skip-permissions"
    # Pre-accept the dangerous mode prompt so claude doesn't exit waiting for input
    SETTINGS_FILE="${HOME}/.claude/settings.json"
    if [ -f "${SETTINGS_FILE}" ]; then
        jq '.skipDangerousModePermissionPrompt = true' "${SETTINGS_FILE}" > "${SETTINGS_FILE}.tmp" \
            && mv "${SETTINGS_FILE}.tmp" "${SETTINGS_FILE}"
    else
        echo '{"skipDangerousModePermissionPrompt":true}' > "${SETTINGS_FILE}"
    fi
    echo "WARNING: Running Claude with --dangerously-skip-permissions. All permission prompts are disabled."
fi

# Kill stale tmux sessions to ensure a clean process tree
# (prevents inherited CLAUDECODE env vars from surviving addon restarts)
tmux kill-server 2>/dev/null || true

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

# --- Debug: dump env and test claude ---
LOG="/data/claude-debug.log"
echo "=== $(date) ===" >> "$LOG"
echo "ENV:" >> "$LOG"
env | sort >> "$LOG"
echo "---" >> "$LOG"
echo "which claude: $(which claude 2>&1)" >> "$LOG"
echo "claude version: $(claude --version 2>&1)" >> "$LOG"
echo "CLAUDE_EXTRA_FLAGS: '$CLAUDE_EXTRA_FLAGS'" >> "$LOG"
echo "---" >> "$LOG"
# --- End debug ---

SESSION="claude"

# If tmux session exists, attach to it; otherwise create one running claude
if tmux has-session -t "$SESSION" 2>/dev/null; then
    exec tmux attach-session -t "$SESSION"
else
    # shellcheck disable=SC2086
    exec tmux new-session -s "$SESSION" "claude $CLAUDE_EXTRA_FLAGS"
fi
WRAPPER
chmod +x /tmp/claude-tmux.sh

# Belt-and-suspenders: also unset nesting-detection vars in the parent process
unset CLAUDECODE
unset CLAUDE_CODE_ENTRYPOINT

# Launch ttyd with the tmux wrapper
exec ttyd \
    --writable \
    --port "${INGRESS_PORT}" \
    --base-path "${INGRESS_ENTRY}" \
    --ping-interval 30 \
    --max-clients 0 \
    /tmp/claude-tmux.sh
