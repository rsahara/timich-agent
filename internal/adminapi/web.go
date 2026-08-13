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
    :root { color-scheme: light dark; --bg: #f6f7f8; --fg: #17191c; --muted: #656d76; --line: #d8dee4; --panel: #ffffff; --accent: #0a7cff; --ok: #217a3a; --warn: #9a6700; --running: #6f42c1; --danger: #c9352b; }
    @media (prefers-color-scheme: dark) { :root { --bg: #111316; --fg: #f3f5f7; --muted: #a0a8b2; --line: #30363d; --panel: #1a1e23; --accent: #58a6ff; --ok: #7ee787; --warn: #d29922; --running: #a371f7; --danger: #ff8178; } }
    * { box-sizing: border-box; }
    body { margin: 0; background: var(--bg); color: var(--fg); font: 14px/1.45 -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    [hidden] { display: none !important; }
    header { position: sticky; top: 0; z-index: 1; border-bottom: 1px solid var(--line); background: color-mix(in srgb, var(--bg), transparent 8%); backdrop-filter: blur(16px); }
    .bar { width: min(1120px, calc(100vw - 32px)); margin: 0 auto; min-height: 64px; display: flex; align-items: center; justify-content: space-between; gap: 16px; }
    h1 { margin: 0; font-size: 20px; letter-spacing: 0; }
    h2 { margin: 0 0 12px; font-size: 16px; letter-spacing: 0; }
    main { width: min(1120px, calc(100vw - 32px)); margin: 24px auto 40px; display: grid; gap: 16px; }
    .tabs { width: min(1120px, calc(100vw - 32px)); margin: 0 auto; display: flex; gap: 8px; overflow-x: auto; padding: 0 0 12px; }
    .tab-link { min-height: 34px; display: inline-flex; align-items: center; justify-content: center; white-space: nowrap; border: 1px solid var(--line); border-radius: 999px; padding: 0 12px; color: var(--muted); text-decoration: none; font-weight: 600; }
    .tab-link.active { border-color: var(--accent); color: var(--accent); background: color-mix(in srgb, var(--accent), transparent 92%); }
    [data-tab-panel][hidden] { display: none !important; }
    .grid { display: grid; grid-template-columns: repeat(12, 1fr); gap: 16px; }
    section { grid-column: span 6; background: var(--panel); border: 1px solid var(--line); border-radius: 8px; padding: 16px; min-width: 0; }
    section.wide { grid-column: 1 / -1; }
    .section-head { display: flex; justify-content: space-between; align-items: center; gap: 12px; margin-bottom: 12px; }
    .section-head h2 { margin: 0; }
    dl { display: grid; grid-template-columns: minmax(110px, 150px) 1fr; gap: 8px 12px; margin: 0; }
    dt { color: var(--muted); }
    dd { margin: 0; min-width: 0; overflow-wrap: anywhere; }
    table { width: 100%; border-collapse: collapse; }
    th, td { padding: 10px 8px; border-bottom: 1px solid var(--line); text-align: left; vertical-align: middle; }
    th { color: var(--muted); font-weight: 500; }
    .datasource-task-table-wrap { overflow-x: auto; }
    .datasource-task-table { min-width: 980px; table-layout: fixed; }
    .datasource-task-activity { width: 104px; min-width: 104px; max-width: 104px; white-space: nowrap; }
    .datasource-task-status { width: 330px; min-width: 330px; max-width: 330px; white-space: nowrap; }
    .datasource-task-label { display: inline-flex; align-items: center; gap: 6px; }
    .datasource-task-note-trigger { width: 24px; min-width: 24px; height: 24px; min-height: 24px; padding: 0; border-radius: 999px; color: var(--muted); font-family: ui-serif, Georgia, serif; font-size: 15px; font-weight: 700; line-height: 1; }
    .datasource-task-note-trigger:hover, .datasource-task-note-trigger:focus-visible, .datasource-task-note-trigger[aria-expanded="true"] { border-color: var(--accent); color: var(--accent); outline: none; }
    .datasource-task-note-popover { position: fixed; z-index: 4; max-width: min(340px, calc(100vw - 24px)); padding: 10px 12px; border: 1px solid var(--line); border-radius: 8px; background: var(--panel); box-shadow: 0 8px 24px color-mix(in srgb, #000, transparent 78%); color: var(--fg); text-align: left; white-space: normal; }
    label { display: grid; gap: 6px; color: var(--muted); font-size: 13px; }
    input, select { width: 100%; min-height: 38px; border: 1px solid var(--line); border-radius: 6px; padding: 0 10px; background: transparent; color: var(--fg); font: inherit; }
    select { appearance: none; padding-right: 34px; background-image: linear-gradient(45deg, transparent 50%, var(--muted) 50%), linear-gradient(135deg, var(--muted) 50%, transparent 50%); background-position: calc(100% - 17px) 50%, calc(100% - 12px) 50%; background-size: 5px 5px, 5px 5px; background-repeat: no-repeat; }
    input[type="checkbox"] { width: 16px; height: 16px; min-height: 16px; padding: 0; margin: 0; accent-color: var(--accent); }
    button { min-height: 34px; border: 1px solid var(--line); border-radius: 6px; padding: 0 12px; background: transparent; color: var(--fg); font: inherit; cursor: pointer; }
    button.primary { border-color: var(--accent); background: var(--accent); color: #fff; font-weight: 600; }
    button.danger { color: var(--danger); }
    button:disabled { opacity: .55; cursor: default; }
    .actions { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; }
    .form-message { min-height: 20px; }
    .status-timestamp { margin-top: 12px; }
    .task-action-link { margin-top: 6px; }
    .field-note { margin-top: -4px; font-size: 13px; }
    details.disclosure { margin-top: 12px; border: 1px solid var(--line); border-radius: 8px; }
    details.disclosure > summary { min-height: 40px; display: flex; align-items: center; justify-content: space-between; gap: 10px; padding: 0 12px; cursor: pointer; font-weight: 600; list-style: none; }
    details.disclosure > summary::-webkit-details-marker { display: none; }
    details.disclosure > summary::after { content: "+"; display: grid; place-items: center; width: 34px; min-width: 34px; height: 34px; border: 1px solid var(--line); border-radius: 6px; color: var(--muted); font-weight: 500; line-height: 1; }
    details.disclosure[open] > summary { border-bottom: 1px solid var(--line); }
    details.disclosure[open] > summary::after { content: "-"; }
    details.disclosure > form { padding: 12px; }
    .muted { color: var(--muted); }
    .section-note { margin: -4px 0 12px; max-width: 760px; color: var(--muted); }
    .worker-note { display: grid; gap: 6px; }
    .worker-note p { margin: 0; }
    .status-ok { color: var(--ok); }
    .status-warn { color: var(--warn); }
    .status-running { color: var(--running); }
    .status-failed { color: var(--danger); }
    .stack { display: grid; gap: 10px; }
    .pairing { display: grid; gap: 12px; margin-top: 12px; padding: 12px; border: 1px solid var(--line); border-radius: 8px; }
    .pairing-grid { display: grid; grid-template-columns: minmax(180px, 280px) minmax(0, 1fr); gap: 14px; align-items: start; }
    .pairing-qr { width: 100%; max-width: 280px; aspect-ratio: 1; padding: 10px; border: 1px solid var(--line); border-radius: 8px; background: #fff; }
    .pairing-qr-slot { width: 100%; max-width: 280px; min-height: 180px; display: grid; align-content: start; gap: 10px; }
    .pairing-link { min-width: 0; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 12px; overflow-wrap: anywhere; color: var(--muted); }
    .pairing-code-row { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 8px; align-items: center; }
    .pairing-url-controls { display: grid; gap: 8px; }
    .pairing-methods { display: grid; gap: 18px; margin-top: 14px; }
    .pairing-method { display: grid; gap: 10px; }
    .pairing-method + .pairing-method { padding-top: 18px; border-top: 1px solid var(--line); }
    .pairing-method h3 { margin: 0; font-size: 14px; letter-spacing: 0; }
    .pairing-method p { margin: 0; max-width: 760px; color: var(--muted); }
    .code { min-width: 0; font-family: ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; font-size: 18px; overflow-wrap: anywhere; }
    .checks { display: grid; gap: 10px; margin-top: 12px; }
    .check { display: grid; gap: 3px; padding-bottom: 10px; border-bottom: 1px solid var(--line); }
    .check:last-child { border-bottom: 0; padding-bottom: 0; }
    .notice { display: grid; gap: 10px; padding: 12px; border: 1px solid var(--line); border-radius: 8px; background: color-mix(in srgb, var(--accent), transparent 94%); }
    .notice.warn { border-color: color-mix(in srgb, var(--warn), transparent 45%); background: color-mix(in srgb, var(--warn), transparent 92%); }
    .notice.failed { border-color: color-mix(in srgb, var(--danger), transparent 55%); background: color-mix(in srgb, var(--danger), transparent 94%); }
    .model-list { display: grid; gap: 10px; }
    .model-item { border: 1px solid var(--line); border-radius: 8px; overflow: hidden; }
    .model-item > summary { display: grid; grid-template-columns: minmax(0, 1fr) auto; gap: 12px; align-items: center; min-height: 52px; padding: 12px; cursor: pointer; list-style: none; }
    .model-item > summary::-webkit-details-marker { display: none; }
    .model-item[open] > summary { border-bottom: 1px solid var(--line); }
    .model-title { display: grid; gap: 4px; min-width: 0; }
    .model-title strong { overflow-wrap: anywhere; }
    .model-tags { display: flex; flex-wrap: wrap; gap: 6px; align-items: center; }
    .tag { display: inline-flex; min-height: 22px; align-items: center; border: 1px solid var(--line); border-radius: 999px; padding: 0 8px; color: var(--muted); font-size: 12px; font-weight: 600; }
    .tag.ok { border-color: color-mix(in srgb, var(--ok), transparent 50%); color: var(--ok); }
    .tag.warn { border-color: color-mix(in srgb, var(--warn), transparent 45%); color: var(--warn); }
    .tag.failed { border-color: color-mix(in srgb, var(--danger), transparent 55%); color: var(--danger); }
    .model-body { display: grid; gap: 12px; padding: 12px; }
    .model-meta { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; }
    .model-meta-item { min-width: 0; display: grid; gap: 2px; }
    .model-meta-label { color: var(--muted); font-size: 12px; font-weight: 600; }
    .model-meta-value { min-width: 0; overflow-wrap: anywhere; }
    .guide { margin: 0; padding-left: 18px; color: var(--muted); }
    .guide li { margin: 4px 0; }
    .tasks { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 10px; }
    .task { display: grid; gap: 4px; padding: 12px; border: 1px solid var(--line); border-radius: 8px; min-width: 0; }
    .task strong { display: flex; justify-content: space-between; gap: 8px; }
    .device-list { display: grid; gap: 12px; }
    .device-card { display: grid; gap: 14px; padding: 14px; border: 1px solid var(--line); border-radius: 8px; }
    .device-summary { display: grid; grid-template-columns: minmax(260px, 1.2fr) minmax(200px, .8fr) auto; gap: 12px; align-items: start; }
    .device-summary-item { min-width: 0; display: grid; gap: 3px; }
    .device-summary-label, .device-subsection-title, .device-meta-label { color: var(--muted); font-size: 12px; font-weight: 600; }
    .device-summary-value { min-width: 0; font-size: 15px; font-weight: 600; overflow-wrap: anywhere; }
    .device-name-form { display: grid; grid-template-columns: minmax(180px, 320px) auto minmax(120px, 1fr); gap: 8px; align-items: end; }
    .device-meta { display: flex; flex-wrap: wrap; justify-content: flex-end; align-items: center; gap: 8px 12px; text-align: right; }
    .device-meta-text { display: grid; gap: 2px; }
    .device-subsection { display: grid; gap: 10px; padding-top: 12px; border-top: 1px solid var(--line); }
    .device-upload-head { display: flex; flex-wrap: wrap; justify-content: space-between; gap: 8px 16px; align-items: flex-start; }
    .device-upload-status { display: grid; gap: 2px; justify-items: end; text-align: right; }
    .checkbox-label { min-height: 38px; display: flex; align-items: center; gap: 8px; color: var(--fg); font-size: 14px; }
    .device-upload-form { display: grid; grid-template-columns: minmax(120px, .45fr) minmax(170px, .9fr) minmax(240px, 1.2fr) minmax(210px, .9fr) auto; gap: 10px; align-items: end; }
    .device-reset-section { display: grid; gap: 10px; padding-top: 10px; border-top: 1px dashed var(--line); }
    .device-reset-title { color: var(--muted); font-size: 13px; font-weight: 600; }
    .device-reset-form { display: grid; grid-template-columns: repeat(2, minmax(180px, 1fr)) auto; gap: 10px; align-items: end; }
    .search-preview-form { display: grid; grid-template-columns: minmax(220px, 1fr) auto; gap: 8px; align-items: end; }
    .asset-preview-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(136px, 1fr)); gap: 10px; }
    .asset-preview-card { min-width: 0; display: grid; gap: 6px; }
    .asset-preview-thumb { width: 100%; aspect-ratio: 4 / 3; object-fit: cover; border: 1px solid var(--line); border-radius: 6px; background: var(--bg); }
    .asset-preview-title { min-width: 0; overflow-wrap: anywhere; font-size: 12px; font-weight: 600; }
    .asset-preview-date { color: var(--muted); font-size: 12px; }
    form { margin: 0; }
    @media (max-width: 820px) { .grid { grid-template-columns: 1fr; } section { grid-column: 1; } .tasks { grid-template-columns: 1fr; } .pairing-grid { grid-template-columns: 1fr; } .bar { align-items: flex-start; flex-direction: column; padding: 14px 0; } dl { grid-template-columns: 1fr; } th:nth-child(3), td:nth-child(3) { display: none; } .model-item > summary, .model-meta, .device-summary, .device-upload-head { grid-template-columns: 1fr; } .device-name-form { grid-template-columns: 1fr; } .device-meta { justify-content: flex-start; text-align: left; } .device-upload-status { justify-items: start; text-align: left; } .device-upload-form, .device-reset-form, .search-preview-form { grid-template-columns: 1fr; } }
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
    <nav class="tabs" aria-label="Admin sections">
      <a class="tab-link" href="#overview" data-tab-link="overview">Overview</a>
      <a class="tab-link" href="#datasources" data-tab-link="datasources">Datasources</a>
      <a class="tab-link" href="#tasks" data-tab-link="tasks">Tasks</a>
      <a class="tab-link" href="#search" data-tab-link="search">Search</a>
      <a class="tab-link" href="#devices" data-tab-link="devices">Devices</a>
      <a class="tab-link" href="#system" data-tab-link="system">System</a>
    </nav>
  </header>
  <main>
    <div class="grid">
      <section class="wide" data-tab-panel="overview">
        <h2>Setup</h2>
        <div class="tasks" id="setupTasks"></div>
      </section>
      <section data-tab-panel="overview">
        <div class="section-head">
          <h2>Status</h2>
          <button type="button" data-refresh-action="status">Refresh</button>
        </div>
        <dl id="statusList"></dl>
      </section>
      <section data-tab-panel="overview">
        <div class="section-head">
          <h2>Agent Update</h2>
          <button type="button" data-refresh-action="update">Refresh</button>
        </div>
        <div class="stack" id="updateCheck">
          <div class="muted">Checking for updates...</div>
        </div>
      </section>
      <section data-tab-panel="overview">
        <h2>Remote Browsing</h2>
        <dl id="remoteBrowsingList"></dl>
      </section>
      <section class="wide" data-tab-panel="datasources">
        <h2>Datasources</h2>
        <div class="stack" id="datasourceList"></div>
        <details class="disclosure" id="datasourceAddPanel">
          <summary>Add datasource</summary>
          <form class="stack" id="datasourceForm">
            <label>Type
              <select id="datasourceKind" name="kind">
                <option value="immich">Immich (Passthrough)</option>
                <option value="immich_indexed">Immich (Indexed)</option>
                <option value="local_filesystem">Local datasource</option>
              </select>
            </label>
            <label>Name
              <input id="datasourceName" name="name" autocomplete="off" placeholder="Immich">
            </label>
            <label data-datasource-kind-field="immich">Immich URL
              <input id="datasourceURL" name="url" inputmode="url" autocomplete="off" placeholder="http://immich_server:2283" required>
            </label>
            <label data-datasource-kind-field="immich">Immich API key
              <input id="datasourceAccessToken" name="accessToken" type="password" autocomplete="off" placeholder="Required for Immich">
            </label>
            <label data-datasource-kind-field="local_filesystem" hidden>Local media root
              <select id="datasourceRootKey" name="rootKey"></select>
            </label>
            <div class="muted field-note" id="datasourceRootHint" data-datasource-kind-field="local_filesystem" hidden></div>
            <div class="muted field-note" id="datasourceTopologyHint"></div>
            <div class="actions">
              <button class="primary" type="submit">Add datasource</button>
            </div>
            <div class="muted form-message" id="datasourceMessage"></div>
          </form>
        </details>
      </section>
      <section class="wide" data-tab-panel="datasources">
        <div class="section-head">
          <h2>Datasource Health</h2>
          <button type="button" data-refresh-action="datasource-status">Refresh</button>
        </div>
        <p class="section-note">Datasource access and catalog registration health.</p>
        <div class="stack" id="datasourceStatus"></div>
      </section>
      <section class="wide" data-tab-panel="tasks">
        <h2>Datasource Tasks</h2>
        <p class="section-note">Background work by phase.</p>
        <div class="stack">
          <div class="stack" id="datasourceTaskStatus"></div>
          <div class="muted form-message" id="datasourceIndexingMessage"></div>
        </div>
      </section>
      <section class="wide" data-tab-panel="tasks">
        <h2>System Resources</h2>
        <p class="section-note">Current host load and storage headroom for background indexing work.</p>
        <div class="stack" id="systemResourcesStatus"></div>
        <div class="muted form-message" id="systemResourcesMessage" hidden></div>
      </section>
      <section data-tab-panel="tasks">
        <h2>Background Workers</h2>
        <div class="section-note worker-note">
          <p>Limits concurrent metadata, thumbnail, video preview, content verification, semantic embedding, and search-index publishing jobs.</p>
          <p>Content verification, semantic embedding, and publishing use at most 1 worker; remaining workers can continue metadata or thumbnail work. Media discovery and status checks run outside this limit.</p>
          <p>After pausing, in-flight work continues until its current batch completes. Queued work then shows paused.</p>
        </div>
        <form class="stack" id="workerSettingsForm">
          <label>Max workers
            <select id="heavyTaskWorkersMode" name="heavyTaskWorkersMode"></select>
          </label>
          <label id="heavyTaskWorkersCustomField" hidden>Number of workers
            <input id="heavyTaskWorkersCustom" name="heavyTaskWorkersCustom" type="number" min="0" step="1" inputmode="numeric" placeholder="Number of workers">
          </label>
          <div class="actions">
            <button class="primary" type="submit">Save setting</button>
            <span class="muted" id="workerRuntimeMessage"></span>
          </div>
        </form>
      </section>
      <section class="wide" data-tab-panel="search">
        <div class="section-head">
          <h2>Search Coverage</h2>
          <button type="button" data-refresh-action="datasource-status">Refresh</button>
        </div>
        <p class="section-note">How much of the registered media library is available to semantic search.</p>
        <div class="stack" id="searchCoverageStatus"></div>
      </section>
      <section class="wide" data-tab-panel="search">
        <div class="section-head">
          <h2>Semantic Models</h2>
          <button type="button" data-refresh-action="semantic-models">Refresh</button>
        </div>
        <p class="section-note">Semantic search is available when one installed model is active. Embedding indexing progress is shown in Datasource Tasks.</p>
        <div class="stack" id="semanticModelStatus"></div>
        <div class="actions">
          <span class="muted" id="semanticModelMessage"></span>
        </div>
      </section>
      <section class="wide" data-tab-panel="search">
        <h2>Semantic Search Preview</h2>
        <div class="stack">
          <form id="semanticSearchPreviewForm" class="search-preview-form">
            <label>Query
              <input id="semanticSearchPreviewQuery" name="query" autocomplete="off">
            </label>
            <label>Model
              <select id="semanticSearchPreviewModel" name="semanticModelId">
                <option value="">Auto</option>
              </select>
            </label>
            <button class="primary" type="submit">Search</button>
          </form>
          <span class="muted" id="semanticSearchPreviewMessage"></span>
          <div class="stack" id="semanticSearchPreviewResult"></div>
        </div>
      </section>
      <section class="wide" data-tab-panel="devices">
        <h2>Pair New Device</h2>
        <p class="section-note">Approve a Nearby Link code shown on a local device, or create a manual one-time code for fallback pairing.</p>
        <div class="pairing-methods">
          <div class="pairing-method">
            <h3>Nearby Link</h3>
            <p>Approve the Link Code shown on a nearby app or TV.</p>
            <form class="stack" id="nearbyLinkForm">
              <label>Nearby Link Code
                <input id="nearbyLinkCode" inputmode="numeric" autocomplete="off" placeholder="123 456">
              </label>
              <div class="actions">
                <button class="primary" type="submit">Approve Nearby Link</button>
                <span class="muted" id="nearbyLinkMessage"></span>
              </div>
            </form>
          </div>
          <div class="pairing-method">
            <h3>Manual pairing code</h3>
            <p>Create a one-time code for devices that cannot use Nearby Link.</p>
            <div class="actions">
              <button class="primary" id="createPairing">Create device pairing code</button>
            </div>
            <div id="pairingResult"></div>
          </div>
        </div>
      </section>
      <section class="wide" data-tab-panel="devices">
        <h2>Device List</h2>
        <div id="devices"></div>
      </section>
      <section class="wide" data-tab-panel="devices">
        <h2>Device Uploads</h2>
        <p class="section-note">Upload destinations are configured by the administrator. The app can only choose its local upload mode.</p>
        <div class="stack" id="uploadRoots"></div>
      </section>
      <section class="wide" data-tab-panel="system">
        <h2>Remote Browsing Readiness</h2>
        <p class="section-note">Remote browsing lets the Timich app browse through the Timich relay when the device is away from the home network. This check verifies datasource access and the relay path; pair at least one device first so the agent can register its relay credential.</p>
        <div class="actions">
          <button id="runCompatibility">Run readiness check</button>
          <span class="muted" id="compatSummary"></span>
        </div>
        <div class="checks" id="compatChecks"></div>
      </section>
      <section data-tab-panel="system">
        <h2>Catalog Integrity</h2>
        <p class="section-note">Exact duplicate media can be stored by more than one datasource, while gallery and search show one canonical asset.</p>
        <div class="stack" id="catalogDedupStatus"></div>
      </section>
      <section class="wide" data-tab-panel="system">
        <h2>Agent Controls</h2>
        <div class="actions">
          <button id="restartAgent">Restart Agent</button>
          <span class="muted" id="restartMessage"></span>
        </div>
      </section>
    </div>
  </main>
  <script id="initialDatasourceIndexingStatus" type="application/json">null</script>
  <script>
    const tabLinks = Array.from(document.querySelectorAll('[data-tab-link]'));
    const tabPanels = Array.from(document.querySelectorAll('[data-tab-panel]'));
    const statusList = document.querySelector('#statusList');
    const workerSettingsForm = document.querySelector('#workerSettingsForm');
    const heavyTaskWorkersMode = document.querySelector('#heavyTaskWorkersMode');
    const heavyTaskWorkersCustomField = document.querySelector('#heavyTaskWorkersCustomField');
    const heavyTaskWorkersCustom = document.querySelector('#heavyTaskWorkersCustom');
    const workerRuntimeMessage = document.querySelector('#workerRuntimeMessage');
    const updateCheckNode = document.querySelector('#updateCheck');
    const setupTasks = document.querySelector('#setupTasks');
    const remoteBrowsingList = document.querySelector('#remoteBrowsingList');
    const datasourceList = document.querySelector('#datasourceList');
    const datasourceAddPanel = document.querySelector('#datasourceAddPanel');
    const datasourceForm = document.querySelector('#datasourceForm');
    const datasourceKind = document.querySelector('#datasourceKind');
    const datasourceName = document.querySelector('#datasourceName');
    const datasourceURL = document.querySelector('#datasourceURL');
    const datasourceAccessToken = document.querySelector('#datasourceAccessToken');
    const datasourceRootKey = document.querySelector('#datasourceRootKey');
    const datasourceRootHint = document.querySelector('#datasourceRootHint');
    const datasourceKindFields = Array.from(document.querySelectorAll('[data-datasource-kind-field]'));
    const datasourceMessage = document.querySelector('#datasourceMessage');
    const datasourceTaskStatus = document.querySelector('#datasourceTaskStatus');
    const initialDatasourceIndexingStatusNode = document.querySelector('#initialDatasourceIndexingStatus');
    const datasourceStatusNode = document.querySelector('#datasourceStatus');
    const searchCoverageStatus = document.querySelector('#searchCoverageStatus');
    const catalogDedupStatus = document.querySelector('#catalogDedupStatus');
    const datasourceIndexingMessage = document.querySelector('#datasourceIndexingMessage');
    const systemResourcesStatus = document.querySelector('#systemResourcesStatus');
    const systemResourcesMessage = document.querySelector('#systemResourcesMessage');
    const semanticModelStatus = document.querySelector('#semanticModelStatus');
    const semanticModelMessage = document.querySelector('#semanticModelMessage');
    const semanticSearchPreviewForm = document.querySelector('#semanticSearchPreviewForm');
    const semanticSearchPreviewQuery = document.querySelector('#semanticSearchPreviewQuery');
    const semanticSearchPreviewModel = document.querySelector('#semanticSearchPreviewModel');
    const semanticSearchPreviewMessage = document.querySelector('#semanticSearchPreviewMessage');
    const semanticSearchPreviewResult = document.querySelector('#semanticSearchPreviewResult');
    const nearbyLinkForm = document.querySelector('#nearbyLinkForm');
    const nearbyLinkCode = document.querySelector('#nearbyLinkCode');
    const nearbyLinkMessage = document.querySelector('#nearbyLinkMessage');
    const devicesNode = document.querySelector('#devices');
    const uploadRootsNode = document.querySelector('#uploadRoots');
    const pairingResult = document.querySelector('#pairingResult');
    const compatSummary = document.querySelector('#compatSummary');
    const compatChecks = document.querySelector('#compatChecks');
    const restartMessage = document.querySelector('#restartMessage');
    const semanticSearchPreviewTimeoutMs = 25000;
    let latestSetupTasks = [];
    let activeDatasourceURL = '';
    let latestDatasourceCheck = null;
    let datasourceCheckGeneration = 0;
    let latestDatasources = [];
    let latestUploadRoots = [];
    let latestLocalMediaRoots = [];
    let latestDatasourceIndexingStatus = { datasources: [] };
    let latestWorkerRuntimeStatus = null;
    let datasourceIndexingLoaded = false;
    let latestSemanticModelStatus = null;
    let semanticModelsLoaded = false;
    let semanticModelsLoadingRetryTimer = null;
    let semanticModelsLoadingRetryAttempts = 0;
    let latestSemanticInstallJob = null;
    let semanticInstallJobTimer = null;
    let datasourceAddPanelTouched = false;
    let syncingDatasourceAddPanel = false;
    let liveStatusTimer = null;
    let datasourceIndexingRefreshInFlight = false;
    let systemResourcesRefreshInFlight = false;
    const pendingDatasourceTaskActions = new Set();
    const liveStatusRefreshMs = 5000;
    const semanticModelsLoadingRetryMs = 1500;
    const semanticModelsLoadingRetryMaxAttempts = 20;
    const datasourceIndexingSessionStorageKey = 'timich.admin.datasourceIndexingStatus.v1';
    const datasourceIndexingSessionCacheMaxAgeMs = 5 * 60 * 1000;
    let activeDatasourceTaskNote = null;
    let pendingDatasourceTaskRender = null;

    function normalizeTab(value) {
      const tab = String(value || '').replace(/^#/, '').trim().toLowerCase();
      return tabLinks.some(link => link.dataset.tabLink === tab) ? tab : '';
    }

    function activeTabFromLocation() {
      return normalizeTab(location.hash) || 'overview';
    }

    function setActiveTab(tab) {
      const active = normalizeTab(tab) || 'overview';
      tabLinks.forEach(link => {
        const selected = link.dataset.tabLink === active;
        link.classList.toggle('active', selected);
        link.setAttribute('aria-selected', selected ? 'true' : 'false');
      });
      tabPanels.forEach(panel => {
        panel.hidden = panel.dataset.tabPanel !== active;
      });
      syncLiveStatusRefresh();
    }

    window.addEventListener('hashchange', () => {
      setActiveTab(activeTabFromLocation());
    });
    document.addEventListener('visibilitychange', syncLiveStatusRefresh);
    setActiveTab(activeTabFromLocation());

    function shouldPollLiveStatus() {
      return activeTabFromLocation() === 'tasks' && document.visibilityState !== 'hidden';
    }

    function syncLiveStatusRefresh() {
      if (shouldPollLiveStatus()) {
        refreshLiveStatus({ preserveOnError: true });
        if (!liveStatusTimer) {
          liveStatusTimer = window.setInterval(() => {
            refreshLiveStatus({ preserveOnError: true });
          }, liveStatusRefreshMs);
        }
        return;
      }
      if (liveStatusTimer) {
        window.clearInterval(liveStatusTimer);
        liveStatusTimer = null;
      }
    }

    function refreshLiveStatus(options = {}) {
      if (!datasourceIndexingRefreshInFlight) {
        datasourceIndexingRefreshInFlight = true;
        loadDatasourceIndexingStatus(options).finally(() => {
          datasourceIndexingRefreshInFlight = false;
        });
      }
      if (!systemResourcesRefreshInFlight) {
        systemResourcesRefreshInFlight = true;
        loadSystemResources(options).finally(() => {
          systemResourcesRefreshInFlight = false;
        });
      }
    }

    async function api(path, options = {}) {
      const response = await fetch(path, { credentials: 'same-origin', cache: 'no-store', ...options });
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

    function taskWaitActivity(value) {
      if (!value) return '';
      const parsed = new Date(value);
      if (Number.isNaN(parsed.getTime())) return '';
      const remainingMs = parsed.getTime() - Date.now();
      if (remainingMs <= 0) return 'synchronizing';
      if (remainingMs < 60 * 1000) return 'waiting';
      const remainingMinutes = Math.ceil(remainingMs / (60 * 1000));
      if (remainingMinutes < 90) return 'waiting ' + remainingMinutes + ' m.';
      const remainingHours = Math.ceil(remainingMinutes / 60);
      return 'waiting ' + remainingHours + ' h.';
    }

    function formatBytes(value) {
      const bytes = Number(value || 0);
      if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
      const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
      let scaled = bytes;
      let unitIndex = 0;
      while (scaled >= 1024 && unitIndex < units.length - 1) {
        scaled /= 1024;
        unitIndex += 1;
      }
      return (unitIndex === 0 ? String(Math.round(scaled)) : scaled.toFixed(1)) + ' ' + units[unitIndex];
    }

    function formatPercent(value) {
      const percent = Number(value);
      if (!Number.isFinite(percent)) return '';
      return percent.toFixed(percent >= 10 ? 0 : 1) + '%';
    }

    function formatLoad(value) {
      const load = Number(value);
      if (!Number.isFinite(load)) return '';
      return load.toFixed(2);
    }

    function formatTemperature(value) {
      const celsius = Number(value);
      if (!Number.isFinite(celsius)) return '';
      return celsius.toFixed(1) + ' °C';
    }

    function formatCount(value) {
      const count = Number(value || 0);
      if (!Number.isFinite(count)) return '0';
      return Math.round(count).toLocaleString();
    }

    function formatUsedAvailable(usedBytes, totalBytes, usedPercent) {
      const total = Number(totalBytes || 0);
      if (!Number.isFinite(total) || total <= 0) return '';
      const used = Math.max(0, Number(usedBytes || 0));
      const percent = formatPercent(usedPercent);
      return formatBytes(used) + ' / ' + formatBytes(total) + (percent ? ' (' + percent + ')' : '');
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
        datasource: 'Datasources',
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
      const currentIdentity = payload.currentReleaseTag || payload.currentVersion || '';
      const latestIdentity = payload.latestReleaseTag || payload.latestVersion || '';
      const latest = latestIdentity ? 'Latest ' + latestIdentity : 'Latest unknown';
      const artifact = payload.artifact ? '<div class="muted">' + escapeHTML(payload.artifact.filename) + '</div>' : '';
      const notesHref = safeHref(payload.notesUrl);
      const downloadHref = safeHref(payload.artifact?.url);
      const notes = notesHref ? '<a href="' + escapeHTML(notesHref) + '" rel="noreferrer">Release notes</a>' : '';
      const download = downloadHref ? '<a href="' + escapeHTML(downloadHref) + '" rel="noreferrer">Download bundle</a>' : '';
      const links = [download, notes].filter(Boolean).join(' · ');
      updateCheckNode.innerHTML =
        '<div class="' + updateNoticeClass(status) + '">' +
          '<strong><span class="' + updateStatusClass(status) + '">' + escapeHTML(status.split('_').join(' ')) + '</span></strong>' +
          '<div>' + escapeHTML(currentIdentity ? 'Current ' + currentIdentity + ' / ' + latest : latest) + '</div>' +
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
      latestSetupTasks = (tasks || []).map(task => ({ ...task }));
      if (latestDatasourceCheck && latestDatasourceCheck.datasourceURL === activeDatasourceURL) {
        latestSetupTasks = latestSetupTasks.map(task => task.id === 'datasource' ? { ...task, ...datasourceTaskPatch(latestDatasourceCheck.check) } : task);
      }
      setupTasks.innerHTML = latestSetupTasks.map(task => {
        const status = task.status || 'pending';
        return '<div class="task"><strong><span>' + escapeHTML(task.label || task.id) + '</span><span class="' + taskStatusClass(status) + '">' + escapeHTML(status) + '</span></strong><span class="muted">' + escapeHTML(task.summary || '') + '</span></div>';
      }).join('');
    }

    function renderSystemResources(status) {
      const cpu = status.cpu || {};
      const memory = status.memory || {};
      const cpuLoad = cpu.load1 == null ? (cpu.message || 'unavailable') : formatLoad(cpu.load1) + ' / ' + formatLoad(cpu.load5) + ' / ' + formatLoad(cpu.load15) + ' (1m / 5m / 15m)';
      const cpuUsage = cpu.usagePercent == null ? 'collecting' : formatPercent(cpu.usagePercent);
      const cpuIOWait = cpu.ioWaitPercent == null ? 'collecting' : formatPercent(cpu.ioWaitPercent);
      const cpuTemperature = cpu.temperatureCelsius == null ? '' : formatTemperature(cpu.temperatureCelsius);
      const memoryUsage = memory.totalBytes ? formatUsedAvailable(memory.usedBytes, memory.totalBytes, memory.usedPercent) : (memory.message || 'unavailable');
      const diskRows = (status.disks || []).map(disk => {
        const usage = disk.totalBytes ? formatUsedAvailable(disk.usedBytes, disk.totalBytes, disk.usedPercent) : (disk.message || 'unavailable');
        const free = disk.availableBytes ? formatBytes(disk.availableBytes) + ' free' : '';
        return '<tr>' +
          '<td>' + escapeHTML(disk.label || '') + '<div class="muted">' + escapeHTML(disk.path || '') + '</div></td>' +
          '<td>' + escapeHTML(usage) + '<div class="muted">' + escapeHTML(free) + '</div></td>' +
        '</tr>';
      }).join('');
      systemResourcesStatus.innerHTML =
        '<dl>' + rows([
          ['System load', cpuLoad],
          ['CPU usage', cpuUsage],
          ['I/O wait', cpuIOWait],
          ...(cpuTemperature ? [['CPU temperature', cpuTemperature]] : []),
          ['Memory usage', memoryUsage],
        ]) + '</dl>' +
        (diskRows ? '<table><thead><tr><th>Disk</th><th>Usage</th></tr></thead><tbody>' + diskRows + '</tbody></table>' : '<div class="notice warn"><strong>No disk paths</strong><div class="muted">No Agent data or media paths are configured.</div></div>');
    }

    async function loadSystemResources(options = {}) {
      try {
        const status = await api('/v1/system/resources');
        renderSystemResources(status);
        systemResourcesMessage.textContent = '';
        systemResourcesMessage.className = 'muted form-message status-timestamp';
        systemResourcesMessage.hidden = true;
      } catch (error) {
        if (!options.preserveOnError || !systemResourcesStatus.innerHTML.trim()) {
          systemResourcesStatus.innerHTML = '<div class="notice failed"><strong class="status-failed">System resources unavailable</strong><div class="muted">' + escapeHTML(error.message) + '</div></div>';
          systemResourcesMessage.textContent = '';
          systemResourcesMessage.hidden = true;
          return;
        }
        systemResourcesMessage.textContent = 'Agent seems busy; retrying resource status... ' + error.message;
        systemResourcesMessage.className = 'muted form-message status-timestamp status-warn';
        systemResourcesMessage.hidden = false;
      }
    }

    function renderWorkerRuntime(status) {
      latestWorkerRuntimeStatus = status || null;
      renderWorkerModeOptions(status || {});
      rerenderDatasourceTasksForWorkerRuntime();
    }

    function rerenderDatasourceTasksForWorkerRuntime() {
      const tasks = latestDatasourceIndexingStatus?.tasks || [];
      if (!tasks.length) return;
      const datasources = latestDatasourceIndexingStatus?.datasources || [];
      const hasLocalDatasource = datasources.some(datasource => datasource.kind === 'local_filesystem');
      const hasIndexedDatasource = datasources.some(datasource => Boolean(datasource.indexingEnabled));
      renderDatasourceTasks(tasks, { hasDatasources: datasources.length > 0, hasIndexedDatasource, hasLocalDatasource });
    }

    function renderWorkerModeOptions(status) {
      const hostCPUCount = Math.max(1, Number(status.hostCpuCount || 1));
      const autoWorkers = Math.max(1, Math.floor(hostCPUCount / 2));
      const maxPreset = Math.max(0, hostCPUCount - 1);
      const configured = typeof status.configuredHeavyTaskWorkers === 'number' ? status.configuredHeavyTaskWorkers : null;
      const autoLabel = String(autoWorkers) + ' worker' + (autoWorkers === 1 ? '' : 's');
      const options = ['<option value="auto">Auto (' + autoLabel + ')</option>'];
      for (let workers = 0; workers <= maxPreset; workers++) {
        let label;
        if (workers === 0) {
          label = 'Paused (0 workers)';
        } else {
          label = String(workers) + ' worker' + (workers === 1 ? '' : 's');
        }
        options.push('<option value="fixed:' + workers + '">' + label + '</option>');
      }
      options.push('<option value="custom">Custom</option>');
      heavyTaskWorkersMode.innerHTML = options.join('');
      if (configured === null) {
        heavyTaskWorkersMode.value = 'auto';
        heavyTaskWorkersCustom.value = '';
      } else if (configured >= 0 && configured <= maxPreset) {
        heavyTaskWorkersMode.value = 'fixed:' + configured;
        heavyTaskWorkersCustom.value = '';
      } else {
        heavyTaskWorkersMode.value = 'custom';
        heavyTaskWorkersCustom.value = String(configured);
      }
      updateWorkerCustomField();
    }

    function updateWorkerCustomField() {
      heavyTaskWorkersCustomField.hidden = heavyTaskWorkersMode.value !== 'custom';
    }

    function selectedWorkerCount() {
      if (heavyTaskWorkersMode.value === 'auto') return null;
      if (heavyTaskWorkersMode.value === 'custom') {
        const custom = Number.parseInt(heavyTaskWorkersCustom.value || '', 10);
        if (!Number.isFinite(custom) || custom < 0) throw new Error('Enter a non-negative custom worker count.');
        return custom;
      }
      if (heavyTaskWorkersMode.value.startsWith('fixed:')) {
        const fixed = Number.parseInt(heavyTaskWorkersMode.value.slice('fixed:'.length), 10);
        if (Number.isFinite(fixed) && fixed >= 0) return fixed;
      }
      return null;
    }

    async function loadWorkerRuntime() {
      try {
        const status = await api('/v1/workers');
        renderWorkerRuntime(status);
        if (!workerRuntimeMessage.textContent || workerRuntimeMessage.className === 'status-failed') {
          workerRuntimeMessage.textContent = '';
          workerRuntimeMessage.className = 'muted';
        }
      } catch (error) {
        workerRuntimeMessage.textContent = error.message;
        workerRuntimeMessage.className = 'status-failed';
      }
    }

    function uploadRootStatusClass(status) {
      return status === 'ready' ? 'status-ok' : 'status-failed';
    }

    function renderUploadRoots(roots) {
      latestUploadRoots = roots || [];
      if (!latestUploadRoots.length) {
        uploadRootsNode.innerHTML = '<div class="notice warn"><strong>No upload roots configured</strong><div class="muted">Add uploadRoots to the Agent config before enabling device uploads.</div></div>';
        return;
      }
      uploadRootsNode.innerHTML = '<table><thead><tr><th>Key</th><th>Path</th><th>Temp path</th><th>Status</th></tr></thead><tbody>' + latestUploadRoots.map(root =>
        '<tr>' +
          '<td>' + escapeHTML(root.key) + '</td>' +
          '<td>' + escapeHTML(root.path) + '</td>' +
          '<td>' + escapeHTML(root.tempPath || '') + '</td>' +
          '<td><span class="' + uploadRootStatusClass(root.status) + '">' + escapeHTML(root.status || '') + '</span><div class="muted">' + escapeHTML(root.message || '') + '</div></td>' +
        '</tr>'
      ).join('') + '</tbody></table>';
    }

    async function loadUploadRoots() {
      const payload = await api('/v1/uploads/roots');
      renderUploadRoots(payload.roots || []);
      return latestUploadRoots;
    }

    function datasourceKindLabel(kind) {
      if (kind === 'immich') return 'Immich (Passthrough)';
      if (kind === 'immich_indexed') return 'Immich (Indexed)';
      if (kind === 'local_filesystem') return 'Local datasource';
      if (kind === 'static_demo') return 'Static demo';
      return kind || 'Datasource';
    }

    function isImmichDatasourceKind(kind) {
      return kind === 'immich' || kind === 'immich_indexed';
    }

    function isImmichPassthroughDatasource(datasource) {
      return datasource?.kind === 'immich' && !Boolean(datasource?.indexingEnabled);
    }

    function datasourceEndpoint(datasource) {
      if (datasource.kind === 'local_filesystem') {
        return datasource.rootKey || '';
      }
      return datasource.url || '';
    }

    function datasourceStatus(datasource, rootsByKey) {
      if (isImmichDatasourceKind(datasource.kind)) {
        return (datasource.hasAccessToken ? 'API key configured' : 'API key missing') +
          (datasource.kind === 'immich' ? ' / passthrough' : (datasource.indexingEnabled ? ' / indexing enabled' : ''));
      }
      if (datasource.kind === 'local_filesystem') {
        const root = rootsByKey.get(datasource.rootKey || '');
        return root ? (root.status || 'unknown') : 'root not configured';
      }
      return datasource.hasAccessToken ? 'API key configured' : '';
    }

    function datasourceStatusClass(datasource, rootsByKey) {
      if (isImmichDatasourceKind(datasource.kind)) {
        return datasource.hasAccessToken ? 'status-ok' : 'status-warn';
      }
      if (datasource.kind === 'local_filesystem') {
        const root = rootsByKey.get(datasource.rootKey || '');
        return root && root.status === 'ready' ? 'status-ok' : 'status-warn';
      }
      return 'muted';
    }

    function renderDatasources(datasources) {
      const items = datasources || [];
      if (datasourceAddPanel && !datasourceAddPanelTouched && datasourceAddPanel.open !== (items.length === 0)) {
        syncingDatasourceAddPanel = true;
        datasourceAddPanel.open = items.length === 0;
        syncingDatasourceAddPanel = false;
      }
      if (!items.length) {
        datasourceList.innerHTML = '<div class="notice warn"><strong>No datasource configured</strong><div class="muted">Add an Immich datasource or a configured local media root.</div></div>';
        return;
      }
      const rootsByKey = new Map((latestLocalMediaRoots || []).map(root => [root.key, root]));
      datasourceList.innerHTML = '<table><thead><tr><th>Name</th><th>Type</th><th>Endpoint</th><th>Status</th><th>Immich fallback</th></tr></thead><tbody>' + items.map(datasource => {
        const endpoint = datasourceEndpoint(datasource);
        const detail = datasource.kind === 'local_filesystem' ? (datasource.rootPath || '') : (datasource.sourceKey || '');
        const root = datasource.kind === 'local_filesystem' ? rootsByKey.get(datasource.rootKey || '') : null;
        const rootAcceptance = root?.rootAcceptanceRequired && root?.observedRootIdentity ?
          '<div class="notice warn"><strong>Root identity changed</strong><div class="muted">Review the mounted path before accepting it. Acceptance can mark media from the previous root missing.</div>' +
          '<button type="button" data-accept-local-root data-source-key="' + escapeHTML(datasource.sourceKey || '') + '" data-root-key="' + escapeHTML(datasource.rootKey || '') + '" data-observed-identity="' + escapeHTML(root.observedRootIdentity) + '">Accept current root and scan</button></div>' : '';
        const fallback = datasource.kind === 'local_filesystem' ?
          '<label class="checkbox-label"><input type="checkbox" data-local-immich-fallback="' + escapeHTML(datasource.sourceKey || '') + '"' + (datasource.immichFallbackEnabled ? ' checked' : '') + '> Enabled</label>' +
          '<div class="muted">Allow ready Immich duplicates in Gallery.</div>' :
          '<span class="muted">—</span>';
        return '<tr>' +
          '<td>' + escapeHTML(datasource.name || datasource.sourceKey || '') + '<div class="muted">' + escapeHTML(datasource.sourceKey || '') + '</div></td>' +
          '<td>' + escapeHTML(datasourceKindLabel(datasource.kind)) + '</td>' +
          '<td>' + escapeHTML(endpoint) + '<div class="muted">' + escapeHTML(detail) + '</div></td>' +
          '<td><span class="' + datasourceStatusClass(datasource, rootsByKey) + '">' + escapeHTML(datasourceStatus(datasource, rootsByKey)) + '</span>' + rootAcceptance + '</td>' +
          '<td>' + fallback + '</td>' +
        '</tr>';
      }).join('') + '</tbody></table>';
    }

    function renderDatasourceRootOptions(roots) {
      const selected = datasourceRootKey.value;
      const readyRoots = (roots || []).filter(root => root.key);
      const options = ['<option value="">' + (readyRoots.length ? 'Select local media root' : 'No configured local media roots') + '</option>'];
      readyRoots.forEach(root => {
        const label = root.key + ' / ' + (root.status || 'unknown');
        options.push('<option value="' + escapeHTML(root.key) + '"' + (root.key === selected ? ' selected' : '') + '>' + escapeHTML(label) + '</option>');
      });
      datasourceRootKey.innerHTML = options.join('');
    }

    function updateDatasourceFormMode() {
      const kind = datasourceKind.value || 'immich';
      const isImmich = isImmichDatasourceKind(kind);
      const isLocal = kind === 'local_filesystem';
      datasourceKindFields.forEach(field => {
        const fieldKind = field.dataset.datasourceKindField;
        field.hidden = fieldKind !== kind && !(fieldKind === 'immich' && isImmich);
      });
      datasourceURL.required = isImmich;
      datasourceAccessToken.required = isImmich;
      datasourceRootKey.required = isLocal;
      datasourceRootKey.disabled = !isLocal || latestLocalMediaRoots.length === 0;
      datasourceURL.disabled = !isImmich;
      datasourceAccessToken.disabled = !isImmich;
      if (datasourceRootHint) {
        datasourceRootHint.textContent = latestLocalMediaRoots.length === 0 ?
          'No local media roots are configured for this Agent. Add a local media root to the Agent config and restart, then it will appear here.' :
          'Local root paths are managed by the Agent config and are not editable here.';
      }
      if (!datasourceName.value.trim()) {
        datasourceName.placeholder = isLocal ? 'Local Files' : 'Immich';
      }
      const button = datasourceForm.querySelector('button[type="submit"]');
      const hasPassthrough = latestDatasources.some(datasource => datasource.kind === 'immich');
      const passthroughConflict = hasPassthrough || (kind === 'immich' && latestDatasources.length > 0);
      const topologyHint = document.querySelector('#datasourceTopologyHint');
      if (topologyHint) {
        topologyHint.textContent = hasPassthrough ?
          'Immich passthrough must remain the only datasource. Convert it to Immich (Indexed) before adding another datasource.' :
          (kind === 'immich' ? 'Immich passthrough relays Immich directly and must be the only datasource.' : 'Indexed datasources can be combined.');
        topologyHint.className = passthroughConflict ? 'status-warn field-note' : 'muted field-note';
      }
      if (button) {
        button.disabled = (isLocal && latestLocalMediaRoots.length === 0) || passthroughConflict;
      }
    }

    function datasourceCoverageMetric(datasource, key) {
      const metric = datasource?.coverage ? datasource.coverage[key] : null;
      if (!metric) {
        return {
          status: isImmichPassthroughDatasource(datasource) ? 'immich-managed' : 'updating',
          count: 0,
          totalCount: 0
        };
      }
      return {
        status: metric.status || 'ready',
        count: Number(metric.count || 0),
        totalCount: Number(metric.totalCount || 0),
        updatedAt: metric.updatedAt || ''
      };
    }

    function datasourceCoverageStatusClass(metric, key) {
      if (metric.status === 'unavailable') return 'muted';
      if (metric.status === 'updating') return 'status-warn';
      if (key === 'issues' && Number(metric.count || 0) > 0) return 'status-failed';
      return 'status-ok';
    }

    function datasourceCoverageText(metric, emptyText = 'updating') {
      if (metric.status === 'immich-managed') return 'Immich-managed';
      if (metric.status === 'unavailable') return 'unavailable';
      if (metric.status === 'updating') return emptyText;
      return formatCount(metric.count || 0);
    }

    function datasourceCoverageMetricHTML(datasource, key, emptyText = 'updating') {
      const metric = datasourceCoverageMetric(datasource, key);
      return '<span class="' + datasourceCoverageStatusClass(metric, key) + '">' +
        escapeHTML(datasourceCoverageText(metric, emptyText)) +
        '</span>';
    }

    function datasourceDiagnosticLinkHTML(datasource) {
      if (!datasource || datasource.kind !== 'local_filesystem' || !datasource.sourceKey) return '';
      return '<div class="muted"><a href="/v1/datasources/local/phase0-diagnostics.csv?sourceKey=' +
        encodeURIComponent(datasource.sourceKey) + '" target="_blank" rel="noreferrer">Download discovery CSV</a></div>';
    }

    function datasourceCoverageIssueHTML(datasource) {
      const metric = datasourceCoverageMetric(datasource, 'issues');
      const messages = [];
      if (datasource?.lastError) messages.push(datasource.lastError);
      if (Number(datasource?.blockedLocations || 0) > 0) messages.push(String(datasource.blockedLocations) + ' blocked locations');
      if (Number(datasource?.missingLocations || 0) > 0) messages.push(String(datasource.missingLocations) + ' missing locations');
      if (Number(datasource?.missingAssets || 0) > 0) messages.push(String(datasource.missingAssets) + ' missing assets');
      const detail = messages.length ? '<div class="muted">' + escapeHTML(messages.join(' / ')) + '</div>' : '';
      return '<span class="' + datasourceCoverageStatusClass(metric, 'issues') + '">' +
        escapeHTML(datasourceCoverageText(metric, 'updating')) +
        '</span>' + detail + datasourceDiagnosticLinkHTML(datasource);
    }

    function datasourceTaskByPhase(tasks, phase) {
      return (tasks || []).find(task => task.phase === phase) || null;
    }

    function datasourceTaskFailureText(task, count) {
      const unit = String(task?.failureUnit || '');
      if (unit === 'publish_jobs' || task?.phase === 'search_index') {
        return 'publish jobs failed: ' + count;
      }
      if (unit === 'items' || task?.phase === 'metadata' || task?.phase === 'thumbnails' || task?.phase === 'embeddings') {
        return 'items failed: ' + count;
      }
      return 'failed: ' + count;
    }

    function searchCoverageModelLabel(payload) {
      const datasources = payload?.datasources || [];
      const models = Array.from(new Set(datasources.map(datasource => datasource.embeddingModelId || '').filter(Boolean)));
      if (models.length) return models.join(', ');
      const semanticModels = latestSemanticModelStatus ? semanticModelItems(latestSemanticModelStatus) : [];
      const installed = semanticModels.find(model => Boolean(model.modelPack?.installed));
      if (installed) return installed.modelPack?.name || installed.modelId || 'Installed semantic model';
      const embeddingTask = datasourceTaskByPhase(payload?.tasks || [], 'embeddings');
      const searchTask = datasourceTaskByPhase(payload?.tasks || [], 'search_index');
      if (Number(embeddingTask?.totalTasks || 0) > 0 || Number(searchTask?.totalTasks || 0) > 0) return 'Configured in Semantic Models';
      return 'No active search model';
    }

    function searchCoverageLine(task, emptyLabel) {
      if (!task) return emptyLabel;
      const done = Number(task.completedTasks || 0);
      const total = Number(task.totalTasks || 0);
      const failed = Number(task.failedTasks || 0);
      if (!done && !total && !failed) return emptyLabel;
      const parts = [];
      if (total > 0) {
        parts.push('done: ' + formatCount(done) + ' / ' + formatCount(total));
      } else if (done > 0) {
        parts.push('done: ' + formatCount(done));
      }
      if (failed > 0) parts.push(datasourceTaskFailureText(task, formatCount(failed)));
      return parts.join('<br>');
    }

    function searchCoverageNoticeHTML(embeddingTask, searchTask) {
      const embeddingDone = Number(embeddingTask?.completedTasks || 0);
      const embeddingTotal = Number(embeddingTask?.totalTasks || 0);
      const searchDone = Number(searchTask?.completedTasks || 0);
      const searchTotal = Number(searchTask?.totalTasks || 0);
      if (!embeddingTotal && !searchTotal) {
        return '<div class="notice warn"><strong>Semantic search is not indexed yet</strong><div class="muted">Install a semantic model and run media discovery to start visual search indexing.</div></div>';
      }
      if (searchDone < embeddingDone || embeddingDone < embeddingTotal) {
        return '<div class="notice"><strong>Search results are partial</strong><div class="muted">Semantic search is available for the media already indexed. More media will appear as background indexing continues.</div></div>';
      }
      return '<div class="notice"><strong class="status-ok">Search index is current</strong><div class="muted">Semantic search can use the indexed media shown below.</div></div>';
    }

    function renderSearchCoverage(payload) {
      const datasources = payload?.datasources || [];
      if (!datasources.some(datasource => Boolean(datasource.indexingEnabled))) {
        const passthrough = datasources.some(datasource => datasource.kind === 'immich');
        const detail = passthrough
          ? 'Immich Passthrough uses the upstream Immich search index directly; no local model or search indexing is required.'
          : 'Add an indexed datasource to configure local semantic search coverage.';
        searchCoverageStatus.innerHTML = '<div class="notice"><strong>No local search indexing</strong><div class="muted">' + escapeHTML(detail) + '</div></div>';
        return;
      }
      const tasks = payload?.tasks || [];
      const embeddingTask = datasourceTaskByPhase(tasks, 'embeddings');
      const searchTask = datasourceTaskByPhase(tasks, 'search_index');
      const coverageRows = [
        ['Model', escapeHTML(searchCoverageModelLabel(payload))],
        ['Media analyzed', searchCoverageLine(embeddingTask, 'Not started')],
        ['Search index', searchCoverageLine(searchTask, 'Not started')]
      ].map(([key, value]) => '<dt>' + escapeHTML(key) + '</dt><dd>' + value + '</dd>').join('');
      const html = searchCoverageNoticeHTML(embeddingTask, searchTask) +
        '<dl>' + coverageRows + '</dl>';
      searchCoverageStatus.innerHTML = html;
    }

    function renderDatasourceTasks(tasks, options = {}) {
      if (activeDatasourceTaskNote) {
        pendingDatasourceTaskRender = { tasks, options };
        return;
      }
      const items = tasks || [];
      if (!items.length) {
        const detail = options.hasDatasources
          ? 'Passthrough datasources use the upstream library directly and do not run local discovery or indexing.'
          : 'Add an indexed datasource to see scan and indexing work.';
        datasourceTaskStatus.innerHTML = '<div class="notice"><strong>No indexing tasks</strong><div class="muted">' + escapeHTML(detail) + '</div></div>';
        return;
      }
      datasourceTaskStatus.innerHTML = '<div class="datasource-task-table-wrap"><table class="datasource-task-table"><thead><tr><th>Task</th><th class="datasource-task-activity">Activity</th><th class="datasource-task-status">Status</th><th>Action</th></tr></thead><tbody>' + items.map(task => {
        const action = datasourceTaskActionHTML(task, options);
        return '<tr>' +
          '<td><span class="datasource-task-label">' + escapeHTML(task.label || task.phase || '') + datasourceTaskNoteHTML(task) + '</span></td>' +
          '<td class="datasource-task-activity">' + datasourceTaskActivityHTML(task) + '</td>' +
          '<td class="datasource-task-status">' + datasourceTaskStatusHTML(task) + '</td>' +
          '<td>' + action + '</td>' +
        '</tr>';
      }).join('') + '</tbody></table></div>';
    }

    function datasourceTaskActivityHTML(task) {
      const rawStatus = task?.status || 'idle';
      const activity = datasourceTaskActivityLabel(task, rawStatus);
      return '<span class="' + datasourceTaskStatusClass(activity) + '">' + escapeHTML(activity) + '</span>';
    }

    function datasourceTaskActivityLabel(task, rawStatus) {
      const activeTasks = Number(task?.activeTasks || 0);
      if (activeTasks > 0) {
        return activeTasks > 1 ? 'running (' + formatCount(activeTasks) + ')' : 'running';
      }
      if (rawStatus === 'disabled') return 'disabled';
      if (task?.waitingReason === 'paused' || rawStatus === 'paused') return 'paused';
      if (latestWorkerRuntimeStatus?.pausedHeavyTaskWorkers &&
        datasourceTaskUsesBackgroundWorker(task?.phase || '') &&
        (Number(task?.queuedTasks || 0) > 0 || task?.queuedTasksUnknown || rawStatus === 'queued' || rawStatus === 'running')) {
        return 'paused';
      }
      if (task?.waitingReason === 'search_index') return 'synchronizing';
      if (rawStatus === 'running') return 'running';
      if (task?.waitingReason === 'worker') return 'queued';
      if (task?.waitingReason === 'queued_target') {
        return 'waiting';
      }
      if (task?.nextRunAt) {
        const waiting = taskWaitActivity(task.nextRunAt);
        if (waiting) return waiting;
      }
      if (Number(task?.queuedTasks || 0) > 0 || rawStatus === 'queued') return 'queued';
      if (rawStatus === 'waiting') return 'waiting';
      if (rawStatus === 'busy') return 'busy';
      return 'idle';
    }

    function datasourceTaskUsesBackgroundWorker(phase) {
      return phase === 'metadata' || phase === 'thumbnails' || phase === 'content_verification' || phase === 'embeddings' || phase === 'search_index';
    }

    function datasourceTaskStatusHTML(task) {
      if (task?.setupRequired === 'search_model') {
        return 'not enabled';
      }
      const rawStatus = task?.status || 'idle';
      if (task?.phase === 'content_verification' && rawStatus === 'not_applicable') {
        return 'not applicable (no local datasource)';
      }
      const parts = [];
      const showWorkCounts = task?.phase !== 'phase0' && task?.phase !== 'content_verification';
      const showScanModeTimes = task?.phase === 'phase0' && Boolean(task?.lastQuickScanAt || task?.lastReconciliationAt);
      if (task?.phase === 'phase0' && rawStatus === 'idle') {
        if (showScanModeTimes) {
          parts.push('quick discovery: ' + (task?.lastQuickScanAt ? date(task.lastQuickScanAt) : '-'));
          parts.push('reconciliation: ' + (task?.lastReconciliationAt ? date(task.lastReconciliationAt) : '-'));
        } else if (task?.lastCompletedAt) {
          parts.push('last: ' + date(task.lastCompletedAt));
        }
      }
      if (task?.phase === 'content_verification') {
        const lastAt = datasourceContentVerificationEventAt(task);
        if (lastAt) {
          parts.push('last: ' + date(lastAt));
        }
        if (task?.lastRunStatus === 'skipped') {
          const reason = task?.lastRunReason === 'no_idle_worker'
            ? 'no idle worker'
            : task?.lastRunReason === 'schedule_missed'
              ? 'schedule missed'
              : task?.lastRunReason === 'root_identity_changed'
                ? 'root changed'
              : (task?.lastRunReason || 'not run');
          parts.push('result: skipped (' + reason + ')');
        } else if (task?.lastRunStatus === 'completed' || task?.lastRunStatus === 'running') {
          const resultParts = [
            'processed ' + formatCount(task?.lastProcessedFiles || 0) + ' files',
            formatCount(task?.lastVerifiedFiles || 0) + ' verified',
            formatBytes(task?.lastReadBytes || 0) + ' read'
          ];
          if (Number(task?.lastChangedFiles || 0) > 0) {
            resultParts.push(formatCount(task.lastChangedFiles) + ' changed');
          }
          if (Number(task?.lastFailedFiles || 0) > 0) {
            resultParts.push('failed files: ' + formatCount(task.lastFailedFiles));
          }
          parts.push((task.lastRunStatus === 'running' ? 'current: ' : 'result: ') + resultParts.join(', '));
        } else if (rawStatus === 'disabled') {
          parts.push('content verification is disabled');
        }
      }
      if (showWorkCounts && task?.queuedTasksUnknown) {
        parts.push('queued: unknown');
      } else if (showWorkCounts && Number(task?.queuedTasks || 0) > 0) {
        parts.push('queued: ' + formatCount(task?.queuedTasks || 0));
      }
      if (showWorkCounts && Number(task?.settlingTasks || 0) > 0) {
        parts.push('settling: ' + formatCount(task.settlingTasks));
      }
      const progress = taskProgressHTML(task);
      if (progress) {
        parts.push('done: ' + progress);
      }
      if (task?.waitingReason === 'queued_target') {
        const target = Number(task?.waitingQueuedTarget || 0);
        parts.push(target > 0 ? 'waiting ' + formatCount(target) + ' queued items' : 'waiting queued items');
      }
      if (task?.failedTasksUnknown) {
        parts.push(datasourceTaskFailureText(task, 'unknown'));
      } else if (Number(task?.failedTasks || 0) > 0) {
        parts.push(datasourceTaskFailureText(task, formatCount(task.failedTasks)));
      }
      return parts.length ? parts.map(part => escapeHTML(part)).join(showScanModeTimes ? '<br>' : ' · ') : '-';
    }

    function datasourceTaskNoteHTML(task) {
      const note = String(task?.note || '').trim();
      if (!note) return '-';
      const phase = String(task?.phase || 'task').replace(/[^a-z0-9_-]/gi, '-');
      const noteID = 'datasource-task-note-' + phase;
      return '<button type="button" class="datasource-task-note-trigger" data-datasource-task-note-trigger aria-controls="' + noteID + '" aria-expanded="false" aria-label="Show task note">?</button>' +
        '<span id="' + noteID + '" class="datasource-task-note-popover" data-datasource-task-note role="tooltip" hidden>' + escapeHTML(note) + '</span>';
    }

    function closeDatasourceTaskNote(applyPendingRender = true) {
      if (!activeDatasourceTaskNote) return;
      activeDatasourceTaskNote.button.setAttribute('aria-expanded', 'false');
      activeDatasourceTaskNote.popover.hidden = true;
      activeDatasourceTaskNote = null;
      if (applyPendingRender) flushPendingDatasourceTaskRender();
    }

    function flushPendingDatasourceTaskRender() {
      if (!pendingDatasourceTaskRender) return;
      const pending = pendingDatasourceTaskRender;
      pendingDatasourceTaskRender = null;
      renderDatasourceTasks(pending.tasks, pending.options);
    }

    function toggleDatasourceTaskNote(button) {
      const popover = button.parentElement?.querySelector('[data-datasource-task-note]');
      const wasOpen = activeDatasourceTaskNote?.button === button;
      closeDatasourceTaskNote(false);
      if (!popover || wasOpen) {
        flushPendingDatasourceTaskRender();
        return;
      }

      popover.hidden = false;
      button.setAttribute('aria-expanded', 'true');
      const padding = 12;
      const width = Math.min(340, Math.max(220, window.innerWidth - padding * 2));
      const rect = button.getBoundingClientRect();
      popover.style.width = String(width) + 'px';
      popover.style.left = String(Math.min(Math.max(padding, rect.left + rect.width / 2 - width / 2), window.innerWidth - width - padding)) + 'px';
      popover.style.top = String(Math.min(rect.bottom + 8, window.innerHeight - popover.offsetHeight - padding)) + 'px';
      activeDatasourceTaskNote = { button, popover };
    }

    function taskProgressHTML(task) {
      const completed = Number(task.completedTasks || 0);
      const total = Number(task.totalTasks || 0);
      if (total > 0) {
        return formatCount(completed) + ' / ' + formatCount(total);
      }
      if (completed > 0) {
        return formatCount(completed);
      }
      return '';
    }

    function datasourceTaskActionHTML(task, options) {
      const phase = task.phase || '';
      const failureLink = datasourceTaskFailureLinkHTML(task, options);
      if (task?.setupRequired === 'search_model') {
        return '<button type="button" data-datasource-task-action="go-to-search">Go to Search tab</button>';
      }
      if (phase === 'phase0') {
        const running = datasourceTaskActionPendingForPhase(phase) || task.status === 'running' || Number(task.activeTasks || 0) > 0;
        const disabled = options.hasIndexedDatasource && !running ? '' : ' disabled';
        return '<button type="button" data-datasource-task-action="media-discovery"' + disabled + '>Run reconciliation now</button>' + failureLink;
      }
      if (phase === 'metadata') {
        const failed = Number(task.failedTasks || 0);
        const disabled = options.hasLocalDatasource && failed > 0 && !datasourceTaskActionPendingForPhase(phase) ? '' : ' disabled';
        return '<button type="button" data-datasource-task-action="requeue-metadata"' + disabled + '>Requeue failed</button>' + failureLink;
      }
      if (phase === 'thumbnails') {
        const failed = Number(task.failedTasks || 0);
        const disabled = options.hasLocalDatasource && failed > 0 && !datasourceTaskActionPendingForPhase(phase) ? '' : ' disabled';
        return '<button type="button" data-datasource-task-action="requeue-thumbnails"' + disabled + '>Requeue failed</button>' + failureLink;
      }
      return failureLink;
    }

    function datasourceTaskFailureLinkHTML(task, options) {
      const hasLocalItemDiagnostics = task?.phase === 'metadata' || task?.phase === 'thumbnails';
      if (!options.hasLocalDatasource || !hasLocalItemDiagnostics || Number(task?.failedTasks || 0) <= 0) return '';
      return '<div class="muted task-action-link"><a href="/v1/datasources/local/failure-diagnostics.csv" target="_blank" rel="noreferrer">Download failures CSV</a></div>';
    }

    function datasourceTaskStatusClass(status) {
      if (status === 'attention') return 'status-failed';
      if (String(status).startsWith('running')) return 'status-running';
      if (status === 'queued' || status === 'busy' || status === 'waiting' || status === 'paused' || status === 'synchronizing' || String(status).startsWith('waiting ')) return 'status-warn';
      return 'status-ok';
    }

    function loadingNotice(title, message) {
      return '<div class="notice"><strong>' + escapeHTML(title) + '</strong><div class="muted">' + escapeHTML(message) + '</div></div>';
    }

    function normalizeDatasourceIndexingCachePayload(payload) {
      if (!payload || !Array.isArray(payload.datasources)) return null;
      return { ...payload, statusSnapshotUsed: true };
    }

    function parseDatasourceIndexingCacheJSON(value) {
      if (!value || value.trim() === 'null') return null;
      try {
        let parsed = JSON.parse(value);
        if (parsed && parsed.payload && parsed.cachedAt) {
          if (Date.now() - Number(parsed.cachedAt) > datasourceIndexingSessionCacheMaxAgeMs) return null;
          parsed = parsed.payload;
        }
        return normalizeDatasourceIndexingCachePayload(parsed);
      } catch (_) {
        return null;
      }
    }

    function readDatasourceIndexingCachePayload() {
      const initial = parseDatasourceIndexingCacheJSON(initialDatasourceIndexingStatusNode?.textContent || '');
      if (initial) return initial;
      try {
        return parseDatasourceIndexingCacheJSON(window.sessionStorage.getItem(datasourceIndexingSessionStorageKey) || '');
      } catch (_) {
        return null;
      }
    }

    function rememberDatasourceIndexingCachePayload(payload) {
      const snapshot = normalizeDatasourceIndexingCachePayload(payload);
      if (!snapshot) return;
      try {
        window.sessionStorage.setItem(datasourceIndexingSessionStorageKey, JSON.stringify({ cachedAt: Date.now(), payload: snapshot }));
      } catch (_) {}
    }

    function hydrateDatasourceIndexingStatusFromCache() {
      const cached = readDatasourceIndexingCachePayload();
      if (!cached) return false;
      datasourceIndexingLoaded = true;
      renderDatasourceIndexingStatus(cached);
      return true;
    }

    function renderDatasourceIndexingLoading() {
      datasourceTaskStatus.innerHTML = loadingNotice('Loading datasource tasks', 'Reading scan and indexing work.');
      datasourceStatusNode.innerHTML = loadingNotice('Loading datasource coverage', 'Reading the latest media coverage snapshot.');
      searchCoverageStatus.innerHTML = loadingNotice('Loading search coverage', 'Reading semantic indexing coverage.');
      renderCatalogDedupIdle();
    }

    function renderDatasourceIndexingStatus(payload) {
      const nextPayload = mergeDatasourceIndexingPayload(payload);
      latestDatasourceIndexingStatus = nextPayload;
      rememberDatasourceIndexingCachePayload(nextPayload);
      const tasks = nextPayload.tasks || [];
      const datasources = nextPayload.datasources || [];
      const hasLocalDatasource = datasources.some(datasource => datasource.kind === 'local_filesystem');
      const hasIndexedDatasource = datasources.some(datasource => Boolean(datasource.indexingEnabled));
      renderDatasourceTasks(tasks, { hasDatasources: datasources.length > 0, hasIndexedDatasource, hasLocalDatasource });
      renderDatasourceStatus(datasources, tasks);
      renderSearchCoverage(nextPayload);
      if (nextPayload.statusSnapshotUsed) {
        const at = nextPayload.statusSnapshotAt ? ' from ' + date(nextPayload.statusSnapshotAt) : '';
        datasourceIndexingMessage.textContent = 'Showing datasource task status' + at + '.';
        datasourceIndexingMessage.className = 'muted';
      }
      if (latestSemanticModelStatus) renderSemanticModels(latestSemanticModelStatus);
    }

    function mergeDatasourceIndexingPayload(payload) {
      const next = payload || { datasources: [] };
      const previousByPhase = new Map((latestDatasourceIndexingStatus.tasks || []).map(task => [task.phase, task]));
      const tasks = (next.tasks || []).map(task => applyDatasourceTaskPendingOverlay(mergeDatasourceTaskRow(task, previousByPhase.get(task.phase))));
      const previousBySource = new Map((latestDatasourceIndexingStatus.datasources || []).map(datasource => [datasource.sourceKey, datasource]));
      const datasources = (next.datasources || []).map(datasource => mergeDatasourceIndexingRow(datasource, previousBySource.get(datasource.sourceKey)));
      return { ...next, tasks, datasources };
    }

    function mergeDatasourceTaskRow(task, previous) {
      if (task?.phase === 'content_verification' && task?.status === 'not_applicable') return task;
      if (!previous) return task;
      const lastCompletedAt = latestDatasourceTaskTimestamp(task.lastCompletedAt, previous.lastCompletedAt);
      const lastQuickScanAt = latestDatasourceTaskTimestamp(task.lastQuickScanAt, previous.lastQuickScanAt);
      const lastReconciliationAt = latestDatasourceTaskTimestamp(task.lastReconciliationAt, previous.lastReconciliationAt);
      const currentContentResultAt = Date.parse(datasourceContentVerificationEventAt(task));
      const previousContentResultAt = Date.parse(datasourceContentVerificationEventAt(previous));
      const contentResult = task.phase === 'content_verification' &&
        (!task.lastRunStatus ||
          (Number.isFinite(previousContentResultAt) &&
            (!Number.isFinite(currentContentResultAt) || previousContentResultAt > currentContentResultAt)))
        ? previous
        : task;
      const contentResultFields = task.phase === 'content_verification' ? {
        lastRunStartedAt: contentResult.lastRunStartedAt || '',
        lastRunStatus: contentResult.lastRunStatus || '',
        lastRunReason: contentResult.lastRunReason || '',
        lastProcessedFiles: Number(contentResult.lastProcessedFiles || 0),
        lastVerifiedFiles: Number(contentResult.lastVerifiedFiles || 0),
        lastChangedFiles: Number(contentResult.lastChangedFiles || 0),
        lastFailedFiles: Number(contentResult.lastFailedFiles || 0),
        lastReadBytes: Number(contentResult.lastReadBytes || 0)
      } : {};
      if (!datasourceTaskHasUnknowns(task) || datasourceTaskHasUnknowns(previous)) {
        return { ...task, ...contentResultFields, lastCompletedAt, lastQuickScanAt, lastReconciliationAt };
      }
      const activeTasks = task.activeTasksUnknown ? Number(previous.activeTasks || 0) : Number(task.activeTasks || 0);
      const queuedTasks = task.queuedTasksUnknown ? Number(previous.queuedTasks || 0) : Number(task.queuedTasks || 0);
      const completedTasks = task.queuedTasksUnknown ? Number(previous.completedTasks || 0) : Number(task.completedTasks || 0);
      const totalTasks = task.queuedTasksUnknown ? Number(previous.totalTasks || 0) : Number(task.totalTasks || 0);
      const failedTasks = task.failedTasksUnknown ? Number(previous.failedTasks || 0) : Number(task.failedTasks || 0);
      return {
        ...task,
        ...contentResultFields,
        activeTasks,
        activeTasksUnknown: false,
        queuedTasks,
        queuedTasksUnknown: false,
        completedTasks,
        totalTasks,
        failedTasks,
        failedTasksUnknown: false,
        waitingReason: task.waitingReason || previous.waitingReason || '',
        waitingQueuedTarget: task.waitingQueuedTarget || previous.waitingQueuedTarget || 0,
        nextRunAt: task.nextRunAt || previous.nextRunAt || '',
        lastCompletedAt,
        lastQuickScanAt,
        lastReconciliationAt,
        status: datasourceTaskMergedStatus(task, previous, activeTasks, queuedTasks, failedTasks)
      };
    }

    function latestDatasourceTaskTimestamp(first, second) {
      const left = String(first || '');
      const right = String(second || '');
      if (!left) return right;
      if (!right) return left;
      const leftMs = Date.parse(left);
      const rightMs = Date.parse(right);
      if (Number.isNaN(leftMs) && Number.isNaN(rightMs)) return left;
      if (Number.isNaN(leftMs)) return right;
      if (Number.isNaN(rightMs)) return left;
      return rightMs > leftMs ? right : left;
    }

    function datasourceContentVerificationEventAt(task) {
      return latestDatasourceTaskTimestamp(task?.lastCompletedAt, task?.lastRunStartedAt);
    }

    function datasourceRunCompletedAt(result) {
      let completedAt = '';
      (result?.results || []).forEach(item => {
        completedAt = latestDatasourceTaskTimestamp(completedAt, item?.completedAt || '');
      });
      return completedAt;
    }

    function datasourceRunCompletedAtForMode(result, mode) {
      let completedAt = '';
      (result?.results || []).forEach(item => {
        if (item?.kind !== 'local_filesystem' || item?.mode !== mode) return;
        completedAt = latestDatasourceTaskTimestamp(completedAt, item?.completedAt || '');
      });
      return completedAt;
    }

    const datasourceTaskNotes = {
      phase0: 'Quick discovery finds likely filesystem changes with low NAS load. Reconciliation inspects every supported path once daily at 04:00 in the Agent timezone and repairs additions, changes, and removals.',
      content_verification: 'At the configured daily time, re-hashes the least recently verified media with an idle heavy-task worker. If no worker is idle, that day is skipped. The default duration is 30 minutes; a duration of 0 disables this task.',
      metadata: 'Registers media information in the media database. Recently added or changed files remain settling before metadata processing (2 minutes by default). Requeue failed moves failed metadata jobs back to the queue at repair priority. Processing starts after settling when a worker is available, and jobs that fail again return to failed.',
      thumbnails: 'Generates thumbnails so media can be previewed quickly. Requeue failed moves failed thumbnails back to the queue at repair priority. Processing starts when a worker is available, and items that fail again return to failed.',
      embeddings: 'Analyzes media features for visual search. Within each datasource, first-attempt work is prioritized ahead of eligible retries, so the failed count may remain unchanged while new embeddings complete. Search becomes available after the Search index processes the embeddings.',
      search_index: 'Updates the search index so media can be searched. Publishing can take several hours for a large library. An existing published index remains searchable while publishing runs. Failed publish jobs are retried automatically on the next eligible run.'
    };

    function datasourceTaskNote(phase) {
      return datasourceTaskNotes[phase] || '';
    }

    function rememberDatasourceDiscoveryCompletedAt(result) {
      const completedAt = datasourceRunCompletedAt(result);
      const quickCompletedAt = datasourceRunCompletedAtForMode(result, 'quick');
      const reconciliationCompletedAt = datasourceRunCompletedAtForMode(result, 'reconciliation');
      if (!completedAt || !latestDatasourceIndexingStatus) return;
      let found = false;
      const tasks = (latestDatasourceIndexingStatus.tasks || []).map(task => {
        if (task.phase !== 'phase0') return task;
        found = true;
        return {
          ...task,
          activeTasks: 0,
          activeTasksUnknown: false,
          queuedTasks: 0,
          queuedTasksUnknown: false,
          waitingReason: '',
          nextRunAt: '',
          lastCompletedAt: latestDatasourceTaskTimestamp(completedAt, task.lastCompletedAt),
          lastQuickScanAt: latestDatasourceTaskTimestamp(quickCompletedAt, task.lastQuickScanAt),
          lastReconciliationAt: latestDatasourceTaskTimestamp(reconciliationCompletedAt, task.lastReconciliationAt),
          status: Number(task.failedTasks || 0) > 0 ? 'attention' : 'idle'
        };
      });
      if (!found) {
        tasks.push({
          phase: 'phase0',
          label: 'Media discovery',
          activeTasks: 0,
          queuedTasks: 0,
          failedTasks: 0,
          status: 'idle',
          note: datasourceTaskNote('phase0'),
          lastCompletedAt: completedAt,
          lastQuickScanAt: quickCompletedAt,
          lastReconciliationAt: reconciliationCompletedAt
        });
      }
      latestDatasourceIndexingStatus = { ...latestDatasourceIndexingStatus, tasks };
      rememberDatasourceIndexingCachePayload(latestDatasourceIndexingStatus);
    }

    function datasourceTaskHasUnknowns(task) {
      if (!task) return false;
      return task.status === 'busy' || Boolean(task.activeTasksUnknown || task.queuedTasksUnknown || task.failedTasksUnknown);
    }

    function datasourceTaskMergedStatus(task, previous, activeTasks, queuedTasks, failedTasks) {
      if (failedTasks > 0) return 'attention';
      if (activeTasks > 0) return 'running';
      if (task.waitingReason || previous.waitingReason) return 'waiting';
      if (queuedTasks > 0) return 'queued';
      if (previous.status && previous.status !== 'busy') return previous.status;
      return task.status && task.status !== 'busy' ? task.status : 'idle';
    }

    function pendingDatasourceTaskPhase(action) {
      if (action === 'media-discovery') return 'phase0';
      if (action === 'requeue-metadata') return 'metadata';
      if (action === 'requeue-thumbnails') return 'thumbnails';
      return '';
    }

    function pendingDatasourceTaskActionForPhase(phase) {
      if (phase === 'phase0') return 'media-discovery';
      if (phase === 'metadata') return 'requeue-metadata';
      if (phase === 'thumbnails') return 'requeue-thumbnails';
      return '';
    }

    function datasourceTaskActionPendingForPhase(phase) {
      const action = pendingDatasourceTaskActionForPhase(phase);
      return action ? pendingDatasourceTaskActions.has(action) : false;
    }

    function applyDatasourceTaskPendingOverlay(task) {
      if (!task || !datasourceTaskActionPendingForPhase(task.phase)) return task;
      const next = { ...task, activeTasksUnknown: false, queuedTasksUnknown: false, failedTasksUnknown: false };
      if (next.phase === 'phase0') {
        next.activeTasks = 1;
        next.queuedTasks = 0;
        next.status = 'running';
        return next;
      }
      next.status = next.status === 'attention' ? 'attention' : 'running';
      return next;
    }

    function markDatasourceTaskActionPending(action) {
      const phase = pendingDatasourceTaskPhase(action);
      if (!phase) return;
      pendingDatasourceTaskActions.add(action);
      const current = latestDatasourceIndexingStatus || { datasources: [], tasks: [] };
      const tasks = Array.isArray(current.tasks) ? current.tasks.slice() : [];
      let found = false;
      const nextTasks = tasks.map(task => {
        if (task.phase !== phase) return task;
        found = true;
        return applyDatasourceTaskPendingOverlay(task);
      });
      if (!found) {
        nextTasks.push(applyDatasourceTaskPendingOverlay({
          phase,
          label: phase === 'phase0' ? 'Media discovery' : (phase === 'metadata' ? 'Metadata' : 'Thumbnails'),
          status: 'running',
          activeTasks: phase === 'phase0' ? 1 : 0,
          queuedTasks: 0,
          failedTasks: 0,
          note: datasourceTaskNote(phase)
        }));
      }
      renderDatasourceIndexingStatus({ ...current, tasks: nextTasks });
    }

    function mergeDatasourceIndexingRow(datasource, previous) {
      if (!previous) return datasource;
      let merged = {
        ...datasource,
        lastQuickScanAt: latestDatasourceTaskTimestamp(datasource.lastQuickScanAt, previous.lastQuickScanAt),
        lastReconciliationAt: latestDatasourceTaskTimestamp(datasource.lastReconciliationAt, previous.lastReconciliationAt)
      };
      if ((datasource.status || '') === 'busy') {
        merged = {
          ...merged,
          activeAssets: previous.activeAssets,
          outOfScopeAssets: previous.outOfScopeAssets,
          missingAssets: previous.missingAssets,
          activeLocations: previous.activeLocations,
          missingLocations: previous.missingLocations,
          blockedLocations: previous.blockedLocations,
          lastError: 'Agent seems busy; showing last known datasource counts. ' + (datasource.lastError || '')
        };
      }
      if ((datasource.embeddingStatus || '') === 'busy' && previous.embeddingStatus && previous.embeddingStatus !== 'busy' && previous.embeddingStatus !== 'unavailable') {
        merged = {
          ...merged,
          embeddingStatus: previous.embeddingStatus,
          embeddingEligibleAssets: previous.embeddingEligibleAssets,
          embeddingCompletedVectors: previous.embeddingCompletedVectors,
          embeddingIndexedVectors: previous.embeddingIndexedVectors,
          embeddingRemainingVectors: previous.embeddingRemainingVectors,
          embeddingLastError: 'Agent seems busy; showing last known embedding coverage. ' + (datasource.embeddingLastError || '')
        };
      }
      return merged;
    }

    function renderDatasourceIndexingLoadError(error) {
      const message = escapeHTML(error?.message || 'Could not load datasource status.');
      datasourceTaskStatus.innerHTML = '<div class="notice warn"><strong>Datasource tasks unavailable</strong><div class="muted">' + message + '</div></div>';
      datasourceStatusNode.innerHTML = '<div class="notice warn"><strong>Datasource coverage unavailable</strong><div class="muted">' + message + '</div></div>';
      searchCoverageStatus.innerHTML = '<div class="notice warn"><strong>Search coverage unavailable</strong><div class="muted">' + message + '</div></div>';
      renderCatalogDedupIdle();
    }

    function renderCatalogDedupIdle() {
      catalogDedupStatus.innerHTML = '<div class="notice">' +
        '<strong>Catalog integrity not checked</strong>' +
        '<div class="muted">Run this manually when duplicate/canonical catalog health needs inspection.</div>' +
        '<div class="actions"><button type="button" data-catalog-action="dedup-check">Check catalog integrity</button></div>' +
      '</div>';
    }

    function renderCatalogDedupStatus(status, lastError = '') {
      if (lastError) {
        catalogDedupStatus.innerHTML = '<div class="notice warn"><strong>Catalog integrity unavailable</strong><div class="muted">' + escapeHTML(lastError) + '</div>' +
          '<div class="actions"><button type="button" data-catalog-action="dedup-check">Check again</button></div></div>';
        return;
      }
      const sourceRows = Number(status.sourceRows || 0);
      if (!sourceRows) {
        catalogDedupStatus.innerHTML = '<div class="notice warn"><strong>No catalog rows</strong><div class="muted">Run reconciliation to populate the catalog.</div>' +
          '<div class="actions"><button type="button" data-catalog-action="dedup-check">Check again</button></div></div>';
        return;
      }
      const needsRepair = Boolean(status.needsRepair);
      catalogDedupStatus.innerHTML = '<dl>' + rows([
        ['Source rows', sourceRows],
        ['Canonical assets', Number(status.canonicalAssets || 0)],
        ['Visible assets', Number(status.activeAssets || 0)],
        ['Duplicate source rows', Number(status.duplicateSourceRows || 0)],
        ['Unlinked source rows', Number(status.unlinkedSourceRows || 0)],
        ['Orphan canonical rows', Number(status.orphanCanonicalRows || 0)]
      ]) + '</dl><div class="actions">' +
        '<button type="button" data-catalog-action="dedup-check">Check again</button>' +
        '<button type="button" data-catalog-action="dedup-repair">' + (needsRepair ? 'Repair catalog links' : 'Rebuild catalog links') + '</button>' +
        '<span class="' + (needsRepair ? 'status-warn' : 'status-ok') + '">' + (needsRepair ? 'Repair recommended' : 'Catalog links healthy') + '</span>' +
      '</div>';
    }

    async function loadCatalogDedupStatus() {
      catalogDedupStatus.innerHTML = loadingNotice('Checking catalog integrity', 'Counting canonical catalog links. This can take a while on large libraries.');
      try {
        const status = await api('/v1/catalog/dedup/status');
        renderCatalogDedupStatus(status || {});
        return true;
      } catch (error) {
        renderCatalogDedupStatus({}, error.message);
        return false;
      }
    }

    function renderDatasourceStatus(datasources, tasks = []) {
      const items = datasources || [];
      if (!items.length) {
        datasourceStatusNode.innerHTML = '<div class="notice warn"><strong>No datasource coverage</strong><div class="muted">Add a datasource to see found, browsable, and searchable media coverage.</div></div>';
        return;
      }
      datasourceStatusNode.innerHTML = '<table><thead><tr><th>Datasource</th><th>Found medias</th><th>Browsable medias</th><th>Searchable medias</th><th>Issues</th></tr></thead><tbody>' + items.map(datasource => {
        return '<tr>' +
          '<td>' + escapeHTML(datasource.name || datasource.sourceKey || '') + '<div class="muted">' + escapeHTML(datasource.sourceKey || '') + '</div></td>' +
          '<td>' + datasourceCoverageMetricHTML(datasource, 'foundMedias') + '</td>' +
          '<td>' + datasourceCoverageMetricHTML(datasource, 'browsableMedias') + '</td>' +
          '<td>' + datasourceCoverageMetricHTML(datasource, 'searchableMedias', 'updating') + '</td>' +
          '<td>' + datasourceCoverageIssueHTML(datasource) + '</td>' +
        '</tr>';
      }).join('') + '</tbody></table>';
    }

    async function loadDatasourceIndexingStatus(options = {}) {
      if (!datasourceIndexingLoaded) {
        renderDatasourceIndexingLoading();
      }
      try {
        const path = options.forceRefresh ? '/v1/datasources/indexing?refresh=1' : '/v1/datasources/indexing';
        const payload = await api(path);
        datasourceIndexingLoaded = true;
        renderDatasourceIndexingStatus(payload);
        if (!payload.statusSnapshotUsed && (!datasourceIndexingMessage.textContent || datasourceIndexingMessage.className === 'status-failed' || datasourceIndexingMessage.className === 'status-warn')) {
          datasourceIndexingMessage.textContent = '';
          datasourceIndexingMessage.className = 'muted';
        }
      } catch (error) {
        datasourceIndexingLoaded = true;
        if (!options.preserveOnError || !latestDatasourceIndexingStatus || !(latestDatasourceIndexingStatus.tasks || []).length) {
          renderDatasourceIndexingLoadError(error);
        }
        datasourceIndexingMessage.textContent = 'Showing the last datasource task status; waiting for a newer status. ' + error.message;
        datasourceIndexingMessage.className = 'status-warn';
      }
    }

    function renderSemanticModelsLoading() {
      semanticModelStatus.innerHTML = loadingNotice('Loading semantic models', 'Reading installed models and runtime pack state.');
    }

    function clearSemanticModelsLoadingRetry() {
      if (semanticModelsLoadingRetryTimer) {
        window.clearTimeout(semanticModelsLoadingRetryTimer);
        semanticModelsLoadingRetryTimer = null;
      }
      semanticModelsLoadingRetryAttempts = 0;
    }

    function scheduleSemanticModelsLoadingRetry() {
      if (semanticModelsLoadingRetryTimer || semanticModelsLoadingRetryAttempts >= semanticModelsLoadingRetryMaxAttempts) return;
      semanticModelsLoadingRetryAttempts += 1;
      semanticModelsLoadingRetryTimer = window.setTimeout(() => {
        semanticModelsLoadingRetryTimer = null;
        loadSemanticModels({ loadingRetry: true, forceRefresh: true }).catch(error => {
          semanticModelMessage.textContent = error.message;
          semanticModelMessage.className = 'status-warn';
        });
      }, semanticModelsLoadingRetryMs);
    }

    function renderSemanticModels(status) {
      status = status || {};
      latestSemanticModelStatus = status;
      const registryStatus = status.registryStatus || '';
      const registryLoading = registryStatus === 'loading';
      if (registryLoading) {
        scheduleSemanticModelsLoadingRetry();
      } else {
        clearSemanticModelsLoadingRetry();
      }
      const recommendedRuntimePack = status.recommendedRuntimePack || {};
      const runtimeNotice = renderSemanticRuntimePackNotice(recommendedRuntimePack, status);
      const models = semanticModelItems(status);
      let list = '';
      if (models.length) {
        list = '<div class="model-list">' + models.map(model => renderSemanticModelItem(model, status)).join('') + '</div>';
      } else if (registryLoading) {
        list = loadingNotice('Loading semantic models', 'Reading installed models and runtime pack state.');
      } else {
        list = '<div class="notice warn"><strong>No semantic models</strong><div class="muted">The model registry did not return any models for this platform.</div></div>';
      }
      semanticModelStatus.innerHTML = runtimeNotice + list;
      renderSemanticSearchModelOptions(status);
      if (latestDatasourceIndexingStatus) renderSearchCoverage(latestDatasourceIndexingStatus);
      applySemanticInstallJobState(latestSemanticInstallJob);
    }

    function semanticInstallJobRunning(job) {
      return Boolean(job && job.status === 'running');
    }

    function applySemanticInstallJobState(job) {
      const running = semanticInstallJobRunning(job);
      semanticModelStatus.querySelectorAll('button[data-semantic-action]').forEach(button => {
        button.disabled = running;
      });
    }

    function renderSemanticInstallJob(job) {
      latestSemanticInstallJob = job || { status: 'idle' };
      applySemanticInstallJobState(latestSemanticInstallJob);
      if (semanticInstallJobRunning(latestSemanticInstallJob)) {
        semanticModelMessage.textContent = latestSemanticInstallJob.message || latestSemanticInstallJob.label || 'Semantic install is running...';
        semanticModelMessage.className = 'muted';
        startSemanticInstallJobPolling();
        return;
      }
      stopSemanticInstallJobPolling();
      if (latestSemanticInstallJob.status === 'complete') {
        semanticModelMessage.textContent = latestSemanticInstallJob.message || 'Semantic install completed.';
        semanticModelMessage.className = 'status-ok';
      } else if (latestSemanticInstallJob.status === 'failed') {
        semanticModelMessage.textContent = latestSemanticInstallJob.errorMessage || latestSemanticInstallJob.message || 'Semantic install failed.';
        semanticModelMessage.className = 'status-failed';
      }
    }

    async function loadSemanticInstallJob(options = {}) {
      const previousRunning = semanticInstallJobRunning(latestSemanticInstallJob);
      const job = await api('/v1/semantic-install-job');
      renderSemanticInstallJob(job);
      if (options.refreshOnFinish && previousRunning && !semanticInstallJobRunning(job)) {
        await loadSemanticModels({ forceRefresh: true });
        await loadDatasourceIndexingStatus({ forceRefresh: true });
      }
      return job;
    }

    function startSemanticInstallJobPolling() {
      if (semanticInstallJobTimer) return;
      semanticInstallJobTimer = window.setInterval(() => {
        loadSemanticInstallJob({ refreshOnFinish: true }).catch(error => {
          semanticModelMessage.textContent = error.message;
          semanticModelMessage.className = 'status-warn';
        });
      }, 2000);
    }

    function stopSemanticInstallJobPolling() {
      if (!semanticInstallJobTimer) return;
      window.clearInterval(semanticInstallJobTimer);
      semanticInstallJobTimer = null;
    }

    function renderSemanticRuntimePackNotice(pack, status) {
      const registry = '<dl>' + rows([
        ['Registry', status.registryStatus || 'unknown'],
        ['Message', status.registryMessage || status.messageCode || ''],
      ]) + '</dl>';
      if (!pack || !pack.id) return registry;
      const installed = Boolean(pack.installed);
      const button = (!installed && pack.status === 'available') ?
        '<button type="button" data-semantic-action="install-runtime">Install runtime pack</button>' : '';
      return '<div class="notice">' +
        '<strong>Runtime pack</strong>' +
        '<div>' + escapeHTML(pack.name || pack.id) + ' ' + semanticTagHTML(installed ? 'installed' : (pack.status || 'available'), installed ? 'ok' : 'warn') + '</div>' +
        '<div class="muted">' + escapeHTML([pack.runtime || '', pack.sizeBytes ? formatBytes(pack.sizeBytes) : '', pack.license || ''].filter(Boolean).join(' / ')) + '</div>' +
        (button ? '<div class="actions">' + button + '</div>' : '') +
        '</div>' + registry;
    }

    function semanticModelItems(status) {
      const byKey = new Map();
      const add = profile => {
        if (!profile || !profile.modelId || !profile.vectorSpaceId) return;
        const key = semanticModelKey(profile.modelId, profile.vectorSpaceId);
        const existing = byKey.get(key) || {};
        byKey.set(key, mergeSemanticModelProfile(existing, profile));
      };
      add(status.active);
      add(status.recommended);
      add(status.candidate);
      (status.profiles || []).forEach(add);
      return Array.from(byKey.values()).sort((left, right) => semanticModelSortKey(left, status).localeCompare(semanticModelSortKey(right, status)));
    }

    function mergeSemanticModelProfile(existing, profile) {
      const merged = { ...existing, ...profile };
      if (existing.modelPack || profile.modelPack) {
        merged.modelPack = { ...(existing.modelPack || {}), ...(profile.modelPack || {}) };
      }
      if (existing.runtime || profile.runtime) {
        merged.runtime = { ...(existing.runtime || {}), ...(profile.runtime || {}) };
      }
      return merged;
    }

    function semanticModelKey(modelId, vectorSpaceId) {
      return String(modelId || '') + '|' + String(vectorSpaceId || '');
    }

    function semanticModelSortKey(model, status) {
      const rank = semanticModelIsActive(model, status) ? '0' :
        semanticModelIsRecommended(model, status) ? '1' :
        semanticModelIsIndexing(model, status) ? '2' :
        model.modelPack?.installed ? '3' : '4';
      return rank + ':' + (model.modelPack?.name || model.modelId || '');
    }

    function semanticModelIsActive(model, status) {
      return semanticSameModel(model, status.active) && model.profileKind === 'modelPack';
    }

    function semanticModelIsRecommended(model, status) {
      return semanticSameModel(model, status.recommended);
    }

	function semanticModelIsIndexing(model, status) {
	  const indexing = status.indexing || {};
	  if (!semanticSameModel(model, status.candidate) && !semanticSameModel(model, indexing)) return false;
	  return !semanticModelIsActive(model, status) && ['pending', 'backfilling', 'indexing'].includes(indexing.status || '');
	}

    function semanticSameModel(left, right) {
      return Boolean(left && right && left.modelId && right.modelId &&
        left.modelId === right.modelId && (left.vectorSpaceId || '') === (right.vectorSpaceId || ''));
    }

    function renderSemanticModelItem(model, status) {
      const pack = model.modelPack || {};
      const runtime = model.runtime || {};
	  const indexing = semanticSameModel(model, status.indexing) ? (status.indexing || {}) : {};
      const tags = semanticModelTags(model, status).map(tag => semanticTagHTML(tag.label, tag.kind)).join('');
      const title = pack.name || model.modelId || 'Semantic model';
      const description = semanticModelDescription(model, status);
      const actions = semanticModelActions(model, status);
      const meta = [
        ['Runtime', pack.runtime || runtime.runtime || runtime.loader || ''],
        ['Languages', (pack.queryLanguages || []).join(', ') || ''],
        ['Embedding', model.embeddingDim ? String(model.embeddingDim) + 'd' : ''],
        ['Size', pack.sizeBytes ? formatBytes(pack.sizeBytes) : ''],
        ['Quantization', pack.quantization || ''],
        ['License', pack.license || ''],
	    ['Vectors', indexing.modelId ? String(indexing.completedVectorCount || 0) + ' / ' + String(indexing.eligibleAssetCount || 0) : ''],
	    ['HNSW', indexing.modelId ? String(indexing.indexedVectorCount || 0) + ' / ' + String(indexing.completedVectorCount || 0) : ''],
      ].filter(([, value]) => value !== '');
      return '<details class="model-item">' +
        '<summary>' +
          '<span class="model-title"><strong>' + escapeHTML(title) + '</strong><span class="muted">' + escapeHTML(description) + '</span></span>' +
          '<span class="model-tags">' + tags + '</span>' +
        '</summary>' +
        '<div class="model-body">' +
          '<div class="model-meta">' + meta.map(([label, value]) =>
            '<div class="model-meta-item"><span class="model-meta-label">' + escapeHTML(label) + '</span><span class="model-meta-value">' + escapeHTML(value) + '</span></div>'
          ).join('') + '</div>' +
          '<div class="muted">' + escapeHTML([model.modelId || '', model.vectorSpaceId || ''].filter(Boolean).join(' / ')) + '</div>' +
          (runtime.status ? '<div class="muted">Runtime: ' + escapeHTML(runtime.status + (runtime.messageCode ? ' / ' + runtime.messageCode : '')) + '</div>' : '') +
          (actions ? '<div class="actions">' + actions + '</div>' : '<div class="muted">No action available.</div>') +
        '</div>' +
      '</details>';
    }

    function semanticTagHTML(label, kind) {
      return '<span class="tag ' + escapeHTML(kind || '') + '">' + escapeHTML(label || '') + '</span>';
    }

    function semanticModelTags(model, status) {
      const tags = [];
      const pack = model.modelPack || {};
      if (pack.installed) tags.push({ label: 'installed', kind: 'ok' });
      if (semanticModelIsActive(model, status)) tags.push({ label: 'active', kind: 'ok' });
      if (semanticModelIsRecommended(model, status)) tags.push({ label: 'recommended', kind: 'ok' });
      if (semanticModelIsIndexing(model, status)) tags.push({ label: 'indexing', kind: 'warn' });
      if (pack.installed && !semanticModelIsActive(model, status) && !semanticModelIsRecommended(model, status) && !semanticModelIsIndexing(model, status)) {
        tags.push({ label: 'deprecated', kind: 'warn' });
      }
      if (!pack.installed && pack.status === 'available') tags.push({ label: 'available', kind: '' });
      if (pack.status === 'unsupported_platform') tags.push({ label: 'unsupported', kind: 'failed' });
      if ((pack.queryLanguages || []).length > 1) tags.push({ label: 'multilingual', kind: '' });
      if (pack.quantization) tags.push({ label: pack.quantization, kind: '' });
      return tags;
    }

    function semanticModelDescription(model, status) {
      const pack = model.modelPack || {};
      const name = (pack.name || model.modelId || '').toLowerCase();
      if (name.includes('siglip') && name.includes('patch16')) {
        return 'Standard multilingual image search model with the best current balance for Timich.';
      }
      if ((pack.quantization || '').toLowerCase().includes('int8')) {
        return 'Lower memory model pack for constrained machines.';
      }
      if (pack.runtime || pack.queryLanguages?.length) {
        return ['Image semantic search model', pack.runtime, (pack.queryLanguages || []).join(', ')].filter(Boolean).join(' / ') + '.';
      }
      return 'Semantic image search model.';
    }

    function semanticModelActions(model, status) {
      const pack = model.modelPack || {};
      const runtime = model.runtime || {};
	  const indexing = semanticSameModel(model, status.indexing) ? (status.indexing || {}) : {};
      const attrs = ' data-model-id="' + escapeHTML(model.modelId || '') + '" data-vector-space-id="' + escapeHTML(model.vectorSpaceId || '') + '"';
      const actions = [];
      if (!pack.installed && pack.status === 'available') {
        actions.push('<button type="button" class="primary" data-semantic-action="install-model"' + attrs + '>Install</button>');
      }
      const ready = pack.installed && !semanticModelIsActive(model, status) && runtime.loaded && runtime.canEmbed &&
	    indexing.status === 'ready' &&
	    Number(indexing.completedVectorCount || 0) >= Number(indexing.eligibleAssetCount || 0) &&
	    Number(indexing.indexedVectorCount || 0) >= Number(indexing.completedVectorCount || 0);
      if (ready) {
        actions.push('<button type="button" class="primary" data-semantic-action="activate-model"' + attrs + '>Activate</button>');
      }
      if (pack.installed && !semanticModelIsActive(model, status) && !semanticModelIsIndexing(model, status)) {
        actions.push('<button type="button" class="danger" data-semantic-action="uninstall-model"' + attrs + '>Uninstall</button>');
      }
      return actions.join('');
    }

    function renderSemanticSearchModelOptions(status) {
      if (!semanticSearchPreviewModel) return;
      const selected = semanticSearchPreviewModel.value;
      const options = ['<option value="">Auto</option>'];
      const seen = new Set();
      (status.profiles || []).forEach(profile => {
        const pack = profile.modelPack || {};
        if (profile.profileKind !== 'modelPack' || !profile.modelId || !pack.installed) return;
        const value = profile.modelId + '|' + (profile.vectorSpaceId || '');
        if (seen.has(value)) return;
        seen.add(value);
        const label = (pack.name || profile.modelId) + ' / ' + String(profile.embeddingDim || '') + 'd';
        options.push('<option value="' + escapeHTML(value) + '"' + (value === selected ? ' selected' : '') + '>' + escapeHTML(label) + '</option>');
      });
      semanticSearchPreviewModel.innerHTML = options.join('');
    }

    async function loadSemanticModels(options = {}) {
      if (!semanticModelsLoaded) {
        renderSemanticModelsLoading();
      }
      if (!options.loadingRetry) {
        semanticModelsLoadingRetryAttempts = 0;
      }
      try {
        semanticModelsLoaded = true;
        renderSemanticModels(await api(options.forceRefresh ? '/v1/semantic-models?refresh=1' : '/v1/semantic-models?cached=1'));
        if ((!latestSemanticInstallJob || latestSemanticInstallJob.status === 'idle') &&
            (!semanticModelMessage.textContent || semanticModelMessage.className === 'status-failed')) {
          semanticModelMessage.textContent = '';
          semanticModelMessage.className = 'muted';
        }
      } catch (error) {
        semanticModelsLoaded = true;
        semanticModelStatus.innerHTML = '<div class="notice failed"><strong>Semantic models unavailable</strong><div class="muted">' + escapeHTML(error.message) + '</div></div>';
      }
    }

    function renderSemanticSearchPreview(page) {
      const semantic = page?.resolved?.semantic || {};
      const items = page?.items || [];
      const summary = '<dl>' + rows([
        ['Mode', page?.resolved?.queryMode || ''],
        ['Semantic status', semanticSearchPreviewStatusLabel(semantic.status)],
        ['Model', semantic.modelId || ''],
        ['Profile', semantic.profileKind || ''],
        ['Input', semantic.inputKind || ''],
        ['Indexed vectors', semantic.modelId ? String(semantic.indexedVectorCount || 0) : ''],
        ['Embedded vectors', semantic.modelId ? String(semantic.completedVectorCount || 0) : ''],
        ['Total', page ? String(page.total || 0) + ' (' + (page.totalAccuracy || '') + ')' : ''],
        ['Elapsed', formatSearchElapsed(page?.elapsedMs)],
        ['Message', semanticSearchPreviewMessageLabel(semantic.messageCode)],
      ]) + '</dl>';
      if (!items.length) {
        semanticSearchPreviewResult.innerHTML = summary + '<div class="muted">No results</div>';
        return;
      }
      const grid = '<div class="asset-preview-grid">' + items.map(asset => {
        const previewURL = '/v1/assets/' + encodeURIComponent(asset.id || '') + '/preview';
        const score = typeof asset.semanticScore === 'number' ? asset.semanticScore.toFixed(3) : '';
        return '<div class="asset-preview-card">' +
          '<img class="asset-preview-thumb" src="' + previewURL + '" alt="">' +
          '<div class="asset-preview-title">' + escapeHTML(asset.filename || asset.id || '') + '</div>' +
          '<div class="asset-preview-date">' + escapeHTML([date(asset.capturedAt), score ? 'score ' + score : ''].filter(Boolean).join(' / ')) + '</div>' +
        '</div>';
      }).join('') + '</div>';
      semanticSearchPreviewResult.innerHTML = summary + grid;
    }

    function formatSearchElapsed(elapsedMs) {
      if (typeof elapsedMs !== 'number' || !Number.isFinite(elapsedMs) || elapsedMs <= 0) {
        return '';
      }
      if (elapsedMs < 1000) {
        return String(Math.round(elapsedMs)) + ' ms';
      }
      return (elapsedMs / 1000).toFixed(elapsedMs < 10000 ? 2 : 1) + ' s';
    }

    function semanticSearchPreviewStatusLabel(status) {
      switch (status || '') {
        case 'backfilling':
        case 'indexing':
        case 'pending':
          return 'indexing';
        case 'ready':
          return 'ready';
        case 'missing':
          return 'not indexed';
        case 'unavailable':
          return 'unavailable';
        default:
          return status || '';
      }
    }

    function semanticSearchPreviewMessageLabel(messageCode) {
      switch (messageCode || '') {
        case 'semantic_index_backfilling':
          return 'Semantic index is still being built; results use the currently indexed vectors.';
        case 'semantic_index_missing':
          return 'Semantic index has not been built yet.';
        case 'semantic_index_unavailable':
          return 'Semantic index is unavailable.';
        case 'semantic_index_missing_filename_fallback':
          return 'Semantic index is missing; filename search was used.';
        case 'semantic_index_unavailable_filename_fallback':
          return 'Semantic index is unavailable; filename search was used.';
        case 'semantic_profile_validation':
          return 'Validation profile is active.';
        default:
          return messageCode || '';
      }
    }

    function semanticSearchPreviewRequest(query) {
      const request = {
        collection: {
          kind: 'search',
          query: {
            text: query,
            mode: 'semantic'
          }
        },
        page: {
          index: 0,
          size: 20
        }
      };
      const selectedModel = (semanticSearchPreviewModel?.value || '').split('|');
      if (selectedModel[0]) {
        request.semanticModelId = selectedModel[0];
        request.semanticVectorSpaceId = selectedModel[1] || '';
      }
      return request;
    }

    function rootOptions(selected) {
      const options = ['<option value="">Select root</option>'];
      latestUploadRoots.forEach(root => {
        options.push('<option value="' + escapeHTML(root.key) + '"' + (root.key === selected ? ' selected' : '') + '>' + escapeHTML(root.key + ' / ' + (root.status || '')) + '</option>');
      });
      return options.join('');
    }

    function effectiveDeviceUploadStatus(policy, rootsByKey) {
      const upload = policy.upload || {};
      const status = policy.status || {};
      if (!upload.enabled || !upload.rootKey) {
        return status;
      }
      const root = rootsByKey.get(upload.rootKey);
      if (root && root.status && root.status !== 'ready') {
        return {
          state: 'blocked',
          reason: root.message || 'Upload root is blocked.',
          root
        };
      }
      return status;
    }

    function deviceUploadStatusClass(status) {
      if (status.state === 'ready') return 'status-ok';
      if (status.state === 'disabled') return 'muted';
      return 'status-warn';
    }

    function datetimeLocalValue(value) {
      if (!value) return '';
      const parsed = new Date(value);
      if (Number.isNaN(parsed.getTime())) return '';
      const local = new Date(parsed.getTime() - parsed.getTimezoneOffset() * 60000);
      return local.toISOString().slice(0, 16);
    }

    function datetimeLocalToISOString(value) {
      if (!value) return null;
      const parsed = new Date(value);
      return Number.isNaN(parsed.getTime()) ? null : parsed.toISOString();
    }

    function updateSetupTask(id, patch) {
      renderSetupTasks(latestSetupTasks.map(task => task.id === id ? { ...task, ...patch } : task));
    }

    function datasourceSetupStatus(check) {
      if (check.status === 'ok') return 'complete';
      if (check.status === 'warning') return 'pending';
      return 'failed';
    }

    function datasourceCheckSummary(check) {
      if (check.status === 'ok') return 'Datasource is configured and reachable from this Agent.';
      const parts = [check.summary || 'Datasource check failed.'];
      if (check.remediation) {
        parts.push(check.remediation);
      }
      return parts.join(' ');
    }

    function datasourceTaskPatch(check) {
      return {
        status: datasourceSetupStatus(check),
        summary: datasourceCheckSummary(check)
      };
    }

    function invalidateDatasourceCheckRequests() {
      datasourceCheckGeneration += 1;
    }

    function clearDatasourceCheck() {
      latestDatasourceCheck = null;
    }

    function canApplyDatasourceCheck(generation, datasourceURLSnapshot) {
      return generation === datasourceCheckGeneration && datasourceURLSnapshot === activeDatasourceURL;
    }

    function applyDatasourceCheck(check, datasourceURLSnapshot) {
      latestDatasourceCheck = { check, datasourceURL: datasourceURLSnapshot };
      updateSetupTask('datasource', datasourceTaskPatch(check));
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

    function shouldCheckDatasourceReachability(datasource) {
      return datasource && isImmichDatasourceKind(datasource.kind || 'immich');
    }

    function mediaHelperLabel(mediaRuntime) {
      if (!mediaRuntime?.mediaHelperAvailable) return 'Unavailable';
      const parts = ['timich-media-helper'];
      const status = mediaRuntime.mediaHelperStatus || (mediaRuntime.mediaHelperUsable ? 'ready' : 'unknown');
      if (mediaRuntime.mediaHelperAutoDetected) parts.push('(auto)');
      if (status) parts.push('/ ' + status);
      if (mediaRuntime.mediaHelperVersion) parts.push('/ ' + mediaRuntime.mediaHelperVersion);
      if (mediaRuntime.mediaHelperPlatform) parts.push('/ ' + mediaRuntime.mediaHelperPlatform);
      const capabilities = [];
      if (mediaRuntime.mediaHelperRenderImage) capabilities.push('image');
      if (mediaRuntime.mediaHelperRenderVideoPoster) capabilities.push('video poster');
      if (capabilities.length > 0) parts.push('/ ' + capabilities.join(', '));
      if (mediaRuntime.mediaHelperLastError) parts.push('/ ' + mediaRuntime.mediaHelperLastError);
      return parts.join(' ');
    }

    function localImageBackendLabel(mediaRuntime) {
      if (!mediaRuntime?.mediaHelperAvailable) return 'Unavailable / media helper unavailable';
      const parts = [mediaRuntime.mediaHelperRenderImage ? 'Ready' : 'Unavailable'];
      if (mediaRuntime.vipsAvailable) {
        parts.push('/ libvips' + (mediaRuntime.vipsBundled ? ' (bundled)' : (mediaRuntime.vipsAutoDetected ? ' (auto)' : '')));
      } else {
        parts.push('/ libvips unavailable');
      }
      if (!mediaRuntime.mediaHelperRenderImage && mediaRuntime.mediaHelperLastError) parts.push('/ ' + mediaRuntime.mediaHelperLastError);
      if (!mediaRuntime.mediaHelperRenderImage && !mediaRuntime.vipsAvailable && !mediaRuntime.mediaHelperLastError) parts.push('/ vips was not found');
      return parts.join(' ');
    }

    function videoPosterBackendLabel(mediaRuntime) {
      if (!mediaRuntime?.mediaHelperAvailable) return 'Unavailable';
      const parts = [mediaRuntime.mediaHelperRenderVideoPoster ? 'Ready' : 'Unavailable'];
      if (!mediaRuntime?.ffmpegAvailable) return parts.join(' ') + ' / ffmpeg unavailable';
      parts.push('/ ffmpeg');
      const status = mediaRuntime.ffmpegStatus || (mediaRuntime.ffmpegUsable ? 'ready' : 'unknown');
      if (mediaRuntime.ffmpegAutoDetected) parts.push('(auto)');
      if (status) parts.push('/ ' + status);
      if (mediaRuntime.ffmpegVersion) parts.push('/ ' + mediaRuntime.ffmpegVersion);
      if (mediaRuntime.ffmpegDecoders) parts.push('/ ' + mediaRuntime.ffmpegDecoders);
      if (mediaRuntime.ffmpegLastError) parts.push('/ ' + mediaRuntime.ffmpegLastError);
      return parts.join(' ');
    }

    nearbyLinkForm.addEventListener('submit', async event => {
      event.preventDefault();
      const button = nearbyLinkForm.querySelector('button[type="submit"]');
      const linkCode = nearbyLinkCode.value.trim();
      if (!linkCode) {
        nearbyLinkMessage.textContent = 'Enter the Link Code shown on the device';
        nearbyLinkMessage.className = 'status-warn';
        nearbyLinkCode.focus();
        return;
      }
      button.disabled = true;
      nearbyLinkMessage.textContent = 'Approving...';
      nearbyLinkMessage.className = 'muted';
      try {
        const link = await api('/v1/nearby-links/approve', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ linkCode }),
        });
        nearbyLinkCode.value = '';
        nearbyLinkMessage.textContent = 'Approved ' + (link.deviceName || 'device') + '. Keep the app open until pairing finishes.';
        nearbyLinkMessage.className = 'status-ok';
        await loadStatus();
      } catch (error) {
        nearbyLinkMessage.textContent = error.message;
        nearbyLinkMessage.className = 'status-failed';
      } finally {
        button.disabled = false;
      }
    });

    async function loadStatus({ checkDatasourceReachability = false } = {}) {
      const status = await api('/status');
      const datasources = status.datasources || [];
      latestDatasources = datasources;
      latestLocalMediaRoots = status.localMediaRoots || [];
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
        ['Storage', (status.storageGuardrail?.writeBlocked ? 'Blocked' : 'Ready') + ' / free ' + formatBytes(status.storageGuardrail?.availableBytes) + ' / min ' + formatBytes(status.storageGuardrail?.minFreeBytes)],
        ['Media helper', mediaHelperLabel(status.mediaRuntime)],
        ['Local image backend', localImageBackendLabel(status.mediaRuntime)],
        ['Video poster backend', videoPosterBackendLabel(status.mediaRuntime)],
        ['Semantic ONNX runtime', (status.semanticRuntime?.onnxRuntime?.status || 'unavailable') + ' / processes ' + String(status.semanticRuntime?.onnxRuntime?.processCount || 0)],
      ]);
      remoteBrowsingList.innerHTML = rows([
        ['Enabled', status.remoteBrowsing?.enabled ? 'Yes' : 'No'],
        ['Relay server', status.remoteBrowsing?.serverURL || ''],
        ['Registration', status.remoteBrowsing?.registrationStatus || ''],
        ['Registered at', date(status.remoteBrowsing?.relayCredentialSyncedAt)],
        ['Waiting for', (status.remoteBrowsing?.registrationBlockedBy || []).join(', ')],
      ]);
      renderDatasourceRootOptions(latestLocalMediaRoots);
      renderDatasources(datasources);
      updateDatasourceFormMode();
      const primaryDatasource = datasources[0];
      activeDatasourceURL = primaryDatasource?.url || '';
      if (!primaryDatasource || (latestDatasourceCheck && latestDatasourceCheck.datasourceURL !== activeDatasourceURL)) {
        clearDatasourceCheck();
      }
      renderSetupTasks(status.setupTasks || []);
      if (checkDatasourceReachability && shouldCheckDatasourceReachability(primaryDatasource)) {
        updateSetupTask('datasource', {
          status: 'checking',
          summary: 'Checking datasource reachability from this Agent...'
        });
        void checkDatasource();
      }
    }

    async function restoreDatasourceSaveFailureState(attemptedDatasource) {
      try {
        await loadStatus();
      } catch (_) {
        // Keep the original save error visible even if status refresh fails.
      }
      datasourceName.value = attemptedDatasource.name;
      datasourceKind.value = attemptedDatasource.kind || 'immich';
      datasourceURL.value = attemptedDatasource.url;
      datasourceAccessToken.value = attemptedDatasource.accessToken;
      datasourceRootKey.value = attemptedDatasource.rootKey || '';
      updateDatasourceFormMode();
    }

    function renderDeviceSummary(device) {
      const lastRefreshedAt = date(device.lastRefreshedAt) || 'Unknown';
      return '<div class="device-summary">' +
        '<div class="device-summary-item"><span class="device-summary-label">ID</span><span class="device-summary-value">' + escapeHTML(device.deviceId) + '</span></div>' +
        '<div class="device-summary-item"><span class="device-summary-label">Device name (display)</span><span class="device-summary-value" data-device-display-name>' + escapeHTML(device.deviceName || 'Unnamed device') + '</span></div>' +
        '<div class="device-meta"><div class="device-meta-text"><span class="device-meta-label">Last refreshed</span><span>' + escapeHTML(lastRefreshedAt) + '</span></div><button class="danger" type="button" data-revoke="' + escapeHTML(device.deviceId) + '">Revoke</button></div>' +
      '</div>';
    }

    function renderDeviceNameSettings(device) {
      return '<div class="device-subsection device-name-section">' +
        '<div class="device-subsection-title">Device name settings</div>' +
        '<form class="device-name-form" data-device-rename="' + escapeHTML(device.deviceId) + '">' +
          '<label>Device name<input name="deviceName" value="' + escapeHTML(device.deviceName || '') + '" autocomplete="off" required></label>' +
          '<button type="submit">Save device name</button><span class="muted" data-device-message="' + escapeHTML(device.deviceId) + '"></span>' +
        '</form>' +
      '</div>';
    }

    function renderDeviceUploadSettings(device, upload, status, roots) {
      const disabled = roots.length ? '' : ' disabled';
      const rootKey = upload.rootKey || '';
      const statusReason = status.reason || '';
      return '<div class="device-subsection device-upload-section">' +
        '<div class="device-upload-head">' +
          '<div class="device-subsection-title">Upload settings</div>' +
          '<div class="device-upload-status"><span class="' + deviceUploadStatusClass(status) + '">' + escapeHTML(status.state || 'unknown') + '</span><div class="muted">' + escapeHTML(statusReason) + '</div></div>' +
        '</div>' +
        '<form class="device-upload-form" data-upload-policy="' + escapeHTML(device.deviceId) + '">' +
          '<label>Mode<span class="checkbox-label"><input type="checkbox" name="enabled"' + (upload.enabled ? ' checked' : '') + '> Enabled</span></label>' +
          '<label>Root<select name="rootKey"' + disabled + '>' + rootOptions(rootKey) + '</select></label>' +
          '<label>Path pattern<input name="pathPattern" value="' + escapeHTML(upload.pathPattern || '{deviceName}/{yyyy}-{MM}-{dd}/{filename}') + '" autocomplete="off"></label>' +
          '<label>Captured after<input name="capturedAfter" type="datetime-local" value="' + escapeHTML(datetimeLocalValue(upload.capturedAfter)) + '"></label>' +
          '<div class="actions"><button class="primary" type="submit">Save upload policy</button><span class="muted" data-upload-message="' + escapeHTML(device.deviceId) + '"></span></div>' +
        '</form>' +
        '<div class="device-reset-section">' +
          '<div class="device-reset-title">Upload state reset</div>' +
          '<form class="device-reset-form" data-upload-reset="' + escapeHTML(device.deviceId) + '">' +
            '<label>Reset from<input name="capturedAfter" type="datetime-local" required></label>' +
            '<label>Reset before<input name="capturedBefore" type="datetime-local" required></label>' +
            '<div class="actions"><button class="danger" type="submit">Reset upload state</button><span class="muted" data-reset-message="' + escapeHTML(device.deviceId) + '"></span></div>' +
          '</form>' +
        '</div>' +
      '</div>';
    }

    async function loadDevices() {
      const roots = await loadUploadRoots();
      const payload = await api('/v1/devices');
      const devices = payload.devices || [];
      if (devices.length === 0) {
        devicesNode.innerHTML = '<div class="muted">No paired devices</div>';
        return;
      }
      const policies = await Promise.all(devices.map(device => api('/v1/devices/' + encodeURIComponent(device.deviceId) + '/upload-policy').catch(error => ({ error: error.message, deviceId: device.deviceId }))));
      const policyByDevice = new Map(policies.map(policy => [policy.deviceId, policy]));
      const rootsByKey = new Map((roots || []).map(root => [root.key, root]));
      devicesNode.innerHTML = '<div class="device-list">' + devices.map(device => {
        const policy = policyByDevice.get(device.deviceId) || {};
        const upload = policy.upload || {};
        const status = effectiveDeviceUploadStatus(policy, rootsByKey);
        return '<div class="device-card" data-device-card="' + escapeHTML(device.deviceId) + '">' +
          renderDeviceSummary(device) +
          renderDeviceNameSettings(device) +
          renderDeviceUploadSettings(device, upload, status, roots) +
        '</div>';
      }).join('') + '</div>';
      devicesNode.querySelectorAll('[data-device-rename]').forEach(form => {
        form.addEventListener('submit', async event => {
          event.preventDefault();
          const deviceID = form.dataset.deviceRename;
          const message = form.querySelector('[data-device-message]');
          const button = form.querySelector('button[type="submit"]');
          const nextName = form.elements.deviceName.value.trim();
          if (!nextName) {
            message.textContent = 'Device name is required';
            message.className = 'status-failed';
            return;
          }
          button.disabled = true;
          message.textContent = 'Renaming...';
          message.className = 'muted';
          try {
            const device = await api('/v1/devices/' + encodeURIComponent(deviceID), {
              method: 'PUT',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ deviceName: nextName })
            });
            form.elements.deviceName.value = device.deviceName || nextName;
            const card = form.closest('[data-device-card]');
            const displayName = card ? card.querySelector('[data-device-display-name]') : null;
            if (displayName) {
              displayName.textContent = device.deviceName || nextName;
            }
            message.textContent = 'Renamed';
            message.className = 'status-ok';
          } catch (error) {
            message.textContent = error.message;
            message.className = 'status-failed';
          } finally {
            button.disabled = false;
          }
        });
      });
      devicesNode.querySelectorAll('[data-upload-policy]').forEach(form => {
        form.addEventListener('submit', async event => {
          event.preventDefault();
          const deviceID = form.dataset.uploadPolicy;
          const message = form.querySelector('[data-upload-message]');
          const button = form.querySelector('button[type="submit"]');
          button.disabled = true;
          message.textContent = 'Saving...';
          message.className = 'muted';
          try {
            await api('/v1/devices/' + encodeURIComponent(deviceID) + '/upload-policy', {
              method: 'PUT',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({
                enabled: form.elements.enabled.checked,
                rootKey: form.elements.rootKey.value,
                pathPattern: form.elements.pathPattern.value,
                capturedAfter: datetimeLocalToISOString(form.elements.capturedAfter.value)
              })
            });
            message.textContent = 'Saved';
            message.className = 'status-ok';
            await loadDevices();
          } catch (error) {
            message.textContent = error.message;
            message.className = 'status-failed';
          } finally {
            button.disabled = false;
          }
        });
      });
      devicesNode.querySelectorAll('[data-upload-reset]').forEach(form => {
        form.addEventListener('submit', async event => {
          event.preventDefault();
          const deviceID = form.dataset.uploadReset;
          const message = form.querySelector('[data-reset-message]');
          const button = form.querySelector('button[type="submit"]');
          button.disabled = true;
          message.textContent = 'Resetting...';
          message.className = 'muted';
          try {
            const result = await api('/v1/devices/' + encodeURIComponent(deviceID) + '/upload-reset', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({
                capturedAfter: datetimeLocalToISOString(form.elements.capturedAfter.value),
                capturedBefore: datetimeLocalToISOString(form.elements.capturedBefore.value),
                reason: 'admin reset'
              })
            });
            const cleanupErrors = result.tempCleanupErrors || [];
            const summary = 'Removed ' + (result.removedUploadedAssets || 0) + ' uploaded rows, ' + (result.removedSessions || 0) + ' sessions, and ' + (result.removedTempFiles || 0) + ' temp files';
            if (cleanupErrors.length) {
              message.textContent = summary + '; temp cleanup warning: ' + cleanupErrors[0] + (cleanupErrors.length > 1 ? ' +' + (cleanupErrors.length - 1) + ' more' : '');
              message.className = 'status-warn';
            } else {
              message.textContent = summary;
              message.className = 'status-ok';
            }
          } catch (error) {
            message.textContent = error.message;
            message.className = 'status-failed';
          } finally {
            button.disabled = false;
          }
        });
      });
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
      datasourceMessage.textContent = 'Adding...';
      datasourceMessage.className = 'muted';
      const kind = datasourceKind.value || 'immich';
      const payload = {
        name: datasourceName.value,
        kind
      };
      if (isImmichDatasourceKind(kind)) {
        payload.url = datasourceURL.value;
        payload.accessToken = datasourceAccessToken.value;
      } else if (kind === 'local_filesystem') {
        payload.rootKey = datasourceRootKey.value;
      }
      const attemptedDatasource = {
        name: datasourceName.value,
        kind,
        url: datasourceURL.value,
        accessToken: datasourceAccessToken.value,
        rootKey: datasourceRootKey.value
      };
      const shouldCheckPrimaryDatasource = isImmichDatasourceKind(kind) && latestDatasources.length === 0;
      invalidateDatasourceCheckRequests();
      try {
        await api('/v1/datasources', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(payload)
        });
        clearDatasourceCheck();
        datasourceName.value = '';
        datasourceURL.value = '';
        datasourceAccessToken.value = '';
        datasourceMessage.textContent = 'Added datasource.';
        datasourceMessage.className = 'status-ok';
        await loadStatus({ checkDatasourceReachability: shouldCheckPrimaryDatasource });
        await loadDatasourceIndexingStatus({ forceRefresh: true });
      } catch (error) {
        await restoreDatasourceSaveFailureState(attemptedDatasource);
        datasourceMessage.textContent = error.message;
        datasourceMessage.className = 'status-failed';
      } finally {
        button.disabled = false;
        updateDatasourceFormMode();
      }
    });

    datasourceList.addEventListener('change', async event => {
      const input = event.target.closest('[data-local-immich-fallback]');
      if (!input) return;
      const sourceKey = input.dataset.localImmichFallback || '';
      const enabled = input.checked;
      input.disabled = true;
      datasourceMessage.textContent = 'Saving Immich fallback setting...';
      datasourceMessage.className = 'muted';
      try {
        await api('/v1/datasources/local/immich-fallback', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ sourceKey, enabled })
        });
        datasourceMessage.textContent = enabled ? 'Immich fallback enabled.' : 'Immich fallback disabled.';
        datasourceMessage.className = 'status-ok';
        await loadStatus();
      } catch (error) {
        input.checked = !enabled;
        input.disabled = false;
        datasourceMessage.textContent = error.message;
        datasourceMessage.className = 'status-failed';
      }
    });

    datasourceList.addEventListener('click', async event => {
      const button = event.target.closest('[data-accept-local-root]');
      if (!button) return;
      const sourceKey = button.dataset.sourceKey || '';
      const rootKey = button.dataset.rootKey || '';
      const observedIdentity = button.dataset.observedIdentity || '';
      if (!confirm('Accept the currently mounted root for ' + rootKey + '? Media that exists only in the previous root may become missing.')) return;
      button.disabled = true;
      datasourceMessage.textContent = 'Accepting the current root and running reconciliation...';
      datasourceMessage.className = 'muted';
      try {
        const result = await api('/v1/datasources/local/root/accept', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ sourceKey, rootKey, observedIdentity })
        });
        const scanCompleted = result?.scanStatus === 'completed';
        datasourceMessage.textContent = scanCompleted ?
          'Current root accepted and reconciliation completed.' :
          'Current root accepted, but reconciliation did not complete' + (result?.scanError ? ': ' + result.scanError : '. Review the scan status before continuing.');
        datasourceMessage.className = scanCompleted ? 'status-ok' : 'status-warn';
        await loadStatus();
        await loadDatasourceIndexingStatus({ forceRefresh: true });
      } catch (error) {
        datasourceMessage.textContent = error.message;
        datasourceMessage.className = 'status-failed';
        await loadStatus();
      }
    });

    datasourceKind.addEventListener('change', updateDatasourceFormMode);
    datasourceAddPanel?.addEventListener('toggle', event => {
      if (!syncingDatasourceAddPanel && event.isTrusted) {
        datasourceAddPanelTouched = true;
      }
    });
    heavyTaskWorkersMode.addEventListener('change', updateWorkerCustomField);

    workerSettingsForm.addEventListener('submit', async event => {
      event.preventDefault();
      const button = workerSettingsForm.querySelector('button[type="submit"]');
      button.disabled = true;
      workerRuntimeMessage.textContent = 'Saving...';
      workerRuntimeMessage.className = 'muted';
      try {
        const configured = selectedWorkerCount();
        const status = await api('/v1/workers', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ heavyTaskWorkers: configured })
        });
        renderWorkerRuntime(status);
        await Promise.all([
          loadStatus(),
          loadSemanticModels({ forceRefresh: true }),
        ]);
        workerRuntimeMessage.textContent = 'Saved worker settings';
        workerRuntimeMessage.className = 'status-ok';
      } catch (error) {
        workerRuntimeMessage.textContent = error.message;
        workerRuntimeMessage.className = 'status-failed';
        await loadWorkerRuntime();
      } finally {
        button.disabled = false;
      }
    });

    datasourceTaskStatus.addEventListener('click', async event => {
      const noteButton = event.target.closest('button[data-datasource-task-note-trigger]');
      if (noteButton) {
        toggleDatasourceTaskNote(noteButton);
        return;
      }
      const button = event.target.closest('button[data-datasource-task-action]');
      if (!button) return;
      const action = button.dataset.datasourceTaskAction || '';
      if (action === 'go-to-search') {
        location.hash = 'search';
        return;
      }
      if (pendingDatasourceTaskActions.has(action)) return;
      markDatasourceTaskActionPending(action);
      datasourceIndexingMessage.className = 'muted';
      datasourceIndexingMessage.textContent = '';
      refreshLiveStatus({ preserveOnError: true });
      try {
        if (action === 'media-discovery') {
          const result = await api('/v1/datasources/indexing/run', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ mode: 'full' })
          });
          rememberDatasourceDiscoveryCompletedAt(result);
          await loadDatasourceIndexingStatus({ preserveOnError: true, forceRefresh: true });
        } else if (action === 'requeue-thumbnails') {
          await api('/v1/datasources/local/thumbnails/repair', { method: 'POST' });
          await loadDatasourceIndexingStatus({ preserveOnError: true, forceRefresh: true });
        } else if (action === 'requeue-metadata') {
          await api('/v1/datasources/local/metadata/repair', { method: 'POST' });
          await loadDatasourceIndexingStatus({ preserveOnError: true, forceRefresh: true });
        }
      } catch (error) {
        datasourceIndexingMessage.textContent = error.message;
        datasourceIndexingMessage.className = 'status-failed';
        await loadDatasourceIndexingStatus();
      } finally {
        pendingDatasourceTaskActions.delete(action);
        await loadDatasourceIndexingStatus({ preserveOnError: true });
      }
    });

    document.addEventListener('click', event => {
      if (!event.target.closest('[data-datasource-task-note-trigger], [data-datasource-task-note]')) {
        closeDatasourceTaskNote();
      }
    });
    document.addEventListener('keydown', event => {
      if (event.key === 'Escape') closeDatasourceTaskNote();
    });
    window.addEventListener('resize', closeDatasourceTaskNote);
    document.addEventListener('scroll', closeDatasourceTaskNote, true);

    catalogDedupStatus.addEventListener('click', async event => {
      const button = event.target.closest('button[data-catalog-action]');
      if (!button) return;
      const action = button.dataset.catalogAction || '';
      button.disabled = true;
      datasourceIndexingMessage.textContent = action === 'dedup-repair' ? 'Repairing catalog links...' : 'Checking catalog integrity...';
      datasourceIndexingMessage.className = 'muted';
      try {
        if (action === 'dedup-repair') {
          const status = await api('/v1/catalog/dedup/repair', { method: 'POST' });
          renderCatalogDedupStatus(status || {});
          datasourceIndexingMessage.textContent = 'Catalog links rebuilt.';
        } else {
          const ok = await loadCatalogDedupStatus();
          if (!ok) {
            throw new Error('Catalog integrity check failed.');
          }
          datasourceIndexingMessage.textContent = 'Catalog integrity checked.';
        }
        datasourceIndexingMessage.className = 'status-ok';
      } catch (error) {
        datasourceIndexingMessage.textContent = error.message;
        datasourceIndexingMessage.className = 'status-failed';
        renderCatalogDedupStatus({}, error.message);
      } finally {
        button.disabled = false;
      }
    });

    semanticModelStatus.addEventListener('click', async event => {
      const button = event.target.closest('button[data-semantic-action]');
      if (!button) return;
      const action = button.dataset.semanticAction || '';
      const modelId = button.dataset.modelId || '';
      const vectorSpaceId = button.dataset.vectorSpaceId || '';
      const payload = modelId ? { modelId, vectorSpaceId } : undefined;
      button.disabled = true;
      semanticModelMessage.textContent = action === 'install-runtime' ? 'Installing runtime pack...' :
        action === 'install-model' ? 'Installing model...' :
        action === 'activate-model' ? 'Activating model...' :
        action === 'uninstall-model' ? 'Uninstalling model...' : 'Working...';
      semanticModelMessage.className = 'muted';
      try {
        let result;
        if (action === 'install-runtime') {
          result = await api('/v1/semantic-runtime-packs/recommended/install', { method: 'POST' });
          renderSemanticInstallJob(result);
        } else if (action === 'install-model') {
          result = await api('/v1/semantic-models/install', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload)
          });
          renderSemanticInstallJob(result);
        } else if (action === 'activate-model') {
          result = await api('/v1/semantic-models/activate', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload)
          });
          semanticModelMessage.textContent = 'Activated ' + (result.profile?.modelPack?.name || result.modelId || modelId);
        } else if (action === 'uninstall-model') {
          result = await api('/v1/semantic-models/uninstall', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(payload)
          });
          semanticModelMessage.textContent = 'Uninstalled ' + (result.modelId || modelId);
        } else {
          return;
        }
        if (action !== 'install-runtime' && action !== 'install-model' && semanticModelMessage.className !== 'status-warn') {
          semanticModelMessage.className = 'status-ok';
        }
        await loadSemanticModels({ forceRefresh: true });
        await loadDatasourceIndexingStatus({ forceRefresh: true });
      } catch (error) {
        semanticModelMessage.textContent = error.message;
        semanticModelMessage.className = 'status-failed';
        await loadSemanticModels();
      }
    });

    semanticSearchPreviewForm.addEventListener('submit', async event => {
      event.preventDefault();
      const button = semanticSearchPreviewForm.querySelector('button[type="submit"]');
      const query = semanticSearchPreviewQuery.value.trim();
      if (!query) {
        semanticSearchPreviewQuery.focus();
        return;
      }
      button.disabled = true;
      semanticSearchPreviewMessage.textContent = 'Searching...';
      semanticSearchPreviewMessage.className = 'muted';
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), semanticSearchPreviewTimeoutMs);
      try {
        const result = await api('/v1/assets/search-preview', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          signal: controller.signal,
          body: JSON.stringify(semanticSearchPreviewRequest(query))
        });
        renderSemanticSearchPreview(result);
        const elapsed = formatSearchElapsed(result.elapsedMs);
        semanticSearchPreviewMessage.textContent = 'Returned ' + String((result.items || []).length) + ' results' + (elapsed ? ' in ' + elapsed : '');
        semanticSearchPreviewMessage.className = 'status-ok';
      } catch (error) {
        semanticSearchPreviewMessage.textContent = error.name === 'AbortError' ? 'Search preview timed out. Try again when the datasource is less busy.' : error.message;
        semanticSearchPreviewMessage.className = 'status-failed';
      } finally {
        clearTimeout(timeout);
        button.disabled = false;
      }
    });

    document.querySelectorAll('[data-refresh-action]').forEach(button => {
      button.addEventListener('click', async event => {
        const target = event.currentTarget;
        const action = target.dataset.refreshAction || '';
        const label = target.textContent;
        target.disabled = true;
        target.textContent = 'Refreshing...';
        try {
          if (action === 'status') {
            await loadStatus();
          } else if (action === 'update') {
            await loadUpdateCheck();
          } else if (action === 'datasource-status') {
            await loadDatasourceIndexingStatus({ forceRefresh: true });
          } else if (action === 'semantic-models') {
            await loadSemanticModels({ forceRefresh: true });
            await loadSemanticInstallJob();
          }
        } catch (error) {
          target.textContent = error.message;
          window.setTimeout(() => {
            target.textContent = label;
          }, 2500);
          return;
        } finally {
          target.disabled = false;
        }
        target.textContent = label;
      });
    });

    async function checkDatasource() {
      const generation = datasourceCheckGeneration + 1;
      datasourceCheckGeneration = generation;
      const datasourceURLSnapshot = activeDatasourceURL;
      try {
        const check = await api('/v1/datasource/primary/check', { method: 'POST' });
        if (!canApplyDatasourceCheck(generation, datasourceURLSnapshot)) {
          return;
        }
        applyDatasourceCheck(check, datasourceURLSnapshot);
      } catch (error) {
        if (!canApplyDatasourceCheck(generation, datasourceURLSnapshot)) {
          return;
        }
        applyDatasourceCheck({
          status: 'failed',
          summary: 'Datasource check failed. ' + error.message
        }, datasourceURLSnapshot);
      }
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

    hydrateDatasourceIndexingStatusFromCache();
    Promise.all([loadStatus({ checkDatasourceReachability: true }), loadDevices(), loadUpdateCheck(), loadWorkerRuntime(), loadSystemResources(), loadDatasourceIndexingStatus(), loadSemanticModels(), loadSemanticInstallJob()]).catch(error => {
      document.querySelector('#agentSubline').textContent = error.message;
    });
  </script>
</body>
</html>`
