package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// Config holds addon configuration read from environment.
type Config struct {
	Port           int
	BasePath       string
	ClaudeBin      string
	WorkDir        string
	HomeDir        string
	SkipPerms      bool
	UseGosu        bool
	SystemPrompt   string
	KeepaliveHours int
	APIKeySet      bool
}

// Bridge manages the Claude subprocess and WebSocket clients.
type Bridge struct {
	cfg       Config
	sessionID string

	mu          sync.Mutex
	proc        *exec.Cmd
	stdin       io.WriteCloser
	working     bool
	clients     map[*websocket.Conn]bool
	history     []json.RawMessage // messages to replay on reconnect
	historyPath string            // path to persisted history file

	// Auth flow state
	authMu           sync.Mutex
	authPending      bool   // true while waiting for user to paste code
	authState        string // OAuth state parameter
	authCodeVerifier string // PKCE code_verifier
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

func main() {
	cfg := loadConfig()

	b := &Bridge{
		cfg:         cfg,
		clients:     make(map[*websocket.Conn]bool),
		historyPath: filepath.Join(cfg.HomeDir, ".claude-bridge-history.json"),
	}

	// Load saved session ID (may be empty if first run)
	b.sessionID = b.loadSessionID()

	// Load persisted chat history
	b.history = b.loadHistory()

	// Auth keepalive
	if !cfg.APIKeySet && cfg.KeepaliveHours > 0 {
		go b.authKeepalive()
	}

	// HTTP routes
	mux := http.NewServeMux()
	base := strings.TrimRight(cfg.BasePath, "/")

	// Serve static files
	staticDir := findStaticDir()
	fs := http.FileServer(http.Dir(staticDir))
	mux.HandleFunc(base+"/ws", b.handleWebSocket)
	mux.HandleFunc(base+"/interrupt", b.handleInterrupt)
	mux.HandleFunc(base+"/health", b.handleHealth)
	mux.HandleFunc(base+"/new-session", b.handleNewSession)
	mux.HandleFunc(base+"/api/prompt", b.handleAPIPrompt)
	mux.Handle(base+"/static/", http.StripPrefix(base+"/static/", fs))
	mux.HandleFunc(base+"/", b.handleIndex(staticDir))

	// Graceful shutdown
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("Shutting down...")
		b.killProc()
		os.Exit(0)
	}()

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Printf("claude-bridge listening on %s (base=%s)", addr, base)
	log.Fatal(http.ListenAndServe(addr, mux))
}

