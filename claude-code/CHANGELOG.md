# Changelog

## 2.0.6

- **Persist session message history**: Chat messages now survive page reloads, navigation, and device switches. The bridge records all conversation events in memory and replays them to new WebSocket clients on connect. History resets when the session is cleared or the addon restarts.

## 2.0.5

- **Implement full OAuth PKCE flow in bridge**: Rather than wrapping `claude auth login` (which uses a local callback server with a mismatched redirect_uri), the bridge now handles the entire OAuth flow itself: generates PKCE code_verifier/challenge, builds the auth URL, and exchanges the code directly at Anthropic's token endpoint. This eliminates the redirect_uri mismatch that caused 400 errors on token exchange.

## 2.0.4

- **Fix auth prompt not appearing**: Auth status is now checked per-client on WebSocket connect instead of broadcasting once on startup (which raced against client connections and was lost)

## 2.0.3

- **In-app OAuth login flow**: When no credentials are found, the chat UI shows a Login button. Clicking it spawns `claude auth login`, extracts the OAuth URL, and presents it as a clickable link. The user authenticates in their browser, copies the authorization code, and pastes it back into the chat UI to complete the flow. No terminal access or API key required.
- Login button also available in the header bar for re-authentication

## 2.0.2

- **Fix "No conversation found with session ID" crash**: The bridge was generating a fake session ID and always passing `--resume`, but Claude Code rejects `--resume` with an ID it never created. Now the first message starts a fresh session (no `--resume`), captures the real session ID from Claude's `init` event, and uses that for subsequent `--resume` calls. If a saved session becomes stale (e.g. after addon rebuild), the bridge auto-retries without `--resume`.

## 2.0.1

- **Fix silent failures**: Capture Claude subprocess stderr and forward errors to the chat UI instead of silently swallowing them
- Show error message when Claude exits with non-zero status or produces no output

## 2.0.0

- **Breaking: Replace ttyd terminal with chat UI**
  - New Claudio-style chat interface (textarea input + markdown-rendered output)
  - Fixes Android IME input corruption (no more xterm.js)
  - Fixes linebreak rendering issues through HA ingress
  - Uses Claude Code's `--output-format stream-json` for structured event streaming
  - Tool calls displayed as collapsible summaries
  - Real-time streaming with visual cursor
  - Interrupt button to stop Claude mid-response
- **Auth keepalive**: Background process refreshes OAuth tokens before they expire
  - Checks every 30 minutes, refreshes when token is within 2h of expiry
  - Configurable via `auth_keepalive_hours` option (default 4, set to 0 to disable)
  - Only active for OAuth authentication (not API key)
- **New option**: `auth_keepalive_hours` — controls how aggressively to keep OAuth tokens alive
- Remove ttyd and tmux dependencies
- Add Go-based bridge binary (claude-bridge) for HTTP/WebSocket serving
- Multi-turn conversations via `--resume` with persistent session ID

## 1.4.1

- Fix gosu resetting HOME, causing .claude and .ssh data to be lost on reboot
- Symlink /home/claude/.claude and .ssh to persistent /data/ storage

## 1.4.0

- Add configurable system prompt option (appended to every new Claude session)
- Refactor launcher into separate script for cleaner argument handling

## 1.0.0

- Initial release
- Claude Code CLI bundled with web terminal (ttyd)
- Home Assistant sidebar integration via ingress
- API key and OAuth authentication support
- Access to HA config directory
- amd64 and aarch64 architecture support
