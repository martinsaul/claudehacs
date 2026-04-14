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
	Port            int
	BasePath        string
	ClaudeBin       string
	WorkDir         string
	HomeDir         string
	SkipPerms       bool
	UseGosu         bool
	SystemPrompt    string
	KeepaliveHours  int
	APIKeySet       bool
}

// Bridge manages the Claude subprocess and WebSocket clients.
type Bridge struct {
	cfg       Config
	sessionID string

	mu        sync.Mutex
	proc      *exec.Cmd
	stdin     io.WriteCloser
	working   bool
	clients   map[*websocket.Conn]bool

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
		cfg:     cfg,
		clients: make(map[*websocket.Conn]bool),
	}

	// Load saved session ID (may be empty if first run)
	b.sessionID = b.loadSessionID()

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
			b.mu.Unlock()
			b.clearSessionID()
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
func (b *Bridge) broadcast(msg interface{}) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for conn := range b.clients {
		if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
			log.Printf("WS write error: %v", err)
			conn.Close()
			delete(b.clients, conn)
		}
	}
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
	log.Printf("Auth keepalive active (check every %v, refresh when ≤2h remaining)", checkInterval)

	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for range ticker.C {
		b.checkAndRefreshAuth()
	}
}

func (b *Bridge) checkAndRefreshAuth() {
	credsPath := filepath.Join(b.cfg.HomeDir, ".claude", ".credentials.json")
	data, err := os.ReadFile(credsPath)
	if err != nil {
		log.Printf("[auth-keepalive] Cannot read credentials: %v", err)
		return
	}

	var creds struct {
		ClaudeAiOauth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		log.Printf("[auth-keepalive] Cannot parse credentials: %v", err)
		return
	}

	expiresAt := creds.ClaudeAiOauth.ExpiresAt
	if expiresAt == 0 {
		log.Printf("[auth-keepalive] No OAuth token found")
		return
	}

	nowMs := time.Now().UnixMilli()
	remainingMs := expiresAt - nowMs
	remainingHr := remainingMs / 3_600_000

	if remainingMs > 2*3_600_000 {
		log.Printf("[auth-keepalive] Token valid for ~%dh, no action needed", remainingHr)
		return
	}

	log.Printf("[auth-keepalive] Token expires in ~%dh, triggering refresh...", remainingHr)

	// Try auth status first
	var cmd *exec.Cmd
	if b.cfg.UseGosu {
		cmd = exec.Command("gosu", "claude", "env", "HOME="+b.cfg.HomeDir, b.cfg.ClaudeBin, "auth", "status", "--json")
	} else {
		cmd = exec.Command(b.cfg.ClaudeBin, "auth", "status", "--json")
	}
	cmd.Env = append(os.Environ(), "HOME="+b.cfg.HomeDir)
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("[auth-keepalive] auth status failed: %v: %s", err, string(output))
	} else {
		log.Printf("[auth-keepalive] auth status OK")
	}

	// Check if token was actually refreshed
	data2, _ := os.ReadFile(credsPath)
	var creds2 struct {
		ClaudeAiOauth struct {
			ExpiresAt int64 `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	json.Unmarshal(data2, &creds2)

	if creds2.ClaudeAiOauth.ExpiresAt > expiresAt {
		log.Printf("[auth-keepalive] Token refreshed successfully (new expiry in ~%dh)",
			(creds2.ClaudeAiOauth.ExpiresAt-nowMs)/3_600_000)
		return
	}

	// Fallback: force an API call
	log.Printf("[auth-keepalive] auth status didn't refresh, forcing API call...")
	var cmd2 *exec.Cmd
	if b.cfg.UseGosu {
		cmd2 = exec.Command("gosu", "claude", "env", "HOME="+b.cfg.HomeDir, b.cfg.ClaudeBin,
			"--print", "--output-format", "json", "--no-session-persistence", "ok")
	} else {
		cmd2 = exec.Command(b.cfg.ClaudeBin,
			"--print", "--output-format", "json", "--no-session-persistence", "ok")
	}
	cmd2.Env = append(os.Environ(), "HOME="+b.cfg.HomeDir)
	out2, err2 := cmd2.CombinedOutput()
	if err2 != nil {
		log.Printf("[auth-keepalive] API call fallback failed: %v: %s", err2, string(out2))
	} else {
		log.Printf("[auth-keepalive] API call succeeded, token should be refreshed")
	}
}

// OAuth constants — extracted from the Claude CLI binary.
const (
	oauthClientID    = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	oauthAuthorizeURL = "https://claude.com/cai/oauth/authorize"
	oauthTokenURL    = "https://platform.claude.com/v1/oauth/token"
	oauthRedirectURI = "https://platform.claude.com/oauth/code/callback"
	oauthScopes      = "org:create_api_key user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
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