func loadConfig() Config {
	port, _ := strconv.Atoi(envOr("INGRESS_PORT", "7681"))
	keepalive, _ := strconv.Atoi(envOr("AUTH_KEEPALIVE_HOURS", "4"))
	return Config{
		Port:           port,
		BasePath:       envOr("INGRESS_ENTRY", "/"),
		ClaudeBin:      envOr("CLAUDE_BIN", "claude"),
		WorkDir:        envOr("CLAUDE_WORKDIR", "/config"),
		HomeDir:        envOr("HOME", "/data"),
		SkipPerms:      envOr("CLAUDE_SKIP_PERMS", "0") == "1",
		UseGosu:        envOr("CLAUDE_USE_GOSU", "0") == "1",
		SystemPrompt:   envOr("CLAUDE_SYSTEM_PROMPT", ""),
		KeepaliveHours: keepalive,
		APIKeySet:      envOr("ANTHROPIC_API_KEY", "") != "",
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func findStaticDir() string {
	// Check next to binary first, then /usr/local/share/claude-bridge/static
	exe, _ := os.Executable()
	dir := filepath.Join(filepath.Dir(exe), "static")
	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir
	}
	return "/usr/local/share/claude-bridge/static"
}

func (b *Bridge) loadSessionID() string {
	path := filepath.Join(b.cfg.HomeDir, ".claude-bridge-session")
	data, err := os.ReadFile(path)
	if err == nil {
		id := strings.TrimSpace(string(data))
		if id != "" {
			log.Printf("Found saved session: %s", id)
			return id
		}
	}
	log.Printf("No saved session found, will start fresh")
	return ""
}

func (b *Bridge) saveSessionID(id string) {
	path := filepath.Join(b.cfg.HomeDir, ".claude-bridge-session")
	if err := os.WriteFile(path, []byte(id), 0644); err != nil {
		log.Printf("Failed to save session ID: %v", err)
	} else {
		log.Printf("Saved session ID: %s", id)
	}
}

func (b *Bridge) clearSessionID() {
	path := filepath.Join(b.cfg.HomeDir, ".claude-bridge-session")
	os.Remove(path)
}

// loadHistory reads persisted chat history from disk.
func (b *Bridge) loadHistory() []json.RawMessage {
	data, err := os.ReadFile(b.historyPath)
	if err != nil {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		log.Printf("[history] parse error, starting fresh: %v", err)
		return nil
	}
	log.Printf("[history] Loaded %d entries from disk", len(entries))
	return entries
}

// saveHistoryLocked writes history to disk. Must be called with b.mu held.
func (b *Bridge) saveHistoryLocked() {
	data, err := json.Marshal(b.history)
	if err != nil {
		log.Printf("[history] marshal error: %v", err)
		return
	}
	if err := os.WriteFile(b.historyPath, data, 0644); err != nil {
		log.Printf("[history] write error: %v", err)
	}
}

// handleIndex serves the chat UI.
func (b *Bridge) handleIndex(staticDir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimRight(b.cfg.BasePath, "/")
		if r.URL.Path != base+"/" && r.URL.Path != base {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(staticDir, "index.html"))
	}
}

// handleHealth returns bridge status.
func (b *Bridge) handleHealth(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	working := b.working
	clients := len(b.clients)
	b.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"working":%t,"clients":%d,"session":"%s"}`, working, clients, b.sessionID)
}

// handleInterrupt sends SIGINT to the running subprocess.
func (b *Bridge) handleInterrupt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	b.mu.Lock()
	proc := b.proc
	b.mu.Unlock()
	if proc != nil && proc.Process != nil {
		proc.Process.Signal(syscall.SIGINT)
		log.Println("Sent SIGINT to claude subprocess")
		w.Write([]byte(`{"ok":true}`))
	} else {
		w.Write([]byte(`{"ok":false,"reason":"no process"}`))
	}
}

// handleNewSession resets to a fresh Claude conversation.
func (b *Bridge) handleNewSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	b.mu.Lock()
	// Kill current process if any
	if b.proc != nil && b.proc.Process != nil {
		b.proc.Process.Signal(syscall.SIGTERM)
	}
	b.working = false
	b.proc = nil
	b.stdin = nil
	b.sessionID = ""
	b.history = nil
	b.mu.Unlock()

	b.clearSessionID()
	os.Remove(b.historyPath)

	log.Printf("Session cleared by user request")

	// Notify all clients
	b.broadcast(map[string]interface{}{
		"type": "session_reset",
	})

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// handleAPIPrompt provides a synchronous HTTP API for running Claude prompts.
// Used by Autobots and other HA integrations. Returns the full text response.
func (b *Bridge) handleAPIPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Prompt       string `json:"prompt"`
		SystemPrompt string `json:"system_prompt,omitempty"`
		MaxTurns     int    `json:"max_turns,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}
	if req.Prompt == "" {
		http.Error(w, `{"error":"prompt is required"}`, http.StatusBadRequest)
		return
	}

	if !b.cfg.APIKeySet && !b.isAuthenticated() {
		http.Error(w, `{"error":"not authenticated"}`, http.StatusUnauthorized)
		return
	}

	// Build claude command for one-shot prompt
	args := []string{
		"--print",
		"--output-format", "json",
	}
	if b.cfg.SkipPerms {
		args = append(args, "--dangerously-skip-permissions")
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	} else if b.cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", b.cfg.SystemPrompt)
	}
	if req.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(req.MaxTurns))
	}
	args = append(args, req.Prompt)

	cmd := exec.Command(b.cfg.ClaudeBin, args...)
	cmd.Dir = b.cfg.WorkDir
	cmd.Env = append(os.Environ(),
		"HOME="+b.cfg.HomeDir,
		"TERM=dumb",
	)

	log.Printf("[api] Running prompt (%d chars)", len(req.Prompt))
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[api] Claude exited with error: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		errResp := map[string]string{
			"error":  "claude failed",
			"detail": string(output),
		}
		json.NewEncoder(w).Encode(errResp)
		return
	}

	// Parse the JSON output to extract the result text
	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		// If we can't parse as JSON, return raw output
		w.Header().Set("Content-Type", "application/json")
		resp := map[string]string{"text": string(output)}
		json.NewEncoder(w).Encode(resp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	resp := map[string]string{"text": result.Result}
	json.NewEncoder(w).Encode(resp)
	log.Printf("[api] Prompt completed (%d chars response)", len(result.Result))
}

