package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
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
	authMu        sync.Mutex
	authProc      *exec.Cmd
	authPending   bool   // true while waiting for user to paste code
	authState     string // OAuth state parameter from the auth URL
	authLocalPort int    // local port claude auth login is listening on
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

	// Check auth status on startup
	go b.checkAuthOnStartup()

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

// checkAuthOnStartup logs auth status at boot.
func (b *Bridge) checkAuthOnStartup() {
	if b.cfg.APIKeySet {
		log.Printf("[auth] API key configured, skipping OAuth check")
		return
	}
	if b.isAuthenticated() {
		log.Printf("[auth] OAuth credentials found and valid")
	} else {
		log.Printf("[auth] No valid OAuth credentials found, clients will be prompted on connect")
	}
}

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
	// Check if token exists and hasn't expired
	return creds.ClaudeAiOauth.ExpiresAt > time.Now().UnixMilli()
}

// handleAuthStart launches `claude auth login` and captures the OAuth URL.
// Claude CLI starts a local HTTP server on a random port and waits for the
// OAuth callback at /callback?code=X&state=Y. We extract the URL (with state),
// find the local port, and store both so handleAuthCode can complete the flow.
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

	defer func() {
		b.authMu.Lock()
		b.authPending = false
		b.authProc = nil
		b.authState = ""
		b.authLocalPort = 0
		b.authMu.Unlock()
	}()

	b.broadcast(map[string]interface{}{
		"type":    "auth_status",
		"status":  "starting",
		"message": "Starting OAuth login...",
	})

	// Build the auth login command
	var cmd *exec.Cmd
	if b.cfg.UseGosu {
		cmd = exec.Command("gosu", "claude", "env", "HOME="+b.cfg.HomeDir,
			b.cfg.ClaudeBin, "auth", "login", "--claudeai")
	} else {
		cmd = exec.Command(b.cfg.ClaudeBin, "auth", "login", "--claudeai")
	}
	cmd.Env = append(os.Environ(),
		"HOME="+b.cfg.HomeDir,
		"BROWSER=/usr/bin/false", // Prevent browser open attempts
	)

	// Capture stdout+stderr via pipes
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Printf("[auth] stdout pipe error: %v", err)
		b.broadcast(map[string]interface{}{"type": "auth_status", "status": "error", "message": err.Error()})
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		log.Printf("[auth] stderr pipe error: %v", err)
		b.broadcast(map[string]interface{}{"type": "auth_status", "status": "error", "message": err.Error()})
		return
	}

	if err := cmd.Start(); err != nil {
		log.Printf("[auth] start error: %v", err)
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Failed to start auth: " + err.Error(),
		})
		return
	}

	pid := cmd.Process.Pid
	log.Printf("[auth] Spawned claude auth login (PID %d)", pid)

	b.authMu.Lock()
	b.authProc = cmd
	b.authMu.Unlock()

	// Read output lines in background
	outputCh := make(chan string, 50)
	var outputCollector bytes.Buffer
	var outputMu sync.Mutex
	readPipe := func(r io.Reader) {
		scanner := bufio.NewScanner(r)
		for scanner.Scan() {
			line := scanner.Text()
			outputMu.Lock()
			outputCollector.WriteString(line + "\n")
			outputMu.Unlock()
			outputCh <- line
		}
	}
	go readPipe(stdoutPipe)
	go readPipe(stderrPipe)

	// Wait for the URL to appear
	authURL := ""
	deadline := time.After(15 * time.Second)
	for authURL == "" {
		select {
		case <-deadline:
			outputMu.Lock()
			collected := outputCollector.String()
			outputMu.Unlock()
			log.Printf("[auth] Timed out waiting for URL. Output: %s", collected)
			b.broadcast(map[string]interface{}{
				"type":    "auth_status",
				"status":  "error",
				"message": "Timed out waiting for auth URL. Output: " + collected,
			})
			cmd.Process.Kill()
			cmd.Wait()
			return
		case line := <-outputCh:
			if url := extractAuthURL(line); url != "" {
				authURL = url
			}
		}
	}

	// Extract state parameter from the URL
	state := extractParam(authURL, "state")
	if state == "" {
		log.Printf("[auth] Could not extract state from URL")
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Could not extract OAuth state from URL.",
		})
		cmd.Process.Kill()
		cmd.Wait()
		return
	}

	// Find the local port the CLI is listening on
	localPort := findListeningPort(pid)
	if localPort == 0 {
		log.Printf("[auth] Could not find local listening port for PID %d", pid)
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Could not find CLI callback port.",
		})
		cmd.Process.Kill()
		cmd.Wait()
		return
	}

	log.Printf("[auth] Got auth URL (state=%s, local port=%d)", state, localPort)

	b.authMu.Lock()
	b.authState = state
	b.authLocalPort = localPort
	b.authMu.Unlock()

	b.broadcast(map[string]interface{}{
		"type":    "auth_url",
		"url":     authURL,
		"message": "Open this URL in your browser, sign in, then paste the code below.",
	})

	// Wait for the process to exit (handleAuthCode will hit the local callback)
	waitErr := cmd.Wait()
	outputMu.Lock()
	output := outputCollector.String()
	outputMu.Unlock()

	if waitErr != nil {
		log.Printf("[auth] claude auth login exited with error: %v (output: %s)", waitErr, output)
		if b.isAuthenticated() {
			log.Printf("[auth] Despite error exit, credentials are valid")
			b.broadcast(map[string]interface{}{
				"type":    "auth_status",
				"status":  "success",
				"message": "Authentication successful!",
			})
			return
		}
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Auth failed: " + strings.TrimSpace(output),
		})
		return
	}

	log.Printf("[auth] claude auth login completed successfully")
	b.broadcast(map[string]interface{}{
		"type":    "auth_status",
		"status":  "success",
		"message": "Authentication successful!",
	})
}

