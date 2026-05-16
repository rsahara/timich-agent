package adminapi

import "html/template"

const adminUsername = "timich-agent-admin"

func loginHTML(message string) string {
	var alert string
	if message != "" {
		alert = `<div class="alert">` + template.HTMLEscapeString(message) + `</div>`
	}
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Timich Agent</title>
  <style>
    :root { color-scheme: light dark; --bg: #f6f7f8; --fg: #17191c; --muted: #656d76; --line: #d8dee4; --panel: #ffffff; --accent: #0a7cff; --danger: #c9352b; }
    @media (prefers-color-scheme: dark) { :root { --bg: #111316; --fg: #f3f5f7; --muted: #a0a8b2; --line: #30363d; --panel: #1a1e23; --accent: #58a6ff; --danger: #ff8178; } }
    * { box-sizing: border-box; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg); font: 15px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    main { width: min(420px, calc(100vw - 32px)); }
    h1 { margin: 0 0 8px; font-size: 28px; letter-spacing: 0; }
    p { margin: 0 0 20px; color: var(--muted); }
    form { display: grid; gap: 12px; padding: 20px; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; }
    label { display: grid; gap: 6px; color: var(--muted); font-size: 13px; }
    input { width: 100%; min-height: 42px; border: 1px solid var(--line); border-radius: 6px; padding: 0 12px; background: transparent; color: var(--fg); font: inherit; }
    input[readonly] { color: var(--muted); }
    button { min-height: 42px; border: 0; border-radius: 6px; padding: 0 14px; background: var(--accent); color: #fff; font: inherit; font-weight: 600; cursor: pointer; }
    .alert { margin-bottom: 12px; padding: 10px 12px; border: 1px solid color-mix(in srgb, var(--danger), transparent 60%); border-radius: 6px; color: var(--danger); background: color-mix(in srgb, var(--danger), transparent 92%); }
  </style>
</head>
<body>
  <main>
    <h1>Timich Agent</h1>
    <p>Local administration</p>
    ` + alert + `
    <form method="post" action="/login">
      <label>Username
        <input name="username" type="text" value="` + adminUsername + `" autocomplete="username" autocapitalize="none" spellcheck="false" readonly>
      </label>
      <label>Admin token
        <input name="token" type="password" autocomplete="current-password" autofocus required>
      </label>
      <button type="submit">Sign in</button>
    </form>
  </main>
</body>
</html>`
}

func setupHTML(message string) string {
	var alert string
	if message != "" {
		alert = `<div class="alert">` + template.HTMLEscapeString(message) + `</div>`
	}
	return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Timich Agent Setup</title>
  <style>
    :root { color-scheme: light dark; --bg: #f6f7f8; --fg: #17191c; --muted: #656d76; --line: #d8dee4; --panel: #ffffff; --accent: #0a7cff; --danger: #c9352b; }
    @media (prefers-color-scheme: dark) { :root { --bg: #111316; --fg: #f3f5f7; --muted: #a0a8b2; --line: #30363d; --panel: #1a1e23; --accent: #58a6ff; --danger: #ff8178; } }
    * { box-sizing: border-box; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg); font: 15px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    main { width: min(440px, calc(100vw - 32px)); }
    h1 { margin: 0 0 8px; font-size: 28px; letter-spacing: 0; }
    p { margin: 0 0 20px; color: var(--muted); }
    form { display: grid; gap: 12px; padding: 20px; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; }
    label { display: grid; gap: 6px; color: var(--muted); font-size: 13px; }
    input { width: 100%; min-height: 42px; border: 1px solid var(--line); border-radius: 6px; padding: 0 12px; background: transparent; color: var(--fg); font: inherit; }
    input[readonly] { color: var(--muted); }
    button { min-height: 42px; border: 0; border-radius: 6px; padding: 0 14px; background: var(--accent); color: #fff; font: inherit; font-weight: 600; cursor: pointer; }
    button.secondary { border: 1px solid var(--line); background: transparent; color: var(--fg); }
    button:disabled { opacity: .55; cursor: default; }
    .actions { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
    .message { min-height: 20px; color: var(--muted); font-size: 13px; }
    .note { margin: -2px 0 0; color: var(--muted); font-size: 13px; }
    .usage { margin-bottom: 4px; color: var(--muted); font-size: 13px; }
    .usage strong { color: var(--fg); font-weight: 600; }
    .alert { margin-bottom: 12px; padding: 10px 12px; border: 1px solid color-mix(in srgb, var(--danger), transparent 60%); border-radius: 6px; color: var(--danger); background: color-mix(in srgb, var(--danger), transparent 92%); }
  </style>
</head>
<body>
  <main>
    <h1>Timich Agent</h1>
    <p>Create the local admin password for this agent.</p>
    ` + alert + `
    <form id="setupForm" method="post" action="/setup-admin-token">
      <p class="usage"><strong>Used for:</strong> Admin UI sign-in, Admin API bearer auth, and CLI admin commands.</p>
      <label>Username
        <input name="username" type="text" value="` + adminUsername + `" autocomplete="username" autocapitalize="none" spellcheck="false" readonly>
      </label>
      <label>Admin token
        <input id="adminToken" name="token" type="password" minlength="16" maxlength="128" pattern="[A-Za-z0-9]{16,128}" autocomplete="new-password" passwordrules="minlength: 16; maxlength: 128; allowed: upper, lower, digit;" title="Use 16 to 128 letters and numbers." autofocus required>
      </label>
      <label>Confirm admin token
        <input id="confirmAdminToken" name="confirmToken" type="password" minlength="16" maxlength="128" pattern="[A-Za-z0-9]{16,128}" autocomplete="new-password" passwordrules="minlength: 16; maxlength: 128; allowed: upper, lower, digit;" title="Use 16 to 128 letters and numbers." required>
      </label>
      <p class="note">Save this as the password for ` + adminUsername + `. After setup, the token is stored in the local agent state file, but the browser session is temporary.</p>
      <div class="actions">
        <button class="secondary" id="generateAdminToken" type="button">Generate token</button>
        <button class="secondary" id="toggleAdminToken" type="button">Show token</button>
        <button type="submit">Create admin token</button>
      </div>
      <div class="message" id="setupMessage" aria-live="polite"></div>
    </form>
  </main>
  <script>
    (function () {
      const minLength = 16;
      const generatedLength = 32;
      const alphabet = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789';
      const form = document.querySelector('#setupForm');
      const tokenInput = document.querySelector('#adminToken');
      const confirmInput = document.querySelector('#confirmAdminToken');
      const generateButton = document.querySelector('#generateAdminToken');
      const toggleButton = document.querySelector('#toggleAdminToken');
      const setupMessage = document.querySelector('#setupMessage');

      function validate() {
        const token = tokenInput.value.trim();
        const confirmation = confirmInput.value.trim();
        tokenInput.setCustomValidity(token.length >= minLength ? '' : 'Use at least 16 characters for the admin token.');
        confirmInput.setCustomValidity(confirmation.length >= minLength ? '' : 'Use at least 16 characters for the confirmation.');
        if (token.length >= minLength && confirmation.length >= minLength && token !== confirmation) {
          confirmInput.setCustomValidity('The admin tokens did not match.');
        }
      }

      function randomToken(length) {
        const values = new Uint32Array(length);
        window.crypto.getRandomValues(values);
        let token = '';
        for (let index = 0; index < values.length; index += 1) {
          token += alphabet[values[index] % alphabet.length];
        }
        return token;
      }

      tokenInput.addEventListener('input', function () {
        validate();
        if (setupMessage.textContent) {
          setupMessage.textContent = '';
        }
      });
      confirmInput.addEventListener('input', validate);
      form.addEventListener('submit', function (event) {
        validate();
        if (!form.reportValidity()) {
          event.preventDefault();
        }
      });
      toggleButton.addEventListener('click', function () {
        const showing = tokenInput.type === 'text';
        tokenInput.type = showing ? 'password' : 'text';
        confirmInput.type = showing ? 'password' : 'text';
        toggleButton.textContent = showing ? 'Show token' : 'Hide token';
      });
      validate();

      if (!window.crypto || !window.crypto.getRandomValues) {
        generateButton.disabled = true;
        return;
      }
      generateButton.addEventListener('click', function () {
        const token = randomToken(generatedLength);
        tokenInput.value = token;
        confirmInput.value = token;
        setupMessage.textContent = 'Generated a 32-character token. Save it with your password manager before creating the admin token.';
        validate();
        tokenInput.focus();
      });
    })();
  </script>
</body>
</html>`
}

const dashboardHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Timich Agent</title>
  <style>
    :root { color-scheme: light dark; --bg: #f6f7f8; --fg: #17191c; --muted: #656d76; --line: #d8dee4; --panel: #ffffff; --accent: #0a7cff; --ok: #217a3a; --warn: #9a6700; --danger: #c9352b; }
    @media (prefers-color-scheme: dark) { :root { --bg: #111316; --fg: #f3f5f7; --muted: #a0a8b2; --line: #30363d; --panel: #1a1e23; --accent: #58a6ff; --ok: #7ee787; --warn: #d29922; --danger: #ff8178; } }
    * { box-sizing: border-box; }
    body { margin: 0; background: var(--bg); color: var(--fg); font: 14px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    header { position: sticky; top: 0; z-index: 1; border-bottom: 1px solid var(--line); background: color-mix(in srgb, var(--bg), transparent 8%); backdrop-filter: blur(16px); }
    .bar { width: min(1120px, calc(100vw - 32px)); margin: 0 auto; min-height: 64px; display: flex; align-items: center; justify-content: space-between; gap: 16px; }
    h1 { margin: 0; font-size: 20px; letter-spacing: 0; }
    h2 { margin: 0 0 12px; font-size: 16px; letter-spacing: 0; }
    main { width: min(1120px, calc(100vw - 32px)); margin: 24px auto 40px; display: grid; gap: 16px; }
    .grid { display: grid; grid-template-columns: repeat(12, 1fr); gap: 16px; }
    section { grid-column: span 6; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 16px; min-width: 0; }
    section.wide { grid-column: 1 / -1; }
    dl { display: grid; grid-template-columns: minmax(110px, 150px) 1fr; gap: 8px 12px; margin: 0; }
    dt { color: var(--muted); }
    dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
    table { width: 100%; border-collapse: collapse; }
    th, td { padding: 10px 8px; border-bottom: 1px solid var(--line); text-align: left; vertical-align: middle; }
    th { color: var(--muted); font-weight: 500; }
    label { display: grid; gap: 6px; color: var(--muted); font-size: 13px; }
    input, select { width: 100%; min-height: 38px; border: 1px solid var(--line); border-radius: 6px; padding: 0 10px; background: transparent; color: var(--fg); font: inherit; }
    button { min-height: 34px; border: 1px solid var(--line); border-radius: 6px; padding: 0 12px; background: transparent; color: var(--fg); font: inherit; cursor: pointer; }
    button.primary { border-color: var(--accent); background: var(--accent); color: #fff; font-weight: 600; }
    button.danger { color: var(--danger); }
    button:disabled { opacity: .55; cursor: default; }
    .actions { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
    .muted { color: var(--muted); }
    .section-note { margin: -4px 0 12px; max-width: 760px; color: var(--muted); }
    .status-ok { color: var(--ok); }
    .status-warn { color: var(--warn); }
    .status-failed { color: var(--danger); }
    .stack { display: grid; gap: 10px; }
    .pairing { display: grid; gap: 12px; margin-top: 12px; padding: 12px; border: 1px solid var(--line); border-radius: 8px; }
    .pairing-grid { display: grid; grid-template-columns: minmax(180px, 280px) minmax(0, 1fr); gap: 14px; align-items: start; }
    .pairing-qr { width: 100%; max-width: 280px; aspect-ratio: 1; padding: 10px; border: 1px solid var(--line); border-radius: 8px; background: #fff; }
    .pairing-qr-slot { width: 100%; max-width: 280px; min-height: 180px; display: grid; align-content: start; gap: 10px; }
    .pairing-link { min-width: 0; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; overflow-wrap: anywhere; color: var(--muted); }
    .pairing-code-row { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 8px; align-items: center; }
    .pairing-url-controls { display: grid; gap: 8px; }
    .code { min-width: 0; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 18px; overflow-wrap: anywhere; }
    .checks { display: grid; gap: 10px; }
    .check { display: grid; gap: 3px; padding-bottom: 10px; border-bottom: 1px solid var(--line); }
    .check:last-child { border-bottom: 0; padding-bottom: 0; }
    .notice { display: grid; gap: 10px; padding: 12px; border: 1px solid var(--line); border-radius: 8px; background: color-mix(in srgb, var(--accent), transparent 94%); }
    .notice.warn { border-color: color-mix(in srgb, var(--warn), transparent 45%); background: color-mix(in srgb, var(--warn), transparent 92%); }
    .notice.failed { border-color: color-mix(in srgb, var(--danger), transparent 55%); background: color-mix(in srgb, var(--danger), transparent 94%); }
    .guide { margin: 0; padding-left: 18px; color: var(--muted); }
    .guide li { margin: 4px 0; }
    .tasks { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; }
    .task { display: grid; gap: 4px; padding: 12px; border: 1px solid var(--line); border-radius: 8px; min-width: 0; }
    .task strong { display: flex; justify-content: space-between; gap: 8px; }
    form { margin: 0; }
    @media (max-width: 820px) { .grid { grid-template-columns: 1fr; } section { grid-column: 1; } .tasks { grid-template-columns: 1fr; } .pairing-grid { grid-template-columns: 1fr; } .bar { align-items: flex-start; flex-direction: column; padding: 14px 0; } dl { grid-template-columns: 1fr; } th:nth-child(3), td:nth-child(3) { display: none; } }
  </style>
</head>
<body>
  <header>
    <div class="bar">
      <div>
        <h1 id="agentName">Timich Agent</h1>
        <div class="muted" id="agentSubline">Loading...</div>
      </div>
      <form method="post" action="/logout"><button type="submit">Sign out</button></form>
    </div>
  </header>
  <main>
    <div class="grid">
      <section class="wide">
        <h2>Setup</h2>
        <div class="tasks" id="setupTasks"></div>
      </section>
      <section>
        <h2>Status</h2>
        <dl id="statusList"></dl>
      </section>
      <section>
        <h2>Agent Update</h2>
        <div class="stack" id="updateCheck">
          <div class="muted">Checking for updates...</div>
        </div>
      </section>
      <section>
        <h2>Remote Browsing</h2>
        <dl id="remoteBrowsingList"></dl>
      </section>
      <section class="wide">
        <h2>Datasource</h2>
        <form class="stack" id="datasourceForm">
          <label>Name
            <input id="datasourceName" name="name" autocomplete="off" placeholder="Immich">
          </label>
          <label>Immich URL
            <input id="datasourceURL" name="url" inputmode="url" autocomplete="off" placeholder="http://immich_server:2283" required>
          </label>
          <label>Immich API key
            <input id="datasourceAccessToken" name="accessToken" type="password" autocomplete="off" placeholder="Leave blank to keep existing key">
          </label>
          <div class="actions">
            <button class="primary" type="submit">Save datasource</button>
            <span class="muted" id="datasourceMessage"></span>
          </div>
        </form>
      </section>
      <section class="wide">
        <h2>Pair New Device</h2>
        <p class="muted">Create a one-time code for pairing the Timich app on a phone or tablet.</p>
        <div class="actions">
          <button class="primary" id="createPairing">Create device pairing code</button>
        </div>
        <div id="pairingResult"></div>
      </section>
      <section class="wide">
        <h2>Paired Devices</h2>
        <div id="devices"></div>
      </section>
      <section class="wide">
        <h2>Remote Browsing Readiness</h2>
        <p class="section-note">Remote browsing lets the Timich app browse through the Timich relay when the device is away from the home network. This check verifies datasource access and the relay path; pair at least one device first so the agent can register its relay credential.</p>
        <div class="actions">
          <button id="runCompatibility">Run readiness check</button>
          <span class="muted" id="compatSummary"></span>
        </div>
        <div class="checks" id="compatChecks"></div>
      </section>
      <section class="wide">
        <h2>Agent Controls</h2>
        <div class="actions">
          <button id="restartAgent">Restart Agent</button>
          <span class="muted" id="restartMessage"></span>
        </div>
      </section>
    </div>
  </main>
  <script>
    const statusList = document.querySelector('#statusList');
    const updateCheckNode = document.querySelector('#updateCheck');
    const setupTasks = document.querySelector('#setupTasks');
    const remoteBrowsingList = document.querySelector('#remoteBrowsingList');
    const datasourceForm = document.querySelector('#datasourceForm');
    const datasourceName = document.querySelector('#datasourceName');
    const datasourceURL = document.querySelector('#datasourceURL');
    const datasourceAccessToken = document.querySelector('#datasourceAccessToken');
    const datasourceMessage = document.querySelector('#datasourceMessage');
    const devicesNode = document.querySelector('#devices');
    const pairingResult = document.querySelector('#pairingResult');
    const compatSummary = document.querySelector('#compatSummary');
    const compatChecks = document.querySelector('#compatChecks');
    const restartMessage = document.querySelector('#restartMessage');

    async function api(path, options = {}) {
      const response = await fetch(path, { credentials: 'same-origin', ...options });
      if (response.status === 401) {
        location.href = '/login';
        throw new Error('unauthorized');
      }
      if (!response.ok) {
        let message = 'Request failed (' + response.status + ')';
        try {
          const payload = await response.json();
          message = payload.message || message;
        } catch (_) {}
        throw new Error(message);
      }
      if (response.status === 204) return null;
      return response.json();
    }

    function rows(items) {
      return items.map(([key, value]) => '<dt>' + escapeHTML(key) + '</dt><dd>' + escapeHTML(value ?? '') + '</dd>').join('');
    }

    function escapeHTML(value) {
      return String(value).replace(/[&<>"']/g, char => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[char]));
    }

    function date(value) {
      if (!value) return '';
      const parsed = new Date(value);
      return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
    }

    function compatibilityStatusLabel(status) {
      if (status === 'ok') return 'Ready';
      if (status === 'warning') return 'Setup incomplete';
      if (status === 'failed') return 'Needs attention';
      return status || '';
    }

    function compatibilityCheckLabel(name) {
      return ({
        agent_config: 'Agent configuration',
        datasource: 'Datasource',
        relay_server: 'Relay server',
        relay_connection: 'Relay connection',
      })[name] || name;
    }

    function taskStatusClass(status) {
      if (status === 'complete') return 'status-ok';
      if (status === 'skipped') return 'muted';
      return status === 'failed' ? 'status-failed' : 'status-warn';
    }

    function updateStatusClass(status) {
      if (status === 'update_available' || status === 'unsupported_platform') return 'status-warn';
      if (status === 'up_to_date') return 'status-ok';
      if (status === 'disabled') return 'muted';
      return 'status-failed';
    }

    function updateNoticeClass(status) {
      if (status === 'update_available' || status === 'unsupported_platform') return 'notice warn';
      if (status === 'up_to_date' || status === 'disabled') return 'notice';
      return 'notice failed';
    }

    function renderGuide(guide) {
      const steps = (guide && guide.dockerCompose) || [];
      if (!steps.length) {
        return '';
      }
      return '<ol class="guide">' + steps.map(step => '<li>' + escapeHTML(step) + '</li>').join('') + '</ol>';
    }

    function safeHref(value) {
      if (!value) return '';
      try {
        const url = new URL(value, window.location.href);
        return url.protocol === 'https:' || url.protocol === 'http:' ? url.href : '';
      } catch (_) {
        return '';
      }
    }

    function renderUpdateCheck(payload) {
      const status = payload.status || 'unavailable';
      const latest = payload.latestVersion ? 'Latest ' + payload.latestVersion : 'Latest unknown';
      const artifact = payload.artifact ? '<div class="muted">' + escapeHTML(payload.artifact.filename) + '</div>' : '';
      const notesHref = safeHref(payload.notesUrl);
      const downloadHref = safeHref(payload.artifact?.url);
      const notes = notesHref ? '<a href="' + escapeHTML(notesHref) + '" rel="noreferrer">Release notes</a>' : '';
      const download = downloadHref ? '<a href="' + escapeHTML(downloadHref) + '" rel="noreferrer">Download bundle</a>' : '';
      const links = [download, notes].filter(Boolean).join(' · ');
      updateCheckNode.innerHTML =
        '<div class="' + updateNoticeClass(status) + '">' +
          '<strong><span class="' + updateStatusClass(status) + '">' + escapeHTML(status.split('_').join(' ')) + '</span></strong>' +
          '<div>' + escapeHTML(payload.currentVersion ? 'Current ' + payload.currentVersion + ' / ' + latest : latest) + '</div>' +
          '<div class="muted">' + escapeHTML(payload.message || '') + '</div>' +
          artifact +
          (links ? '<div>' + links + '</div>' : '') +
          renderGuide(payload.guide) +
        '</div>';
    }

    async function loadUpdateCheck() {
      try {
        const payload = await api('/v1/update-check');
        renderUpdateCheck(payload);
      } catch (error) {
        updateCheckNode.innerHTML = '<div class="notice failed"><strong class="status-failed">Update check failed</strong><div class="muted">' + escapeHTML(error.message) + '</div></div>';
      }
    }

    function renderSetupTasks(tasks) {
      setupTasks.innerHTML = (tasks || []).map(task => {
        const status = task.status || 'pending';
        return '<div class="task"><strong><span>' + escapeHTML(task.label || task.id) + '</span><span class="' + taskStatusClass(status) + '">' + escapeHTML(status) + '</span></strong><span class="muted">' + escapeHTML(task.summary || '') + '</span></div>';
      }).join('');
    }

    async function copyText(value) {
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(value);
        return;
      }
      const textarea = document.createElement('textarea');
      textarea.value = value;
      textarea.setAttribute('readonly', '');
      textarea.style.position = 'fixed';
      textarea.style.top = '-1000px';
      textarea.style.left = '-1000px';
      document.body.appendChild(textarea);
      textarea.focus();
      textarea.select();
      try {
        if (!document.execCommand('copy')) {
          throw new Error('copy failed');
        }
      } finally {
        textarea.remove();
      }
    }

    async function loadStatus() {
      const status = await api('/status');
      document.querySelector('#agentName').textContent = status.agentName || 'Timich Agent';
      document.querySelector('#agentSubline').textContent = (status.mode || 'agent') + ' / ' + (status.version || 'dev') + ' / ' + (status.agentId || '');
      statusList.innerHTML = rows([
        ['Agent ID', status.agentId],
        ['Version', status.version],
        ['Uptime', String(status.uptimeSeconds || 0) + 's'],
        ['Admin', status.adminAuthReady ? 'Authenticated' : 'Unavailable'],
        ['Devices', String(status.pairedDeviceCount || 0) + ' / ' + String(status.deviceLimit || 0)],
        ['Pairing sessions', status.activePairingCount || 0],
        ['Media API', status.mediaListenAddress],
      ]);
      remoteBrowsingList.innerHTML = rows([
        ['Enabled', status.remoteBrowsing?.enabled ? 'Yes' : 'No'],
        ['Relay server', status.remoteBrowsing?.serverURL || ''],
        ['Registration', status.remoteBrowsing?.registrationStatus || ''],
        ['Registered at', date(status.remoteBrowsing?.relayCredentialSyncedAt)],
        ['Waiting for', (status.remoteBrowsing?.registrationBlockedBy || []).join(', ')],
        ['Datasources', (status.datasources || []).map(item => (item.name || item.kind) + ' (' + item.kind + ')').join(', ')],
      ]);
      renderSetupTasks(status.setupTasks || []);
      const primaryDatasource = (status.datasources || [])[0];
      datasourceName.value = primaryDatasource?.name || 'Immich';
      datasourceURL.value = primaryDatasource?.url || '';
      datasourceAccessToken.placeholder = primaryDatasource?.hasAccessToken ? 'Leave blank to keep existing key' : 'Required';
      if (!primaryDatasource) {
        datasourceMessage.textContent = 'No datasource configured';
        datasourceMessage.className = 'status-warn';
      } else if (datasourceMessage.textContent === 'No datasource configured') {
        datasourceMessage.textContent = '';
        datasourceMessage.className = 'muted';
      }
    }

    async function loadDevices() {
      const payload = await api('/v1/devices');
      const devices = payload.devices || [];
      if (devices.length === 0) {
        devicesNode.innerHTML = '<div class="muted">No paired devices</div>';
        return;
      }
      devicesNode.innerHTML = '<table><thead><tr><th>Name</th><th>Device ID</th><th>Last refreshed</th><th></th></tr></thead><tbody>' + devices.map(device =>
        '<tr>' +
          '<td>' + escapeHTML(device.deviceName || 'Unnamed device') + '</td>' +
          '<td>' + escapeHTML(device.deviceId) + '</td>' +
          '<td>' + escapeHTML(date(device.lastRefreshedAt)) + '</td>' +
          '<td><button class="danger" data-revoke="' + escapeHTML(device.deviceId) + '">Revoke</button></td>' +
        '</tr>'
      ).join('') + '</tbody></table>';
      devicesNode.querySelectorAll('[data-revoke]').forEach(button => {
        button.addEventListener('click', async () => {
          button.disabled = true;
          try {
            await api('/v1/devices/' + encodeURIComponent(button.dataset.revoke), { method: 'DELETE' });
            await Promise.all([loadStatus(), loadDevices()]);
          } catch (error) {
            alert(error.message);
            button.disabled = false;
          }
        });
      });
    }

    document.querySelector('#createPairing').addEventListener('click', async event => {
      const button = event.currentTarget;
      button.disabled = true;
      pairingResult.textContent = '';
      try {
        const pairing = await api('/v1/pairing-sessions', { method: 'POST' });
        const code = pairing.pairingCode || pairing.code || '';
        const choices = pairing.agentBaseURLChoices || [];
        const choiceOptions = choices.map(choice => '<option value="' + escapeHTML(choice.url) + '">' + escapeHTML(choice.label + ' / ' + choice.url) + '</option>').join('');
        pairingResult.innerHTML =
          '<div class="pairing">' +
            '<div class="muted">Copy the code manually to pair the device. QR/link pairing is optional.</div>' +
            '<div class="pairing-grid">' +
              '<div class="pairing-qr-slot" id="pairingQRSlot">' +
                '<div class="notice"><strong>QR code</strong><div class="muted">Select a phone-reachable Media API URL to show a QR code.</div></div>' +
              '</div>' +
              '<div class="stack">' +
                '<div class="muted">Device pairing code</div>' +
                '<div class="pairing-code-row"><div class="code">' + escapeHTML(code) + '</div><button type="button" id="copyPairingCode">Copy code</button></div>' +
                '<div class="muted">Expires ' + escapeHTML(date(pairing.expiresAt)) + '</div>' +
                '<div class="pairing-url-controls">' +
                  (choices.length ? '<label>Media API URL<select id="pairingAgentBaseURLSelect"><option value="">Select or enter URL</option>' + choiceOptions + '<option value="">Enter custom URL</option></select></label>' : '') +
                  '<label>' + (choices.length ? 'Selected or custom URL' : 'Media API URL') +
                    '<input id="pairingAgentBaseURL" inputmode="url" autocomplete="off" placeholder="http://AGENT_LAN_HOST:8082">' +
                  '</label>' +
                  '<div class="actions"><button type="button" id="showPairingQR">Show QR code</button><span class="muted" id="pairingQRStatus"></span></div>' +
                '</div>' +
                '<div id="pairingLinkResult"></div>' +
                '<div class="muted" id="copyPairingStatus"></div>' +
              '</div>' +
            '</div>' +
          '</div>';
        const copyButton = pairingResult.querySelector('#copyPairingCode');
        const copyStatus = pairingResult.querySelector('#copyPairingStatus');
        const qrSlot = pairingResult.querySelector('#pairingQRSlot');
        const qrStatus = pairingResult.querySelector('#pairingQRStatus');
        const linkResult = pairingResult.querySelector('#pairingLinkResult');
        const agentBaseURLInput = pairingResult.querySelector('#pairingAgentBaseURL');
        const agentBaseURLSelect = pairingResult.querySelector('#pairingAgentBaseURLSelect');
        const showQRButton = pairingResult.querySelector('#showPairingQR');
        let latestPairingURL = '';
        copyButton.addEventListener('click', async () => {
          copyButton.disabled = true;
          try {
            await copyText(code);
            copyStatus.textContent = 'Copied';
            copyStatus.className = 'status-ok';
          } catch (_) {
            copyStatus.textContent = 'Copy failed';
            copyStatus.className = 'status-failed';
          } finally {
            copyButton.disabled = false;
          }
        });
        async function showPairingLink() {
          const agentBaseURL = agentBaseURLInput.value.trim();
          if (!agentBaseURL) {
            qrStatus.textContent = 'Enter a Media API URL';
            qrStatus.className = 'status-warn';
            return;
          }
          showQRButton.disabled = true;
          qrStatus.textContent = 'Creating QR...';
          qrStatus.className = 'muted';
          try {
            const link = await api('/v1/pairing-links', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({
                agentBaseURL,
                pairingCode: code,
              }),
            });
            latestPairingURL = link.pairingURL || '';
            qrSlot.innerHTML = '<img class="pairing-qr" id="pairingQRCode" alt="Timich app pairing QR code" src="' + escapeHTML(link.pairingQRCodeDataURL || '') + '">';
            linkResult.innerHTML =
              '<div class="muted">Timich app link</div>' +
              '<div class="pairing-code-row"><div class="pairing-link">' + escapeHTML(latestPairingURL) + '</div><button type="button" id="copyPairingLink">Copy link</button></div>' +
              '<div class="muted">Agent URL ' + escapeHTML(link.pairingPayload?.agentBaseURL || agentBaseURL) + '</div>';
            const copyLinkButton = pairingResult.querySelector('#copyPairingLink');
            copyLinkButton.addEventListener('click', async () => {
              copyLinkButton.disabled = true;
              try {
                await copyText(latestPairingURL);
                copyStatus.textContent = 'Link copied';
                copyStatus.className = 'status-ok';
              } catch (_) {
                copyStatus.textContent = 'Copy failed';
                copyStatus.className = 'status-failed';
              } finally {
                copyLinkButton.disabled = false;
              }
            });
            qrStatus.textContent = 'QR ready';
            qrStatus.className = 'status-ok';
          } catch (error) {
            qrStatus.textContent = error.message;
            qrStatus.className = 'status-failed';
          } finally {
            showQRButton.disabled = false;
          }
        }
        if (agentBaseURLSelect) {
          agentBaseURLSelect.addEventListener('change', () => {
            agentBaseURLInput.value = agentBaseURLSelect.value;
            if (agentBaseURLSelect.value) {
              void showPairingLink();
            }
          });
        }
        showQRButton.addEventListener('click', () => {
          void showPairingLink();
        });
        await loadStatus();
      } catch (error) {
        pairingResult.innerHTML = '<div class="pairing status-failed">' + escapeHTML(error.message) + '</div>';
      } finally {
        button.disabled = false;
      }
    });

    document.querySelector('#runCompatibility').addEventListener('click', async event => {
      const button = event.currentTarget;
      button.disabled = true;
      compatSummary.textContent = 'Running...';
      compatChecks.innerHTML = '';
      try {
        const report = await api('/v1/compatibility-check', { method: 'POST' });
        compatSummary.textContent = compatibilityStatusLabel(report.status);
        compatSummary.className = report.status === 'ok' ? 'status-ok' : report.status === 'warning' ? 'status-warn' : 'status-failed';
        compatChecks.innerHTML = (report.checks || []).map(check => {
          const statusClass = check.status === 'ok' ? 'status-ok' : check.status === 'warning' ? 'status-warn' : 'status-failed';
          const remediation = check.remediation ? '<span class="muted">' + escapeHTML(check.remediation) + '</span>' : '';
          return '<div class="check"><strong>' + escapeHTML(compatibilityCheckLabel(check.name)) + ' <span class="' + statusClass + '">' + escapeHTML(compatibilityStatusLabel(check.status)) + '</span></strong><span>' + escapeHTML(check.summary || '') + '</span>' + remediation + '</div>';
        }).join('');
      } catch (error) {
        compatSummary.textContent = error.message;
        compatSummary.className = 'status-failed';
      } finally {
        button.disabled = false;
      }
    });

    datasourceForm.addEventListener('submit', async event => {
      event.preventDefault();
      const button = datasourceForm.querySelector('button[type="submit"]');
      button.disabled = true;
      datasourceMessage.textContent = 'Saving...';
      datasourceMessage.className = 'muted';
      const payload = {
        name: datasourceName.value,
        kind: 'immich',
        url: datasourceURL.value
      };
      if (datasourceAccessToken.value.trim() !== '') {
        payload.accessToken = datasourceAccessToken.value;
      }
      try {
        await api('/v1/datasource/primary', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        datasourceAccessToken.value = '';
        datasourceMessage.textContent = 'Saved. Checking datasource...';
        datasourceMessage.className = 'muted';
        await loadStatus();
        await checkDatasource();
      } catch (error) {
        datasourceMessage.textContent = error.message;
        datasourceMessage.className = 'status-failed';
      } finally {
        button.disabled = false;
      }
    });

    async function checkDatasource() {
      const check = await api('/v1/datasource/primary/check', { method: 'POST' });
      if (check.status === 'ok') {
        datasourceMessage.textContent = 'Saved. Datasource reachable from Agent.';
        datasourceMessage.className = 'status-ok';
        return;
      }
      const detail = check.remediation ? ' ' + check.remediation : '';
      datasourceMessage.textContent = (check.summary || 'Datasource check failed.') + detail;
      datasourceMessage.className = check.status === 'warning' ? 'status-warn' : 'status-failed';
    }

    document.querySelector('#restartAgent').addEventListener('click', async event => {
      const button = event.currentTarget;
      button.disabled = true;
      restartMessage.textContent = 'Restarting...';
      restartMessage.className = 'muted';
      try {
        await api('/v1/restart', { method: 'POST' });
        await pollReady();
        restartMessage.textContent = 'Back online';
        restartMessage.className = 'status-ok';
        location.reload();
      } catch (error) {
        restartMessage.textContent = error.message;
        restartMessage.className = 'status-failed';
        button.disabled = false;
      }
    });

    async function pollReady() {
      const deadline = Date.now() + 30000;
      while (Date.now() < deadline) {
        await new Promise(resolve => setTimeout(resolve, 1000));
        try {
          const response = await fetch('/readyz', { cache: 'no-store' });
          if (response.ok) return;
        } catch (_) {}
      }
      throw new Error('Restart requested, but the agent did not become ready in time.');
    }

    Promise.all([loadStatus(), loadDevices(), loadUpdateCheck()]).catch(error => {
      document.querySelector('#agentSubline').textContent = error.message;
    });
  </script>
</body>
</html>`