// handleWebSocket upgrades to WS and relays messages.
func (b *Bridge) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS upgrade error: %v", err)
		return
	}
	defer conn.Close()

	b.mu.Lock()
	b.clients[conn] = true
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		delete(b.clients, conn)
		b.mu.Unlock()
	}()

	log.Printf("WS client connected (%d total)", len(b.clients))

	// Send current status
	b.mu.Lock()
	working := b.working
	b.mu.Unlock()
	conn.WriteJSON(map[string]interface{}{
		"type":    "bridge_status",
		"working": working,
		"session": b.sessionID,
	})

	// Replay message history so the client sees the full conversation
	b.replayHistory(conn)

	// If not authenticated, tell this client immediately
	if !b.cfg.APIKeySet && !b.isAuthenticated() {
		conn.WriteJSON(map[string]interface{}{
			"type":    "auth_required",
			"message": "Not authenticated. Click Login to sign in with your Claude account.",
		})
	}

	// Read messages from client
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("WS read error: %v", err)
			return
		}

		var envelope struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(msg, &envelope); err != nil {
			log.Printf("WS parse error: %v", err)
			continue
		}

		switch envelope.Type {
		case "user_message":
			go b.handleUserMessage(envelope.Message)
		case "interrupt":
			b.mu.Lock()
			proc := b.proc
			b.mu.Unlock()
			if proc != nil && proc.Process != nil {
				proc.Process.Signal(syscall.SIGINT)
			}
		case "auth_start":
			go b.handleAuthStart()
		case "auth_code":
			go b.handleAuthCode(envelope.Message)
		}
	}
}