// handleAuthCode submits the authorization code to the CLI's local callback server.
func (b *Bridge) handleAuthCode(code string) {
	code = strings.TrimSpace(code)
	if code == "" {
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Empty code. Please paste the code from the browser.",
		})
		return
	}

	b.authMu.Lock()
	pending := b.authPending
	state := b.authState
	port := b.authLocalPort
	b.authMu.Unlock()

	if !pending || state == "" || port == 0 {
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "No auth flow in progress. Click Login to start.",
		})
		return
	}

	log.Printf("[auth] Submitting code to local callback (port %d, state %s)", port, state)
	b.broadcast(map[string]interface{}{
		"type":    "auth_status",
		"status":  "completing",
		"message": "Submitting code...",
	})

	// The code may contain # (fragment separator) — must be URL-encoded.
	// Also strip any trailing #state the user may have pasted from the
	// platform callback page (the state is already known).
	if idx := strings.Index(code, "#"); idx >= 0 {
		code = code[:idx]
	}

	// Hit the CLI's local callback endpoint
	callbackURL := fmt.Sprintf("http://localhost:%d/callback?code=%s&state=%s",
		port, url.QueryEscape(code), url.QueryEscape(state))
	resp, err := http.Get(callbackURL)
	if err != nil {
		log.Printf("[auth] Callback request failed: %v", err)
		b.broadcast(map[string]interface{}{
			"type":    "auth_status",
			"status":  "error",
			"message": "Failed to submit code: " + err.Error(),
		})
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	bodyStr := strings.TrimSpace(string(body))
	log.Printf("[auth] Callback response: %d %s", resp.StatusCode, bodyStr)

	if resp.StatusCode != http.StatusOK && bodyStr != "" {
		// The CLI will exit and handleAuthStart will broadcast the final status
		log.Printf("[auth] Callback returned error: %s", bodyStr)
	}
	// The handleAuthStart goroutine waiting on cmd.Wait() will handle the result
}

// extractAuthURL finds the OAuth URL in claude auth login output.
func extractAuthURL(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if idx := strings.Index(line, "https://claude.com/"); idx >= 0 {
			return strings.TrimSpace(line[idx:])
		}
		if idx := strings.Index(line, "https://console.anthropic.com/"); idx >= 0 {
			return strings.TrimSpace(line[idx:])
		}
	}
	return ""
}

// extractParam extracts a query parameter value from a URL string.
func extractParam(rawURL, param string) string {
	re := regexp.MustCompile(param + `=([A-Za-z0-9_\-]+)`)
	m := re.FindStringSubmatch(rawURL)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

// findListeningPort finds a TCP port that the given PID is listening on.
// It reads /proc/net/tcp6 and /proc/<pid>/fd to correlate.
func findListeningPort(pid int) int {
	// Read listening sockets for this process from /proc/<pid>/fd
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		log.Printf("[auth] Cannot read %s: %v", fdDir, err)
		return findListeningPortFallback(pid)
	}

	// Collect inode numbers for sockets owned by this process
	socketInodes := make(map[string]bool)
	for _, e := range entries {
		link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}
		if strings.HasPrefix(link, "socket:[") {
			inode := strings.TrimPrefix(strings.TrimSuffix(link, "]"), "socket:[")
			socketInodes[inode] = true
		}
	}

	if len(socketInodes) == 0 {
		return findListeningPortFallback(pid)
	}

	// Parse /proc/net/tcp6 (and tcp) for listening sockets matching our inodes
	for _, tcpFile := range []string{"/proc/net/tcp6", "/proc/net/tcp"} {
		data, err := os.ReadFile(tcpFile)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 {
				continue
			}
			// State 0A = LISTEN
			if fields[3] != "0A" {
				continue
			}
			inode := fields[9]
			if socketInodes[inode] {
				// Parse port from local_address (field 1): addr:port in hex
				parts := strings.Split(fields[1], ":")
				if len(parts) == 2 {
					portHex := parts[len(parts)-1]
					port, err := strconv.ParseInt(portHex, 16, 32)
					if err == nil && port > 0 {
						return int(port)
					}
				}
			}
		}
	}

	return findListeningPortFallback(pid)
}

// findListeningPortFallback uses net.Dial probing as a last resort.
func findListeningPortFallback(pid int) int {
	// Try common ephemeral port range - scan quickly
	log.Printf("[auth] Falling back to port scan for PID %d", pid)
	for port := 30000; port < 65535; port++ {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Millisecond)
		if err == nil {
			conn.Close()
			// Verify it responds like the Claude callback server
			resp, err := http.Get(fmt.Sprintf("http://localhost:%d/callback?code=probe&state=probe", port))
			if err == nil {
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if strings.Contains(string(body), "Invalid state") {
					log.Printf("[auth] Found CLI callback server on port %d via fallback", port)
					return port
				}
			}
		}
	}
	return 0
}
