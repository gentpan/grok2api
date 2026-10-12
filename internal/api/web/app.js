/* Grok2API admin panel — dependency-free, DOM built with textContent only. */
(() => {
  'use strict';

  const STORE_KEY = 'g2a_admin_key';
  const PAGE_SIZE = 50;
  const MODE_ORDER = ['auto', 'fast', 'expert', 'heavy', 'console'];

  // ------------------------------------------------------------------ utils
  const state = { key: '', version: '', route: 'overview', authed: false };
  try { state.key = sessionStorage.getItem(STORE_KEY) || ''; } catch (_) { /* private mode */ }

  function h(tag, props, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(props || {})) {
      if (v === null || v === undefined || v === false) continue;
      if (k === 'class') node.className = v;
      else if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
      else if (k === 'value' || k === 'checked' || k === 'disabled') node[k] = v;
      else node.setAttribute(k, v === true ? '' : String(v));
    }
    for (const c of children.flat(Infinity)) {
      if (c === null || c === undefined || c === false) continue;
      node.append(c instanceof Node ? c : document.createTextNode(String(c)));
    }
    return node;
  }

  const ICONS = {
    refresh: '<path d="M3 12a9 9 0 0 1 9-9 9.75 9.75 0 0 1 6.74 2.74L21 8"/><path d="M21 3v5h-5"/><path d="M21 12a9 9 0 0 1-9 9 9.75 9.75 0 0 1-6.74-2.74L3 16"/><path d="M8 16H3v5"/>',
    trash: '<path d="M3 6h18"/><path d="M19 6v14c0 1-1 2-2 2H7c-1 0-2-1-2-2V6"/><path d="M8 6V4c0-1 1-2 2-2h4c1 0 2 1 2 2v2"/><line x1="10" x2="10" y1="11" y2="17"/><line x1="14" x2="14" y1="11" y2="17"/>',
    plus: '<path d="M5 12h14"/><path d="M12 5v14"/>',
    x: '<path d="M18 6 6 18"/><path d="m6 6 12 12"/>',
    check: '<path d="M20 6 9 17l-5-5"/>',
    eye: '<path d="M2 12s3-7 10-7 10 7 10 7-3 7-10 7-10-7-10-7Z"/><circle cx="12" cy="12" r="3"/>',
    eyeOff: '<path d="M9.88 9.88a3 3 0 1 0 4.24 4.24"/><path d="M10.73 5.08A10.43 10.43 0 0 1 12 5c7 0 10 7 10 7a13.16 13.16 0 0 1-1.67 2.68"/><path d="M6.61 6.61A13.526 13.526 0 0 0 2 12s3 7 10 7a9.74 9.74 0 0 0 5.39-1.61"/><line x1="2" x2="22" y1="2" y2="22"/>',
    logout: '<path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4"/><polyline points="16 17 21 12 16 7"/><line x1="21" x2="9" y1="12" y2="12"/>',
    send: '<path d="m22 2-7 20-4-9-9-4Z"/><path d="M22 2 11 13"/>',
    play: '<polygon points="6 3 20 12 6 21 6 3"/>',
    activity: '<path d="M22 12h-4l-3 9L9 3l-3 9H2"/>',
    users: '<path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M22 21v-2a4 4 0 0 0-3-3.87"/><path d="M16 3.13a4 4 0 0 1 0 7.75"/>',
    message: '<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>',
    sliders: '<line x1="4" x2="4" y1="21" y2="14"/><line x1="4" x2="4" y1="10" y2="3"/><line x1="12" x2="12" y1="21" y2="12"/><line x1="12" x2="12" y1="8" y2="3"/><line x1="20" x2="20" y1="21" y2="16"/><line x1="20" x2="20" y1="12" y2="3"/><line x1="2" x2="6" y1="14" y2="14"/><line x1="10" x2="14" y1="8" y2="8"/><line x1="18" x2="22" y1="16" y2="16"/>',
    key: '<path d="m21 2-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0 3 3L22 7l-3-3m-3.5 3.5L19 4"/>',
    copy: '<rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c1.1 0 2 .9 2 2"/>',
  };
  function icon(name) {
    const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    svg.setAttribute('viewBox', '0 0 24 24');
    svg.setAttribute('class', 'icon');
    svg.setAttribute('aria-hidden', 'true');
    // ICONS values are constants defined above, never user data.
    svg.innerHTML = ICONS[name] || '';
    return svg;
  }

  function maskToken(t) {
    t = String(t || '');
    if (t.length <= 12) return t.slice(0, 2) + '…';
    return t.slice(0, 6) + '…' + t.slice(-4);
  }
  function fmtTime(ms) {
    if (!ms) return '—';
    const t = ms < 1e11 ? ms * 1000 : ms;
    const d = new Date(t);
    if (isNaN(d)) return '—';
    const pad = (n) => String(n).padStart(2, '0');
    return `${d.getMonth() + 1}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  }
  function quotaText(q) {
    const parts = [];
    for (const m of MODE_ORDER) {
      if (q && q[m]) parts.push(`${m} ${q[m].remaining}/${q[m].total}`);
    }
    return parts.join(' · ') || '—';
  }
  function statusBadge(s) {
    const map = { active: ['ok', '正常'], cooling: ['warn', '冷却'], disabled: ['', '已禁用'], expired: ['err', '已失效'] };
    const [cls, label] = map[s] || ['warn', s || '未知'];
    return h('span', { class: 'badge ' + cls }, label);
  }

  const toastRoot = h('div', { class: 'toasts' });
  function toast(msg, kind) {
    const t = h('div', { class: 'toast ' + (kind || '') }, msg);
    toastRoot.append(t);
    setTimeout(() => t.remove(), kind === 'err' ? 6000 : 3000);
  }

  function modal(title, body, foot, opts) {
    const close = () => overlay.remove();
    const overlay = h('div', { class: 'overlay', onmousedown: (e) => { if (e.target === overlay && !(opts && opts.sticky)) close(); } },
      h('div', { class: 'modal ' + ((opts && opts.cls) || '') },
        h('div', { class: 'modal-head' }, h('h3', {}, title),
          h('button', { class: 'btn icon ghost', title: '关闭', onclick: close }, icon('x'))),
        h('div', { class: 'modal-body' }, body),
        foot ? h('div', { class: 'modal-foot' }, foot) : null));
    document.body.append(overlay);
    return { close, root: overlay };
  }
  function confirmDialog(message, okLabel, danger) {
    return new Promise((resolve) => {
      let m;
      const done = (v) => { m.close(); resolve(v); };
      m = modal('请确认', h('p', {}, message), [
        h('button', { class: 'btn', onclick: () => done(false) }, '取消'),
        h('button', { class: 'btn ' + (danger ? 'danger' : 'primary'), onclick: () => done(true) }, okLabel || '确定'),
      ], { cls: 'narrow', sticky: true });
    });
  }

  // -------------------------------------------------------------------- api
  class ApiError extends Error {
    constructor(message, status) { super(message); this.status = status; }
  }
  async function api(path, opts) {
    const o = opts || {};
    const headers = {};
    if (state.key) headers.Authorization = 'Bearer ' + state.key;
    if (o.body !== undefined) headers['Content-Type'] = 'application/json';
    const res = await fetch(path, {
      method: o.method || 'GET',
      headers,
      body: o.body !== undefined ? JSON.stringify(o.body) : undefined,
      signal: o.signal,
    });
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (_) { /* not json */ }
    if (!res.ok) {
      const msg = (data && data.error && data.error.message) || (data && data.message) || res.statusText || '请求失败';
      if (res.status === 401 && !o.noLogout) { logout('登录已失效，请重新登录'); }
      throw new ApiError(msg, res.status);
    }
    return data;
  }

  // -------------------------------------------- paste → credential detection
  const CF_NAMES = ['cf_clearance', '__cf_bm', '_cfuvid', 'x-anonuserid', 'x-challenge', 'x-signature', 'x-userid'];
  const COOKIE_NAMES = ['sso', 'sso-rw'].concat(CF_NAMES);

  function cleanValue(v) {
    return String(v || '').trim().replace(/^["']+|["',;]+$/g, '');
  }

  // extractCredentials finds SSO tokens and Cloudflare/session cookies inside
  // whatever the user pasted: a Cookie header, a curl command, Set-Cookie
  // lines, a DevTools cookie table, a JSON cookie export, or bare tokens.
  function extractCredentials(raw) {
    const text = String(raw || '');
    const tokens = [];
    const cookies = {};
    const addToken = (v) => {
      v = cleanValue(v);
      if (v.length >= 20 && !tokens.includes(v)) tokens.push(v);
    };
    const addCookie = (name, value) => {
      name = String(name || '').trim().toLowerCase();
      value = cleanValue(value);
      if (!name || !value) return;
      if (name === 'sso' || name === 'sso-rw') addToken(value);
      else if (CF_NAMES.includes(name)) cookies[name] = value;
    };
    const walk = (node) => {
      if (Array.isArray(node)) { node.forEach(walk); return; }
      if (!node || typeof node !== 'object') return;
      if (typeof node.name === 'string' && typeof node.value === 'string') addCookie(node.name, node.value);
      for (const [k, v] of Object.entries(node)) {
        if (typeof v === 'string') addCookie(k, v);
        else walk(v);
      }
    };

    // 1) JSON (cookie-editor / DevTools export)
    const trimmed = text.trim();
    if (trimmed.startsWith('{') || trimmed.startsWith('[')) {
      try { walk(JSON.parse(trimmed)); } catch (_) { /* fall through */ }
    }
    // 2) name=value pairs anywhere (Cookie header, curl, Set-Cookie)
    const pair = /(?:^|[\s;'"{,])([A-Za-z0-9_-]+)=([^;\s'"]+)/g;
    let m;
    while ((m = pair.exec(text)) !== null) addCookie(m[1], m[2]);
    // 3) DevTools "Application → Cookies" table rows: name<TAB|spaces>value
    const row = new RegExp('^\\s*(' + COOKIE_NAMES.map((n) => n.replace(/[-_]/g, '\\$&')).join('|') + ')\\s+([A-Za-z0-9_.%-]{8,})', 'gim');
    while ((m = row.exec(text)) !== null) addCookie(m[1], m[2]);
    // 4) Nothing labelled: look for bare JWT-shaped tokens, then bare long words
    if (tokens.length === 0) {
      const jwt = /[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}/g;
      while ((m = jwt.exec(text)) !== null) addToken(m[0]);
    }
    if (tokens.length === 0) {
      text.split(/[\s,;]+/).forEach((w) => { if (/^[A-Za-z0-9_.-]{40,}$/.test(w)) addToken(w); });
    }
    return { tokens, cookies };
  }
  function cookieString(cookies) {
    return Object.entries(cookies).map(([k, v]) => k + '=' + v).join('; ');
  }

  // --------------------------------------------------------- shell + router
  const root = document.getElementById('app');
  document.body.append(toastRoot);

  // ------------------------------------------------------------ sign-in
  async function publicPost(path, body, query) {
    const res = await fetch(path + (query || ''), {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
    });
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (_) { /* ignore */ }
    if (!res.ok) throw new ApiError((data && data.error && data.error.message) || res.statusText || '请求失败', res.status);
    return data;
  }

  function logout(message) {
    state.authed = false;
    const hadKey = !!state.key;
    state.key = '';
    try { sessionStorage.removeItem(STORE_KEY); } catch (_) { /* ignore */ }
    const done = () => renderLogin(typeof message === 'string' ? message : '');
    if (hadKey) { done(); return; }
    publicPost('/admin/auth/logout').catch(() => {}).then(done);
  }

  // WebAuthn helpers: the server speaks base64url, the browser wants buffers.
  const b64uToBuf = (str) => {
    let s = String(str).replace(/-/g, '+').replace(/_/g, '/');
    while (s.length % 4) s += '=';
    const bin = atob(s);
    const out = new Uint8Array(bin.length);
    for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
    return out.buffer;
  };
  const bufToB64u = (buf) => {
    const u = new Uint8Array(buf);
    let s = '';
    u.forEach((b) => { s += String.fromCharCode(b); });
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  };
  function credentialToJSON(c) {
    if (typeof c.toJSON === 'function') return c.toJSON();
    const r = c.response;
    const out = { id: c.id, rawId: bufToB64u(c.rawId), type: c.type, clientExtensionResults: c.getClientExtensionResults ? c.getClientExtensionResults() : {} };
    if (r.attestationObject) {
      out.response = { clientDataJSON: bufToB64u(r.clientDataJSON), attestationObject: bufToB64u(r.attestationObject), transports: r.getTransports ? r.getTransports() : [] };
    } else {
      out.response = { clientDataJSON: bufToB64u(r.clientDataJSON), authenticatorData: bufToB64u(r.authenticatorData), signature: bufToB64u(r.signature) };
      if (r.userHandle) out.response.userHandle = bufToB64u(r.userHandle);
    }
    return out;
  }
  function webauthnSupported() { return !!(window.PublicKeyCredential && navigator.credentials); }
  function explainWebAuthnError(ex) {
    if (ex && ex.name === 'NotAllowedError') return '操作被取消或超时';
    if (ex && ex.name === 'InvalidStateError') return '这个设备上已经注册过该通行密钥';
    return ex && ex.message ? ex.message : '通行密钥操作失败';
  }

  async function passkeyLogin() {
    const begin = await publicPost('/admin/auth/passkey/login/begin');
    const pk = begin.options.publicKey;
    pk.challenge = b64uToBuf(pk.challenge);
    (pk.allowCredentials || []).forEach((c) => { c.id = b64uToBuf(c.id); });
    const cred = await navigator.credentials.get({ publicKey: pk });
    await publicPost('/admin/auth/passkey/login/finish', credentialToJSON(cred), '?challenge_id=' + encodeURIComponent(begin.challenge_id));
  }

  async function renderLogin(message) {
    state.authed = false;
    let status = { setup_required: false, passkey_available: false, passkey_supported: true };
    try {
      const res = await fetch('/admin/auth/status');
      if (res.ok) status = await res.json();
      if (status.authenticated) { start(); return; }
    } catch (_) { /* offline: still show the form */ }

    const err = h('div', { class: 'alert err hidden' });
    const showErr = (m) => { err.textContent = m; err.classList.remove('hidden'); };
    if (message) showErr(message);
    const busy = (btn, on) => { btn.disabled = on; };

    if (status.setup_required) {
      const key = h('input', { class: 'input', type: 'password', placeholder: '管理密钥 app_key', autocomplete: 'off' });
      const user = h('input', { class: 'input', type: 'text', placeholder: '用户名（3–64 位字母数字）', autocomplete: 'username' });
      const pw = h('input', { class: 'input', type: 'password', placeholder: '密码（至少 10 位）', autocomplete: 'new-password' });
      const pw2 = h('input', { class: 'input', type: 'password', placeholder: '再次输入密码', autocomplete: 'new-password' });
      const btn = h('button', { class: 'btn primary', type: 'submit' }, '创建管理员账号');
      const submit = async (e) => {
        e.preventDefault();
        if (pw.value !== pw2.value) { showErr('两次输入的密码不一致'); return; }
        busy(btn, true);
        try {
          await publicPost('/admin/auth/setup', { app_key: key.value.trim(), username: user.value.trim(), password: pw.value });
          location.hash = '#/account';
          start();
        } catch (ex) { showErr(ex.message); busy(btn, false); }
      };
      root.replaceChildren(h('div', { class: 'login-wrap' },
        h('form', { class: 'card login stack', onsubmit: submit },
          h('div', {}, h('h1', {}, '首次设置'), h('p', { class: 'muted' }, '创建登录面板用的用户名和密码')),
          h('div', { class: 'alert info' }, '为防止他人抢先占用，需要先输入部署时设置的管理密钥（app_key）。'),
          err,
          h('div', { class: 'field' }, h('label', {}, '管理密钥'), key),
          h('div', { class: 'field' }, h('label', {}, '用户名'), user),
          h('div', { class: 'field' }, h('label', {}, '密码'), pw, pw2),
          btn)));
      return;
    }

    const user = h('input', { class: 'input', type: 'text', placeholder: '用户名', autocomplete: 'username webauthn', autofocus: true });
    const pw = h('input', { class: 'input', type: 'password', placeholder: '密码', autocomplete: 'current-password' });
    const btn = h('button', { class: 'btn primary', type: 'submit' }, '登录');
    const submit = async (e) => {
      e.preventDefault();
      busy(btn, true);
      try {
        await publicPost('/admin/auth/login', { username: user.value.trim(), password: pw.value });
        start();
      } catch (ex) { showErr(ex.message); busy(btn, false); }
    };

    const keyIn = h('input', { class: 'input', type: 'password', placeholder: 'app_key', autocomplete: 'off' });
    const keyBtn = h('button', { class: 'btn', type: 'button' }, '用管理密钥登录');
    keyBtn.addEventListener('click', async () => {
      const k = keyIn.value.trim();
      if (!k) return;
      busy(keyBtn, true);
      state.key = k;
      try {
        await api('/admin/api/verify', { noLogout: true });
        try { sessionStorage.setItem(STORE_KEY, k); } catch (_) { /* ignore */ }
        start();
      } catch (ex) { state.key = ''; showErr(ex.message); busy(keyBtn, false); }
    });

    const parts = [
      h('div', {}, h('h1', {}, 'Grok2API'), h('p', { class: 'muted' }, '管理面板')),
      err,
      h('div', { class: 'field' }, h('label', {}, '用户名'), user),
      h('div', { class: 'field' }, h('label', {}, '密码'), pw),
      btn,
    ];
    if (status.passkey_available && status.passkey_supported && webauthnSupported()) {
      const pkBtn = h('button', { class: 'btn', type: 'button' }, icon('key'), '使用通行密钥登录');
      pkBtn.addEventListener('click', async () => {
        busy(pkBtn, true);
        try { await passkeyLogin(); start(); } catch (ex) { showErr(explainWebAuthnError(ex)); busy(pkBtn, false); }
      });
      parts.push(h('div', { class: 'divider' }, '或'), pkBtn);
    } else if (status.passkey_available && !status.passkey_supported) {
      parts.push(h('p', { class: 'hint' }, '通行密钥需要通过域名访问，当前是 IP 地址。'));
    }
    parts.push(h('details', { class: 'fallback' },
      h('summary', {}, '忘记密码？使用管理密钥登录'),
      h('div', { class: 'stack' }, keyIn, keyBtn,
        h('p', { class: 'hint' }, '登录后可以在「账户安全」里重设密码。'))));
    root.replaceChildren(h('div', { class: 'login-wrap' }, h('form', { class: 'card login stack', onsubmit: submit }, parts)));
  }

  const ROUTES = [
    { id: 'overview', label: '概览', icon: 'activity', view: viewOverview },
    { id: 'accounts', label: '账号', icon: 'users', view: viewAccounts },
    { id: 'models', label: '模型', icon: 'sliders', view: viewModels },
    { id: 'chat', label: '聊天测试', icon: 'message', view: viewChat },
    { id: 'stats', label: '用量统计', icon: 'activity', view: viewStats },
    { id: 'keys', label: 'API 密钥', icon: 'key', view: viewKeys },
    { id: 'usage', label: '接入说明', icon: 'send', view: viewUsage },
    { id: 'config', label: '配置', icon: 'sliders', view: viewConfig },
    { id: 'account', label: '账户安全', icon: 'key', view: viewAccount },
  ];

  function currentRoute() {
    const id = (location.hash || '').replace(/^#\/?/, '') || 'overview';
    return ROUTES.find((r) => r.id === id) || ROUTES[0];
  }

  let activeCleanup = null;
  async function renderRoute() {
    if (activeCleanup) { try { activeCleanup(); } catch (_) { /* ignore */ } activeCleanup = null; }
    const route = currentRoute();
    document.querySelectorAll('.nav-item').forEach((n) => n.classList.toggle('active', n.dataset.route === route.id));
    const main = document.getElementById('main');
    main.replaceChildren(h('p', { class: 'muted' }, '加载中…'));
    try {
      const out = await route.view(main);
      if (typeof out === 'function') activeCleanup = out;
    } catch (ex) {
      main.replaceChildren(h('div', { class: 'alert err' }, '加载失败：' + ex.message));
    }
  }

  async function start() {
    state.authed = true;
    const nav = ROUTES.map((r) => h('a', { class: 'nav-item', href: '#/' + r.id, 'data-route': r.id }, icon(r.icon), r.label));
    root.replaceChildren(h('div', { class: 'shell' },
      h('aside', { class: 'sidebar' },
        h('div', { class: 'brand' }, 'Grok2API', h('small', {}, state.version ? 'v' + state.version : '')),
        nav,
        h('div', { class: 'sidebar-spacer' }),
        h('a', { class: 'nav-item', href: '#', onclick: (e) => { e.preventDefault(); logout(); } }, icon('logout'), '退出登录')),
      h('main', { class: 'main', id: 'main' })));
    fetch('/meta').then((r) => r.json()).then((j) => {
      state.version = j.version || '';
      const small = document.querySelector('.brand small');
      if (small && state.version) small.textContent = 'v' + state.version;
    }).catch(() => {});
    renderRoute();
  }
  window.addEventListener('hashchange', () => { if (state.authed) renderRoute(); });

  // --------------------------------------------------------------- overview
  async function fetchTokens() {
    const j = await api('/admin/api/tokens');
    return (j && j.tokens) || [];
  }

  async function viewOverview(main) {
    const [status, tokens] = await Promise.all([api('/admin/api/status'), fetchTokens()]);
    const byStatus = {};
    const byPool = {};
    const quota = {};
    for (const t of tokens) {
      byStatus[t.status] = (byStatus[t.status] || 0) + 1;
      byPool[t.pool] = (byPool[t.pool] || 0) + 1;
      if (t.status !== 'active') continue;
      for (const m of MODE_ORDER) {
        const q = t.quota && t.quota[m];
        if (!q) continue;
        quota[m] = quota[m] || { remaining: 0, total: 0, n: 0 };
        quota[m].remaining += q.remaining;
        quota[m].total += q.total;
        quota[m].n += 1;
      }
    }
    const stat = (label, value) => h('div', { class: 'stat' }, h('div', { class: 'label' }, label), h('div', { class: 'value' }, value));
    const quotaRows = MODE_ORDER.filter((m) => quota[m]).map((m) => h('tr', {},
      h('td', {}, m), h('td', { class: 'num' }, quota[m].n), h('td', { class: 'num' }, quota[m].remaining), h('td', { class: 'num' }, quota[m].total)));

    const hints = [];
    if (tokens.length === 0) hints.push(h('div', { class: 'alert info' }, '账号池还是空的：到「账号」页点「导入账号」，把浏览器里复制的 Cookie 整段贴进去即可。'));

    main.replaceChildren(
      h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, '概览'), h('p', { class: 'sub' }, '账号池与额度的整体情况'))),
      h('div', { class: 'stack' },
        hints,
        h('div', { class: 'stats' },
          stat('账号总数', tokens.length),
          stat('正常', byStatus.active || 0),
          stat('冷却中', byStatus.cooling || 0),
          stat('已禁用', byStatus.disabled || 0),
          stat('已失效', byStatus.expired || 0),
          stat('选号策略', status.selection_strategy || '—')),
        h('div', { class: 'card flush' },
          h('div', { class: 'toolbar' }, h('strong', {}, '正常账号的剩余额度')),
          quotaRows.length
            ? h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '模式'), h('th', { class: 'num' }, '账号数'), h('th', { class: 'num' }, '剩余'), h('th', { class: 'num' }, '总量'))), h('tbody', {}, quotaRows)))
            : h('div', { class: 'empty' }, '暂无额度数据')),
        h('div', { class: 'card' }, h('div', { class: 'card-title' }, '账号池分布'),
          h('div', { class: 'row' }, Object.keys(byPool).length
            ? Object.entries(byPool).map(([p, n]) => h('span', { class: 'badge info' }, `${p}：${n}`))
            : h('span', { class: 'muted' }, '—')))));
  }

  // --------------------------------------------------------------- accounts
  async function viewAccounts(main) {
    const st = { rows: [], selected: new Set(), reveal: new Set(), q: '', pool: '', status: '', page: 1 };

    async function load() {
      st.rows = await fetchTokens();
      for (const t of Array.from(st.selected)) if (!st.rows.some((r) => r.token === t)) st.selected.delete(t);
      draw();
    }
    const filtered = () => st.rows.filter((r) =>
      (!st.pool || r.pool === st.pool) && (!st.status || r.status === st.status) &&
      (!st.q || r.token.toLowerCase().includes(st.q) || (r.tags || []).some((g) => String(g).toLowerCase().includes(st.q))));

    async function act(label, fn) {
      try { await fn(); toast(label + '完成', 'ok'); } catch (ex) { toast(label + '失败：' + ex.message, 'err'); }
      await load();
    }
    const sync = () => api('/admin/api/sync', { method: 'POST' }).catch(() => {});
    const selTokens = () => Array.from(st.selected);

    async function testTokens(tokens) {
      if (!tokens.length) return;
      toast('正在测试 ' + tokens.length + ' 个账号…');
      const out = [];
      for (let i = 0; i < tokens.length; i += 50) {
        const j = await api('/admin/api/tokens/test', { method: 'POST', body: { tokens: tokens.slice(i, i + 50) } });
        out.push(...j.results.map((r, k) => ({ ...r, full: tokens[i + k] })));
      }
      showTestResults(out);
    }
    function showTestResults(results) {
      const ok = results.filter((r) => r.ok).length;
      const rows = results.map((r) => h('tr', {},
        h('td', { class: 'mono' }, r.token),
        h('td', {}, r.ok ? h('span', { class: 'badge ok' }, '可用') : h('span', { class: 'badge err' }, r.invalid ? '凭证无效' : '失败')),
        h('td', {}, r.ok ? quotaText(r.quota) : h('span', { class: 'muted' }, r.error || '')),
        h('td', { class: 'num' }, r.latency_ms + ' ms')));
      modal(`测试结果：${ok}/${results.length} 可用`,
        h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, 'Token'), h('th', {}, '状态'), h('th', {}, '额度 / 错误'), h('th', { class: 'num' }, '耗时'))), h('tbody', {}, rows))),
        null);
    }

    function openImport() {
      const area = h('textarea', { class: 'textarea', rows: '7', spellcheck: 'false',
        placeholder: '把从浏览器复制的内容整段粘贴到这里即可，例如：\n· 整行 Cookie：sso=eyJ...; sso-rw=eyJ...; cf_clearance=...\n· 请求头 / curl 命令 / 开发者工具里的 Cookie 表格\n· 导出的 JSON\n· 或者直接一串 / 多串 Token（每行一个）' });
      const summary = h('div', {});
      const poolSel = h('select', { class: 'select' },
        h('option', { value: 'auto' }, '自动识别（推荐）'), h('option', { value: 'basic' }, 'basic（免费）'),
        h('option', { value: 'super' }, 'super（付费）'), h('option', { value: 'heavy' }, 'heavy'));
      const tagsIn = h('input', { class: 'input', placeholder: '标签，逗号分隔（可选）' });
      let found = { tokens: [], cookies: {} };
      const picked = new Set();
      const verdict = new Map(); // full token -> result
      const saveCookies = h('input', { type: 'checkbox', checked: true });
      const saveUA = h('input', { type: 'checkbox', checked: true });
      const onlyOk = h('input', { type: 'checkbox', checked: false });
      const testBtn = h('button', { class: 'btn', onclick: runTest }, icon('play'), '先测试');
      const importBtn = h('button', { class: 'btn primary', onclick: runImport }, icon('plus'), '导入');

      function redraw() {
        const parts = [];
        if (!area.value.trim()) { summary.replaceChildren(); return; }
        if (found.tokens.length === 0) {
          parts.push(h('div', { class: 'alert warn' }, '没有识别到 SSO Token。请确认复制的内容里包含名为 sso 的 Cookie 值。'));
        } else {
          parts.push(h('div', { class: 'alert ok' }, `识别到 ${found.tokens.length} 个 SSO Token，勾选要导入的：`));
          parts.push(h('div', { class: 'found-list' }, found.tokens.map((t) => {
            const v = verdict.get(t);
            const cb = h('input', { type: 'checkbox', checked: picked.has(t), onchange: (e) => { if (e.target.checked) picked.add(t); else picked.delete(t); } });
            return h('label', { class: 'found-item' }, cb, h('span', { class: 'mono' }, maskToken(t)),
              v ? (v.ok ? h('span', { class: 'badge ok' }, '可用 · ' + quotaText(v.quota)) : h('span', { class: 'badge err' }, v.invalid ? '凭证无效' : '测试失败')) : null);
          })));
        }
        const names = Object.keys(found.cookies);
        if (names.length) {
          parts.push(h('div', { class: 'alert info stack' },
            h('div', {}, '同时识别到 Cloudflare / 会话 Cookie：' + names.join('、')),
            h('label', { class: 'check' }, saveCookies, '写入配置（proxy.clearance.cf_cookies），用于通过 Cloudflare 校验'),
            h('label', { class: 'check' }, saveUA, '同时把 User-Agent 设为当前浏览器的值（cf_clearance 与 UA 绑定）'),
            h('div', { class: 'hint' }, 'cf_clearance 通常还与获取它时的出口 IP 绑定；服务器 IP 与你本机不同时可能仍被拦，需要给服务器配置出口代理。')));
        }
        summary.replaceChildren(...parts);
      }
      area.addEventListener('input', () => {
        found = extractCredentials(area.value);
        picked.clear(); verdict.clear();
        found.tokens.forEach((t) => picked.add(t));
        redraw();
      });

      async function runTest() {
        const list = found.tokens.filter((t) => picked.has(t));
        if (!list.length) { toast('请先勾选要测试的 Token', 'err'); return; }
        testBtn.disabled = true;
        try {
          for (let i = 0; i < list.length; i += 50) {
            const part = list.slice(i, i + 50);
            const j = await api('/admin/api/tokens/test', { method: 'POST', body: { tokens: part } });
            j.results.forEach((r, k) => verdict.set(part[k], r));
          }
          redraw();
          const ok = list.filter((t) => verdict.get(t) && verdict.get(t).ok).length;
          toast(`测试完成：${ok}/${list.length} 可用`, ok === list.length ? 'ok' : '');
        } catch (ex) { toast('测试失败：' + ex.message, 'err'); } finally { testBtn.disabled = false; }
      }

      async function runImport() {
        let list = found.tokens.filter((t) => picked.has(t));
        if (onlyOk.checked) list = list.filter((t) => verdict.get(t) && verdict.get(t).ok);
        const wantCookies = Object.keys(found.cookies).length > 0 && saveCookies.checked;
        if (!list.length && !wantCookies) { toast('没有可导入的内容', 'err'); return; }
        importBtn.disabled = true;
        try {
          let summaryText = '';
          if (list.length) {
            const tags = tagsIn.value.split(/[,，]/).map((s) => s.trim()).filter(Boolean);
            const j = await api('/admin/api/tokens/add', { method: 'POST', body: { tokens: list, pool: poolSel.value, tags } });
            summaryText += `账号：新增 ${j.count}，跳过 ${j.skipped}。`;
          }
          if (wantCookies) {
            const patch = { proxy: { clearance: { cf_cookies: cookieString(found.cookies) } } };
            if (saveUA.checked) patch.proxy.clearance.user_agent = navigator.userAgent;
            await api('/admin/api/config', { method: 'POST', body: patch });
            summaryText += '已写入 Cloudflare Cookie 配置。';
          }
          toast(summaryText, 'ok');
          dlg.close();
          await sync();
          await load();
        } catch (ex) { toast('导入失败：' + ex.message, 'err'); importBtn.disabled = false; }
      }

      const dlg = modal('导入账号',
        [h('div', { class: 'field' }, h('label', {}, '粘贴内容'), area,
            h('span', { class: 'hint' }, '内容只发送到你自己的服务器，不会经过第三方。')),
          summary,
          h('div', { class: 'row' }, h('div', { class: 'grow field' }, h('label', {}, '账号池'), poolSel), h('div', { class: 'grow field' }, h('label', {}, '标签'), tagsIn)),
          h('label', { class: 'check' }, onlyOk, '仅导入测试通过的账号（需先点「先测试」）')],
        [h('button', { class: 'btn', onclick: () => dlg.close() }, '取消'), testBtn, importBtn], { sticky: true });
      setTimeout(() => area.focus(), 0);
    }

    function draw() {
      const rows = filtered();
      const pages = Math.max(1, Math.ceil(rows.length / PAGE_SIZE));
      st.page = Math.min(st.page, pages);
      const slice = rows.slice((st.page - 1) * PAGE_SIZE, st.page * PAGE_SIZE);
      const allOnPage = slice.length > 0 && slice.every((r) => st.selected.has(r.token));
      const nSel = st.selected.size;

      const poolOptions = ['', ...new Set(st.rows.map((r) => r.pool))];
      const statusOptions = ['', ...new Set(st.rows.map((r) => r.status))];
      const search = h('input', { class: 'input', placeholder: '搜索 Token / 标签', value: st.q, oninput: (e) => { st.q = e.target.value.trim().toLowerCase(); st.page = 1; draw(); const el = document.querySelector('.toolbar input'); if (el) { el.focus(); el.setSelectionRange(el.value.length, el.value.length); } } });

      const tbody = slice.map((r) => {
        const shown = st.reveal.has(r.token) ? r.token : maskToken(r.token);
        return h('tr', {},
          h('td', {}, h('input', { type: 'checkbox', checked: st.selected.has(r.token), onchange: (e) => { if (e.target.checked) st.selected.add(r.token); else st.selected.delete(r.token); draw(); } })),
          h('td', {}, h('span', { class: 'mono' }, shown),
            h('button', { class: 'btn icon ghost sm', title: st.reveal.has(r.token) ? '隐藏' : '显示', onclick: () => { if (st.reveal.has(r.token)) st.reveal.delete(r.token); else st.reveal.add(r.token); draw(); } }, icon(st.reveal.has(r.token) ? 'eyeOff' : 'eye')),
            h('button', { class: 'btn icon ghost sm', title: '复制', onclick: () => copyText(r.token) }, icon('copy'))),
          h('td', {}, r.pool),
          h('td', {}, statusBadge(r.status)),
          h('td', {}, quotaText(r.quota)),
          h('td', { class: 'num' }, r.use_count || 0),
          h('td', {}, fmtTime(r.last_used_at)),
          h('td', {}, (r.tags || []).map((g) => h('span', { class: 'badge' }, g))),
          h('td', {}, h('div', { class: 'row' },
            h('button', { class: 'btn sm', title: '测试', onclick: () => testTokens([r.token]).catch((ex) => toast('测试失败：' + ex.message, 'err')) }, icon('play')),
            h('button', { class: 'btn sm', title: r.status === 'disabled' ? '启用' : '禁用', onclick: () => act(r.status === 'disabled' ? '启用' : '禁用', async () => { await api('/admin/api/tokens/disabled', { method: 'POST', body: { token: r.token, disabled: r.status !== 'disabled' } }); await sync(); }) }, r.status === 'disabled' ? '启用' : '禁用'),
            h('button', { class: 'btn sm danger', title: '删除', onclick: async () => { if (await confirmDialog('确定删除这个账号？', '删除', true)) act('删除', () => api('/admin/api/tokens', { method: 'DELETE', body: [r.token] })); } }, icon('trash')))));
      });

      const bulk = nSel ? [
        h('span', { class: 'muted' }, `已选 ${nSel} 项`),
        h('button', { class: 'btn', onclick: () => testTokens(selTokens()).catch((ex) => toast('测试失败：' + ex.message, 'err')) }, icon('play'), '测试'),
        h('button', { class: 'btn', onclick: () => act('刷新额度', () => api('/admin/api/batch/refresh', { method: 'POST', body: { tokens: selTokens() } })) }, icon('refresh'), '刷新额度'),
        h('button', { class: 'btn', onclick: () => act('禁用', async () => { await api('/admin/api/tokens/disabled/batch', { method: 'POST', body: { tokens: selTokens(), disabled: true } }); await sync(); }) }, '禁用'),
        h('button', { class: 'btn', onclick: () => act('启用', async () => { await api('/admin/api/tokens/disabled/batch', { method: 'POST', body: { tokens: selTokens(), disabled: false } }); await sync(); }) }, '启用'),
        h('button', { class: 'btn danger', onclick: async () => { if (await confirmDialog(`确定删除选中的 ${nSel} 个账号？`, '删除', true)) act('删除', () => api('/admin/api/tokens', { method: 'DELETE', body: selTokens() })); } }, icon('trash'), '删除'),
      ] : [];

      main.replaceChildren(
        h('div', { class: 'page-head' },
          h('div', {}, h('h1', {}, '账号'), h('p', { class: 'sub' }, `共 ${st.rows.length} 个账号，当前筛选 ${rows.length} 个`)),
          h('div', { class: 'row' },
            h('button', { class: 'btn', onclick: async () => { if (await confirmDialog('清理所有失效账号？此操作不可恢复。', '清理', true)) act('清理失效账号', () => api('/admin/api/tokens/invalid', { method: 'DELETE' })); } }, icon('trash'), '清理失效'),
            h('button', { class: 'btn', onclick: () => act('刷新全部额度', () => api('/admin/api/batch/refresh', { method: 'POST', body: { tokens: st.rows.map((r) => r.token) } })) }, icon('refresh'), '刷新全部'),
            h('button', { class: 'btn primary', onclick: openImport }, icon('plus'), '导入账号'))),
        h('div', { class: 'card flush' },
          h('div', { class: 'toolbar' }, search,
            h('select', { class: 'select', onchange: (e) => { st.pool = e.target.value; st.page = 1; draw(); } }, poolOptions.map((p) => h('option', { value: p, selected: p === st.pool }, p || '全部池'))),
            h('select', { class: 'select', onchange: (e) => { st.status = e.target.value; st.page = 1; draw(); } }, statusOptions.map((s) => h('option', { value: s, selected: s === st.status }, s || '全部状态'))),
            bulk),
          rows.length === 0
            ? h('div', { class: 'empty' }, st.rows.length ? '没有符合筛选条件的账号' : '还没有账号，点右上角「导入账号」开始')
            : h('div', { class: 'table-wrap' }, h('table', {},
              h('thead', {}, h('tr', {},
                h('th', {}, h('input', { type: 'checkbox', checked: allOnPage, onchange: (e) => { slice.forEach((r) => { if (e.target.checked) st.selected.add(r.token); else st.selected.delete(r.token); }); draw(); } })),
                ['Token', '池', '状态', '额度', '使用次数', '最近使用', '标签', '操作'].map((t) => h('th', { class: t === '使用次数' ? 'num' : '' }, t)))),
              h('tbody', {}, tbody))),
          h('div', { class: 'pager' }, h('span', {}, `第 ${st.page} / ${pages} 页`),
            h('div', { class: 'row' },
              h('button', { class: 'btn sm', disabled: st.page <= 1, onclick: () => { st.page--; draw(); } }, '上一页'),
              h('button', { class: 'btn sm', disabled: st.page >= pages, onclick: () => { st.page++; draw(); } }, '下一页')))));
    }
    await load();
  }

  // ------------------------------------------------------------------- chat
  async function viewChat(main) {
    const [mj, tokens] = await Promise.all([api('/admin/api/test/models'), fetchTokens()]);
    const models = (mj.models || []).filter((m) => m.capability === 'chat' || m.capability === 'console-chat' || m.capability === 'image');
    const history = []; // {role, content}
    let controller = null;

    const modelSel = h('select', { class: 'select' }, models.map((m) =>
      h('option', { value: m.id }, `${m.name || m.id}${m.available ? '' : '（暂无可用账号）'}`)));
    const firstAvail = models.find((m) => m.available);
    if (firstAvail) modelSel.value = firstAvail.id;
    const acctSel = h('select', { class: 'select' },
      h('option', { value: '' }, '自动（从账号池选取）'),
      tokens.map((t) => h('option', { value: t.token }, `${maskToken(t.token)} · ${t.pool} · ${t.status}`)));
    const streamCb = h('input', { type: 'checkbox', checked: true });
    const thinkCb = h('input', { type: 'checkbox', checked: true });
    const sysIn = h('textarea', { class: 'textarea', rows: '3', placeholder: '可选：系统提示词' });
    const log = h('div', { class: 'chat-log' });
    const input = h('textarea', { class: 'textarea', rows: '1', placeholder: '输入消息，Enter 发送，Shift+Enter 换行' });
    const sendBtn = h('button', { class: 'btn primary', onclick: send }, icon('send'), '发送');
    const stopBtn = h('button', { class: 'btn hidden', onclick: () => controller && controller.abort() }, '停止');

    function renderContent(container, text) {
      const re = /!\[[^\]]*\]\(([^)\s]+)\)/g;
      let last = 0; let m;
      while ((m = re.exec(text)) !== null) {
        if (m.index > last) container.append(document.createTextNode(text.slice(last, m.index)));
        const url = m[1];
        if (/^https:\/\//i.test(url) || url.startsWith('/')) container.append(h('img', { src: url, alt: 'image', loading: 'lazy' }));
        else container.append(document.createTextNode(m[0]));
        last = m.index + m[0].length;
      }
      if (last < text.length) container.append(document.createTextNode(text.slice(last)));
    }
    function addMsg(role, text) {
      const body = h('div', {});
      const node = h('div', { class: 'msg ' + role }, body);
      if (text) renderContent(body, text);
      log.append(node);
      log.scrollTop = log.scrollHeight;
      return { node, body };
    }

    async function send() {
      const text = input.value.trim();
      if (!text || controller) return;
      if (!modelSel.value) { toast('没有可选模型', 'err'); return; }
      input.value = '';
      history.push({ role: 'user', content: text });
      addMsg('user', text);
      const messages = [];
      if (sysIn.value.trim()) messages.push({ role: 'system', content: sysIn.value.trim() });
      messages.push(...history);

      const out = addMsg('assistant', '');
      const thinkEl = h('div', { class: 'think hidden' });
      const textEl = h('div', {});
      out.body.append(thinkEl, textEl);
      const meta = h('div', { class: 'msg-meta' });
      out.node.append(meta);

      const stream = streamCb.checked;
      const body = { model: modelSel.value, messages, stream };
      if (acctSel.value) body.token = acctSel.value;
      controller = new AbortController();
      sendBtn.disabled = true; stopBtn.classList.remove('hidden');
      const t0 = performance.now();
      let tFirst = 0; let answer = ''; let reasoning = ''; let failed = false;
      const showThink = () => { if (thinkCb.checked && reasoning) { thinkEl.classList.remove('hidden'); thinkEl.textContent = reasoning; } };
      try {
        const res = await fetch('/admin/api/test/chat', {
          method: 'POST',
          headers: Object.assign({ 'Content-Type': 'application/json' }, state.key ? { Authorization: 'Bearer ' + state.key } : {}),
          body: JSON.stringify(body),
          signal: controller.signal,
        });
        if (!res.ok) {
          let msg = res.statusText;
          try { const j = await res.json(); msg = (j.error && j.error.message) || msg; } catch (_) { /* ignore */ }
          throw new Error(`HTTP ${res.status}：${msg}`);
        }
        if (stream) {
          const reader = res.body.getReader();
          const dec = new TextDecoder();
          let buf = '';
          for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            buf += dec.decode(value, { stream: true });
            let nl;
            while ((nl = buf.indexOf('\n')) >= 0) {
              const line = buf.slice(0, nl).replace(/\r$/, '');
              buf = buf.slice(nl + 1);
              if (!line.startsWith('data:')) continue;
              const data = line.slice(5).trim();
              if (!data || data === '[DONE]') continue;
              let j; try { j = JSON.parse(data); } catch (_) { continue; }
              if (j.error) throw new Error(j.error.message || '上游返回错误');
              const d = (j.choices && j.choices[0] && j.choices[0].delta) || {};
              if (!tFirst && (d.content || d.reasoning_content)) tFirst = performance.now();
              if (d.reasoning_content) { reasoning += d.reasoning_content; showThink(); }
              if (d.content) { answer += d.content; textEl.textContent = answer; log.scrollTop = log.scrollHeight; }
            }
          }
        } else {
          const j = await res.json();
          const msg = (j.choices && j.choices[0] && j.choices[0].message) || {};
          answer = msg.content || '';
          reasoning = msg.reasoning_content || '';
          tFirst = performance.now();
          showThink();
        }
        textEl.replaceChildren();
        renderContent(textEl, answer || '（空响应）');
        history.push({ role: 'assistant', content: answer });
      } catch (ex) {
        failed = true;
        if (ex.name === 'AbortError') {
          if (answer) history.push({ role: 'assistant', content: answer });
          meta.textContent = '已停止';
        } else {
          out.node.className = 'msg error';
          out.body.replaceChildren(document.createTextNode(ex.message));
          history.pop(); // drop the user turn so a retry does not duplicate it
        }
      } finally {
        const total = Math.round(performance.now() - t0);
        if (!failed || answer) {
          meta.textContent = (meta.textContent ? meta.textContent + ' · ' : '') +
            (tFirst ? `首字 ${Math.round(tFirst - t0)} ms · ` : '') + `总计 ${total} ms`;
        }
        controller = null;
        sendBtn.disabled = false; stopBtn.classList.add('hidden');
        input.focus();
      }
    }
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
    });

    const side = h('div', { class: 'card stack' },
      h('div', { class: 'field' }, h('label', {}, '模型'), modelSel),
      h('div', { class: 'field' }, h('label', {}, '使用账号'), acctSel,
        h('span', { class: 'hint' }, '指定某个账号可以单独排查它是否可用。')),
      h('label', { class: 'check' }, streamCb, '流式输出'),
      h('label', { class: 'check' }, thinkCb, '显示思考过程'),
      h('div', { class: 'field' }, h('label', {}, '系统提示词'), sysIn),
      h('button', { class: 'btn', onclick: () => { history.length = 0; log.replaceChildren(); } }, '清空对话'));

    const noPool = tokens.length === 0
      ? h('div', { class: 'alert warn' }, '账号池为空，聊天测试暂时无法使用。请先到「账号」页导入账号。') : null;

    main.replaceChildren(
      h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, '聊天测试'), h('p', { class: 'sub' }, '直接用账号池里的账号发一条消息，验证整条链路'))),
      noPool,
      h('div', { class: 'chat-layout' }, side,
        h('div', { class: 'card flush chat-box' }, log,
          h('div', { class: 'chat-input' }, h('div', { class: 'grow' }, input), stopBtn, sendBtn))));
    return () => { if (controller) controller.abort(); };
  }


  // ------------------------------------------------------------------ usage
  function copyText(text, okMsg) {
    const done = () => toast(okMsg || '已复制', 'ok');
    const fallback = () => {
      const ta = h('textarea', { class: 'textarea', value: text });
      document.body.append(ta);
      ta.select();
      try { document.execCommand('copy'); done(); } catch (_) { toast('复制失败，请手动选择', 'err'); }
      ta.remove();
    };
    if (navigator.clipboard && window.isSecureContext) navigator.clipboard.writeText(text).then(done, fallback);
    else fallback();
  }

  async function viewUsage(main) {
    const [kj, mj] = await Promise.all([api('/admin/api/keys'), api('/admin/api/test/models')]);
    const origin = location.origin;
    const baseV1 = origin + '/v1';
    const keyList = kj.keys || [];
    // Prefer a panel-created key over the one generated at deploy time.
    const managed = keyList.filter((k) => k.source === 'managed');
    let apiKey = ((managed[managed.length - 1] || keyList[0]) || { key: '' }).key;
    let keyForCode = apiKey || 'any';
    const models = (mj.models || []).filter((m) => m.capability === 'chat' || m.capability === 'console-chat');
    const avail = models.filter((m) => m.available);
    const model = (avail[0] || models[0] || { id: 'grok-4.20-0309-non-reasoning' }).id;
    let showKey = false;

    const maskKey = (k) => (k.length > 10 ? k.slice(0, 6) + '…' + k.slice(-4) : k);
    const snippets = [
      { id: 'env', label: '.env', build: (k) => `OPENAI_BASE_URL=${baseV1}\nOPENAI_API_KEY=${k}\nOPENAI_MODEL=${model}\n` },
      { id: 'curl', label: 'curl', build: (k) => `curl ${baseV1}/chat/completions \\\n  -H "Authorization: Bearer ${k}" \\\n  -H "Content-Type: application/json" \\\n  -d '{\n    "model": "${model}",\n    "messages": [{"role": "user", "content": "你好"}],\n    "stream": true\n  }'\n` },
      { id: 'py', label: 'Python', build: (k) => `from openai import OpenAI\n\nclient = OpenAI(base_url="${baseV1}", api_key="${k}")\n\nresp = client.chat.completions.create(\n    model="${model}",\n    messages=[{"role": "user", "content": "你好"}],\n    stream=True,\n)\nfor chunk in resp:\n    delta = chunk.choices[0].delta\n    if delta.content:\n        print(delta.content, end="", flush=True)\n` },
      { id: 'node', label: 'Node.js', build: (k) => `import OpenAI from "openai";\n\nconst client = new OpenAI({ baseURL: "${baseV1}", apiKey: "${k}" });\n\nconst stream = await client.chat.completions.create({\n  model: "${model}",\n  messages: [{ role: "user", content: "你好" }],\n  stream: true,\n});\nfor await (const chunk of stream) {\n  process.stdout.write(chunk.choices[0]?.delta?.content ?? "");\n}\n` },
      { id: 'anthropic', label: 'Anthropic SDK', build: (k) => `import anthropic\n\nclient = anthropic.Anthropic(base_url="${origin}", api_key="${k}")\n\nmsg = client.messages.create(\n    model="${model}",\n    max_tokens=1024,\n    messages=[{"role": "user", "content": "你好"}],\n)\nprint(msg.content[0].text)\n` },
    ];
    let active = snippets[0];

    const codeBox = h('pre', { class: 'code' });
    const tabs = h('div', { class: 'row' });
    function drawCode() {
      codeBox.textContent = active.build(showKey || !apiKey ? keyForCode : maskKey(keyForCode));
      tabs.replaceChildren(...snippets.map((sn) => h('button', { class: 'btn sm' + (sn === active ? ' primary' : ''), onclick: () => { active = sn; drawCode(); } }, sn.label)));
    }
    drawCode();

    const kv = (label, value, opts) => h('div', { class: 'kv' },
      h('div', { class: 'kv-label' }, label),
      h('code', { class: 'kv-value' }, value),
      h('button', { class: 'btn sm', onclick: () => copyText((opts && opts.copy) || value) }, icon('copy'), '复制'));

    const keyValue = h('code', { class: 'kv-value' }, apiKey ? maskKey(apiKey) : '（未设置，接口对外开放）');
    const keySel = h('select', { class: 'select', onchange: (e) => {
      apiKey = e.target.value; keyForCode = apiKey || 'any';
      keyValue.textContent = showKey ? apiKey : maskKey(apiKey);
      drawCode();
    } }, keyList.map((k) => h('option', { value: k.key, selected: k.key === apiKey }, `${k.label || '未命名'}  ${maskKey(k.key)}`)));
    const keyRow = h('div', { class: 'kv' },
      h('div', { class: 'kv-label' }, '调用密钥 (API Key)'), keyValue,
      apiKey ? h('button', { class: 'btn sm', onclick: (e) => { showKey = !showKey; keyValue.textContent = showKey ? apiKey : maskKey(apiKey); e.currentTarget.lastChild.textContent = showKey ? '隐藏' : '显示'; drawCode(); } }, icon('eye'), '显示') : null,
      apiKey ? h('button', { class: 'btn sm', onclick: () => copyText(apiKey) }, icon('copy'), '复制') : null);

    const portText = location.port ? location.port : (location.protocol === 'https:' ? '443（HTTPS 默认）' : '80（HTTP 默认）');
    main.replaceChildren(
      h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, '接入说明'), h('p', { class: 'sub' }, '把下面的地址和密钥填进你的项目即可，兼容 OpenAI 与 Anthropic 接口'))),
      h('div', { class: 'stack' },
        apiKey ? null : h('div', { class: 'alert warn' }, '当前没有设置调用密钥，任何人都能调用。建议到「配置」页设置 api_key。'),
        h('div', { class: 'card stack' }, h('div', { class: 'card-title' }, '连接信息'),
          kv('域名（节点）', location.hostname),
          kv('端口', portText, { copy: location.port || (location.protocol === 'https:' ? '443' : '80') }),
          kv('OpenAI Base URL', baseV1),
          kv('Anthropic Base URL', origin),
          kv('聊天接口', baseV1 + '/chat/completions'),
          kv('模型列表接口', baseV1 + '/models'),
          keyList.length > 1 ? h('div', { class: 'kv' }, h('div', { class: 'kv-label' }, '选择密钥'), h('div', { class: 'grow' }, keySel)) : null,
          keyRow),
        h('div', { class: 'card stack' },
          h('div', { class: 'row spread' }, h('div', { class: 'card-title' }, '代码片段'),
            h('button', { class: 'btn primary', onclick: () => copyText(active.build(keyForCode), '已复制完整内容（含真实密钥）') }, icon('copy'), '一键复制')),
          tabs, codeBox,
          h('p', { class: 'hint' }, showKey ? '当前显示的是真实密钥。' : '显示的密钥已遮盖，点「一键复制」得到的是带真实密钥的完整内容。')),
        h('div', { class: 'card stack' },
          h('div', { class: 'card-title' }, `可用模型（${avail.length}）`),
          avail.length === 0
            ? h('p', { class: 'muted' }, '当前账号池里没有可用账号，请先到「账号」页导入。')
            : h('div', { class: 'row' }, avail.map((m) => h('button', { class: 'btn sm', title: m.name || m.id, onclick: () => copyText(m.id, '已复制模型名：' + m.id) }, m.id))),
          h('p', { class: 'hint' }, '点击模型名即可复制。完整列表也可以通过 GET ' + baseV1 + '/models 获取。'))));
  }



  // ----------------------------------------------------------------- models
  const KIND_LABEL = { 'console-chat': '控制台聊天', 'web-chat': '网页聊天', image: '图片', 'image-edit': '图片编辑', video: '视频', other: '其他' };

  async function viewModels(main) {
    const st = { q: '', kind: '' };
    let rows = (await api('/admin/api/models')).models || [];

    const reload = async () => { rows = (await api('/admin/api/models')).models || []; draw(); };

    function openAdd(prefillId) {
      const idIn = h('input', { class: 'input', placeholder: '例如 grok-5', value: prefillId || '', autocomplete: 'off' });
      const nameIn = h('input', { class: 'input', placeholder: '显示名称（可选）' });
      const kindSel = h('select', { class: 'select' },
        h('option', { value: 'console-chat' }, '控制台聊天（console.x.ai，模型名原样发给上游）'),
        h('option', { value: 'web-chat' }, '网页聊天（grok.com，映射到一个模式）'));
      const modeSel = h('select', { class: 'select' }, ['auto', 'fast', 'expert', 'heavy'].map((m) => h('option', { value: m }, m)));
      const tierSel = h('select', { class: 'select' }, ['basic', 'super', 'heavy'].map((m) => h('option', { value: m }, m)));
      const webOnly = h('div', { class: 'row hidden' },
        h('div', { class: 'grow field' }, h('label', {}, '模式'), modeSel),
        h('div', { class: 'grow field' }, h('label', {}, '账号池档位'), tierSel));
      const hint = h('div', { class: 'alert info' }, '控制台聊天：模型名必须是上游真实存在的模型 ID，思考强度由请求里的 reasoning_effort 传递，不需要在名字里加 low / high 后缀。');
      kindSel.addEventListener('change', () => {
        const web = kindSel.value === 'web-chat';
        webOnly.classList.toggle('hidden', !web);
        hint.textContent = web
          ? '网页聊天：只是给 grok.com 已有的某个模式起一个新名字，不会获得新模型能力。'
          : '控制台聊天：模型名必须是上游真实存在的模型 ID，思考强度由请求里的 reasoning_effort 传递，不需要在名字里加 low / high 后缀。';
      });
      let dlg;
      const go = async () => {
        try {
          await api('/admin/api/models', { method: 'POST', body: Object.assign({ id: idIn.value.trim(), name: nameIn.value.trim(), kind: kindSel.value }, kindSel.value === 'web-chat' ? { mode: modeSel.value, tier: tierSel.value } : {}) });
          dlg.close();
          toast('模型已添加', 'ok');
          await reload();
        } catch (ex) { toast('添加失败：' + ex.message, 'err'); }
      };
      dlg = modal('添加模型', [
        h('div', { class: 'field' }, h('label', {}, '模型名（客户端调用时填写的 model）'), idIn),
        h('div', { class: 'field' }, h('label', {}, '显示名称'), nameIn),
        h('div', { class: 'field' }, h('label', {}, '类型'), kindSel),
        webOnly, hint],
      [h('button', { class: 'btn', onclick: () => dlg.close() }, '取消'), h('button', { class: 'btn primary', onclick: go }, '添加')], { cls: 'narrow' });
      setTimeout(() => idIn.focus(), 0);
    }

    async function discover() {
      const body = h('div', { class: 'stack' }, h('p', { class: 'muted' }, '正在向上游查询模型列表…'));
      const dlg = modal('拉取最新模型', body, [h('button', { class: 'btn', onclick: () => dlg.close() }, '关闭')]);
      try {
        const r = await api('/admin/api/models/discover', { method: 'POST', body: {} });
        const parts = [h('div', { class: 'alert info' }, `查询来源：${r.source}（使用账号 ${r.account}）。此接口的返回格式没有文档，未经真实账号验证，请以结果为准。`)];
        if (!r.ok) {
          parts.push(h('div', { class: 'alert err' }, '查询失败：' + r.error));
          parts.push(h('p', { class: 'muted' }, '可能是上游没有开放这个列表接口，或该账号没有权限。你仍然可以用「添加模型」手动填写新的模型 ID。'));
        } else if (!r.models || r.models.length === 0) {
          parts.push(h('div', { class: 'alert warn' }, '上游返回成功，但没有识别到模型 ID。原始返回：'));
          parts.push(h('pre', { class: 'code' }, JSON.stringify(r.raw, null, 2)));
        } else {
          const fresh = r.models.filter((m) => !m.known);
          const picked = new Set(fresh.map((m) => m.id));
          parts.push(h('div', { class: 'alert ' + (fresh.length ? 'ok' : 'info') }, fresh.length ? `上游共 ${r.models.length} 个模型，其中 ${fresh.length} 个是本地还没有的：` : `上游共 ${r.models.length} 个模型，本地都已包含。`));
          if (fresh.length) {
            parts.push(h('div', { class: 'found-list' }, fresh.map((m) => h('label', { class: 'found-item' },
              h('input', { type: 'checkbox', checked: true, onchange: (e) => { if (e.target.checked) picked.add(m.id); else picked.delete(m.id); } }),
              h('span', { class: 'mono' }, m.id)))));
            parts.push(h('button', { class: 'btn primary', onclick: async () => {
              let ok = 0; const fail = [];
              for (const id of picked) {
                try { await api('/admin/api/models', { method: 'POST', body: { id, name: id, kind: 'console-chat' } }); ok++; } catch (ex) { fail.push(id + '：' + ex.message); }
              }
              toast(`已添加 ${ok} 个` + (fail.length ? `，失败 ${fail.length} 个` : ''), fail.length ? 'err' : 'ok');
              dlg.close();
              await reload();
            } }, icon('plus'), '添加所选为控制台聊天模型'));
          }
          const known = r.models.filter((m) => m.known);
          if (known.length) parts.push(h('p', { class: 'hint' }, '已有：' + known.map((m) => m.id).join('、')));
        }
        body.replaceChildren(...parts);
      } catch (ex) { body.replaceChildren(h('div', { class: 'alert err' }, '查询失败：' + ex.message)); }
    }

    async function setEnabled(m, enabled) {
      try { await api(`/admin/api/models/${encodeURIComponent(m.id)}/enabled`, { method: 'POST', body: { enabled } }); m.enabled = enabled; }
      catch (ex) { toast('操作失败：' + ex.message, 'err'); }
      draw();
    }

    function draw() {
      const q = st.q.toLowerCase();
      const list = rows.filter((m) => (!st.kind || m.kind === st.kind) && (!q || m.id.toLowerCase().includes(q) || (m.name || '').toLowerCase().includes(q)));
      const kinds = ['', ...new Set(rows.map((m) => m.kind))];
      const body = list.map((m) => h('tr', {},
        h('td', {}, h('span', { class: 'mono' }, m.id)),
        h('td', {}, m.name),
        h('td', {}, h('span', { class: 'badge' }, KIND_LABEL[m.kind] || m.kind)),
        h('td', {}, `${m.mode} / ${m.tier}`),
        h('td', {}, m.builtin ? h('span', { class: 'badge' }, '内置') : h('span', { class: 'badge info' }, '自定义')),
        h('td', {}, m.available ? h('span', { class: 'badge ok' }, '有可用账号') : h('span', { class: 'badge warn' }, '无可用账号')),
        h('td', {}, h('input', { type: 'checkbox', checked: m.enabled, title: m.enabled ? '点击停用' : '点击启用', onchange: (e) => setEnabled(m, e.target.checked) })),
        h('td', {}, m.builtin ? null : h('button', { class: 'btn sm danger', onclick: async () => {
          if (!(await confirmDialog(`确定删除自定义模型「${m.id}」？`, '删除', true))) return;
          try { await api('/admin/api/models/' + encodeURIComponent(m.id), { method: 'DELETE' }); toast('已删除', 'ok'); await reload(); }
          catch (ex) { toast('删除失败：' + ex.message, 'err'); }
        } }, icon('trash')))));
      const search = h('input', { class: 'input', placeholder: '搜索模型', value: st.q, oninput: (e) => { st.q = e.target.value.trim(); draw(); const el = document.querySelector('.toolbar input'); if (el) { el.focus(); el.setSelectionRange(el.value.length, el.value.length); } } });
      main.replaceChildren(
        h('div', { class: 'page-head' },
          h('div', {}, h('h1', {}, '模型'), h('p', { class: 'sub' }, `共 ${rows.length} 个，其中 ${rows.filter((m) => m.enabled).length} 个已启用；停用的模型不会出现在 /v1/models，也不能被调用`)),
          h('div', { class: 'row' },
            h('button', { class: 'btn', onclick: discover }, icon('refresh'), '拉取最新模型'),
            h('button', { class: 'btn primary', onclick: () => openAdd('') }, icon('plus'), '添加模型'))),
        h('div', { class: 'card flush' },
          h('div', { class: 'toolbar' }, search,
            h('select', { class: 'select', onchange: (e) => { st.kind = e.target.value; draw(); } }, kinds.map((k) => h('option', { value: k, selected: k === st.kind }, k ? (KIND_LABEL[k] || k) : '全部类型')))),
          list.length
            ? h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['模型名', '显示名', '类型', '模式 / 档位', '来源', '账号', '启用', ''].map((x) => h('th', {}, x)))), h('tbody', {}, body)))
            : h('div', { class: 'empty' }, '没有符合条件的模型')));
    }
    draw();
  }

  // ------------------------------------------------------------------ stats
  function relTime(ms) {
    if (!ms) return '—';
    const diff = ms - Date.now();
    const abs = Math.abs(diff);
    const min = Math.round(abs / 60000);
    const txt = min < 60 ? `${min} 分钟` : (min < 1440 ? `${(min / 60).toFixed(1)} 小时` : `${(min / 1440).toFixed(1)} 天`);
    return diff >= 0 ? `约 ${txt}后` : `${txt}前`;
  }

  function showPlanInfo(token, masked) {
    const body = h('div', { class: 'stack' }, h('p', { class: 'muted' }, '正在向上游查询…'));
    const dlg = modal('套餐信息 · ' + masked, body, [h('button', { class: 'btn', onclick: () => dlg.close() }, '关闭')]);
    api('/admin/api/tokens/info', { method: 'POST', body: { token } }).then((r) => {
      const parts = [];
      parts.push(h('div', { class: 'alert info' }, '套餐与到期信息来自上游的 ' + r.endpoint + '，字段按名称自动提取，未经真实账号验证；下方同时给出原始返回，请以原始数据为准。'));
      if (!r.ok) {
        parts.push(h('div', { class: 'alert err' }, '查询失败：' + (r.error || '未知错误')));
      } else {
        const sum = r.summary || [];
        if (sum.length === 0) parts.push(h('div', { class: 'alert warn' }, '上游返回成功，但没有识别到套餐或到期字段，请查看原始数据。'));
        else parts.push(h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '项目'), h('th', {}, '值'), h('th', {}, '字段'))),
          h('tbody', {}, sum.map((it) => h('tr', {},
            h('td', {}, it.label),
            h('td', {}, it.time ? `${it.time.replace('T', ' ').replace('Z', ' UTC')}（${relTime(it.time_ms)}）` : String(it.value)),
            h('td', { class: 'mono muted' }, it.path)))))));
        parts.push(h('div', { class: 'field' }, h('label', {}, '原始返回'),
          h('pre', { class: 'code' }, JSON.stringify(r.raw, null, 2))));
      }
      body.replaceChildren(...parts);
    }).catch((ex) => body.replaceChildren(h('div', { class: 'alert err' }, '查询失败：' + ex.message)));
  }

  async function viewStats(main) {
    const [u, tokens] = await Promise.all([api('/admin/api/usage?days=14'), fetchTokens()]);
    const t = u.totals || {};
    const rate = t.requests ? ((t.errors / t.requests) * 100).toFixed(1) + '%' : '—';
    const today = (u.days || [])[(u.days || []).length - 1] || { requests: 0, errors: 0 };
    const maxReq = Math.max(1, ...(u.days || []).map((d) => d.requests));
    const stat = (label, value) => h('div', { class: 'stat' }, h('div', { class: 'label' }, label), h('div', { class: 'value' }, value));

    const dayRows = (u.days || []).slice().reverse().map((d) => h('tr', {},
      h('td', {}, d.date), h('td', { class: 'num' }, d.requests), h('td', { class: 'num' }, d.errors),
      h('td', { class: 'num' }, d.requests ? d.avg_ms + ' ms' : '—'),
      h('td', {}, h('progress', { class: 'bar', value: d.requests, max: maxReq }))));
    const modelRows = (u.models || []).map((m) => h('tr', {}, h('td', { class: 'mono' }, m.model), h('td', { class: 'num' }, m.requests), h('td', { class: 'num' }, m.errors)));

    const acctRows = tokens.map((tk) => {
      const cells = MODE_ORDER.filter((m) => tk.quota && tk.quota[m]).map((m) => {
        const q = tk.quota[m];
        const used = Math.max(0, q.total - q.remaining);
        return h('div', { class: 'quota-line' },
          h('span', { class: 'quota-name' }, m),
          h('progress', { class: 'bar', value: used, max: Math.max(1, q.total) }),
          h('span', { class: 'quota-num' }, `已用 ${used}/${q.total}`),
          h('span', { class: 'muted' }, q.reset_at ? '· 重置 ' + relTime(q.reset_at) : ''));
      });
      return h('tr', {},
        h('td', { class: 'mono' }, maskToken(tk.token)),
        h('td', {}, tk.pool), h('td', {}, statusBadge(tk.status)),
        h('td', {}, cells.length ? cells : h('span', { class: 'muted' }, '暂无额度数据（点「刷新全部」同步）')),
        h('td', { class: 'num' }, tk.use_count || 0),
        h('td', {}, h('button', { class: 'btn sm', onclick: () => showPlanInfo(tk.token, maskToken(tk.token)) }, '查询套餐')));
    });

    main.replaceChildren(
      h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, '用量统计'),
        h('p', { class: 'sub' }, `统计自 ${u.since || '—'} 起，最近 ${u.window || 14} 天；记录的是经过本网关的请求`))),
      h('div', { class: 'stack' },
        h('div', { class: 'stats' },
          stat('今日请求', today.requests), stat('今日失败', today.errors),
          stat(`${u.window || 14} 天请求`, t.requests || 0), stat('失败率', rate), stat('平均耗时', t.requests ? t.avg_ms + ' ms' : '—')),
        h('div', { class: 'card flush' },
          h('div', { class: 'toolbar' }, h('strong', {}, '账号额度用量'),
            h('span', { class: 'muted' }, '来自上游 rate-limits：已用 = 总量 − 剩余；重置时间按窗口时长推算，并非上游给出的精确时刻')),
          acctRows.length
            ? h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['账号', '池', '状态', '各模式用量', '使用次数', '套餐'].map((x) => h('th', {}, x)))), h('tbody', {}, acctRows)))
            : h('div', { class: 'empty' }, '还没有账号')),
        h('div', { class: 'card flush' },
          h('div', { class: 'toolbar' }, h('strong', {}, '每日请求')),
          h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '日期'), h('th', { class: 'num' }, '请求'), h('th', { class: 'num' }, '失败'), h('th', { class: 'num' }, '平均耗时'), h('th', {}, '趋势'))), h('tbody', {}, dayRows)))),
        h('div', { class: 'card flush' },
          h('div', { class: 'toolbar' }, h('strong', {}, '按模型')),
          modelRows.length
            ? h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, h('th', {}, '模型'), h('th', { class: 'num' }, '请求'), h('th', { class: 'num' }, '失败'))), h('tbody', {}, modelRows)))
            : h('div', { class: 'empty' }, '暂无请求记录')),
        h('p', { class: 'hint' }, '说明：流式响应的「失败」只统计 HTTP 状态码为错误的请求；上游不返回 token 用量，所以这里统计的是请求次数而不是 token 数。')));
  }

  // ------------------------------------------------------------------- keys
  async function viewKeys(main) {
    const j = await api('/admin/api/keys');
    const keys = j.keys || [];
    const reveal = new Set();

    async function createKey() {
      const label = h('input', { class: 'input', placeholder: '用途备注，例如：我的项目 / 手机客户端', maxlength: '60' });
      let dlg;
      const go = async () => {
        try {
          const r = await api('/admin/api/keys', { method: 'POST', body: { label: label.value.trim() } });
          dlg.close();
          showCreated(r);
          await renderRoute();
        } catch (ex) { toast('创建失败：' + ex.message, 'err'); }
      };
      label.addEventListener('keydown', (e) => { if (e.key === 'Enter') go(); });
      dlg = modal('创建 API 密钥', h('div', { class: 'field' }, h('label', {}, '备注'), label,
        h('span', { class: 'hint' }, '给不同的项目各建一个密钥，泄露时只需吊销对应的那一个。')),
      [h('button', { class: 'btn', onclick: () => dlg.close() }, '取消'), h('button', { class: 'btn primary', onclick: go }, '创建')], { cls: 'narrow' });
      setTimeout(() => label.focus(), 0);
    }
    function showCreated(r) {
      const d = modal('密钥已创建', [
        h('div', { class: 'alert ok' }, '请立即复制保存。之后仍可在本页查看，但请妥善保管，不要提交到公开仓库。'),
        h('code', { class: 'kv-value' }, r.key)],
      [h('button', { class: 'btn primary', onclick: () => copyText(r.key, '密钥已复制') }, icon('copy'), '复制密钥'),
        h('button', { class: 'btn', onclick: () => d.close() }, '完成')]);
    }
    async function revoke(k) {
      const warn = keys.length <= 1 ? '这是最后一个密钥，不能删除。' : `吊销后使用该密钥的项目会立即收到 401。确定吊销「${k.label || maskToken(k.key)}」？`;
      if (keys.length <= 1) { toast(warn, 'err'); return; }
      if (!(await confirmDialog(warn, '吊销', true))) return;
      try { await api('/admin/api/keys/' + encodeURIComponent(k.id), { method: 'DELETE' }); toast('已吊销', 'ok'); await renderRoute(); }
      catch (ex) { toast('吊销失败：' + ex.message, 'err'); }
    }

    const rows = keys.map((k) => h('tr', {},
      h('td', {}, k.label || h('span', { class: 'muted' }, '未命名'),
        k.source === 'config' ? h('div', { class: 'hint' }, '部署时生成，写在配置文件里；建议创建自己的密钥后吊销它') : null),
      h('td', {}, h('span', { class: 'mono' }, reveal.has(k.id) ? k.key : maskToken(k.key)),
        h('button', { class: 'btn icon ghost sm', title: reveal.has(k.id) ? '隐藏' : '显示', onclick: () => { if (reveal.has(k.id)) reveal.delete(k.id); else reveal.add(k.id); viewKeys(main); } }, icon(reveal.has(k.id) ? 'eyeOff' : 'eye')),
        h('button', { class: 'btn icon ghost sm', title: '复制', onclick: () => copyText(k.key, '密钥已复制') }, icon('copy'))),
      h('td', {}, k.created_at ? fmtTime(k.created_at) : '—'),
      h('td', {}, k.last_used_at ? relTime(k.last_used_at) : (k.source === 'managed' ? '从未使用' : '—')),
      h('td', { class: 'num' }, k.source === 'managed' ? (k.requests || 0) : '—'),
      h('td', {}, h('button', { class: 'btn sm danger', onclick: () => revoke(k) }, icon('trash'), '吊销'))));

    main.replaceChildren(
      h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, 'API 密钥'), h('p', { class: 'sub' }, '对外调用 /v1/* 用的密钥，可以按项目分别创建和吊销')),
        h('button', { class: 'btn primary', onclick: createKey }, icon('plus'), '创建密钥')),
      h('div', { class: 'stack' },
        j.open_mode ? h('div', { class: 'alert err' }, '当前没有任何密钥，接口对外完全开放。请立即创建一个密钥。') : null,
        h('div', { class: 'card flush' },
          keys.length
            ? h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['备注', '密钥', '创建时间', '最近使用', '请求数', '操作'].map((x) => h('th', { class: x === '请求数' ? 'num' : '' }, x)))), h('tbody', {}, rows)))
            : h('div', { class: 'empty' }, '还没有密钥')),
        h('div', { class: 'alert info' }, '管理密钥（app_key，用于登录本面板）与这里的调用密钥是两回事：调用密钥只能调用 /v1/*，不能进入管理后台。')));
  }

  // ----------------------------------------------------------------- config
  const FIELDS = [
    { group: '访问控制', items: [
      { path: 'app.api_key', label: '调用密钥 api_key', type: 'secret', hint: '客户端调用 /v1/* 时使用，多个用英文逗号分隔；留空即不鉴权（不推荐）。' },
      { path: 'app.app_key', label: '管理密钥 app_key', type: 'secret', hint: '登录本面板用。修改后需要用新密钥重新登录。' },
    ] },
    { group: '功能', items: [
      { path: 'features.stream', label: '默认流式输出', type: 'bool' },
      { path: 'features.thinking', label: '输出思考过程', type: 'bool' },
      { path: 'features.temporary', label: '临时对话', type: 'bool' },
      { path: 'features.memory', label: '会话记忆', type: 'bool' },
      { path: 'features.auto_chat_mode_fallback', label: 'auto 额度耗尽后自动降级', type: 'bool' },
      { path: 'features.conversation_continuation', label: '续接上一次会话', type: 'bool', hint: '多个调用方共用账号池时会串上下文，建议保持关闭。' },
      { path: 'features.custom_instruction', label: '全局附加指令', type: 'text' },
    ] },
    { group: '网络与 Cloudflare', items: [
      { path: 'proxy.egress.proxy_url', label: '出口代理', type: 'text', hint: '例如 http://127.0.0.1:7890 或 socks5://127.0.0.1:1080。' },
      { path: 'proxy.clearance.cf_cookies', label: 'Cloudflare Cookie', type: 'secret', hint: '浏览器里的整段 Cookie（含 cf_clearance），需与下面的 UA 匹配。' },
      { path: 'proxy.clearance.user_agent', label: 'User-Agent', type: 'text' },
    ] },
    { group: '重试与账号', items: [
      { path: 'retry.max_retries', label: '换账号重试次数', type: 'number' },
      { path: 'retry.on_codes', label: '触发重试的状态码', type: 'text', hint: '英文逗号分隔，如 429,401,503。' },
      { path: 'account.refresh.enabled', label: '按额度选号（定时刷新额度）', type: 'bool' },
      { path: 'account.selection.max_inflight', label: '单账号并发上限', type: 'number' },
    ] },
    { group: '缓存', items: [
      { path: 'cache.local.image_max_mb', label: '图片缓存上限（MB，0 为不限）', type: 'number' },
      { path: 'cache.local.video_max_mb', label: '视频缓存上限（MB，0 为不限）', type: 'number' },
    ] },
  ];
  const getPath = (obj, path) => path.split('.').reduce((o, k) => (o && typeof o === 'object' ? o[k] : undefined), obj);
  function setPath(obj, path, value) {
    const keys = path.split('.');
    let o = obj;
    keys.slice(0, -1).forEach((k) => { o[k] = o[k] || {}; o = o[k]; });
    o[keys[keys.length - 1]] = value;
  }

  async function viewConfig(main) {
    const cfg = await api('/admin/api/config');
    const controls = [];
    const groups = FIELDS.map((g) => h('div', { class: 'card stack' },
      h('div', { class: 'card-title' }, g.group),
      g.items.map((f) => {
        let cur = getPath(cfg, f.path);
        if (Array.isArray(cur)) cur = cur.join(',');
        let el; let read;
        if (f.type === 'bool') {
          el = h('input', { type: 'checkbox', checked: !!cur });
          read = () => el.checked;
        } else if (f.type === 'number') {
          el = h('input', { class: 'input', type: 'number', value: cur === undefined ? '' : cur });
          read = () => (el.value === '' ? undefined : Number(el.value));
        } else {
          el = h('input', { class: 'input', type: f.type === 'secret' ? 'password' : 'text', value: cur === undefined ? '' : String(cur), autocomplete: 'off' });
          read = () => el.value;
        }
        controls.push({ f, el, read, initial: f.type === 'bool' ? !!cur : (f.type === 'number' ? (cur === undefined ? undefined : Number(cur)) : (cur === undefined ? '' : String(cur))) });
        const reveal = f.type === 'secret'
          ? h('button', { class: 'btn sm', type: 'button', onclick: () => { el.type = el.type === 'password' ? 'text' : 'password'; } }, '显示/隐藏') : null;
        return h('div', { class: 'field' },
          f.type === 'bool' ? h('label', { class: 'check' }, el, f.label) : [h('label', {}, f.label), h('div', { class: 'row' }, h('div', { class: 'grow' }, el), reveal)],
          f.hint ? h('span', { class: 'hint' }, f.hint) : null);
      })));

    async function save() {
      const patch = {};
      let appKeyChanged = null;
      for (const c of controls) {
        const v = c.read();
        if (v === c.initial || v === undefined) continue;
        setPath(patch, c.f.path, v);
        if (c.f.path === 'app.app_key') appKeyChanged = v;
      }
      if (!Object.keys(patch).length) { toast('没有修改'); return; }
      try {
        await api('/admin/api/config', { method: 'POST', body: patch });
        toast('配置已保存', 'ok');
        if (appKeyChanged) {
          state.key = appKeyChanged;
          try { sessionStorage.setItem(STORE_KEY, appKeyChanged); } catch (_) { /* ignore */ }
        }
        await renderRoute();
      } catch (ex) { toast('保存失败：' + ex.message, 'err'); }
    }

    main.replaceChildren(
      h('div', { class: 'page-head' },
        h('div', {}, h('h1', {}, '配置'), h('p', { class: 'sub' }, '保存后立即生效；启动期专用项（存储路径等）不能在这里修改')),
        h('button', { class: 'btn primary', onclick: save }, icon('check'), '保存修改')),
      h('div', { class: 'stack' }, groups));
  }


  // ---------------------------------------------------------------- account
  async function viewAccount(main) {
    const acct = await api('/admin/api/auth/account');
    const viaKey = !!state.key;
    const supported = webauthnSupported() && !/^\d+\.\d+\.\d+\.\d+$/.test(location.hostname);

    if (!acct.configured) {
      main.replaceChildren(
        h('div', { class: 'page-head' }, h('h1', {}, '账户安全')),
        h('div', { class: 'alert warn' }, '还没有设置用户名和密码。请退出登录后在登录页完成首次设置。'));
      return;
    }

    const nameIn = h('input', { class: 'input', value: acct.username, autocomplete: 'username' });
    const curIn = h('input', { class: 'input', type: 'password', autocomplete: 'current-password', placeholder: viaKey ? '已用管理密钥登录，可留空' : '当前密码' });
    const newIn = h('input', { class: 'input', type: 'password', autocomplete: 'new-password', placeholder: '新密码（至少 10 位，不修改请留空）' });
    const saveBtn = h('button', { class: 'btn primary' }, icon('check'), '保存');
    saveBtn.addEventListener('click', async () => {
      const body = { current_password: curIn.value };
      if (nameIn.value.trim() && nameIn.value.trim() !== acct.username) body.username = nameIn.value.trim();
      if (newIn.value) body.new_password = newIn.value;
      if (!body.username && !body.new_password) { toast('没有修改'); return; }
      saveBtn.disabled = true;
      try {
        const r = await api('/admin/api/auth/account', { method: 'POST', body, noLogout: true });
        toast('已保存', 'ok');
        if (r.relogin) { state.key = ''; try { sessionStorage.removeItem(STORE_KEY); } catch (_) { /* ignore */ } renderLogin('密码已修改，请重新登录'); return; }
        await renderRoute();
      } catch (ex) { toast('保存失败：' + ex.message, 'err'); saveBtn.disabled = false; }
    });

    async function addPasskey() {
      const label = h('input', { class: 'input', placeholder: '给这把通行密钥起个名字，例如：我的 MacBook', maxlength: '40' });
      let dlg;
      const go = async () => {
        try {
          const begin = await api('/admin/api/auth/passkey/register/begin', { method: 'POST', body: {} });
          const pk = begin.options.publicKey;
          pk.challenge = b64uToBuf(pk.challenge);
          pk.user.id = b64uToBuf(pk.user.id);
          (pk.excludeCredentials || []).forEach((c) => { c.id = b64uToBuf(c.id); });
          const cred = await navigator.credentials.create({ publicKey: pk });
          await api('/admin/api/auth/passkey/register/finish?challenge_id=' + encodeURIComponent(begin.challenge_id) + '&label=' + encodeURIComponent(label.value.trim()),
            { method: 'POST', body: credentialToJSON(cred) });
          dlg.close();
          toast('通行密钥已添加', 'ok');
          await renderRoute();
        } catch (ex) { toast('添加失败：' + explainWebAuthnError(ex), 'err'); }
      };
      dlg = modal('添加通行密钥', h('div', { class: 'field' }, h('label', {}, '名称'), label,
        h('span', { class: 'hint' }, '点「继续」后，浏览器会让你用指纹、面容或设备密码确认。')),
      [h('button', { class: 'btn', onclick: () => dlg.close() }, '取消'), h('button', { class: 'btn primary', onclick: go }, '继续')], { cls: 'narrow' });
      setTimeout(() => label.focus(), 0);
    }

    const rows = (acct.passkeys || []).map((p) => h('tr', {},
      h('td', {}, p.label || '通行密钥'),
      h('td', {}, p.created_at ? fmtTime(p.created_at) : '—'),
      h('td', {}, p.last_used_at ? relTime(p.last_used_at) : '从未使用'),
      h('td', {}, h('button', { class: 'btn sm danger', onclick: async () => {
        if (!(await confirmDialog(`确定删除通行密钥「${p.label || '通行密钥'}」？`, '删除', true))) return;
        try { await api('/admin/api/auth/passkey/' + encodeURIComponent(p.id), { method: 'DELETE' }); toast('已删除', 'ok'); await renderRoute(); }
        catch (ex) { toast('删除失败：' + ex.message, 'err'); }
      } }, icon('trash'), '删除'))));

    main.replaceChildren(
      h('div', { class: 'page-head' }, h('div', {}, h('h1', {}, '账户安全'), h('p', { class: 'sub' }, '登录面板用的用户名、密码和通行密钥'))),
      h('div', { class: 'stack' },
        h('div', { class: 'card stack' }, h('div', { class: 'card-title' }, '用户名与密码'),
          h('div', { class: 'field' }, h('label', {}, '用户名'), nameIn),
          h('div', { class: 'field' }, h('label', {}, '当前密码'), curIn),
          h('div', { class: 'field' }, h('label', {}, '新密码'), newIn,
            h('span', { class: 'hint' }, '修改密码后，所有已登录的设备都会被退出。')),
          h('div', {}, saveBtn)),
        h('div', { class: 'card flush' },
          h('div', { class: 'toolbar' }, h('strong', {}, '通行密钥（Passkey）'),
            h('span', { class: 'grow' }),
            h('button', { class: 'btn primary', disabled: !supported, onclick: addPasskey }, icon('plus'), '添加通行密钥')),
          !supported ? h('div', { class: 'alert warn' }, '当前无法添加：通行密钥需要 HTTPS 域名访问，并且浏览器要支持 WebAuthn。') : null,
          rows.length
            ? h('div', { class: 'table-wrap' }, h('table', {}, h('thead', {}, h('tr', {}, ['名称', '创建时间', '最近使用', '操作'].map((x) => h('th', {}, x)))), h('tbody', {}, rows)))
            : h('div', { class: 'empty' }, '还没有通行密钥。添加后可以直接用指纹或面容登录，不用输入密码。')),
        h('div', { class: 'alert info' }, '通行密钥登录不需要输入用户名；它绑定在当前域名上，换域名需要重新添加。管理密钥 app_key 仍可用于脚本调用和忘记密码时恢复。')));
  }

  // ------------------------------------------------------------------- boot
  if (state.key) {
    api('/admin/api/verify', { noLogout: true }).then(start, () => { state.key = ''; renderLogin(); });
  } else {
    renderLogin();
  }
})();