// handleUserMessage spawns claude --print --resume and streams output.
func (b *Bridge) handleUserMessage(message string) {
	b.mu.Lock()
	if b.working {
		b.mu.Unlock()
		b.broadcast(map[string]interface{}{
			"type":    "bridge_error",
			"message": "Claude is still working. Use interrupt first.",
		})
		return
	}
	b.working = true
	b.mu.Unlock()

	// Notify clients
	b.broadcast(map[string]interface{}{
		"type":    "bridge_status",
		"working": true,
	})

	defer func() {
		b.mu.Lock()
		b.working = false
		b.proc = nil
		b.stdin = nil
		b.mu.Unlock()
		b.broadcast(map[string]interface{}{
			"type":    "bridge_status",
			"working": false,
		})
	}()

	// Echo user message to clients
	b.broadcast(map[string]interface{}{
		"type":    "user_message",
		"message": message,
	})

	// Build command
	args := []string{
		"--print",
		"--output-format", "stream-json",
		"--verbose",
	}
	b.mu.Lock()
	resumeID := b.sessionID
	b.mu.Unlock()
	if resumeID != "" {
		args = append(args, "--resume", resumeID)
	}
	if b.cfg.SkipPerms {
		args = append(args, "--dangerously-skip-permissions")
	}
	if b.cfg.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", b.cfg.SystemPrompt)
	}
	args = append(args, message)

	var cmd *exec.Cmd
	if b.cfg.UseGosu {
		gosuArgs := append([]string{"claude", "env", "HOME=" + b.cfg.HomeDir, b.cfg.ClaudeBin}, args...)
		cmd = exec.Command("gosu", gosuArgs...)
	} else {
		cmd = exec.Command(b.cfg.ClaudeBin, args...)
	}
	cmd.Dir = b.cfg.WorkDir
	cmd.Env = append(os.Environ(),
		"HOME="+b.cfg.HomeDir,
		"TERM=dumb",
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("stdout pipe error: %v", err)
		b.broadcast(map[string]interface{}{"type": "bridge_error", "message": err.Error()})
		return
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	b.mu.Lock()
	b.proc = cmd
	b.mu.Unlock()

	if err := cmd.Start(); err != nil {
		log.Printf("claude start error: %v", err)
		b.broadcast(map[string]interface{}{"type": "bridge_error", "message": err.Error()})
		return
	}

	log.Printf("Spawned claude (PID %d) for session %s", cmd.Process.Pid, b.sessionID)

	// Stream stdout line-by-line — each line is a JSON event
	gotEvents := false
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024) // 1MB buffer for large events
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		// Parse to validate JSON, then forward raw
		var event map[string]interface{}
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			log.Printf("Non-JSON line from claude: %s", line)
			continue
		}
		gotEvents = true

		// Capture the real session ID from the init event
		if eventType, _ := event["type"].(string); eventType == "system" {
			if sid, ok := event["session_id"].(string); ok && sid != "" {
				b.mu.Lock()
				b.sessionID = sid
				b.mu.Unlock()
				b.saveSessionID(sid)
			}
		}

		// Wrap in a bridge envelope
		b.broadcast(map[string]interface{}{
			"type":  "claude_event",
			"event": event,
		})
	}

	waitErr := cmd.Wait()
	errMsg := strings.TrimSpace(stderrBuf.String())

	if waitErr != nil {
		log.Printf("claude exited: %v (stderr: %s)", waitErr, errMsg)

		// If --resume failed because the session doesn't exist, clear it and retry
		if resumeID != "" && strings.Contains(errMsg, "No conversation found with session ID") {
			log.Printf("Session %s not found, clearing and retrying without --resume", resumeID)
			b.mu.Lock()
			b.sessionID = ""
			b.history = nil
			b.mu.Unlock()
			b.clearSessionID()
			os.Remove(b.historyPath)
			// Retry this message without --resume (recursive, but resumeID will be "" so no infinite loop)
			b.mu.Lock()
			b.working = false
			b.proc = nil
			b.stdin = nil
			b.mu.Unlock()
			b.handleUserMessage(message)
			return
		}

		if errMsg == "" {
			errMsg = waitErr.Error()
		}
		b.broadcast(map[string]interface{}{"type": "bridge_error", "message": errMsg})
	} else if !gotEvents {
		log.Printf("claude produced no output (stderr: %s)", errMsg)
		if errMsg != "" {
			b.broadcast(map[string]interface{}{"type": "bridge_error", "message": errMsg})
		} else {
			b.broadcast(map[string]interface{}{"type": "bridge_error", "message": "Claude exited without producing any output."})
		}
	} else {
		log.Printf("claude completed successfully")
	}
}

// broadcast sends a message to all connected WebSocket clients.
// Messages with replayable types are appended to history for new clients.
func (b *Bridge) broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if shouldRecord(msg) {
		b.history = append(b.history, json.RawMessage(data))
		b.saveHistoryLocked()
	}

	for conn := range b.clients {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("WS write error: %v", err)
			conn.Close()
			delete(b.clients, conn)
		}
	}
}

