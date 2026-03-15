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

# Ensure claude binary is on PATH (use shared location accessible to non-root user)
export PATH="/usr/local/share/claude-bin:$PATH"
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
tmux kill-server 2>/dev/null || true

# Grant the claude user access to necessary directories
chown -R claude:claude /data/.claude
chmod -R 755 /config
chown -R claude:claude /home/claude

# Create a wrapper script that runs as the non-root 'claude' user via tmux
cat > /tmp/claude-tmux.sh << 'WRAPPER'
#!/bin/bash
export HOME="/data"
export PATH="/usr/local/share/claude-bin:$PATH"
export USE_BUILTIN_RIPGREP=0
export TERM=xterm-256color

# Prevent Claude Code from detecting a "nested" invocation
unset CLAUDECODE
unset CLAUDE_CODE_ENTRYPOINT

cd /config

SESSION="claude"

# --- Debug: test claude as non-root user ---
echo "=== CLAUDE-DEBUG $(date) ===" >&2
echo "whoami: $(whoami)" >&2
echo "Contents of /usr/local/share/claude-bin:" >&2
ls -la /usr/local/share/claude-bin/ >&2 || echo "DIR NOT FOUND" >&2
echo "Contents of /root/.local/bin:" >&2
ls -la /root/.local/bin/ >&2 || echo "DIR NOT FOUND" >&2
echo "File type of claude:" >&2
file /root/.local/bin/claude >&2 || true
echo "---" >&2
echo "Testing claude as claude user..." >&2
su -s /bin/bash -c 'export HOME=/data && export PATH=/usr/local/share/claude-bin:$PATH && echo "running as: $(whoami)" && echo "PATH: $PATH" && which claude 2>&1 && claude --version 2>&1 && claude --dangerously-skip-permissions -p "say hello" 2>&1' claude 2>&1 | tee /dev/stderr || true
echo "--- EXIT CODE: $? ---" >&2
echo "Debug complete. Press enter to continue..."
read
# --- End debug ---
WRAPPER
chmod +x /tmp/claude-tmux.sh

# Belt-and-suspenders: unset nesting-detection vars
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
