(function() {
  'use strict';

  const messagesEl = document.getElementById('messages');
  const inputEl = document.getElementById('input');
  const sendBtn = document.getElementById('send-btn');
  const statusEl = document.getElementById('status');
  const interruptBtn = document.getElementById('interrupt-btn');
  const loginBtn = document.getElementById('login-btn');
  const newBtn = document.getElementById('new-btn');

  const base = location.pathname.replace(/\/$/, '');

  let ws = null;
  let working = false;
  let authPending = false;
  let currentAssistantEl = null;
  let currentAssistantText = '';
  let reconnectDelay = 1000;

  // --- WebSocket ---

  function connect() {
    const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
    ws = new WebSocket(proto + '//' + location.host + base + '/ws');

    ws.onopen = function() {
      reconnectDelay = 1000;
      setStatus('connected');
    };

    ws.onclose = function() {
      setStatus('disconnected');
      setTimeout(connect, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 1.5, 10000);
    };

    ws.onerror = function() {
      ws.close();
    };

    ws.onmessage = function(e) {
      try {
        handleMessage(JSON.parse(e.data));
      } catch (err) {
        console.error('Parse error:', err, e.data);
      }
    };
  }

  function send(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) {
      ws.send(JSON.stringify(msg));
    }
  }

  // --- Message handling ---

  function handleMessage(msg) {
    switch (msg.type) {
      case 'bridge_status':
        working = msg.working;
        setStatus(working ? 'working' : 'connected');
        interruptBtn.disabled = !working;
        if (!working) finishAssistant();
        break;

      case 'message_history':
        replayHistory(msg.messages);
        break;

      case 'session_reset':
        messagesEl.innerHTML = '';
        working = false;
        currentAssistantEl = null;
        currentAssistantText = '';
        interruptBtn.disabled = true;
        setStatus('connected');
        break;

      case 'user_message':
        addMessage('user', msg.message);
        break;

      case 'claude_event':
        handleClaudeEvent(msg.event);
        break;

      case 'bridge_error':
        addMessage('error', msg.message);
        break;

      case 'auth_required':
        showAuthPrompt(msg.message);
        break;

      case 'auth_url':
        showAuthURL(msg.url, msg.message);
        break;

      case 'auth_status':
        handleAuthStatus(msg);
        break;
    }
  }

  function replayHistory(messages) {
    if (!messages || !messages.length) return;
    messagesEl.innerHTML = '';
    currentAssistantEl = null;
    currentAssistantText = '';
    for (var i = 0; i < messages.length; i++) {
      var m = messages[i];
      if (typeof m === 'string') {
        try { m = JSON.parse(m); } catch(e) { continue; }
      }
      handleMessage(m);
    }
    finishAssistant();
    scrollToBottom();
  }

  function handleClaudeEvent(event) {
    if (!event || !event.type) return;

    switch (event.type) {
      case 'system':
        if (event.subtype === 'init') {
          addMessage('system', 'Session started (model: ' + (event.model || 'unknown') + ')');
        }
        break;

      case 'assistant':
        handleAssistantMessage(event);
        break;

      case 'tool_use':
        finishAssistant();
        handleToolUse(event);
        break;

      case 'tool_result':
        handleToolResult(event);
        break;

      case 'result':
        finishAssistant();
        if (event.total_cost_usd) {
          addCostLine(event);
        }
        break;

      case 'rate_limit_event':
        // Silently ignore
        break;

      default:
        // Unknown events — show as system message for debugging
        if (event.subtype !== 'init') {
          console.log('Unknown event:', event);
        }
        break;
    }
  }

  function handleAssistantMessage(event) {
    const msg = event.message;
    if (!msg || !msg.content) return;

    for (const block of msg.content) {
      if (block.type === 'text' && block.text) {
        if (!currentAssistantEl) {
          currentAssistantEl = document.createElement('div');
          currentAssistantEl.className = 'msg msg-assistant streaming';
          messagesEl.appendChild(currentAssistantEl);
          currentAssistantText = '';
        }
        currentAssistantText = block.text;
        currentAssistantEl.innerHTML = renderMarkdown(currentAssistantText);
        scrollToBottom();
      }
    }
  }

  function finishAssistant() {
    if (currentAssistantEl) {
      currentAssistantEl.classList.remove('streaming');
      currentAssistantEl = null;
      currentAssistantText = '';
    }
  }

  function handleToolUse(event) {
    const tool = event.tool_use || event;
    const name = tool.name || 'Unknown tool';
    const input = tool.input || {};

    const el = document.createElement('div');
    el.className = 'msg msg-tool';
    el.dataset.toolId = tool.id || '';

    let summary = name;
    if (name === 'Read' && input.file_path) summary = 'Read ' + input.file_path;
    else if (name === 'Write' && input.file_path) summary = 'Write ' + input.file_path;
    else if (name === 'Edit' && input.file_path) summary = 'Edit ' + input.file_path;
    else if (name === 'Bash' && input.command) summary = 'Bash: ' + truncate(input.command, 60);
    else if (name === 'Glob' && input.pattern) summary = 'Glob ' + input.pattern;
    else if (name === 'Grep' && input.pattern) summary = 'Grep ' + truncate(input.pattern, 40);

    const header = document.createElement('div');
    header.className = 'tool-header';
    header.textContent = summary;
    header.onclick = function() { header.classList.toggle('open'); };

    const body = document.createElement('div');
    body.className = 'tool-body';
    body.innerHTML = '<pre><code>' + escapeHtml(JSON.stringify(input, null, 2)) + '</code></pre>';

    el.appendChild(header);
    el.appendChild(body);
    messagesEl.appendChild(el);
    scrollToBottom();
  }

  function handleToolResult(event) {
    // Tool results often contain large output; show as collapsible under the tool call
    const content = event.content || event.result || '';
    const text = typeof content === 'string' ? content :
      (Array.isArray(content) ? content.map(c => c.text || '').join('\n') : JSON.stringify(content));

    if (text && text.length > 0) {
      const el = document.createElement('div');
      el.className = 'msg msg-tool';
      const header = document.createElement('div');
      header.className = 'tool-header';
      header.textContent = 'Result (' + text.length + ' chars)';
      header.onclick = function() { header.classList.toggle('open'); };
      const body = document.createElement('div');
      body.className = 'tool-body';
      body.innerHTML = '<pre><code>' + escapeHtml(truncate(text, 5000)) + '</code></pre>';
      el.appendChild(header);
      el.appendChild(body);
      messagesEl.appendChild(el);
      scrollToBottom();
    }
  }

  function addCostLine(event) {
    const el = document.createElement('div');
    el.className = 'msg-cost';
    const cost = event.total_cost_usd ? '$' + event.total_cost_usd.toFixed(4) : '';
    const tokens = event.usage ? (event.usage.output_tokens || 0) + ' tokens out' : '';
    const turns = event.num_turns ? event.num_turns + ' turn' + (event.num_turns > 1 ? 's' : '') : '';
    const time = event.duration_ms ? (event.duration_ms / 1000).toFixed(1) + 's' : '';
    el.textContent = [cost, tokens, turns, time].filter(Boolean).join(' · ');
    messagesEl.appendChild(el);
    scrollToBottom();
  }

  // --- UI helpers ---

  function addMessage(type, text) {
    const el = document.createElement('div');
    el.className = 'msg msg-' + type;
    if (type === 'user') {
      el.textContent = text;
    } else if (type === 'assistant') {
      el.innerHTML = renderMarkdown(text);
    } else {
      el.textContent = text;
    }
    messagesEl.appendChild(el);
    scrollToBottom();
  }

  function setStatus(state) {
    statusEl.className = 'status ' + state;
    statusEl.textContent = state;
  }

  function scrollToBottom() {
    requestAnimationFrame(function() {
      messagesEl.scrollTop = messagesEl.scrollHeight;
    });
  }

  function truncate(s, max) {
    return s.length > max ? s.substring(0, max) + '...' : s;
  }

  function escapeHtml(s) {
    return s.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  // --- Simple markdown renderer ---

  function renderMarkdown(text) {
    // Code blocks
    text = text.replace(/```(\w*)\n([\s\S]*?)```/g, function(_, lang, code) {
      return '<pre><code>' + escapeHtml(code.trimEnd()) + '</code></pre>';
    });
    // Inline code
    text = text.replace(/`([^`]+)`/g, function(_, code) {
      return '<code>' + escapeHtml(code) + '</code>';
    });
    // Bold
    text = text.replace(/\*\*(.+?)\*\*/g, '<strong>$1</strong>');
    // Italic
    text = text.replace(/(?<!\*)\*(?!\*)(.+?)(?<!\*)\*(?!\*)/g, '<em>$1</em>');
    // Links
    text = text.replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank">$1</a>');
    // Line breaks outside of pre blocks
    var tokens = text.split(/(<\/?pre[^>]*>)/);
    var inPre = false;
    var result = '';
    for (var i = 0; i < tokens.length; i++) {
      if (/<pre[^>]*>/.test(tokens[i])) { inPre = true; result += tokens[i]; }
      else if (/<\/pre>/.test(tokens[i])) { inPre = false; result += tokens[i]; }
      else if (!inPre) { result += tokens[i].replace(/\n/g, '<br>'); }
      else { result += tokens[i]; }
    }
    return result;
  }

  // --- Auth flow ---

  function showAuthPrompt(message) {
    removeAuthUI();
    loginBtn.style.display = 'inline-block';
    var el = document.createElement('div');
    el.className = 'msg msg-auth';
    el.id = 'auth-prompt';
    el.innerHTML =
      '<div class="auth-message">' + escapeHtml(message) + '</div>' +
      '<button class="btn-auth" onclick="window._startAuth()">Login with Claude</button>';
    messagesEl.appendChild(el);
    scrollToBottom();
  }

  function showAuthURL(url, message) {
    removeAuthUI();
    authPending = true;
    var el = document.createElement('div');
    el.className = 'msg msg-auth';
    el.id = 'auth-prompt';
    el.innerHTML =
      '<div class="auth-message">' + escapeHtml(message) + '</div>' +
      '<div class="auth-url"><a href="' + escapeHtml(url) + '" target="_blank" rel="noopener">Open login page</a></div>' +
      '<div class="auth-code-input">' +
        '<input type="text" id="auth-code" placeholder="Paste authorization code here..." />' +
        '<button class="btn-auth" onclick="window._submitAuthCode()">Submit</button>' +
      '</div>';
    messagesEl.appendChild(el);
    scrollToBottom();
    var codeInput = document.getElementById('auth-code');
    if (codeInput) codeInput.focus();
  }

  function handleAuthStatus(msg) {
    if (msg.status === 'success') {
      authPending = false;
      loginBtn.style.display = 'none';
      removeAuthUI();
      addMessage('system', msg.message);
    } else if (msg.status === 'error') {
      authPending = false;
      removeAuthUI();
      showAuthPrompt(msg.message);
    } else if (msg.status === 'starting' || msg.status === 'completing') {
      addMessage('system', msg.message);
    }
  }

  function removeAuthUI() {
    var existing = document.getElementById('auth-prompt');
    if (existing) existing.remove();
  }

  window._startAuth = function() {
    send({ type: 'auth_start' });
    removeAuthUI();
    addMessage('system', 'Starting login...');
  };

  window._submitAuthCode = function() {
    var codeInput = document.getElementById('auth-code');
    if (!codeInput) return;
    var code = codeInput.value.trim();
    if (!code) return;
    send({ type: 'auth_code', message: code });
  };

  // --- Input handling ---

  inputEl.addEventListener('keydown', function(e) {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      sendMessage();
    }
  });

  inputEl.addEventListener('input', function() {
    // Auto-resize textarea
    this.style.height = 'auto';
    this.style.height = Math.min(this.scrollHeight, 150) + 'px';
  });

  sendBtn.addEventListener('click', sendMessage);

  interruptBtn.addEventListener('click', function() {
    send({ type: 'interrupt' });
  });

  loginBtn.addEventListener('click', function() {
    window._startAuth();
  });

  newBtn.addEventListener('click', function() {
    fetch(base + '/new-session', { method: 'POST' })
      .catch(function(err) { console.error('new-session error:', err); });
  });

  function sendMessage() {
    const text = inputEl.value.trim();
    if (!text) return;
    if (working) return;
    send({ type: 'user_message', message: text });
    inputEl.value = '';
    inputEl.style.height = 'auto';
    inputEl.focus();
  }

  // --- Init ---

  connect();

})();