func shouldRecord(msg interface{}) bool {
	m, ok := msg.(map[string]interface{})
	if !ok {
		return false
	}
	switch m["type"] {
	case "user_message", "claude_event", "bridge_error":
		return true
	}
	return false
}

// replayHistory sends stored messages to a single client.
func (b *Bridge) replayHistory(conn *websocket.Conn) {
	b.mu.Lock()
	msgs := make([]json.RawMessage, len(b.history))
	copy(msgs, b.history)
	b.mu.Unlock()

	if len(msgs) == 0 {
		return
	}

	wrapper, _ := json.Marshal(map[string]interface{}{
		"type":     "message_history",
		"messages": msgs,
	})
	conn.WriteMessage(websocket.TextMessage, wrapper)
}

func (b *Bridge) killProc() {
	b.mu.Lock()
	proc := b.proc
	b.mu.Unlock()
	if proc != nil && proc.Process != nil {
		proc.Process.Signal(syscall.SIGTERM)
		time.Sleep(500 * time.Millisecond)
		proc.Process.Kill()
	}
}

// authKeepalive periodically checks and refreshes OAuth tokens.
func (b *Bridge) authKeepalive() {
	interval := time.Duration(b.cfg.KeepaliveHours) * time.Hour
	if interval <= 0 {
		return
	}
	checkInterval := 30 * time.Minute
	refreshThreshold := 12 * time.Hour
	log.Printf("Auth keepalive active (check every %v, refresh when ≤%v remaining)", checkInterval, refreshThreshold)

	// Check immediately on startup — catches already-expired tokens before first tick
	b.checkAndRefreshAuth(refreshThreshold)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		b.checkAndRefreshAuth(refreshThreshold)
	}
}

// readCredentials reads and parses the OAuth credentials file.
func (b *Bridge) readCredentials() (credsPath string, accessToken, refreshToken string, expiresAt int64, err error) {
	credsPath = filepath.Join(b.cfg.HomeDir, ".claude", ".credentials.json")
	data, readErr := os.ReadFile(credsPath)
	if readErr != nil {
		err = readErr
		return
	}

	var creds struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
			Scopes       string `json:"scopes"`
		} `json:"claudeAiOauth"`
	}
	if jsonErr := json.Unmarshal(data, &creds); jsonErr != nil {
		err = jsonErr
		return
	}

	accessToken = creds.ClaudeAiOauth.AccessToken
	refreshToken = creds.ClaudeAiOauth.RefreshToken
	expiresAt = creds.ClaudeAiOauth.ExpiresAt
	return
}

// writeCredentials writes new OAuth tokens to the credentials file.
func (b *Bridge) writeCredentials(credsPath, accessToken, refreshToken string, expiresAt int64) error {
	// Read existing file to preserve any extra fields
	creds := map[string]interface{}{
		"claudeAiOauth": map[string]interface{}{
			"accessToken":  accessToken,
			"refreshToken": refreshToken,
			"expiresAt":    expiresAt,
			"scopes":       oauthScopes,
		},
	}
	credsJSON, _ := json.MarshalIndent(creds, "", "  ")
	return os.WriteFile(credsPath, credsJSON, 0600)
}

func (b *Bridge) checkAndRefreshAuth(threshold time.Duration) {
	credsPath, _, refreshToken, expiresAt, err := b.readCredentials()
	if err != nil {
		log.Printf("[auth-keepalive] Cannot read credentials: %v", err)
		return
	}

	if expiresAt == 0 {
		log.Printf("[auth-keepalive] No OAuth token found")
		return
	}

	nowMs := time.Now().UnixMilli()
	remainingMs := expiresAt - nowMs
	thresholdMs := threshold.Milliseconds()

	if remainingMs > thresholdMs {
		remainingHr := remainingMs / 3_600_000
		log.Printf("[auth-keepalive] Token valid for ~%dh, no action needed", remainingHr)
		return
	}

	if remainingMs <= 0 {
		log.Printf("[auth-keepalive] Token EXPIRED %dh ago, attempting refresh...",
			(-remainingMs)/3_600_000)
	} else {
		log.Printf("[auth-keepalive] Token expires in ~%dh, refreshing...",
			remainingMs/3_600_000)
	}

	if refreshToken == "" {
		log.Printf("[auth-keepalive] No refresh_token available, cannot refresh")
		return
	}

	// Use the refresh_token grant directly against the OAuth token endpoint
	tokenReq := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
		"client_id":     oauthClientID,
	}
	reqBody, _ := json.Marshal(tokenReq)

	resp, err := http.Post(oauthTokenURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[auth-keepalive] Refresh request failed: %v", err)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[auth-keepalive] Refresh failed (HTTP %d): %s", resp.StatusCode, string(body))
		return
	}

	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		log.Printf("[auth-keepalive] Failed to parse refresh response: %v", err)
		return
	}

	newExpiresAt := time.Now().UnixMilli() + tokenResp.ExpiresIn*1000

	// Use the new refresh_token if provided (token rotation), otherwise keep the old one
	newRefreshToken := refreshToken
	if tokenResp.RefreshToken != "" {
		newRefreshToken = tokenResp.RefreshToken
	}

	if err := b.writeCredentials(credsPath, tokenResp.AccessToken, newRefreshToken, newExpiresAt); err != nil {
		log.Printf("[auth-keepalive] Failed to write refreshed credentials: %v", err)
		return
	}

	newRemainingHr := (newExpiresAt - time.Now().UnixMilli()) / 3_600_000
	log.Printf("[auth-keepalive] Token refreshed successfully (new expiry in ~%dh)", newRemainingHr)
}

// OAuth constants — extracted from the Claude CLI binary.
const (
	oauthClientID     = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthAuthorizeURL = "https://claude.com/cai/oauth/authorize"
	oauthTokenURL     = "https://platform.claude.com/v1/oauth/token"
	oauthRedirectURI  = "https://platform.claude.com/oauth/code/callback"
	oauthScopes       = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

// isAuthenticated checks if valid OAuth credentials exist.
func (b *Bridge) isAuthenticated() bool {
	credsPath := filepath.Join(b.cfg.HomeDir, ".claude", ".credentials.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		return false
	}
	var creds struct {
		ClaudeAiOauth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return false
	}
	return creds.ClaudeAiOauth.ExpiresAt > time.Now().UnixMilli()
}

// generatePKCE creates a PKCE code_verifier and code_challenge (S256).
func generatePKCE() (verifier, challenge string) {
	buf := make([]byte, 32)
	rand.Read(buf)
	verifier = base64.RawURLEncoding.EncodeToString(buf)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

// generateState creates a random state parameter.
func generateState() string {
	buf := make([]byte, 32)
	rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// handleAuthStart generates an OAuth URL with PKCE and sends it to the client.
func (b *Bridge) handleAuthStart() {
	b.authMu.Lock()
	if b.authPending {
		b.authMu.Unlock()
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "pending",
			"message": "Auth flow already in progress. Paste the code from the browser.",
		})
		return
	}
	b.authPending = true
	b.authMu.Unlock()

	verifier, challenge := generatePKCE()
	state := generateState()

	b.authMu.Lock()
	b.authCodeVerifier = verifier
	b.authState = state
	b.authMu.Unlock()

	// Build the authorization URL
	params := url.Values{}
	params.Set("code", "true")
	params.Set("client_id", oauthClientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", oauthRedirectURI)
	params.Set("scope", oauthScopes)
	params.Set("code_challenge", challenge)
	params.Set("code_challenge_method", "S256")
	params.Set("state", state)

	authURL := oauthAuthorizeURL + "?" + params.Encode()

	log.Printf("[auth] Generated OAuth URL (state=%s)", state)
	b.broadcast(map[string]interface{}{
		"type":    "auth_url",
		"url":     authURL,
		"message": "Open this URL in your browser, sign in, then paste the code below.",
	})
}

// handleAuthCode exchanges the authorization code for tokens via Anthropic's token endpoint.
func (b *Bridge) handleAuthCode(input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Empty code. Please paste the code from the browser.",
		})
		return
	}

	b.authMu.Lock()
	pending := b.authPending
	verifier := b.authCodeVerifier
	expectedState := b.authState
	b.authMu.Unlock()

	if !pending || verifier == "" || expectedState == "" {
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "No auth flow in progress. Click Login to start.",
		})
		return
	}

	// Parse code#state format
	code := input
	if idx := strings.Index(input, "#"); idx >= 0 {
		code = input[:idx]
		pastedState := input[idx+1:]
		if pastedState != expectedState {
			log.Printf("[auth] State mismatch: expected %s, got %s", expectedState, pastedState)
			b.broadcast(map[string]interface{}{
				"type":    "auth_status",
				"status":  "error",
				"message": "State mismatch. Please start a new login.",
			})
			b.authMu.Lock()
			b.authPending = false
			b.authMu.Unlock()
			return
		}
	}

	log.Printf("[auth] Exchanging authorization code for tokens")
	b.broadcast(map[string]interface{}{
		"type":    "auth_status",
		"status":  "completing",
		"message": "Exchanging code for tokens...",
	})

	// Exchange the code for tokens
	tokenReq := map[string]string{
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  oauthRedirectURI,
		"client_id":     oauthClientID,
		"code_verifier": verifier,
		"state":         expectedState,
	}
	reqBody, _ := json.Marshal(tokenReq)

	resp, err := http.Post(oauthTokenURL, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		log.Printf("[auth] Token exchange request failed: %v", err)
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Token exchange failed: " + err.Error(),
		})
		b.authMu.Lock()
		b.authPending = false
		b.authMu.Unlock()
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		log.Printf("[auth] Token exchange failed: %d %s", resp.StatusCode, string(body))
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": fmt.Sprintf("Token exchange failed (HTTP %d). Try again.", resp.StatusCode),
		})
		b.authMu.Lock()
		b.authPending = false
		b.authMu.Unlock()
		return
	}

	// Parse the token response
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
		Scope        string `json:"scope"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		log.Printf("[auth] Failed to parse token response: %v", err)
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Failed to parse token response.",
		})
		b.authMu.Lock()
		b.authPending = false
		b.authMu.Unlock()
		return
	}

	// Write credentials in the format Claude CLI expects
	expiresAt := time.Now().UnixMilli() + tokenResp.ExpiresIn*1000
	creds := map[string]interface{}{
		"claudeAiOauth": map[string]interface{}{
			"accessToken":  tokenResp.AccessToken,
			"refreshToken": tokenResp.RefreshToken,
			"expiresAt":    expiresAt,
			"scopes":       tokenResp.Scope,
		},
	}
	credsJSON, _ := json.MarshalIndent(creds, "", "  ")
	credsDir := filepath.Join(b.cfg.HomeDir, ".claude")
	os.MkdirAll(credsDir, 0700)
	credsPath := filepath.Join(credsDir, ".credentials.json")

	if err := os.WriteFile(credsPath, credsJSON, 0600); err != nil {
		log.Printf("[auth] Failed to write credentials: %v", err)
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Failed to save credentials: " + err.Error(),
		})
		b.authMu.Lock()
		b.authPending = false
		b.authMu.Unlock()
		return
	}

	log.Printf("[auth] Authentication successful, credentials saved to %s", credsPath)
	b.broadcast(map[string]interface{}{
		"type":    "auth_status",
		"status":  "success",
		"message": "Authentication successful!",
	})

	b.authMu.Lock()
	b.authPending = false
	b.authCodeVerifier = ""
	b.authState = ""
	b.authMu.Unlock()
}
