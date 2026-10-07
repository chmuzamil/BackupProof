// BackupProof dashboard. Vanilla ES module, no build step, CSP-safe:
// every DOM node is created through h() and all server text goes through
// textContent, never innerHTML. Visible wording is written for people who
// are not backup experts (see VOCABULARY notes next to each page).

'use strict';

const S = { status: null, user: null, csrf: '', gen: 0, timers: [], openAlerts: 0, ledgerFrom: 1, detailJob: null, importPrefill: null, openConnect: false };

// ---------------------------------------------------------------- helpers

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  if (attrs != null && (typeof attrs !== 'object' || attrs instanceof Node || Array.isArray(attrs))) {
    kids.unshift(attrs);
    attrs = null;
  }
  if (attrs) {
    for (const [k, v] of Object.entries(attrs)) {
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else if (k === 'value') el.value = v;
      else if (k === 'checked' || k === 'disabled' || k === 'selected') el[k] = !!v;
      else el.setAttribute(k, v === true ? '' : String(v));
    }
  }
  add(el, kids);
  return el;
}

function add(el, kids) {
  for (const k of kids) {
    if (k == null || k === false || k === true) continue;
    if (Array.isArray(k)) add(el, k);
    else el.append(k instanceof Node ? k : document.createTextNode(String(k)));
  }
  return el;
}

function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }
function fill(el, ...kids) { return add(clear(el), kids); }

function toast(msg, kind = '') {
  const box = document.getElementById('toasts');
  const t = h('div', { class: 'toast ' + kind, role: 'status' }, msg);
  box.append(t);
  setTimeout(() => t.remove(), kind === 'bad' ? 8000 : 4000);
}

class ApiError extends Error {
  constructor(msg, status) { super(msg); this.status = status; }
}

async function api(path, opts = {}) {
  const method = opts.method || 'GET';
  const init = { method, credentials: 'same-origin', headers: { Accept: 'application/json' } };
  if (method !== 'GET') init.headers['X-CSRF-Token'] = S.csrf;
  if (opts.body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(opts.body);
  }
  const r = await fetch('/api' + path, init);
  const txt = await r.text();
  let data = null;
  try { data = txt ? JSON.parse(txt) : null; } catch { data = txt; }
  if (r.status === 401 && !opts.noAuthRedirect) {
    S.user = null;
    renderAuth();
    throw new ApiError('Your session has expired. Please sign in again.', 401);
  }
  if (!r.ok) throw new ApiError((data && data.error) || `${r.status} ${r.statusText}`, r.status);
  return data;
}

const post = (p, body = {}) => api(p, { method: 'POST', body });
const put = (p, body) => api(p, { method: 'PUT', body });
const del = (p) => api(p, { method: 'DELETE' });

function qs(obj) {
  const u = new URLSearchParams();
  for (const [k, v] of Object.entries(obj)) if (v != null && v !== '') u.set(k, v);
  return u.toString();
}

// formatting

function toDate(t) {
  if (!t) return null;
  const d = t instanceof Date ? t : new Date(t);
  if (isNaN(d) || d.getFullYear() < 1971) return null;
  return d;
}

const plural = (n, unit) => `${n} ${unit}${n === 1 ? '' : 's'}`;

// rel renders a time the way people say it: "3 hours ago", "in 5 minutes".
function rel(t) {
  const d = toDate(t);
  if (!d) return '—';
  const s = Math.round((d - Date.now()) / 1000);
  const a = Math.abs(s);
  if (a < 45) return s < 0 ? 'just now' : 'in a moment';
  const units = [[60, 'minute', 3600], [3600, 'hour', 86400 * 2], [86400, 'day', 86400 * 60], [86400 * 30, 'month', 86400 * 730], [86400 * 365, 'year', Infinity]];
  let w = '';
  for (const [size, name, max] of units) { if (a < max) { w = plural(Math.max(1, Math.round(a / size)), name); break; } }
  return s < 0 ? w + ' ago' : 'in ' + w;
}

function absTime(t) { const d = toDate(t); return d ? d.toLocaleString() : ''; }
function day(t) { const d = toDate(t); return d ? d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' }) : '—'; }

function timeEl(t) {
  const d = toDate(t);
  return d ? h('time', { datetime: d.toISOString(), title: d.toLocaleString() }, rel(d)) : h('span', { class: 'muted' }, '—');
}

function bytes(n) {
  if (n == null || isNaN(n)) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0, v = Number(n);
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)) + ' ' + u[i];
}

// dur is the compact technical form; durWords is for plain summaries.
function dur(ms) {
  if (ms == null || isNaN(ms)) return '—';
  ms = Number(ms);
  if (ms < 1000) return Math.round(ms) + ' ms';
  const s = ms / 1000;
  if (s < 60) return s.toFixed(s < 10 ? 1 : 0) + 's';
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${Math.round(s % 60)}s`;
  const hr = Math.floor(m / 60);
  if (hr < 48) return `${hr}h ${m % 60}m`;
  return `${Math.floor(hr / 24)}d ${hr % 24}h`;
}

function durWords(ms) {
  if (ms == null || isNaN(ms)) return '—';
  const s = Math.round(Number(ms) / 1000);
  if (s < 1) return 'less than a second';
  if (s < 60) return plural(s, 'second');
  const m = Math.floor(s / 60);
  if (m < 60) return plural(m, 'minute') + (s % 60 ? ' ' + plural(s % 60, 'second') : '');
  const hr = Math.floor(m / 60);
  return plural(hr, 'hour') + (m % 60 ? ' ' + plural(m % 60, 'minute') : '');
}

function short(id, n = 8) { return id ? String(id).slice(0, n) : '—'; }

function pred(p) {
  if (!p || p.predicate == null) return {};
  if (typeof p.predicate === 'string') { try { return JSON.parse(p.predicate); } catch { return {}; } }
  return p.predicate;
}

function lines(text) { return String(text || '').split('\n').map((s) => s.trim()).filter(Boolean); }
function list(text) { return String(text || '').split(/[\n,]/).map((s) => s.trim()).filter(Boolean); }
function num(v) { const n = Number(v); return v === '' || isNaN(n) ? 0 : n; }
function joinWords(arr) { return arr.length < 2 ? arr.join('') : arr.slice(0, -1).join(', ') + ' and ' + arr[arr.length - 1]; }
function baseName(p) { const parts = String(p || '').replace(/[\\/]+$/, '').split(/[\\/]/); return parts[parts.length - 1] || p; }

// role checks
const role = () => (S.user && S.user.role) || '';
const isAdmin = () => role() === 'admin';
const canOperate = () => role() === 'admin' || role() === 'operator';

// ------------------------------------------------------------------ icons
// Simple stroke icons drawn with SVG DOM (no external assets, CSP-safe).

const ICONS = {
  home: 'M3 11l9-7 9 7M5 10v10h5v-6h4v6h5V10',
  shield: 'M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z',
  shieldCheck: ['M12 3l8 3v6c0 5-3.5 8-8 9-4.5-1-8-4-8-9V6z', 'M8.5 12l2.5 2.5 4.5-5'],
  folder: 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z',
  db: ['M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3z', 'M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6', 'M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3'],
  globe: ['M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18z', 'M3 12h18', 'M12 3c2.5 2.5 3.5 5.5 3.5 9s-1 6.5-3.5 9c-2.5-2.5-3.5-5.5-3.5-9s1-6.5 3.5-9z'],
  import: ['M12 3v12', 'M7 10l5 5 5-5', 'M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2'],
  computer: ['M3 5h18v11H3z', 'M8 21h8', 'M12 16v5'],
  cloud: 'M7 18a4 4 0 0 1-.5-8A6 6 0 0 1 18 9a4.5 4.5 0 0 1-.5 9z',
  disk: ['M3 13h18v6H3z', 'M5 13l2-8h10l2 8', 'M17 16h.01'],
  server: ['M4 4h16v6H4z', 'M4 14h16v6H4z', 'M8 7h.01', 'M8 17h.01'],
  sliders: ['M4 6h16', 'M4 12h16', 'M4 18h16', 'M8 4v4', 'M16 10v4', 'M10 16v4'],
  file: ['M6 3h8l4 4v14H6z', 'M14 3v4h4'],
  box: ['M3 7l9-4 9 4-9 4z', 'M3 7v10l9 4 9-4V7', 'M12 11v10'],
  check: ['M9 12l2 2 4-4', 'M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18z'],
  bell: ['M6 16V11a6 6 0 0 1 12 0v5l2 2H4z', 'M10 21h4'],
  plus: ['M12 5v14', 'M5 12h14'],
  history: ['M3 12a9 9 0 1 0 3-6.7', 'M3 4v5h5', 'M12 7v5l3 3'],
};

function icon(name, cls = '') {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('class', ('ico-svg ' + cls).trim());
  svg.setAttribute('aria-hidden', 'true');
  for (const d of [].concat(ICONS[name] || ICONS.box)) {
    const p = document.createElementNS(NS, 'path');
    p.setAttribute('d', d);
    svg.append(p);
  }
  return svg;
}

// ------------------------------------------------------------- vocabulary

const STATUS = {
  proven: { cls: 'ok', label: 'Restore tested ✓', help: 'The latest restore test passed recently, so this backup is known to work.' },
  unproven: { cls: '', label: 'Not tested yet', help: 'No restore test has passed yet.' },
  'at-risk': { cls: 'warn', label: 'Needs attention', help: 'A backup or a restore test is overdue.' },
  failing: { cls: 'bad', label: 'Problem', help: 'The last backup or restore test failed.' },
};

const JOB_WORDS = { backup: 'Backup', drill: 'Restore test', check: 'Storage health check', maintain: 'Cleanup' };
function jobWord(kind, isImport) { return kind === 'backup' && isImport ? 'Conversion' : JOB_WORDS[kind] || kind; }
const STATE_WORDS = { queued: 'Waiting', running: 'Running', succeeded: 'Done ✓', failed: 'Failed' };

function kindInfo(spec) {
  const k = (spec && spec.kind) || '';
  if (k === 'files') return { label: 'Folders & files', icon: 'folder' };
  if (k === 'mysql' && spec.wpConfig) return { label: 'WordPress database', icon: 'db' };
  if (k === 'postgres') return { label: 'PostgreSQL database', icon: 'db' };
  if (k === 'mysql') return { label: 'MySQL / MariaDB database', icon: 'db' };
  if (k === 'mongodb') return { label: 'MongoDB database', icon: 'db' };
  if (k === 'sqlite') return { label: 'SQLite database file', icon: 'db' };
  if (k === 'import') return { label: 'Imported backups', icon: 'import' };
  if (k === 'command') return { label: 'Output of a command', icon: 'sliders' };
  return { label: k || 'Unknown', icon: 'box' };
}

function osName(os) {
  const o = String(os || '').split('/')[0].toLowerCase();
  return { windows: 'Windows', linux: 'Linux', darwin: 'macOS', freebsd: 'FreeBSD' }[o] || os || 'Unknown system';
}

function repoKind(b) {
  if (!b) return { label: 'Unknown', icon: 'box' };
  if (b.type === 'local') return { label: 'Folder or disk', icon: 'disk' };
  if (b.type === 'sftp') return { label: 'Another server (SFTP)', icon: 'server' };
  const e = String(b.endpoint || '').toLowerCase();
  if (e.includes('backblazeb2')) return { label: 'Backblaze B2', icon: 'cloud' };
  if (e.includes('r2.cloudflarestorage')) return { label: 'Cloudflare R2', icon: 'cloud' };
  if (e.includes('wasabisys')) return { label: 'Wasabi', icon: 'cloud' };
  if (!e) return { label: 'Amazon S3', icon: 'cloud' };
  return { label: 'S3-compatible storage', icon: 'cloud' };
}

function repoLocation(b) {
  if (!b) return '—';
  if (b.type === 's3') return `bucket “${b.bucket || ''}”` + (b.prefix ? `, folder “${b.prefix}”` : '');
  if (b.type === 'sftp') return `${b.user ? b.user + '@' : ''}${b.host || ''}${b.port && b.port !== 22 ? ':' + b.port : ''} ${b.path || ''}`.trim();
  return b.path || '—';
}

// repoUrl is the --repo value for the backupproof command line.
function repoUrl(b) {
  if (!b) return '';
  if (b.type === 's3') {
    const q = qs({ endpoint: b.endpoint, region: b.region, lock: b.objectLockMode, lockDays: b.objectLockMode ? b.objectLockDays : '' });
    return `s3://${b.bucket || ''}/${b.prefix || ''}` + (q ? '?' + q : '');
  }
  if (b.type === 'sftp') return `sftp://${b.user ? b.user + '@' : ''}${b.host || ''}:${b.port || 22}${b.path || ''}`;
  return b.path || '';
}

function lockWords(b) {
  if (!b || !b.objectLockMode) return null;
  return b.objectLockMode === 'COMPLIANCE' ? `Ransomware protection: locked ${b.objectLockDays} days` : `Ransomware protection: ${b.objectLockDays} days`;
}

const CRON_WORDS = {
  '@hourly': 'every hour',
  '0 2 * * *': 'every night at 2:00',
  '@every 6h': 'every 6 hours',
  '0 3 * * 0': 'every Sunday at 3:00',
  '0 5 * * *': 'every day at 5:00',
  '0 4 * * *': 'every day at 4:00',
  '0 4 * * 0': 'every Sunday at 4:00',
  '0 4 1 * *': 'on the 1st of every month at 4:00',
  '@daily': 'every day at midnight',
  '@weekly': 'every Sunday at midnight',
  manual: 'only when you click the button',
};

function cronWords(c) {
  c = String(c || '').trim();
  if (CRON_WORDS[c]) return CRON_WORDS[c];
  let m = /^@every\s+(\d+)([smhd])$/.exec(c);
  if (m) return 'every ' + plural(Number(m[1]), { s: 'second', m: 'minute', h: 'hour', d: 'day' }[m[2]]);
  m = /^(\d{1,2}) (\d{1,2}) \* \* \*$/.exec(c);
  if (m) return `every day at ${m[2]}:${m[1].padStart(2, '0')}`;
  return `on a custom schedule (${c})`;
}

function retentionWords(r) {
  r = r || {};
  const parts = [];
  if (r.keepLast) parts.push(`the last ${r.keepLast}`);
  if (r.keepHourly) parts.push(`${r.keepHourly} hourly`);
  if (r.keepDaily) parts.push(`${r.keepDaily} daily`);
  if (r.keepWeekly) parts.push(`${r.keepWeekly} weekly`);
  if (r.keepMonthly) parts.push(`${r.keepMonthly} monthly`);
  if (r.keepYearly) parts.push(`${r.keepYearly} yearly`);
  if (!parts.length) return 'every copy (nothing is deleted)';
  return joinWords(parts) + ' copies';
}

const CHECK_WORDS = [
  [/^restore-from-storage$/, 'Downloaded and decrypted every file'],
  [/^content-root$/, 'Every file matches the original exactly'],
  [/^(sqlite-integrity-check|amcheck-btree-heapallindexed|check-table|validate-collections)$/, 'Database structure is healthy'],
  [/^row-count-reconciliation$/, 'All rows are there'],
  [/^exact-row-counts$/, 'Counted rows'],
  [/^expected-paths$/, 'Important files are present'],
  [/^minimum-file-count$/, 'Enough files restored'],
  [/^(pg_restore|import-dump|mongorestore)$/, 'Database loaded successfully'],
  [/^sandbox-start$/, 'Started a safe test environment'],
  [/^sandbox$/, 'A safe test environment is available'],
  [/^(locate-dump|locate-database)$/, 'Found the database backup'],
  [/^custom-verify-command$/, 'Your custom check'],
];

function checkWords(name) {
  const m = /^assert:\s*(.*)$/.exec(name || '');
  if (m) return 'Your check: ' + m[1];
  const dd = /^database-dump:s*(.*)$/.exec(name || '');
  if (dd) return `Loaded ${dd[1]} into a test database`;
  const di = /^database-dump-integrity:s*(.*)$/.exec(name || '');
  if (di) return `${di[1]}: database structure is healthy`;
  if (name === 'database-dumps') return 'Database dumps found';
  for (const [re, w] of CHECK_WORDS) if (re.test(name || '')) return w;
  return name || 'Check';
}

const ALERT_WORDS = {
  'job-failed': 'Something failed', 'backup-overdue': 'Backup overdue', 'proof-stale': 'Restore test overdue',
  'agent-offline': 'Server offline', 'drill-failed': 'Restore test failed',
};

// --------------------------------------------------------------- widgets

function statusPill(status, reason) {
  const s = STATUS[status] || { cls: '', label: status || 'Unknown', help: '' };
  const tip = reason || s.help;
  return h('span', { class: 'pill ' + s.cls, title: tip, tabindex: '0', role: 'button', onclick: (e) => { e.stopPropagation(); e.preventDefault(); toast(tip || s.label); } }, s.label);
}

function statePill(state) {
  const cls = { succeeded: 'ok', failed: 'bad', running: 'warn', queued: '' }[state] ?? '';
  return h('span', { class: 'pill ' + cls }, STATE_WORDS[state] || state || '—');
}

function passPill(passed) { return h('span', { class: 'pill ' + (passed ? 'ok' : 'bad') }, passed ? 'Passed' : 'Failed'); }

function btn(label, onclick, cls = '') { return h('button', { type: 'button', class: 'btn ' + cls, onclick }, label); }

// Runs an async action with the button disabled and errors toasted.
function busy(fn) {
  return async (e) => {
    const b = e && e.currentTarget;
    if (b) b.disabled = true;
    try { await fn(e); } catch (err) { if (err.status !== 401) toast(err.message, 'bad'); } finally { if (b) b.disabled = false; }
  };
}

function field(label, input, hint) {
  return h('label', { class: 'field' }, h('span', null, label), input, hint ? h('span', { class: 'hint' }, hint) : null);
}

function input(attrs = {}) { return h('input', { type: 'text', ...attrs }); }

function select(options, value, attrs = {}) {
  return h('select', attrs, options.map(([v, l]) => h('option', { value: String(v), selected: String(v) === String(value ?? '') }, l)));
}

function checkbox(label, checked, hint) {
  const cb = h('input', { type: 'checkbox', checked });
  return { el: h('label', { class: 'check' }, cb, h('span', null, label, hint ? h('span', { class: 'hint block' }, hint) : null)), cb };
}

function card(title, ...body) {
  return h('section', { class: 'card' }, title ? h('h2', null, title) : null, ...body);
}

function cardHead(title, right) {
  return h('div', { class: 'card-head' }, h('h2', null, title), right || null);
}

function table(headers, rows, opts = {}) {
  return h('div', { class: 'table-wrap' },
    h('table', { class: opts.stack === false ? '' : 'stack' },
      h('thead', null, h('tr', null, headers.map((x) => h('th', null, x)))),
      h('tbody', null, rows)));
}

function tr(cells, headers, attrs = {}) {
  return h('tr', attrs, cells.map((c, i) => h('td', { 'data-label': headers[i] || '' }, c)));
}

function empty(msg, ...extra) { return h('div', { class: 'empty' }, h('p', null, msg), ...extra); }

function pageHead(title, sub, right) {
  return h('div', { class: 'page-head' }, h('div', null, h('h1', null, title), sub ? h('p', { class: 'muted' }, sub) : null), right || null);
}

// details() hides technical or optional content behind a toggle.
function details(summary, ...kids) {
  return h('details', { class: 'more' }, h('summary', null, summary), h('div', { class: 'more-body' }, ...kids));
}
const tech = (...kids) => details('Show technical details', ...kids);

async function copyText(text) {
  try { await navigator.clipboard.writeText(text); }
  catch {
    const ta = h('textarea', { class: 'offscreen' }, text);
    document.body.append(ta); ta.select(); document.execCommand('copy'); ta.remove();
  }
}

function copyBtn(getText, label = 'Copy') {
  const b = btn(label, async () => {
    await copyText(getText());
    b.textContent = 'Copied ✓';
    setTimeout(() => { b.textContent = label; }, 1500);
  }, 'sm');
  return b;
}

function download(filename, text) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
  const a = h('a', { href: url, download: filename, class: 'offscreen' });
  document.body.append(a);
  a.click();
  setTimeout(() => { a.remove(); URL.revokeObjectURL(url); }, 1000);
}

function modal(title, body, actions = [], opts = {}) {
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    bg.remove();
    document.removeEventListener('keydown', onKey);
    if (opts.onClose) opts.onClose();
  };
  const onKey = (e) => { if (e.key === 'Escape') close(); };
  const bg = h('div', { class: 'modal-bg', onclick: (e) => { if (e.target === bg) close(); } },
    h('div', { class: 'modal' + (opts.wide ? ' wide' : ''), role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
      h('h2', null, title), body,
      h('div', { class: 'modal-foot' }, actions, btn(opts.closeLabel || 'Close', () => close()))));
  document.addEventListener('keydown', onKey);
  document.body.append(bg);
  return close;
}

function confirmDlg(msg, okLabel = 'Confirm') {
  return new Promise((resolve) => {
    let ok = false;
    const close = modal('Please confirm', h('p', null, msg), [btn(okLabel, () => { ok = true; close(); }, 'primary')], { closeLabel: 'Cancel', onClose: () => resolve(ok) });
  });
}

function every(ms, fn) { const id = setInterval(fn, ms); S.timers.push(id); return id; }

// choices renders big clickable cards. Options: {value,title,desc,icon,badge,extra,disabled}.
function choices(options, { value, onPick, multi = false, small = false } = {}) {
  let sel = multi ? new Set(value || []) : value;
  const wrap = h('div', { class: 'choices' + (small ? ' small' : ''), role: multi ? 'group' : 'radiogroup' });
  const btns = options.map((o) => {
    const b = h('button', {
      type: 'button', class: 'choice', disabled: o.disabled, role: multi ? 'checkbox' : 'radio',
      onclick: () => {
        if (multi) { if (sel.has(o.value)) sel.delete(o.value); else sel.add(o.value); } else sel = o.value;
        paint();
        if (onPick) onPick(multi ? [...sel] : sel, o);
      },
    },
    o.icon ? h('span', { class: 'choice-ico' }, icon(o.icon)) : null,
    h('span', { class: 'choice-body' },
      h('span', { class: 'choice-title' }, o.title, o.badge ? [' ', h('span', { class: 'pill' + (o.badgeCls ? ' ' + o.badgeCls : '') }, o.badge)] : null),
      o.desc ? h('span', { class: 'choice-desc' }, o.desc) : null,
      o.extra || null));
    b.pvValue = o.value;
    return b;
  });
  const paint = () => btns.forEach((b) => {
    const on = multi ? sel.has(b.pvValue) : sel === b.pvValue;
    b.classList.toggle('on', on);
    b.setAttribute('aria-checked', String(on));
  });
  add(wrap, btns);
  paint();
  return wrap;
}

function stepsBar(labels, cur) {
  return h('ol', { class: 'wsteps' }, labels.map((l, i) => h('li', { class: i < cur ? 'done' : i === cur ? 'cur' : '' },
    h('span', { class: 'n' }, i < cur ? '✓' : String(i + 1)), h('span', { class: 'l' }, l))));
}

function spaceBar(free, total) {
  const used = total ? Math.min(100, Math.max(0, Math.round(100 * (total - free) / total))) : 0;
  const fill = h('span', { class: 'bar-fill' + (used > 90 ? ' bad' : used > 75 ? ' warn' : '') });
  fill.style.width = used + '%';
  return h('span', { class: 'bar-wrap' }, h('span', { class: 'bar' }, fill), h('span', { class: 'hint' }, `${bytes(free)} free of ${bytes(total)}`));
}

function agentTitle(a) { return a.builtin ? 'This server' : a.name; }
function agentOrder(list) { return [...list].sort((a, b) => (b.builtin ? 1 : 0) - (a.builtin ? 1 : 0) || String(a.name).localeCompare(b.name)); }
function inventory(a) {
  let inv = a && a.inventory;
  if (typeof inv === 'string') { try { inv = JSON.parse(inv); } catch { inv = null; } }
  return inv || { items: [], databases: [], drives: [] };
}

// pickFolder opens a folder browser for this server's own disks.
function pickFolder(start = '') {
  return new Promise((resolve) => {
    let chosen = null, cur = '', up = '';
    const listBox = h('div', { class: 'picker-list' });
    const pathEl = h('code', { class: 'break' });
    const upBtn = btn('↑ Up', () => go(up), 'sm');
    const pickBtn = btn('Choose this folder', () => { chosen = cur; close(); }, 'primary');
    const go = async (p) => {
      fill(listBox, h('p', { class: 'muted' }, 'Loading…'));
      try {
        const r = await api('/browse?' + qs({ path: p }));
        cur = r.path || '';
        up = r.parent || '';
        pathEl.textContent = cur || 'Pick a place to start';
        upBtn.disabled = !cur;
        pickBtn.disabled = !cur;
        const dirs = (r.entries || []).filter((e) => e.dir);
        const files = (r.entries || []).length - dirs.length;
        fill(listBox, 
          dirs.length ? h('ul', null, dirs.map((e) => h('li', null, h('button', { type: 'button', class: 'picker-item', onclick: () => go(e.path) }, icon(cur ? 'folder' : 'disk'), h('span', null, e.name))))) : h('p', { class: 'muted' }, 'No folders inside.'),
          files > 0 ? h('p', { class: 'hint' }, `${plural(files, 'file')} in this folder (not shown).`) : null);
      } catch (e) { fill(listBox, h('p', { class: 'form-error' }, e.message)); }
    };
    const close = modal('Choose a folder', h('div', null, h('div', { class: 'picker-head' }, upBtn, pathEl), listBox), [pickBtn], { closeLabel: 'Cancel', onClose: () => resolve(chosen) });
    go(start);
  });
}

// ------------------------------------------------------------------ auth

function renderAuth(errMsg) {
  stopTimers();
  const app = clear(document.getElementById('app'));
  app.className = '';
  const setup = S.status && S.status.setupRequired;
  const user = input({ autocomplete: 'username', required: true, autofocus: true });
  const pass = input({ type: 'password', autocomplete: setup ? 'new-password' : 'current-password', required: true, minlength: setup ? 10 : null });
  const pass2 = input({ type: 'password', autocomplete: 'new-password', required: true });
  const err = h('div', { class: 'form-error' }, errMsg || '');
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      if (setup && pass.value !== pass2.value) { err.textContent = 'The passwords do not match.'; return; }
      if (setup && pass.value.length < 10) { err.textContent = 'The password must be at least 10 characters.'; return; }
      const b = form.querySelector('button'); b.disabled = true;
      try {
        const r = await api(setup ? '/setup' : '/login', { method: 'POST', body: { username: user.value.trim(), password: pass.value }, noAuthRedirect: true });
        S.user = r.user; S.csrf = r.csrf || S.csrf;
        if (S.status) S.status.setupRequired = false;
        if (!location.hash || location.hash === '#/') location.hash = '#/dashboard';
        renderShell();
      } catch (ex) { err.textContent = ex.status === 401 ? 'Wrong username or password.' : ex.message; b.disabled = false; }
    },
  },
  field('Username', user),
  field('Password', pass, setup ? 'At least 10 characters. You will use it to sign in to this dashboard.' : null),
  setup ? field('Type the password again', pass2) : null,
  err,
  h('button', { type: 'submit', class: 'btn primary block' }, setup ? 'Create my account' : 'Sign in'));
  app.append(h('div', { class: 'auth' },
    h('div', { class: 'card' },
      h('div', { class: 'brand' }, 'Proof', h('span', null, 'Vault')),
      h('p', { class: 'muted' }, setup ? 'Welcome! Create your administrator account to get started.' : 'Sign in to continue.'),
      form),
    h('p', { class: 'muted small' }, 'Auditors: ', h('a', { href: '/api/public/keys', target: '_blank', rel: 'noopener' }, 'public keys for checking proofs'),
      S.status && S.status.version ? ` · v${S.status.version}` : '')));
  user.focus();
}

// ----------------------------------------------------------------- shell

const NAV = [
  ['dashboard', 'Home', 'home'],
  ['protected', 'Protected', 'shield'],
  ['repositories', 'Storage', 'cloud'],
  ['agents', 'Servers', 'computer'],
  ['import', 'Import', 'import'],
  ['proofs', 'Proof history', 'history'],
  ['alerts', 'Alerts', 'bell'],
  ['settings', 'Settings', 'sliders'],
];

const NAV_ALIAS = { sources: 'protected', protect: 'protected', storage: 'repositories', keys: 'proofs' };

function renderShell() {
  const app = clear(document.getElementById('app'));
  app.className = '';
  const layout = h('div', { class: 'layout' });
  const toggle = () => layout.classList.toggle('nav-open');
  const nav = h('nav', { class: 'nav', id: 'nav' }, NAV.map(([k, l, ic]) =>
    h('a', { href: '#/' + k, 'data-k': k, onclick: () => layout.classList.remove('nav-open') }, h('span', { class: 'nav-l' }, icon(ic), h('span', null, l)),
      k === 'alerts' ? h('span', { class: 'count hidden', id: 'alert-count' }) : null)));
  add(layout, [
    h('div', { class: 'topbar' },
      h('button', { type: 'button', class: 'btn icon', 'aria-label': 'Menu', onclick: toggle }, '☰'),
      h('div', { class: 'brand' }, 'Proof', h('span', null, 'Vault'))),
    h('div', { class: 'scrim', onclick: toggle }),
    h('aside', { class: 'sidebar' },
      h('div', { class: 'brand' }, 'Proof', h('span', null, 'Vault')),
      nav,
      h('div', { class: 'userbox' },
        h('div', null, S.user.username), h('div', { class: 'role' }, { admin: 'Administrator', operator: 'Can set up backups', auditor: 'View only' }[S.user.role] || S.user.role),
        h('div', { class: 'btns' }, btn('Sign out', busy(logout), 'sm')),
        S.status && S.status.version ? h('div', { class: 'role' }, 'v' + S.status.version) : null)),
    h('main', { class: 'main', id: 'main' }),
  ]);
  app.append(layout);
  route();
  refreshAlertCount();
  clearInterval(S.alertTimer);
  S.alertTimer = setInterval(refreshAlertCount, 60000);
}

async function logout() {
  try { await post('/logout'); } catch { /* ignore */ }
  S.user = null;
  try { S.status = await api('/status', { noAuthRedirect: true }); S.csrf = S.status.csrf || ''; } catch { /* ignore */ }
  renderAuth();
}

async function refreshAlertCount() {
  if (!S.user) return;
  try { setAlertCount((await api('/alerts?open=1')) || []); } catch { /* ignore */ }
}

function setAlertCount(alerts) {
  const el = document.getElementById('alert-count');
  if (!el) return;
  el.textContent = String(alerts.length);
  el.classList.toggle('hidden', alerts.length === 0);
}

function stopTimers() { S.timers.forEach(clearInterval); S.timers = []; }

const ROUTES = [
  [/^\/dashboard$/, pageDashboard],
  [/^\/protected$/, pageProtected],
  [/^\/protect$/, pageProtect],
  [/^\/import$/, pageImport],
  [/^\/sources\/new$/, () => pageSourceForm(null)],
  [/^\/sources\/(\d+)\/edit$/, (m) => pageSourceForm(Number(m[1]))],
  [/^\/sources\/(\d+)$/, (m) => pageSource(Number(m[1]))],
  [/^\/agents$/, pageAgents],
  [/^\/repositories$/, pageRepositories],
  [/^\/storage\/new$/, pageStorageNew],
  [/^\/proofs$/, pageProofs],
  [/^\/keys$/, pageKeys],
  [/^\/alerts$/, pageAlerts],
  [/^\/settings$/, pageSettings],
];

async function route(keepScroll) {
  if (!S.user) return;
  const main = document.getElementById('main');
  if (!main) return;
  stopTimers();
  const path = (location.hash || '#/dashboard').slice(1) || '/dashboard';
  const top = path.split('/')[1];
  const navKey = NAV_ALIAS[top] || top;
  document.querySelectorAll('#nav a').forEach((a) => a.classList.toggle('active', a.dataset.k === navKey));
  const gen = ++S.gen;
  let node;
  try {
    const r = ROUTES.find(([re]) => re.test(path));
    node = r ? await r[1](path.match(r[0])) : card('Page not found', h('p', null, 'There is no such page. '), h('a', { href: '#/dashboard' }, 'Go to Home'));
  } catch (err) {
    if (err.status === 401) return;
    node = h('div', { class: 'banner bad' }, 'Could not load this page: ', err.message);
  }
  if (gen !== S.gen) return;
  const y = window.scrollY;
  fill(main, node);
  window.scrollTo(0, keepScroll ? y : 0);
}

const reload = () => route(true);

// Periodic refresh that never interrupts someone reading details or typing.
function autoRefresh() {
  if (document.querySelector('.modal-bg') || document.querySelector('#main details[open]')) return;
  const a = document.activeElement;
  if (a && /^(INPUT|TEXTAREA|SELECT)$/.test(a.tagName)) return;
  reload();
}

// ------------------------------------------------------------------ home

async function runJob(id, kind, after, isImport) {
  const r = await post(`/sources/${id}/run`, { kind });
  toast(`${jobWord(kind, isImport)} started. This can take a few minutes.`, 'ok');
  if (after) after(r.jobId);
  else reload();
}

function runButtons(id, running, after, withCheck, isImport) {
  if (!canOperate()) return null;
  return h('div', { class: 'btns' },
    btn(isImport ? 'Convert again' : 'Back up now', busy(() => runJob(id, 'backup', after, isImport)), 'sm primary'),
    btn('Test restore', busy(() => runJob(id, 'drill', after, isImport)), 'sm'),
    withCheck ? btn('Check storage health', busy(() => runJob(id, 'check', after, isImport)), 'sm') : null);
}

function runningLine(j, isImport) {
  if (!j) return null;
  const what = j.kind === 'drill' ? 'Testing the restore' : j.kind === 'check' ? 'Checking storage health' : isImport ? 'Converting' : 'Backing up';
  return h('div', { class: 'running' }, h('span', { class: 'spinner' }), j.state === 'queued' ? `${what} — waiting for the server to start…` : `${what} now…`);
}

function itemCard(s) {
  const src = s.source, k = kindInfo(src.spec), isImp = src.spec && src.spec.kind === 'import';
  const lb = s.lastBackup, ld = s.lastDrill;
  return h('article', { class: 'item' },
    h('div', { class: 'item-ico' }, icon(k.icon)),
    h('div', { class: 'item-main' },
      h('div', { class: 'item-title' }, h('a', { href: `#/sources/${src.id}` }, src.name), statusPill(s.status, s.reason)),
      h('div', { class: 'item-sub' }, [k.label, s.agentName ? 'on ' + s.agentName : '', s.repoName ? 'stored in ' + s.repoName : '', src.enabled === false ? 'paused' : ''].filter(Boolean).join(' · ')),
      s.reason ? h('p', { class: 'item-reason' }, s.reason) : null,
      h('div', { class: 'item-facts' },
        h('span', null, isImp ? 'Last conversion: ' : 'Last backup: ', lb ? [timeEl(lb.created), lb.passed === false ? ' (failed)' : ''] : h('span', { class: 'muted' }, 'not yet')),
        h('span', null, 'Last restore test: ', ld ? [ld.passed ? 'passed ' : 'failed ', timeEl(ld.created)] : h('span', { class: 'muted' }, 'not yet'))),
      runningLine(s.running, isImp)),
    h('div', { class: 'item-actions' }, runButtons(src.id, s.running, null, false, isImp)));
}

function itemList(sources) {
  return h('div', { class: 'items' }, sources.map(itemCard));
}

function welcomeCard() {
  return h('section', { class: 'card welcome' },
    h('div', { class: 'welcome-ico' }, icon('shieldCheck')),
    h('h2', { class: 'big' }, "Let's protect your first thing"),
    h('p', { class: 'muted' }, 'BackupProof backs up your files, websites and databases, then regularly tests that each backup really restores — so you know it works before you need it.'),
    stepsBar(['Choose what', 'Choose where', 'How often', 'Done'], -1),
    canOperate() ? h('div', { class: 'btns center' },
      h('a', { class: 'btn primary lg', href: '#/protect' }, 'Protect something'),
      h('a', { class: 'btn lg stack', href: '#/import' }, 'Import old backups', h('span', { class: 'btn-sub' }, 'from S3, B2, a disk or another server')))
      : h('p', { class: 'hint' }, 'Ask an administrator to set up the first backup.'));
}

function tile(n, label, cls) {
  return h('div', { class: 'tile ' + (cls || '') }, h('div', { class: 'num' }, String(n)), h('div', { class: 'lbl' }, label));
}

async function pageDashboard() {
  const d = await api('/dashboard');
  const sources = d.sources || [], agents = d.agents || [], alerts = d.alerts || [];
  const c = d.counts || {};
  setAlertCount(alerts);
  every(15000, autoRefresh);
  const head = pageHead('Home', 'Everything you protect, and whether its backups are proven to restore.',
    sources.length && canOperate() ? h('div', { class: 'btns' }, h('a', { class: 'btn primary', href: '#/protect' }, '+ Protect something')) : null);
  if (!sources.length) return h('div', null, head, welcomeCard());

  const total = c.sources ?? sources.length;
  const tiles = h('div', { class: 'tiles' },
    tile(`${c.proven ?? 0}/${total}`, 'restore tested ✓', 'ok'),
    tile(c.atRisk ?? 0, 'need attention or not tested yet', c.atRisk ? 'warn' : ''),
    tile(c.failing ?? 0, (c.failing === 1 ? 'problem' : 'problems'), c.failing ? 'bad' : ''),
    tile(`${c.agentsOnline ?? 0}/${c.agentsTotal ?? agents.length}`, 'servers online', c.agentsTotal && c.agentsOnline < c.agentsTotal ? 'warn' : ''));

  const banner = alerts.length ? h('div', { class: 'banner bad' },
    h('strong', null, `${alerts.length} thing${alerts.length > 1 ? 's need' : ' needs'} your attention`), ' · ', h('a', { href: '#/alerts' }, 'see all alerts'),
    h('ul', null, alerts.slice(0, 5).map((a) => h('li', null, a.message, ' ', h('span', { class: 'small' }, '(', rel(a.created), ')'))))) : null;

  return h('div', null, head, tiles, banner,
    h('section', { class: 'card' }, cardHead('What\'s protected', h('a', { href: '#/protected', class: 'small' }, 'See all')), itemList(sources)));
}

async function pageProtected() {
  const d = await api('/dashboard');
  const sources = d.sources || [];
  every(15000, autoRefresh);
  const actions = canOperate() ? h('div', { class: 'btns' },
    h('a', { class: 'btn', href: '#/import' }, 'Import old backups'),
    h('a', { class: 'btn primary', href: '#/protect' }, '+ Protect something')) : null;
  return h('div', null,
    pageHead('Protected', 'Everything BackupProof backs up and restore-tests.', actions),
    sources.length ? h('section', { class: 'card' }, itemList(sources),
      h('div', { class: 'legend' }, Object.values(STATUS).map((x) => h('span', null, h('span', { class: 'pill ' + x.cls }, x.label), ' ', x.help))))
      : welcomeCard());
}

// ---------------------------------------------------------- item detail

async function pageSource(id) {
  const [st, jobs, proofs] = await Promise.all([
    api(`/sources/${id}`), api(`/jobs?${qs({ source: id, limit: 50 })}`), api(`/proofs?${qs({ source: id, limit: 100 })}`),
  ]);
  const src = st.source, k = kindInfo(src.spec), isImp = src.spec && src.spec.kind === 'import';
  if (S.detailJob && S.detailJob.source !== id) S.detailJob = null;
  const drill = st.lastDrill, dp = pred(drill), backup = st.lastBackup, bp = pred(backup);
  const statusInfo = STATUS[st.status] || { cls: '', label: st.status };
  if (st.running) every(5000, autoRefresh);
  const after = (jid) => { S.detailJob = { source: id, id: jid }; reload(); };

  const head = h('div', { class: 'page-head' },
    h('div', null,
      h('div', { class: 'small' }, h('a', { href: '#/protected' }, '← Protected')),
      h('h1', { class: 'with-ico' }, icon(k.icon), h('span', null, src.name)),
      h('p', { class: 'muted' }, k.label)),
    canOperate() ? h('div', { class: 'btns' },
      h('a', { class: 'btn', href: `#/sources/${id}/edit` }, 'Edit (advanced)'),
      isAdmin() ? btn('Delete', busy(async () => {
        if (!(await confirmDlg(`Stop protecting "${src.name}"? Backup copies already in storage and the proof history are kept.`, 'Stop protecting'))) return;
        await del(`/sources/${id}`); toast('Removed', 'ok'); location.hash = '#/protected';
      }), 'danger') : null) : null);

  const rto = drill ? (drill.rtoMs ?? dp.rtoMs) : null;
  const hero = h('section', { class: 'card hero ' + statusInfo.cls },
    h('div', { class: 'hero-status' }, h('span', { class: 'hero-label' }, statusInfo.label), h('span', { class: 'hero-reason' }, st.reason || statusInfo.help || '')),
    runningLine(st.running, isImp),
    h('dl', { class: 'kv' },
      h('dt', null, isImp ? 'Last conversion' : 'Last backup'),
      h('dd', null, backup ? [timeEl(backup.created), backup.passed === false ? ' — failed' : '', bp.bytes != null ? ` · ${bytes(bp.bytes)}` : '', bp.entries != null ? ` · ${plural(bp.entries, 'file')}` : ''] : 'not yet'),
      h('dt', null, 'Last restore test'),
      h('dd', null, drill ? [drill.passed ? 'passed ' : 'failed ', timeEl(drill.created)] : 'not yet'),
      h('dt', null, 'Time to restore'), h('dd', null, rto != null ? durWords(rto) : '—'),
      h('dt', null, isImp ? 'Converts' : 'Backs up'), h('dd', null, cronWords(src.backupCron), src.nextBackup && src.backupCron !== 'manual' ? [' (next ', timeEl(src.nextBackup), ')'] : ''),
      h('dt', null, 'Tests a restore'), h('dd', null, cronWords(src.drillCron), src.nextDrill ? [' (next ', timeEl(src.nextDrill), ')'] : ''),
      h('dt', null, 'Keeps'), h('dd', null, retentionWords(src.retention)),
      h('dt', null, 'Server'), h('dd', null, st.agentName || '—'),
      h('dt', null, 'Stored in'), h('dd', null, st.repoName || '—'),
      h('dt', null, 'Restore tests run on'), h('dd', null, st.verifierName ? `${st.verifierName} (a different server)` : 'the same server'),
      src.enabled === false ? [h('dt', null, 'Schedule'), h('dd', null, 'Paused — nothing runs automatically')] : null),
    h('div', { class: 'form-actions start' }, runButtons(id, st.running, after, true, isImp)));

  const checks = dp.checks || [];
  const rootsMatch = dp.restoredRoot && dp.restoredRoot === dp.expectedRoot;
  const drillCard = card('What the last restore test checked', drill ? h('div', null,
    h('p', { class: 'muted' }, `The backup copy from ${day(dp.snapshotTime || drill.created)} was restored into a safe, separate place and checked. `,
      drill.passed ? 'Everything passed.' : 'Something did not pass — see below.'),
    checks.length ? h('ul', { class: 'checks' }, checks.map((c) => h('li', null,
      h('span', { class: 'ico ' + (c.passed ? 'ok' : 'bad'), 'aria-label': c.passed ? 'passed' : 'failed' }, c.passed ? '✓' : '✗'),
      h('div', null, h('div', null, checkWords(c.name)), (!c.passed || c.name === 'database-dumps') && c.detail ? h('div', { class: 'detail' }, c.detail) : null),
      h('span', { class: 'muted small' }, c.durationMs ? durWords(c.durationMs) : '')))) : h('p', { class: 'muted' }, 'No checks were recorded.'),
    tech(h('dl', { class: 'kv' },
      h('dt', null, 'Backup copy ID'), h('dd', null, h('code', { class: 'break' }, drill.snapshotId || '—')),
      h('dt', null, 'Restore time (RTO)'), h('dd', null, dur(rto)),
      h('dt', null, 'Test environment'), h('dd', null, dp.sandbox || '—'),
      h('dt', null, 'Tested by'), h('dd', null, dp.verifier || drill.signer || '—'),
      h('dt', null, 'Expected root'), h('dd', null, h('code', { class: 'break' }, dp.expectedRoot || '—')),
      h('dt', null, 'Restored root'), h('dd', null, h('code', { class: 'break' }, dp.restoredRoot || '—'), ' ', rootsMatch ? h('span', { class: 'pill ok' }, '✓ match') : h('span', { class: 'pill bad' }, '✗ differ'))),
    h('h3', null, 'Raw checks'),
    h('ul', { class: 'checks' }, checks.map((c) => h('li', null,
      h('span', { class: 'ico ' + (c.passed ? 'ok' : 'bad') }, c.passed ? '✓' : '✗'),
      h('div', null, h('code', null, c.name), c.detail ? h('div', { class: 'detail' }, c.detail) : null),
      h('span', { class: 'muted small' }, c.durationMs ? dur(c.durationMs) : ''))))))
    : empty('No restore test has run yet. One runs automatically right after the first backup.'));

  const backupCard = card(isImp ? 'Latest conversion' : 'Latest backup', backup ? h('div', null,
    h('dl', { class: 'kv' },
      h('dt', null, 'When'), h('dd', null, timeEl(backup.created), ' ', passPill(backup.passed)),
      h('dt', null, 'Size'), h('dd', null, `${bytes(bp.bytes)} · ${bp.entries != null ? plural(bp.entries, 'file') : '—'}`),
      h('dt', null, 'Took'), h('dd', null, durWords(bp.durationMs)),
      h('dt', null, 'Ransomware protection'), h('dd', null, bp.storage && bp.storage.objectLockMode ? `${bp.storage.objectLockDays} days (${bp.storage.objectLockMode === 'COMPLIANCE' ? 'nobody can delete' : 'admin can lift'})` : 'off')),
    tech(h('dl', { class: 'kv' },
      h('dt', null, 'Backup copy ID'), h('dd', null, h('code', { class: 'break' }, backup.snapshotId || '—')),
      h('dt', null, 'Storage location'), h('dd', null, (bp.storage && bp.storage.location) || '—'),
      bp.sourceMeta ? [h('dt', null, 'Details'), h('dd', null, h('pre', null, JSON.stringify(bp.sourceMeta, null, 2)))] : null)))
    : empty(isImp ? 'Not converted yet. Click “Convert again” to start.' : 'No backup yet. Click “Back up now” to make the first one.'));

  const logBox = h('div');
  const JH = ['What', 'Result', 'Started by', 'When', 'Took', ''];
  const trig = (t) => (t || '').startsWith('manual:') ? 'you (' + t.slice(7) + ')' : t === 'first-backup' ? 'automatically after the first backup' : t === 'schedule' ? 'the schedule' : t || '—';
  const jobsCard = h('section', { class: 'card' }, cardHead('Activity'),
    (jobs || []).length ? table(JH, jobs.map((j) => tr([
      jobWord(j.kind, isImp), h('div', null, statePill(j.state), j.error ? h('div', { class: 'sub break' }, j.error) : null), trig(j.trigger), timeEl(j.created),
      j.started && j.finished ? durWords(toDate(j.finished) - toDate(j.started)) : j.started ? 'running…' : '—',
      btn('Show details', () => { S.detailJob = { source: id, id: j.id }; showJobLog(logBox, id, j.id, isImp); logBox.scrollIntoView({ behavior: 'smooth', block: 'nearest' }); }, 'sm'),
    ], JH))) : empty('Nothing has run yet.'),
    logBox);
  if (S.detailJob) showJobLog(logBox, id, S.detailJob.id, isImp);
  else if (st.running) showJobLog(logBox, id, st.running.id, isImp);

  const PH = ['When', 'What', 'Result', 'Time to restore', ''];
  const proofsCard = h('section', { class: 'card' }, cardHead('Proof history'),
    h('p', { class: 'muted small' }, 'Each backup and restore test leaves a proof: a tamper-proof record that it really happened.'),
    (proofs || []).length ? details(`Show ${plural(proofs.length, 'proof')}`, table(PH, proofs.map((p) => proofRow(p, PH, false)))) : empty('No proofs yet.'));

  return h('div', null, head, hero, h('div', { class: 'grid2' }, drillCard, backupCard), jobsCard, proofsCard);
}

async function showJobLog(box, sourceId, jobId, isImport) {
  const gen = S.gen;
  const tok = {};
  box.pvTok = tok; // a newer viewer in the same box cancels this one
  const pre = h('pre', { class: 'log' }, 'Loading…');
  const title = h('span');
  fill(box, h('h3', null, title), pre);
  let timer = null;
  const tick = async () => {
    if (gen !== S.gen || box.pvTok !== tok || !box.isConnected) { if (timer) clearInterval(timer); return; }
    try {
      const j = await api(`/jobs/${jobId}`);
      fill(title, `${jobWord(j.kind, isImport)} · ${absTime(j.created)} · `, statePill(j.state));
      const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
      pre.textContent = (j.log || '(nothing written yet)') + (j.error ? `\n\nERROR: ${j.error}` : '');
      if (atBottom) pre.scrollTop = pre.scrollHeight;
      const live = j.state === 'queued' || j.state === 'running';
      if (live && !timer) { timer = every(2000, tick); }
      if (!live && timer) {
        clearInterval(timer); timer = null;
        toast(`${jobWord(j.kind, isImport)} ${j.state === 'succeeded' ? 'finished' : 'failed'}`, j.state === 'succeeded' ? 'ok' : 'bad');
        S.detailJob = { source: sourceId, id: jobId };
        reload();
      }
    } catch (err) { pre.textContent = 'Could not load the details: ' + err.message; if (timer) clearInterval(timer); }
  };
  await tick();
}

function proofRow(p, headers, withSource) {
  const rto = p.rtoMs != null ? p.rtoMs : null;
  const cells = [
    timeEl(p.created),
    p.kind === 'drill' ? 'Restore test' : p.kind === 'backup' ? 'Backup' : p.kind,
    passPill(p.passed),
    rto != null ? durWords(rto) : '—',
    h('div', { class: 'btns' }, btn('View', busy(() => viewProof(p.id)), 'sm'),
      h('a', { class: 'btn sm', href: `/api/proofs/${p.id}/bundle`, download: `proof-${p.id}.json` }, 'Download')),
  ];
  if (withSource) cells.splice(1, 0, p.sourceId ? h('a', { href: `#/sources/${p.sourceId}` }, p.sourceName) : p.sourceName || '—');
  return tr(cells, headers);
}

function hostOf(u) { try { return new URL(u).host; } catch { return u || ''; } }

async function viewProof(id) {
  const p = await api(`/proofs/${id}`);
  const env = p.envelope || {};
  const what = p.kind === 'drill' ? 'Restore test' : p.kind === 'backup' ? 'Backup' : p.kind;
  modal(`Proof: ${what}`, h('div', null,
    h('p', { class: 'muted small' }, 'A tamper-proof record that this really happened. Anyone with the public keys can check it, even without this dashboard.'),
    h('dl', { class: 'kv' },
      h('dt', null, 'Item'), h('dd', null, p.sourceName || '—'),
      h('dt', null, 'Result'), h('dd', null, passPill(p.passed)),
      h('dt', null, 'When'), h('dd', null, absTime(p.created)),
      p.rtoMs != null ? [h('dt', null, 'Time to restore'), h('dd', null, durWords(p.rtoMs))] : null,
      h('dt', null, 'Signed by'), h('dd', null, p.signer || '—'),
      h('dt', null, 'Independent timestamp'), h('dd', null, p.timestamp ? `${hostOf(p.timestamp.tsa)} · ${absTime(p.timestamp.time)}` : 'none')),
    tech(h('dl', { class: 'kv' },
      h('dt', null, 'Proof number'), h('dd', null, '#' + p.id),
      h('dt', null, 'Backup copy ID'), h('dd', null, h('code', { class: 'break' }, p.snapshotId || '—')),
      h('dt', null, 'Ledger entry'), h('dd', null, p.ledgerSeq ? '#' + p.ledgerSeq : '—'),
      h('dt', null, 'Signatures'), h('dd', null, (env.signatures || []).map((s) => h('div', null, h('code', { class: 'break' }, s.keyid || '(no key id)'))))),
    h('pre', { class: 'log' }, JSON.stringify(pred(p), null, 2)))),
  [h('a', { class: 'btn primary', href: `/api/proofs/${p.id}/bundle`, download: `proof-${p.id}.json` }, 'Download proof file')]);
}

// ------------------------------------------------------- live progress
// Follows a backup (or conversion) job, then the restore test the server
// starts automatically after the first backup, in plain words.

function jobProgress(sourceId, jobId, name, isImport) {
  const line = h('div', { class: 'progress' });
  const pre = h('pre', { class: 'log' }, '');
  const el = h('div', { class: 'prog' }, h('div', { class: 'prog-name' }, h('a', { href: `#/sources/${sourceId}` }, name)), line, details('Show details', pre));
  const gen = S.gen;
  const started = Date.now();
  let phase = 'backup', cur = jobId, findStart = 0, first = true;
  const logs = {};
  const set = (cls, text, spin, extra) => {
    line.className = 'progress ' + cls;
    fill(line, spin ? h('span', { class: 'spinner' }) : h('span', { class: 'prog-ico' }, cls === 'ok' ? '✓' : cls === 'bad' ? '✗' : '•'), h('span', null, text), extra || null);
  };
  const showLogs = () => { pre.textContent = Object.values(logs).join('\n\n') || '(nothing written yet)'; };
  const doing = isImport ? 'Converting old backups…' : 'Backing up…';
  set('', 'Waiting for the server to start…', true);
  const tick = async () => {
    if (gen !== S.gen || (!first && !el.isConnected)) return;
    first = false;
    try {
      if (phase === 'backup' || phase === 'drill') {
        const j = await api(`/jobs/${cur}`);
        logs[cur] = `--- ${jobWord(j.kind, isImport)} ---\n` + (j.log || '') + (j.error ? `\nERROR: ${j.error}` : '');
        showLogs();
        if (phase === 'backup') {
          if (j.state === 'queued') set('', Date.now() - started > 60000 ? 'Still waiting for the server — is it switched on and online?' : 'Waiting for the server to start…', true);
          else if (j.state === 'running') set('', doing, true);
          else if (j.state === 'failed') { set('bad', (isImport ? 'The conversion didn\'t finish: ' : 'The backup didn\'t finish: ') + (j.error || 'unknown error')); return; }
          else if (j.state === 'succeeded') { phase = 'find'; findStart = Date.now(); set('', isImport ? 'Converted. Starting the restore test…' : 'Backed up. Starting the restore test…', true); }
        } else {
          if (j.state === 'queued' || j.state === 'running') set('', 'Testing the restore…', true);
          else {
            const st = await api(`/sources/${sourceId}`);
            if (j.state === 'succeeded' && st.status === 'proven') { set('ok', 'Protected and restore tested'); return; }
            set('bad', 'The restore test did not pass: ' + (j.error || st.reason || 'see details'), false, h('a', { href: `#/sources/${sourceId}`, class: 'small' }, ' Open'));
            return;
          }
        }
      } else if (phase === 'find') {
        const jobs = (await api(`/jobs?${qs({ source: sourceId, limit: 10 })}`)) || [];
        const d = jobs.find((x) => x.kind === 'drill' && x.id > jobId);
        if (d) { phase = 'drill'; cur = d.id; set('', 'Testing the restore…', true); }
        else if (Date.now() - findStart > 120000) { set('ok', 'Backed up. A restore test will run soon.'); return; }
      }
    } catch (err) { if (err.status === 401) return; }
    setTimeout(tick, 2000);
  };
  setTimeout(tick, 300);
  return el;
}

// ------------------------------------------------------- storage chooser
// "Where should backups be stored?" Used by #/storage/new, inline in the
// protect wizard, and (without password generation) by the import wizard.

const STORE_TYPES = [
  { value: 'local', title: 'A folder or disk on the server', desc: 'An extra hard drive, USB disk or network folder.', icon: 'disk' },
  { value: 'b2', title: 'Backblaze B2', desc: 'Low-cost cloud storage. You need an application key.', icon: 'cloud' },
  { value: 's3', title: 'Amazon S3', desc: 'Amazon Web Services cloud storage.', icon: 'cloud' },
  { value: 'r2', title: 'Cloudflare R2', desc: 'Cloud storage with no download fees.', icon: 'cloud' },
  { value: 'wasabi', title: 'Wasabi', desc: 'Flat-price cloud storage.', icon: 'cloud' },
  { value: 'other', title: 'Other S3-compatible', desc: 'MinIO, DigitalOcean Spaces, Hetzner, Scaleway and others.', icon: 'cloud' },
  { value: 'sftp', title: 'Another server (SFTP)', desc: 'Any Linux server or NAS you can log in to with SSH.', icon: 'server' },
];
const S3_REGIONS = ['us-east-1', 'us-east-2', 'us-west-1', 'us-west-2', 'ca-central-1', 'eu-west-1', 'eu-west-2', 'eu-west-3', 'eu-central-1', 'eu-central-2', 'eu-north-1', 'eu-south-1', 'ap-south-1', 'ap-southeast-1', 'ap-southeast-2', 'ap-northeast-1', 'ap-northeast-2', 'sa-east-1', 'me-south-1', 'af-south-1'];
const WASABI_REGIONS = ['us-east-1', 'us-east-2', 'us-central-1', 'us-west-1', 'ca-central-1', 'eu-central-1', 'eu-central-2', 'eu-west-1', 'eu-west-2', 'ap-northeast-1', 'ap-northeast-2', 'ap-southeast-1', 'ap-southeast-2'];
const isS3Like = (t) => ['b2', 's3', 'r2', 'wasabi', 'other'].includes(t);

function hostOnly(v) { return String(v || '').trim().replace(/^[a-z]+:\/\//i, '').replace(/\/.*$/, ''); }

function joinPath(p, name) {
  p = String(p || '').trim();
  if (!p) return '';
  if (baseName(p).toLowerCase() === name.toLowerCase()) return p;
  const win = /^[A-Za-z]:|\\/.test(p);
  return p.replace(/[\\/]+$/, '') + (win ? '\\' : '/') + name;
}

// uiType works out which card a saved backend config belongs to.
function uiType(b) {
  if (!b || b.type === 'local') return 'local';
  if (b.type === 'sftp') return 'sftp';
  const e = String(b.endpoint || '').toLowerCase();
  if (e.includes('backblazeb2')) return 'b2';
  if (e.includes('r2.cloudflarestorage')) return 'r2';
  if (e.includes('wasabisys')) return 'wasabi';
  return e ? 'other' : 's3';
}

// storageFields builds the connection form. mode: 'repo' (new storage for
// BackupProof backups) or 'import' (where old backups live).
function storageFields({ mode, agent, initial }) {
  const ib = (initial && initial.backend) || {}, ic = (initial && initial.credentials) || {};
  let type = initial ? uiType(ib) : (mode === 'import' ? 'b2' : 'local');
  const listeners = [];
  const changed = () => listeners.forEach((fn) => fn());
  const inp = (attrs) => { const el = input(attrs); el.addEventListener('input', changed); return el; };
  const ta = (attrs, val) => { const el = h('textarea', attrs, val || ''); el.addEventListener('input', changed); return el; };
  const ihost = hostOnly(ib.endpoint);
  const f = {
    path: inp({ value: ib.type === 'local' ? ib.path || '' : '', placeholder: mode === 'import' ? 'e.g. D:\\OldBackups or /mnt/backups' : 'e.g. D:\\ or /mnt/backup-disk' }),
    bucket: inp({ value: ib.bucket || '', autocomplete: 'off' }),
    prefix: inp({ value: initial ? ib.prefix || '' : mode === 'repo' ? 'backupproof' : '', placeholder: mode === 'repo' ? 'backupproof' : 'leave empty if the backups are at the top' }),
    accessKey: inp({ value: ic.accessKey || '', autocomplete: 'off', spellcheck: 'false' }),
    secretKey: inp({ type: 'password', value: ic.secretKey || '', autocomplete: 'new-password' }),
    b2Endpoint: inp({ value: type === 'b2' ? ihost : '', placeholder: 's3.us-west-004.backblazeb2.com' }),
    s3Region: select([...S3_REGIONS.map((r) => [r, r]), ['_other', 'Other…']], type === 's3' ? (S3_REGIONS.includes(ib.region) ? ib.region : ib.region ? '_other' : 'us-east-1') : 'us-east-1'),
    s3RegionOther: inp({ value: type === 's3' && !S3_REGIONS.includes(ib.region) ? ib.region || '' : '', placeholder: 'e.g. ap-east-1' }),
    r2Account: inp({ value: type === 'r2' ? ihost.split('.')[0] : '', autocomplete: 'off', spellcheck: 'false' }),
    wasabiRegion: select(WASABI_REGIONS.map((r) => [r, r]), type === 'wasabi' ? ib.region : 'us-east-1'),
    endpoint: inp({ value: type === 'other' ? ib.endpoint || '' : '', placeholder: 'https://s3.example.com' }),
    region: inp({ value: type === 'other' ? ib.region || '' : '', placeholder: 'us-east-1' }),
    host: inp({ value: ib.host || '', placeholder: 'backup.example.com' }),
    port: inp({ type: 'number', min: 1, max: 65535, value: ib.port || 22 }),
    user: inp({ value: ib.user || '', autocomplete: 'off' }),
    sftpPass: inp({ type: 'password', value: ic.password || '', autocomplete: 'new-password' }),
    privateKey: ta({ rows: 3, placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----', spellcheck: 'false' }, ic.privateKey),
    sftpPath: inp({ value: ib.type === 'sftp' ? ib.path || '' : '', placeholder: mode === 'repo' ? '/home/me/backupproof' : '/backups' }),
    hostKey: ta({ rows: 2, placeholder: 'backup.example.com ssh-ed25519 AAAA…', spellcheck: 'false' }, ib.hostKey),
  };
  [f.s3Region, f.wasabiRegion].forEach((s) => s.addEventListener('change', () => { renderType(); changed(); }));

  const inv = inventory(agent);
  const builtin = !!(agent && agent.builtin);
  const box = h('div', { class: 'type-fields' });
  const where = h('p', { class: 'hint' });
  const updWhere = () => {
    const p = f.path.value.trim();
    where.textContent = mode === 'repo' && p ? `Backups will be saved in: ${joinPath(p, 'BackupProof')}` : '';
  };
  f.path.addEventListener('input', updWhere);

  const browseBtn = (target, start) => builtin
    ? btn('Browse…', async () => { const p = await pickFolder(start ? start() : ''); if (p) { target.value = p; target.dispatchEvent(new Event('input')); renderType(); } }, 'sm')
    : null;

  const keyFields = (idLabel, secretLabel, help) => h('div', { class: 'row' },
    field(idLabel, f.accessKey, help),
    field(secretLabel, f.secretKey, 'Kept encrypted on this server.'));
  const bucketRow = (bucketHelp) => h('div', { class: 'row' },
    field('Bucket name', f.bucket, bucketHelp || 'The bucket you created for backups.'),
    field('Folder inside the bucket', f.prefix, mode === 'repo' ? 'Optional. Keeps BackupProof files tidy in their own folder.' : 'Where the old backups are, e.g. “backups/server1”. For example the folder your backup script uploads to.'));

  const renderType = () => {
    const t = type;
    const drives = (inv.drives || []);
    add(clear(box), [
      t === 'local' ? [
        mode === 'repo' && drives.length ? [h('p', { class: 'label' }, 'Disks on ' + (agent ? agentTitle(agent) : 'the server')),
          choices(drives.map((d) => ({ value: d.path, title: d.label || d.path, desc: d.path, icon: 'disk', extra: d.total ? spaceBar(d.free, d.total) : null })),
            { value: f.path.value.trim(), small: true, onPick: (v) => { f.path.value = v; updWhere(); changed(); } })] : null,
        h('div', { class: 'inline-field' },
          field(mode === 'repo' ? 'Folder or disk' : 'Folder where the old backups are', f.path,
            builtin ? (mode === 'repo' ? 'Pick a disk above, browse, or type a folder path.' : 'Browse or type the folder path.') : 'Type the folder path on that server (for example D:\\Backups or /mnt/backup).'),
          browseBtn(f.path, () => f.path.value.trim())),
        where,
        mode === 'repo' ? h('div', { class: 'banner warn' }, 'Keep at least one copy somewhere else too (another disk or the cloud). A backup on the same disk won\'t help if that disk breaks.') : null,
      ] : null,
      t === 'b2' ? [
        keyFields('Key ID', 'Application Key', 'From Backblaze: App Keys → Add a New Application Key.'),
        bucketRow(),
        field('Endpoint', f.b2Endpoint, 'Shown on the bucket page, like s3.us-west-004.backblazeb2.com'),
      ] : null,
      t === 's3' ? [
        keyFields('Access key', 'Secret key', 'From AWS: IAM → Users → Security credentials.'),
        bucketRow(),
        h('div', { class: 'row' }, field('Region', f.s3Region, 'Where the bucket was created.'), f.s3Region.value === '_other' ? field('Region name', f.s3RegionOther) : null),
      ] : null,
      t === 'r2' ? [
        field('Account ID', f.r2Account, 'Shown on the right side of the Cloudflare R2 overview page.'),
        keyFields('Access key ID', 'Secret access key', 'From R2: Manage R2 API Tokens → Create API token.'),
        bucketRow(),
      ] : null,
      t === 'wasabi' ? [
        keyFields('Access key', 'Secret key', 'From Wasabi: Access Keys → Create new access key.'),
        bucketRow(),
        field('Region', f.wasabiRegion, 'The region shown next to your bucket.'),
      ] : null,
      t === 'other' ? [
        h('div', { class: 'row' }, field('Endpoint (address)', f.endpoint, 'From your provider, e.g. https://fsn1.your-objectstorage.com'), field('Region', f.region, 'Often “us-east-1” if your provider doesn\'t mention one.')),
        keyFields('Access key', 'Secret key'),
        bucketRow(),
      ] : null,
      t === 'sftp' ? [
        h('div', { class: 'row' }, field('Server address', f.host, 'Name or IP address of the server.'), field('Port', f.port, 'Usually 22.'), field('Username', f.user)),
        h('div', { class: 'row' }, field('Password', f.sftpPass, 'Leave empty if you use a private key.'), field('Private key (optional)', f.privateKey, 'Paste the whole key file if you log in with a key.')),
        field(mode === 'repo' ? 'Folder on the server' : 'Folder where the old backups are', f.sftpPath, mode === 'repo' ? 'Created if it doesn\'t exist.' : null),
        field('Server fingerprint', f.hostKey, 'Proves you\'re talking to the right server. Run “ssh-keyscan your-server” on any server and paste one of the lines.'),
      ] : null,
    ]);
    updWhere();
  };

  const typePicker = choices(STORE_TYPES.map((o) => ({ ...o, desc: o.value === 'local' && mode === 'import' ? 'A folder or disk on this server.' : o.desc })),
    { value: type, small: true, onPick: (v) => { type = v; renderType(); changed(); } });
  renderType();

  // collect returns {backend, credentials, label} or {error}.
  const collect = () => {
    const t = type;
    const backend = {}, credentials = {};
    const need = (v, what) => { if (!String(v || '').trim()) throw new Error(`Please fill in: ${what}.`); return String(v).trim(); };
    try {
      if (t === 'local') {
        const p = need(f.path.value, mode === 'repo' ? 'folder or disk' : 'folder');
        Object.assign(backend, { type: 'local', path: mode === 'repo' ? joinPath(p, 'BackupProof') : p });
        const disk = /^([A-Za-z]:[\\/]?|\/)$/.test(p);
        return { backend, credentials, label: disk ? `Disk ${p.replace(/[\\/]+$/, '') || '/'}` : `Folder – ${baseName(p)}` };
      }
      if (isS3Like(t)) {
        Object.assign(credentials, { accessKey: need(f.accessKey.value, 'access key'), secretKey: need(f.secretKey.value, 'secret key') });
        Object.assign(backend, { type: 's3', bucket: need(f.bucket.value, 'bucket name'), prefix: f.prefix.value.trim().replace(/^\/+|\/+$/g, '') });
        let label = '';
        if (t === 'b2') {
          const host = hostOnly(need(f.b2Endpoint.value, 'endpoint'));
          const m = /^s3\.([^.]+)\.backblazeb2\.com$/i.exec(host);
          if (!m) throw new Error('The endpoint should look like s3.us-west-004.backblazeb2.com (copy it from the bucket page).');
          Object.assign(backend, { endpoint: 'https://' + host, region: m[1] });
          label = 'Backblaze B2';
        } else if (t === 's3') {
          backend.region = f.s3Region.value === '_other' ? need(f.s3RegionOther.value, 'region') : f.s3Region.value;
          label = 'Amazon S3';
        } else if (t === 'r2') {
          const acct = hostOnly(need(f.r2Account.value, 'account ID')).split('.')[0];
          Object.assign(backend, { endpoint: `https://${acct}.r2.cloudflarestorage.com`, region: 'auto' });
          label = 'Cloudflare R2';
        } else if (t === 'wasabi') {
          Object.assign(backend, { region: f.wasabiRegion.value, endpoint: `https://s3.${f.wasabiRegion.value}.wasabisys.com` });
          label = 'Wasabi';
        } else {
          let ep = need(f.endpoint.value, 'endpoint');
          if (!/^https?:\/\//i.test(ep)) ep = 'https://' + ep;
          Object.assign(backend, { endpoint: ep.replace(/\/+$/, ''), region: f.region.value.trim() || 'us-east-1' });
          label = 'S3 storage';
        }
        return { backend, credentials, label: `${label} – ${backend.bucket}` };
      }
      if (t === 'sftp') {
        Object.assign(backend, { type: 'sftp', host: need(f.host.value, 'server address'), port: num(f.port.value) || 22, user: need(f.user.value, 'username'), path: need(f.sftpPath.value, 'folder on the server'), hostKey: f.hostKey.value.trim() });
        if (!f.sftpPass.value && !f.privateKey.value.trim()) throw new Error('Enter the password or the private key for the server.');
        Object.assign(credentials, { password: f.sftpPass.value, privateKey: f.privateKey.value.trim() });
        return { backend, credentials, label: `Server ${backend.host}` };
      }
    } catch (e) { return { error: e.message }; }
    return { error: 'Choose a type of storage.' };
  };

  const el = h('div', { class: 'storage-fields' }, typePicker, box);
  return { el, collect, kind: () => type, onChange: (fn) => listeners.push(fn) };
}

const PW_ALPHABET = 'abcdefghjkmnpqrstuvwxyz23456789';
function genPassword() {
  const out = [];
  const buf = new Uint8Array(1);
  while (out.length < 24) {
    crypto.getRandomValues(buf);
    if (buf[0] < 248) out.push(PW_ALPHABET[buf[0] % PW_ALPHABET.length]); // 248 = 8*31, avoids bias
  }
  return out.join('').match(/.{4}/g).join('-');
}

function recoveryKit(name, kindLabel, b, password) {
  const loc = b.type === 'local' ? `Folder: ${b.path}`
    : b.type === 'sftp' ? `Server: ${b.user}@${b.host}:${b.port || 22}\nFolder: ${b.path}`
      : `Bucket: ${b.bucket}\nFolder inside the bucket: ${b.prefix || '(top level)'}\nEndpoint: ${b.endpoint || 'Amazon S3 default'}\nRegion: ${b.region || ''}`;
  const env = b.type === 's3' ? 'AWS_ACCESS_KEY_ID=<your access key> AWS_SECRET_ACCESS_KEY=<your secret key> '
    : b.type === 'sftp' ? 'BP_SFTP_PASSWORD=<server password> ' : '';
  return [
    'BACKUPPROOF RECOVERY KIT',
    '=======================',
    '',
    'Keep this file somewhere safe and separate from the server it protects',
    '(print it, or store it in a password manager). Anyone with this password',
    'and access to the storage can read your backups.',
    '',
    `Storage name: ${name}`,
    `Storage type: ${kindLabel}`,
    loc,
    '',
    `ENCRYPTION PASSWORD: ${password}`,
    '',
    `Created: ${new Date().toLocaleString()}`,
    '',
    'To restore without the dashboard:',
    '1. Download the backupproof program for your system.',
    '2. List the backup copies:',
    `   ${env}BP_PASSWORD='${password}' backupproof snapshots --repo '${repoUrl(b)}'`,
    '3. Restore one of them into an empty folder:',
    `   ${env}BP_PASSWORD='${password}' backupproof restore --repo '${repoUrl(b)}' --target ./restored <BACKUP-COPY-ID>`,
    '',
    'Without this password nobody can restore these backups - not even BackupProof support.',
    '',
  ].join('\n');
}

// storageChooser: full "add storage" form. Calls onSaved(id) after creating it.
function storageChooser({ agent, onSaved, onCancel }) {
  const sf = storageFields({ mode: 'repo', agent });
  const name = input({ required: true });
  let nameTouched = false;
  name.addEventListener('input', () => { nameTouched = true; });
  const autoName = () => { if (!nameTouched) { const c = sf.collect(); name.value = c.label || name.value; } };
  sf.onChange(autoName);

  // ransomware protection
  let lock = '';
  const lockDays = input({ type: 'number', min: 1, value: 30 });
  const lockBox = h('div', null,
    choices([
      { value: '', title: 'Off', desc: 'Backups can be deleted normally.' },
      { value: 'GOVERNANCE', title: 'Protect for some days', desc: 'Nobody can delete or change backups for N days. An admin of the bucket can still lift it.' },
      { value: 'COMPLIANCE', title: 'Locked — nobody can delete', desc: 'Not even you, the bucket owner, or the provider, until the days are over. Use with care.' },
    ], { value: '', small: true, onPick: (v) => { lock = v; lockExtra.classList.toggle('hidden', !v); } }),
    h('div', { class: 'hidden' }));
  const lockExtra = lockBox.lastChild;
  add(lockExtra, [field('Number of days', lockDays, 'Each backup copy is protected for this many days after it is made.'),
    h('p', { class: 'hint' }, 'The bucket must have “Object Lock” turned on when it is created (in Backblaze: “Object Lock: Enable”).')]);
  const lockSection = details('Ransomware protection (optional)', h('p', { class: 'muted small' }, 'Stops anyone — including a virus or a hacker who gets into this server — from deleting your backups.'), lockBox);
  const showLock = () => lockSection.classList.toggle('hidden', !isS3Like(sf.kind()));
  sf.onChange(showLock);

  // encryption password
  let pwMode = 'auto';
  let pw = genPassword();
  const pwShow = h('code', { class: 'pw' }, pw);
  const own1 = input({ type: 'password', autocomplete: 'new-password', minlength: 12 });
  const own2 = input({ type: 'password', autocomplete: 'new-password' });
  const saved = checkbox('I saved the password somewhere safe — without it nobody can restore', false);
  const kitBtn = btn('Download recovery kit', () => {
    const c = sf.collect();
    if (c.error) { toast(c.error, 'bad'); return; }
    const kindLabel = (STORE_TYPES.find((x) => x.value === sf.kind()) || {}).title || '';
    download(`backupproof-recovery-kit-${(name.value || 'storage').replace(/[^\w.-]+/g, '-')}.txt`, recoveryKit(name.value, kindLabel, c.backend, currentPw()));
  }, 'sm');
  const autoBox = h('div', null,
    h('div', { class: 'pwbox' }, pwShow, h('div', { class: 'btns' }, copyBtn(() => pw), btn('Make a new one', () => { pw = genPassword(); pwShow.textContent = pw; }, 'sm'), kitBtn)),
    h('p', { class: 'hint' }, 'Write it down or download the recovery kit and keep it somewhere other than this server.'));
  const ownBox = h('div', { class: 'hidden' }, h('div', { class: 'row' }, field('Password', own1, 'At least 12 characters.'), field('Type it again', own2)));
  const modePick = h('div', { class: 'btns' });
  const setMode = (m) => {
    pwMode = m;
    autoBox.classList.toggle('hidden', m !== 'auto');
    ownBox.classList.toggle('hidden', m !== 'own');
    fill(modePick, m === 'auto' ? btn('Use my own password', () => setMode('own'), 'sm link') : btn('Make one for me', () => setMode('auto'), 'sm link'));
  };
  setMode('auto');
  const currentPw = () => pwMode === 'auto' ? pw : own1.value;

  // test + save
  const result = h('div');
  const backendWithLock = (c) => {
    const b = { ...c.backend };
    if (lock && isS3Like(sf.kind())) { b.objectLockMode = lock; b.objectLockDays = num(lockDays.value) || 30; }
    return b;
  };
  const showTest = (r, c) => {
    fill(result, h('div', { class: 'banner ' + (r.ok ? (r.existing && r.passwordOk !== true ? 'warn' : 'ok') : 'bad') }, r.ok ? '✓ ' : '✗ ', r.message));
    if (r.existing && r.passwordOk !== true) {
      result.append(h('p', { class: 'hint' }, 'This storage already contains BackupProof backups — enter its existing password instead.'));
      if (pwMode !== 'own') setMode('own');
    }
    if (r.oldBackups) {
      result.append(h('div', { class: 'callout' }, h('strong', null, `We found old ${r.oldBackups} backups here.`), ' ',
        'You can convert them so they are restore-tested and proven too. ',
        btn('Convert them', () => {
          S.importPrefill = { format: /restic/i.test(r.oldBackups) ? 'restic' : /kopia/i.test(r.oldBackups) ? 'kopia' : 'files', backend: c.backend, credentials: c.credentials };
          location.hash = '#/import';
        }, 'sm primary')));
    }
  };
  const remoteLocal = () => sf.kind() === 'local' && agent && !agent.builtin;
  const test = async (quiet) => {
    const c = sf.collect();
    if (c.error) { fill(result, h('div', { class: 'banner bad' }, c.error)); return null; }
    if (remoteLocal()) {
      if (!quiet) fill(result, h('div', { class: 'banner info' }, 'This folder is on another server, so it can only be checked when the first backup runs there.'));
      return { ok: true };
    }
    if (!quiet) fill(result, h('p', { class: 'muted' }, 'Checking the connection…'));
    const r = await post('/repositories/test', { backend: backendWithLock(c), credentials: c.credentials, password: pwMode === 'own' ? own1.value : '' });
    showTest(r, c);
    return r;
  };
  const err = h('div', { class: 'form-error' });
  const save = async () => {
    err.textContent = '';
    const c = sf.collect();
    if (c.error) { err.textContent = c.error; return; }
    if (!name.value.trim()) { err.textContent = 'Give this storage a name.'; return; }
    if (pwMode === 'own') {
      if (own1.value.length < 12) { err.textContent = 'The password must be at least 12 characters.'; return; }
      if (own1.value !== own2.value) { err.textContent = 'The two passwords are different.'; return; }
    }
    if (!saved.cb.checked) { err.textContent = 'Please save the password first and tick the box — without it nobody can restore.'; return; }
    const r = await test(true);
    if (!r || !r.ok) { err.textContent = 'The connection test failed — see the message above.'; return; }
    if (r.existing && r.passwordOk !== true) { err.textContent = 'This storage already has backups. Enter its existing password (the test must say it works).'; return; }
    const res = await post('/repositories', { name: name.value.trim(), backend: backendWithLock(c), password: currentPw(), credentials: c.credentials });
    toast('Storage added', 'ok');
    if (onSaved) onSaved(res.id);
  };

  const el = h('div', { class: 'storage-chooser' },
    h('h3', null, 'Where should backups be stored?'),
    sf.el,
    field('Name', name, 'So you can recognise it later.'),
    lockSection,
    h('fieldset', null, h('legend', null, 'Encryption password'),
      h('p', { class: 'muted small' }, 'Everything is encrypted before it leaves the server. The storage provider can\'t read your files — but you need this password to restore if this server is ever lost.'),
      autoBox, ownBox, modePick, saved.el),
    result, err,
    h('div', { class: 'form-actions' },
      onCancel ? btn('Cancel', onCancel) : null,
      btn('Test connection', busy(() => test(false))),
      btn('Save storage', busy(save), 'primary')));
  autoName();
  showLock();
  return el;
}

async function pageStorageNew() {
  if (!canOperate()) return h('div', { class: 'banner warn' }, 'Your account can only view. Ask an administrator to add storage.');
  const agents = ((await api('/agents')) || []).filter((a) => !a.revoked);
  const agent = agents.find((a) => a.builtin) || agents[0] || null;
  return h('div', null,
    h('div', { class: 'small' }, h('a', { href: '#/repositories' }, '← Storage')),
    pageHead('Add backup storage', 'Choose where encrypted backup copies are kept.'),
    h('section', { class: 'card' }, storageChooser({ agent, onSaved: () => { location.hash = '#/repositories'; }, onCancel: () => { location.hash = '#/repositories'; } })));
}

async function pageRepositories() {
  const repos = (await api('/repositories')) || [];
  return h('div', null,
    pageHead('Storage', 'Where your encrypted backup copies are kept.', canOperate() ? h('a', { class: 'btn primary', href: '#/storage/new' }, '+ Add storage') : null),
    repos.length ? h('div', { class: 'cards' }, repos.map((r) => {
      const k = repoKind(r.backend);
      const lw = lockWords(r.backend);
      return h('section', { class: 'card mini' },
        h('div', { class: 'mini-head' }, h('span', { class: 'item-ico' }, icon(k.icon)), h('div', null, h('h2', null, r.name), h('div', { class: 'muted small' }, k.label))),
        h('p', { class: 'break small' }, repoLocation(r.backend)),
        h('div', { class: 'btns' }, lw ? h('span', { class: 'pill ok' }, lw) : h('span', { class: 'pill' }, 'No ransomware protection'), h('span', { class: 'pill' }, 'Encrypted')),
        tech(h('dl', { class: 'kv' },
          h('dt', null, 'Address'), h('dd', null, h('code', { class: 'break' }, repoUrl(r.backend))),
          h('dt', null, 'Storage ID'), h('dd', null, h('code', { class: 'break' }, r.repoId || 'created on first backup')),
          h('dt', null, 'Added'), h('dd', null, absTime(r.created)))));
    })) : h('section', { class: 'card' }, empty('No backup storage yet. Add a disk, a cloud bucket or another server to keep your backups in.',
      canOperate() ? h('a', { class: 'btn primary', href: '#/storage/new' }, 'Add storage') : null)));
}

// ------------------------------------------------------ protect wizard

const BACKUP_CHOICES = [
  { value: '@hourly', title: 'Every hour', desc: 'For things that change all day.' },
  { value: '0 2 * * *', title: 'Every night', desc: 'At 2:00 at night.', badge: 'recommended', badgeCls: 'ok' },
  { value: '@every 6h', title: 'Every 6 hours', desc: 'Four times a day.' },
  { value: '0 3 * * 0', title: 'Every week', desc: 'Sundays at 3:00 at night.' },
  { value: 'custom', title: 'Custom', desc: 'Advanced: your own schedule.' },
];
const DRILL_CHOICES = [
  { value: 'daily', title: 'Every day', desc: 'At 5:00 in the morning.', cron: '0 5 * * *', maxAge: 26, words: 'every day' },
  { value: 'weekly', title: 'Every week', desc: 'Sundays at 4:00 in the morning.', cron: '0 4 * * 0', maxAge: 192, words: 'every week', badge: 'recommended', badgeCls: 'ok' },
  { value: 'monthly', title: 'Every month', desc: 'On the 1st at 4:00 in the morning.', cron: '0 4 1 * *', maxAge: 768, words: 'every month' },
];
const KEEP_CHOICES = [
  { value: '30d', title: 'Last 30 days', desc: 'One copy for each of the last 30 days.', retention: { keepDaily: 30 } },
  { value: 'rec', title: '7 daily + 4 weekly + 12 monthly', desc: 'Goes back a whole year while using little space.', badge: 'recommended', badgeCls: 'ok', retention: { keepDaily: 7, keepWeekly: 4, keepMonthly: 12 } },
  { value: 'all', title: 'Keep everything', desc: 'Never delete old copies. Uses the most space.', retention: {} },
];
const DB_DEFAULT_PORT = { postgres: 5432, mysql: 3306, mongodb: 27017 };
const DB_NAMES = { postgres: 'PostgreSQL', mysql: 'MySQL or MariaDB', mongodb: 'MongoDB', sqlite: 'SQLite file' };

function bind(el, obj, key, after) {
  el.value = obj[key] ?? '';
  el.addEventListener('input', () => { obj[key] = el.value; if (after) after(); });
  return el;
}

function storageStep({ repos, value, adding, agent, onPick, onAdded }) {
  const opts = repos.map((r) => {
    const k = repoKind(r.backend);
    return { value: r.id, title: r.name, desc: `${k.label} · ${repoLocation(r.backend)}`, icon: k.icon, badge: r.backend && r.backend.objectLockMode ? 'Ransomware protection' : null, badgeCls: 'ok' };
  });
  opts.push({ value: 'new', title: 'Add new storage', desc: 'A disk, a cloud bucket (Backblaze B2, S3…) or another server.', icon: 'plus' });
  return h('div', null,
    choices(opts, { value: adding ? 'new' : value, onPick }),
    adding ? h('div', { class: 'card inset' }, storageChooser({ agent, onSaved: onAdded, onCancel: repos.length ? () => onPick(value || repos[0].id) : null })) : null);
}

async function pageProtect() {
  if (!canOperate()) return h('div', { class: 'banner warn' }, 'Your account can only view. Ask an administrator to set up backups.');
  const [agentsAll, repos0] = await Promise.all([api('/agents'), api('/repositories')]);
  const agents = agentOrder((agentsAll || []).filter((a) => !a.revoked));
  let repos = repos0 || [];
  if (!agents.length) {
    return h('div', null, pageHead('Protect something'),
      card(null, empty('No server is connected yet. Connect the server that has the data first.', h('a', { class: 'btn primary', href: '#/agents' }, 'Connect a server'))));
  }
  const W = {
    step: agents.length > 1 ? 0 : 1, agentId: (agents.find((a) => a.builtin) || agents[0]).id,
    what: null, folders: [], newFolder: '', excludes: '',
    site: null, sitePath: '', siteDb: true,
    dbVal: null, db: { host: '', port: '', user: '', password: '', database: '', file: '' },
    name: '', nameTouched: false,
    repoId: repos.length === 1 ? repos[0].id : null, addingRepo: repos.length === 0,
    backup: '0 2 * * *', customCron: '', drill: 'weekly', keep: 'rec', useVerifier: false, verifierId: '',
  };
  const STEPS = ['Server', 'What', 'Where', 'How often', 'Review'];
  const first = agents.length > 1 ? 0 : 1;
  const root = h('div');
  const agent = () => agents.find((a) => a.id === W.agentId) || agents[0];
  const inv = () => inventory(agent());
  const err = h('div', { class: 'form-error' });

  const siteItems = () => (inv().items || []).filter((i) => i.kind === 'website' || i.kind === 'wordpress');
  const siteItem = () => siteItems().find((i) => i.path === W.site) || null;
  const dbOpt = () => {
    if (!W.dbVal) return null;
    const [src, v] = W.dbVal.split(':');
    if (src === 'inv') { const d = (inv().databases || [])[Number(v)]; return d ? { kind: d.kind, inv: d } : null; }
    return { kind: v, manual: true };
  };
  const folderLabel = (p) => ((inv().items || []).find((i) => i.path === p) || {}).label || baseName(p);

  const suggestName = () => {
    if (W.what === 'folders') {
      const l = W.folders.map(folderLabel);
      return l.length > 2 ? `${l[0]}, ${l[1]} and ${l.length - 2} more` : joinWords(l);
    }
    if (W.what === 'website') {
      const it = siteItem();
      if (it) return (it.kind === 'wordpress' ? 'WordPress: ' : 'Website: ') + baseName(it.path);
      return W.sitePath ? 'Website: ' + baseName(W.sitePath) : '';
    }
    if (W.what === 'database') {
      const o = dbOpt();
      if (!o) return '';
      if (o.inv) return o.inv.label;
      if (o.kind === 'sqlite') return W.db.file ? 'Database: ' + baseName(W.db.file) : 'SQLite database';
      return W.db.database ? `Database: ${W.db.database}` : `${DB_NAMES[o.kind]} database`;
    }
    return '';
  };
  const nameInput = input();
  nameInput.addEventListener('input', () => { W.name = nameInput.value; W.nameTouched = true; });
  const autoName = () => { if (!W.nameTouched) { W.name = suggestName(); nameInput.value = W.name; } };

  // buildSources turns the answers into source definitions (website + its
  // WordPress database become two items).
  const buildSources = () => {
    const name = (W.name || '').trim();
    if (!name) throw new Error('Give it a name.');
    if (W.what === 'folders') {
      if (!W.folders.length) throw new Error('Choose at least one folder.');
      return [{ name, spec: { kind: 'files', paths: [...W.folders], excludes: list(W.excludes) } }];
    }
    if (W.what === 'website') {
      const it = siteItem();
      const path = it ? it.path : W.sitePath.trim();
      if (!path) throw new Error('Choose the website, or type its folder.');
      const out = [{ name, spec: { kind: 'files', paths: [path] } }];
      if (it && it.wpConfig && W.siteDb) out.push({ name: `${name} (database)`, spec: { kind: 'mysql', wpConfig: it.wpConfig } });
      return out;
    }
    if (W.what === 'database') {
      const o = dbOpt();
      if (!o) throw new Error('Choose a database.');
      const d = W.db;
      if (o.kind === 'sqlite') {
        if (!d.file.trim()) throw new Error('Type the path of the SQLite database file.');
        return [{ name, spec: { kind: 'sqlite', paths: [d.file.trim()] } }];
      }
      if (o.inv && o.inv.container) {
        const spec = { kind: o.kind, container: o.inv.container };
        if (d.database.trim()) spec.database = d.database.trim();
        return [{ name, spec }];
      }
      if (o.inv && o.inv.wpConfig) return [{ name, spec: { kind: 'mysql', wpConfig: o.inv.wpConfig } }];
      if (!d.host.trim()) throw new Error('Type the database server address.');
      if (o.kind !== 'mongodb' && !d.database.trim()) throw new Error('Type the database name.');
      const spec = { kind: o.kind, host: d.host.trim(), port: num(d.port) || DB_DEFAULT_PORT[o.kind], user: d.user.trim(), database: d.database.trim() };
      return [{ name, spec, secret: d.password ? { password: d.password } : null }];
    }
    throw new Error('Choose what to protect.');
  };

  const stepComputer = () => h('div', null,
    h('h2', null, 'Which server has the data?'),
    choices(agents.map((a) => ({
      value: a.id, title: agentTitle(a), badge: a.builtin ? 'built in' : null, icon: 'computer',
      desc: [a.online ? 'Online' : 'Offline', osName(a.os), a.builtin ? 'the server running this dashboard' : a.hostname].filter(Boolean).join(' · '),
    })), { value: W.agentId, onPick: (v) => { W.agentId = v; W.folders = []; W.site = null; W.dbVal = null; autoName(); } }),
    h('p', { class: 'small' }, h('a', { href: '#/agents', onclick: () => { S.openConnect = true; } }, '+ Connect another server')));

  const foldersForm = () => {
    const sugg = (inv().items || []).filter((i) => i.kind === 'folder');
    const builtin = agent().builtin;
    const listBox = h('div');
    const renderCustom = () => {
      const custom = W.folders.filter((p) => !sugg.some((s) => s.path === p));
      fill(listBox, custom.length ? h('ul', { class: 'path-list' }, custom.map((p) => h('li', null, icon('folder'), h('span', { class: 'break' }, p),
        btn('Remove', () => { W.folders = W.folders.filter((x) => x !== p); renderCustom(); autoName(); }, 'sm')))) : null);
    };
    const addPath = (p) => { p = String(p || '').trim(); if (p && !W.folders.includes(p)) W.folders.push(p); renderCustom(); autoName(); };
    const newIn = input({ placeholder: agent().os && agent().os.startsWith('windows') ? 'C:\\Users\\Me\\Documents' : '/home/me/documents' });
    newIn.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addPath(newIn.value); newIn.value = ''; } });
    const ex = bind(input({ placeholder: '*.tmp, cache' }), W, 'excludes');
    renderCustom();
    return h('div', null,
      sugg.length ? [h('p', { class: 'label' }, 'Folders we found on ' + agentTitle(agent())),
        h('div', { class: 'check-list' }, sugg.map((i) => {
          const cb = h('input', { type: 'checkbox', checked: W.folders.includes(i.path), onchange: () => { if (cb.checked) addPath(i.path); else { W.folders = W.folders.filter((x) => x !== i.path); autoName(); } } });
          return h('label', { class: 'check-card' }, cb, h('span', null, h('strong', null, i.label), h('span', { class: 'hint block break' }, i.path)));
        }))] : h('p', { class: 'hint' }, inv().collected ? 'We didn\'t find the usual folders on this server — add one below.' : 'This server hasn\'t reported its folders yet — add one below.'),
      listBox,
      h('div', { class: 'inline-field' },
        field('Add a folder path', newIn, builtin ? 'Type a path and press Enter, or browse.' : 'Type the folder path on that server and press Enter. (Browsing works only for this server.)'),
        btn('Add', () => { addPath(newIn.value); newIn.value = ''; }, 'sm'),
        builtin ? btn('Browse…', async () => { const p = await pickFolder(''); if (p) addPath(p); }, 'sm') : null),
      details('More options', field('Skip these files', ex, 'Separate with commas. Files and folders matching these names are not backed up, e.g. *.tmp, cache, node_modules.')));
  };

  const websiteForm = () => {
    const items = siteItems();
    const manual = bind(input({ placeholder: '/var/www/mysite' }), W, 'sitePath', () => { W.site = null; autoName(); });
    const it = siteItem();
    const dbCb = checkbox('Also back up its database (found automatically)', W.siteDb, 'The login is read from the WordPress settings file, so you don\'t need to type a password.');
    dbCb.cb.addEventListener('change', () => { W.siteDb = dbCb.cb.checked; });
    return h('div', null,
      items.length ? choices(items.map((i) => ({ value: i.path, title: i.label, desc: i.path, icon: 'globe', badge: i.kind === 'wordpress' ? 'WordPress' : null })),
        { value: W.site, onPick: (v) => { W.site = v; W.sitePath = ''; W.nameTouched = false; autoName(); render(); } })
        : h('p', { class: 'hint' }, 'We didn\'t find any websites on this server. Type the website\'s folder below, or choose “Folders & files”.'),
      it && it.wpConfig ? dbCb.el : null,
      field(items.length ? 'Or type the website\'s folder' : 'Website folder', manual, 'The folder that contains the website files.'));
  };

  const databaseForm = () => {
    const dbs = inv().databases || [];
    const o = dbOpt();
    const opts = dbs.map((d, i) => ({
      value: 'inv:' + i, title: d.label, icon: 'db',
      desc: d.container ? 'We\'ll read the login from the container automatically.' : d.wpConfig ? 'The login is read from the WordPress settings automatically.' : d.needsLogin ? 'You\'ll need its username and password.' : '',
    }));
    const manual = ['postgres', 'mysql', 'mongodb', 'sqlite'].map((k) => ({ value: 'manual:' + k, title: DB_NAMES[k], desc: k === 'sqlite' ? 'A single database file.' : 'Enter the connection details yourself.', icon: 'db' }));
    const pick = (v) => {
      W.dbVal = v;
      const n = dbOpt();
      W.db = { host: (n.inv && n.inv.host) || '127.0.0.1', port: String((n.inv && n.inv.port) || DB_DEFAULT_PORT[n.kind] || ''), user: '', password: '', database: '', file: '' };
      W.nameTouched = false; autoName(); render();
    };
    const d = W.db;
    let fields = null;
    if (o && o.kind === 'sqlite') {
      fields = field('Database file', bind(input({ placeholder: '/var/lib/app/app.db' }), d, 'file', autoName), 'The full path of the .db or .sqlite file.');
    } else if (o && o.inv && o.inv.container) {
      fields = [h('div', { class: 'banner ok' }, 'No password needed: we\'ll read the login from the container automatically.'),
        o.kind !== 'mongodb' ? field('Database name (optional)', bind(input({ placeholder: 'detect automatically' }), d, 'database'), 'Leave empty unless the container has several databases.') : null];
    } else if (o && o.inv && o.inv.wpConfig) {
      fields = h('div', { class: 'banner ok' }, 'No password needed: the login is read from the WordPress settings file.');
    } else if (o) {
      fields = [h('div', { class: 'row' },
        field('Server address', bind(input(), d, 'host'), '127.0.0.1 means the same server.'),
        field('Port', bind(input({ type: 'number', min: 1, max: 65535 }), d, 'port'), 'Usually ' + DB_DEFAULT_PORT[o.kind] + '.')),
      h('div', { class: 'row' },
        field('Username', bind(input({ autocomplete: 'off' }), d, 'user')),
        field('Password', bind(input({ type: 'password', autocomplete: 'new-password' }), d, 'password'), 'Kept encrypted on this server.'),
        field(o.kind === 'mongodb' ? 'Database name (optional)' : 'Database name', bind(input(), d, 'database', autoName), o.kind === 'mongodb' ? 'Leave empty to back up all databases.' : 'The name of the database to back up.'))];
    }
    return h('div', null,
      dbs.length ? [h('p', { class: 'label' }, 'Databases we found'), choices(opts, { value: W.dbVal, onPick: pick })] : h('p', { class: 'hint' }, 'We didn\'t find any databases automatically. Choose the type below.'),
      h('p', { class: 'label' }, dbs.length ? 'Or enter one yourself' : 'Type of database'),
      choices(manual, { value: W.dbVal, onPick: pick, small: true }),
      fields);
  };

  const stepWhat = () => h('div', null,
    h('h2', null, 'What do you want to protect?'),
    choices([
      { value: 'folders', title: 'Folders & files', desc: 'Documents, photos, project folders…', icon: 'folder' },
      { value: 'website', title: 'Website', desc: 'A website\'s files. WordPress sites can include their database.', icon: 'globe' },
      { value: 'database', title: 'Database', desc: 'PostgreSQL, MySQL/MariaDB, MongoDB or SQLite.', icon: 'db' },
      { value: 'other', title: 'Something else (advanced)', desc: 'All settings: commands, hooks, custom checks.', icon: 'sliders' },
    ], { value: W.what, onPick: (v) => { if (v === 'other') { location.hash = '#/sources/new'; return; } W.what = v; W.nameTouched = false; autoName(); render(); } }),
    W.what ? h('div', { class: 'subform' },
      W.what === 'folders' ? foldersForm() : W.what === 'website' ? websiteForm() : databaseForm(),
      field('Name', nameInput, 'How it appears in your list. You can change it.')) : null);

  const stepWhere = () => h('div', null,
    h('h2', null, 'Where should backups be stored?'),
    h('p', { class: 'muted' }, 'Backups are encrypted before they leave the server.'),
    storageStep({
      repos, value: W.repoId, adding: W.addingRepo, agent: agent(),
      onPick: (v) => { if (v === 'new') W.addingRepo = true; else { W.addingRepo = false; W.repoId = v; } render(); },
      onAdded: async (id) => { repos = (await api('/repositories')) || []; W.repoId = id; W.addingRepo = false; render(); },
    }));

  const stepWhen = () => {
    const custom = bind(input({ placeholder: '30 1 * * *' }), W, 'customCron');
    const others = agents.filter((a) => a.id !== W.agentId);
    const ver = checkbox('Test restores on a different server', W.useVerifier, 'Stronger proof: the restore test runs on another server that only has access to the storage.');
    const verSel = select([['', '— choose a server —'], ...others.map((a) => [a.id, agentTitle(a)])], W.verifierId);
    verSel.addEventListener('change', () => { W.verifierId = verSel.value; });
    ver.cb.addEventListener('change', () => { W.useVerifier = ver.cb.checked; render(); });
    return h('div', null,
      h('h2', null, 'How often?'),
      h('h3', null, 'Back up'),
      choices(BACKUP_CHOICES, { value: W.backup, small: true, onPick: (v) => { W.backup = v; render(); } }),
      W.backup === 'custom' ? field('Custom schedule', custom, 'A cron expression like “30 1 * * *” (1:30 every night) or “@every 12h”.') : null,
      h('h3', null, 'Test a restore'),
      h('p', { class: 'hint' }, 'BackupProof restores the latest backup into a safe, separate place and checks every file — so you know it really works.'),
      choices(DRILL_CHOICES, { value: W.drill, small: true, onPick: (v) => { W.drill = v; } }),
      h('h3', null, 'Keep old copies'),
      choices(KEEP_CHOICES, { value: W.keep, small: true, onPick: (v) => { W.keep = v; } }),
      others.length ? h('div', { class: 'subform' }, ver.el, W.useVerifier ? field('Server for restore tests', verSel) : null) : null);
  };

  const plan = () => {
    const a = agent(), repo = repos.find((r) => r.id === W.repoId);
    const dr = DRILL_CHOICES.find((x) => x.value === W.drill), kp = KEEP_CHOICES.find((x) => x.value === W.keep);
    const cron = W.backup === 'custom' ? W.customCron.trim() : W.backup;
    return { a, repo, dr, kp, cron };
  };

  const stepReview = () => {
    let srcs;
    try { srcs = buildSources(); } catch (e) { return h('div', { class: 'banner bad' }, e.message); }
    const { a, repo, dr, kp, cron } = plan();
    const whatWords = W.what === 'folders' ? joinWords(W.folders.map(folderLabel)) : srcs.length > 1 ? `${srcs[0].name} and its database` : srcs[0].name;
    const sentence = `Back up ${whatWords} on ${agentTitle(a)} ${cronWords(cron)} to “${repo.name}” (${repoKind(repo.backend).label}), test a restore ${dr.words}, and keep ${retentionWords(kp.retention)}.`;
    return h('div', null,
      h('h2', null, 'Review & protect'),
      h('p', { class: 'summary' }, sentence),
      W.useVerifier && W.verifierId ? h('p', { class: 'muted' }, `Restore tests run on ${agentTitle(agents.find((x) => String(x.id) === String(W.verifierId)) || {})}.`) : null,
      srcs.length > 1 ? h('p', { class: 'muted' }, `This creates ${srcs.length} protected items: ${srcs.map((s) => s.name).join(' and ')}.`) : null,
      h('p', { class: 'muted small' }, 'The first backup starts right away, followed by a restore test.'));
  };

  const validate = () => {
    if (W.step === 0 && !W.agentId) throw new Error('Choose a server.');
    if (W.step === 1) buildSources();
    if (W.step === 2) {
      if (W.addingRepo) throw new Error('Save the new storage first (or pick an existing one).');
      if (!repos.some((r) => r.id === W.repoId)) throw new Error('Choose where to store the backups.');
    }
    if (W.step === 3) {
      if (W.backup === 'custom' && !W.customCron.trim()) throw new Error('Type the custom schedule, or pick one of the choices.');
      if (W.useVerifier && !W.verifierId) throw new Error('Choose the server for restore tests.');
    }
  };

  const protect = async () => {
    const srcs = buildSources();
    const { a, repo, dr, kp, cron } = plan();
    const created = [];
    for (const s of srcs) {
      const body = {
        name: s.name, agentId: a.id, verifierId: W.useVerifier && W.verifierId ? Number(W.verifierId) : null, repoId: repo.id,
        spec: { ...s.spec, name: s.name }, backupCron: cron, drillCron: dr.cron, retention: { ...kp.retention }, proofMaxAgeHours: dr.maxAge, enabled: true,
      };
      if (s.secret) body.secret = s.secret;
      const r = await post('/sources', body);
      created.push({ id: r.id, name: s.name });
    }
    for (const c of created) {
      try { c.jobId = (await post(`/sources/${c.id}/run`, { kind: 'backup' })).jobId; } catch (e) { c.error = e.message; }
    }
    fill(root, 
      pageHead('Done!', 'Your first backup is running now. You can leave this page — it carries on in the background.'),
      h('section', { class: 'card' },
        stepsBar(['Choose what', 'Choose where', 'How often', 'Done'], 4),
        created.map((c) => c.jobId ? jobProgress(c.id, c.jobId, c.name, false) : h('div', { class: 'banner bad' }, `${c.name}: couldn't start the backup: ${c.error}`)),
        h('div', { class: 'form-actions' },
          h('a', { class: 'btn', href: '#/protect', onclick: () => setTimeout(() => route(), 0) }, 'Protect something else'),
          h('a', { class: 'btn primary', href: `#/sources/${created[0].id}` }, 'Open ' + created[0].name))));
  };

  function render() {
    err.textContent = '';
    const body = [stepComputer, stepWhat, stepWhere, stepWhen, stepReview][W.step]();
    const back = W.step > first ? btn('Back', () => { W.step--; render(); }) : h('a', { class: 'btn', href: '#/dashboard' }, 'Cancel');
    const next = W.step < 4
      ? btn('Next', () => { try { validate(); W.step++; render(); window.scrollTo(0, 0); } catch (e) { err.textContent = e.message; } }, 'primary')
      : btn('Protect now', busy(async () => { try { validate(); } catch (e) { err.textContent = e.message; return; } await protect(); }), 'primary lg');
    fill(root, 
      pageHead('Protect something', 'A few simple questions. You can change everything later.'),
      stepsBar(STEPS.slice(first), W.step - first),
      h('section', { class: 'card wizard' }, body, err, h('div', { class: 'form-actions' }, back, next)));
  }
  render();
  return root;
}

// -------------------------------------------------------- import wizard

const IMPORT_FORMATS = [
  { value: 'files', title: 'Backup files in a bucket or folder', desc: 'Database dumps, .zip/.tar.gz archives, or encrypted files (.gpg, .enc, .age) made by any tool or script.', icon: 'file' },
  { value: 'restic', title: 'restic', desc: 'A restic repository. The restic program must be installed on the server that converts.', icon: 'box' },
  { value: 'kopia', title: 'Kopia', desc: 'A Kopia repository (on S3, B2, a disk or SFTP). The kopia program must be installed on the server that converts.', icon: 'box' },
  { value: 'borg', title: 'BorgBackup', desc: 'A Borg repository, e.g. on another server over SSH, BorgBase or a Hetzner Storage Box. Needs the borg program.', icon: 'box' },
  { value: 'cloud', title: 'Google Drive, Dropbox, OneDrive…', desc: 'Backup files on a cloud drive (70+ services via rclone, including rclone-encrypted folders). Same options as backup files.', icon: 'cloud' },
];
const GROUPING = [
  ['auto', 'Automatically (one backup copy per day if file names contain dates)'],
  ['day', 'One copy per day'], ['folder', 'One copy per folder'], ['file', 'One copy per file'], ['all', 'Everything is one copy'],
];
const DECRYPT = [
  { value: 'auto', title: 'Not encrypted / not sure', desc: 'We detect it automatically.' },
  { value: 'gpg', title: 'GPG', desc: '.gpg, .pgp, .asc — also used by duplicity.' },
  { value: 'openssl', title: 'OpenSSL', desc: '“openssl enc”, files starting with Salted__.' },
  { value: 'age', title: 'age', desc: '.age files.' },
  { value: 'none', title: 'Definitely not encrypted', desc: 'Skip detection.' },
];

async function pageImport() {
  if (!canOperate()) return h('div', { class: 'banner warn' }, 'Your account can only view. Ask an administrator to import old backups.');
  const [agentsAll, repos0] = await Promise.all([api('/agents'), api('/repositories')]);
  const agents = agentOrder((agentsAll || []).filter((a) => !a.revoked));
  let repos = repos0 || [];
  const builtin = agents.find((a) => a.builtin) || agents[0] || null;
  const pre = S.importPrefill;
  S.importPrefill = null;
  const I = {
    step: pre ? 1 : 0, format: (pre && pre.format) || 'files',
    password: '', passwordFile: '', privateKey: '', decrypt: 'auto', unpack: true, grouping: 'auto', opensslIter: '',
    scan: null,
    agentId: builtin ? builtin.id : null, repoId: repos.length === 1 ? repos[0].id : null, addingRepo: repos.length === 0,
    name: '', nameTouched: false, drill: 'weekly', schedule: 'nightly',
    resticMode: pre ? 'manual' : 'local', resticEnvFile: '/etc/restic/env', resticPasswordFile: '/etc/restic/password', resticRepository: '',
    kopiaMode: pre ? 'manual' : 'local', kopiaConfigFile: '/root/.config/kopia/repository.config',
    borgRepository: '', borgSshKeyFile: '',
    cloudRemote: '', rcloneConfig: '',
  };
  const resticLocal = () => I.format === 'restic' && I.resticMode === 'local';
  const kopiaLocal = () => I.format === 'kopia' && I.kopiaMode === 'local';
  const repoTool = () => ['restic', 'kopia', 'borg'].includes(I.format);
  // These are read on the converting server itself, so the dashboard can't list them in advance.
  const noScan = () => resticLocal() || kopiaLocal() || I.format === 'borg' || I.format === 'cloud';
  const TOOL = { restic: 'restic', kopia: 'kopia', borg: 'borg', cloud: 'rclone' };
  const sf = storageFields({ mode: 'import', agent: builtin, initial: pre ? { backend: pre.backend, credentials: pre.credentials } : null });
  sf.onChange(() => { I.scan = null; clear(scanOut); });
  const root = h('div');
  const err = h('div', { class: 'form-error' });
  const scanOut = h('div');
  const STEPS = ['What made them', 'Where they are', 'Where to keep them', 'Convert'];

  const filesOptions = (spec) => {
    Object.assign(spec, { decrypt: I.decrypt, unpack: I.unpack, grouping: I.grouping });
    if (I.privateKey.trim()) spec.privateKey = I.privateKey.trim();
    if (num(I.opensslIter) > 0) spec.opensslIter = num(I.opensslIter);
    return spec;
  };

  const importSpec = () => {
    const base = { storage: {}, credentials: {}, password: I.password };
    if (resticLocal()) {
      if (!I.resticEnvFile.trim() || !I.resticPasswordFile.trim()) throw new Error('Fill in both restic file paths.');
      return { ...base, format: 'restic', password: '', resticEnvFile: I.resticEnvFile.trim(), resticPasswordFile: I.resticPasswordFile.trim() };
    }
    if (kopiaLocal()) {
      if (!I.kopiaConfigFile.trim()) throw new Error('Enter the path of the Kopia settings file.');
      if (!I.password && !I.passwordFile.trim()) throw new Error('Enter the Kopia password, or the path of a file that holds it.');
      const spec = { ...base, format: 'kopia', kopiaConfigFile: I.kopiaConfigFile.trim() };
      if (I.passwordFile.trim()) spec.passwordFile = I.passwordFile.trim();
      return spec;
    }
    if (I.format === 'borg') {
      if (!I.borgRepository.trim()) throw new Error('Enter the Borg repository address.');
      const spec = { ...base, format: 'borg', borgRepository: I.borgRepository.trim() };
      if (I.passwordFile.trim()) spec.passwordFile = I.passwordFile.trim();
      if (I.borgSshKeyFile.trim()) spec.borgSshKeyFile = I.borgSshKeyFile.trim();
      return spec;
    }
    if (I.format === 'cloud') {
      const remote = I.cloudRemote.trim();
      if (!/^:?[^:\s]+:/.test(remote)) throw new Error('Enter the rclone remote and folder, for example “gdrive:Backups”.');
      const storage = { type: 'rclone', remote };
      if (I.rcloneConfig.trim()) storage.rcloneConfig = I.rcloneConfig.trim();
      return filesOptions({ ...base, format: 'files', storage });
    }
    const c = sf.collect();
    if (c.error) throw new Error(c.error);
    const spec = { ...base, format: I.format, storage: c.backend, credentials: c.credentials };
    if (I.format === 'files') filesOptions(spec);
    if (I.format === 'restic' && I.resticRepository.trim()) spec.resticRepository = I.resticRepository.trim();
    return spec;
  };

  const groupsSummary = (r) => {
    const g = r.groups || [];
    const oldest = g.map((x) => toDate(x.oldest)).filter(Boolean).sort((a, b) => a - b)[0];
    const newest = g.map((x) => toDate(x.newest)).filter(Boolean).sort((a, b) => b - a)[0];
    const total = g.reduce((n, x) => n + (x.bytes || 0), 0);
    return `Found ${plural(r.found, 'backup copy').replace('copys', 'copies')}` + (oldest ? ` from ${day(oldest)} to ${day(newest)}` : '') + (total ? ` (${bytes(total)})` : '') + '.';
  };
  const groupLine = (x) => `${x.label} (${x.count}, from ${day(x.oldest)} to ${day(x.newest)}${x.bytes ? ', ' + bytes(x.bytes) : ''})`;

  const showScan = () => {
    const r = I.scan;
    clear(scanOut);
    if (!r) return;
    if (!r.found) {
      scanOut.append(h('div', { class: 'banner warn' }, sf.kind() === 'local'
        ? 'We found no old backups in this folder. Check that the folder path is right and that the backups are inside it.'
        : 'We connected, but found no old backups here. Check the bucket and the “folder inside the bucket”.'));
      return;
    }
    scanOut.append(h('div', { class: 'banner ok' }, '✓ ', groupsSummary(r)),
      details(`Show the list (${r.groups.length})`, h('ul', { class: 'plain-list' }, r.groups.map((x) => h('li', null, groupLine(x))))));
    if (I.format === 'files') scanOut.append(h('p', { class: 'hint' }, 'We only listed the files so far. If a password is wrong, you\'ll see it when the conversion runs.'));
  };

  const scan = async () => {
    err.textContent = '';
    let spec;
    try { spec = importSpec(); } catch (e) { err.textContent = e.message; return; }
    if (repoTool() && !I.password) { err.textContent = `Enter the ${I.format === 'restic' ? 'restic' : 'Kopia'} repository password.`; return; }
    fill(scanOut, h('div', { class: 'running' }, h('span', { class: 'spinner' }), 'Looking for backups… this can take a minute for big buckets.'));
    try {
      I.scan = await post('/import/scan', spec);
      showScan();
      autoName();
    } catch (e) {
      I.scan = null;
      fill(scanOut, h('div', { class: 'banner bad' }, '✗ ', e.message));
    }
  };

  const nameInput = input();
  nameInput.addEventListener('input', () => { I.name = nameInput.value; I.nameTouched = true; });
  function autoName() {
    if (I.nameTouched) return;
    let what = '';
    if (resticLocal()) what = 'restic';
    else if (kopiaLocal()) what = 'Kopia';
    else if (I.format === 'borg') what = baseName(I.borgRepository.replace(/\/+$/, '')) || 'Borg';
    else if (I.format === 'cloud') what = I.cloudRemote.trim();
    else {
      const c = sf.collect();
      if (!c.error) what = c.backend.bucket ? c.backend.bucket + (c.backend.prefix ? '/' + c.backend.prefix : '') : baseName(c.backend.path || c.backend.host || '');
    }
    I.name = 'Old backups: ' + (what || 'imported');
    nameInput.value = I.name;
  }

  const stepFormat = () => h('div', null,
    h('h2', null, 'What made your old backups?'),
    choices(IMPORT_FORMATS, { value: I.format, onPick: (v) => { I.format = v; I.scan = null; } }));

  const pwField = (label, help) => field(label, bind(input({ type: 'password', autocomplete: 'off' }), I, 'password', () => { I.scan = null; }), help);
  const pwFileField = (label, placeholder, help) => field(label, bind(input({ spellcheck: 'false', placeholder }), I, 'passwordFile'), help);
  const toolNote = () => h('p', { class: 'hint' }, `The server that converts needs the ${TOOL[I.format]} program installed (it already is if your backups are made there).`);
  const localOnly = () => h('div', { class: 'banner info' }, 'Nothing secret is sent to the dashboard — the server reads these files itself. In the next step, pick the server that has them.');
  const listLater = () => h('p', { class: 'muted' }, 'We\'ll list the backups when the conversion starts.');

  const filesSecretBlock = () => {
    const key = bind(h('textarea', { rows: 4, spellcheck: 'false', placeholder: '-----BEGIN PGP PRIVATE KEY BLOCK-----  or  AGE-SECRET-KEY-…' }), I, 'privateKey');
    const unpack = checkbox('Open archives (.zip, .tar.gz, .gz) so each file inside is checked', I.unpack);
    unpack.cb.addEventListener('change', () => { I.unpack = unpack.cb.checked; I.scan = null; });
    const grp = select(GROUPING, I.grouping);
    grp.addEventListener('change', () => { I.grouping = grp.value; I.scan = null; clear(scanOut); });
    const iter = bind(input({ type: 'number', min: 1, placeholder: 'e.g. 10000' }), I, 'opensslIter');
    return h('div', null,
      h('h3', null, 'Are the files encrypted?'),
      h('p', { class: 'hint' }, 'We recognise GPG (.gpg/.pgp/.asc, also used by duplicity), OpenSSL (openssl enc, files starting with Salted__) and age (.age). Encryption done by the storage itself needs nothing extra.'),
      choices(DECRYPT, { value: I.decrypt, small: true, onPick: (v) => { I.decrypt = v; } }),
      pwField('Password used to encrypt them (if any)', 'Kept encrypted on this server and only used to read your old backups.'),
      details('I use a key file instead of a password', field('Private key', key, 'Paste your GPG private key (-----BEGIN PGP PRIVATE KEY BLOCK-----) or age key (AGE-SECRET-KEY-…).')),
      unpack.el,
      field('How are your backups organised?', grp, 'Decides how files are grouped into backup copies.'),
      details('Advanced', field('OpenSSL PBKDF2 iterations (only if you used -iter)', iter)));
  };

  const modeChoice = (key, localTitle, localDesc, manualDesc) => choices([
    { value: 'local', title: localTitle, badge: 'recommended', badgeCls: 'ok', desc: localDesc },
    { value: 'manual', title: 'Enter the repository details', desc: manualDesc },
  ], { value: I[key], small: true, onPick: (v) => { I[key] = v; I.scan = null; render(); } });

  const scanButton = () => h('div', null, h('div', { class: 'form-actions start' }, btn('Look for backups', busy(scan), 'primary')), scanOut);

  const stepWhere = () => {
    const head = [h('h2', null, 'Where are they?')];
    switch (I.format) {
      case 'restic':
        head.push(modeChoice('resticMode', 'Use the restic settings already on that server', 'Best if a script already runs restic there (for example with /etc/restic/env).', 'Connect to the bucket or server where the restic repository is.'));
        if (resticLocal()) {
          return h('div', null, head,
            field('restic settings file', bind(input({ spellcheck: 'false' }), I, 'resticEnvFile'), 'The file your backup script loads with “set -a; . /etc/restic/env”; it contains the repository address and storage keys.'),
            field('restic password file', bind(input({ spellcheck: 'false' }), I, 'resticPasswordFile'), 'The file that holds the restic repository password.'),
            localOnly(), toolNote(), listLater());
        }
        return h('div', null, head,
          h('p', { class: 'muted' }, 'Connect to the place where the restic repository is. It is only read, never changed.'),
          sf.el,
          pwField('restic repository password', 'The password you use with restic for this repository.'),
          field('Repository address (optional)', bind(input({ placeholder: 'b2:my-bucket:server1  or  sftp:user@host:/backups' }), I, 'resticRepository'),
            'Only if restic uses a native address like b2:… or sftp:… instead of the storage above.'),
          toolNote(), scanButton());
      case 'kopia':
        head.push(modeChoice('kopiaMode', 'Use the Kopia connection already on that server', 'Best if Kopia already runs there. It reuses Kopia\'s own settings file.', 'Connect to the bucket, folder or SFTP server where the Kopia repository is.'));
        if (kopiaLocal()) {
          return h('div', null, head,
            field('Kopia settings file', bind(input({ spellcheck: 'false' }), I, 'kopiaConfigFile'), 'Usually /root/.config/kopia/repository.config on Linux, or %APPDATA%\\kopia\\repository.config on Windows.'),
            pwField('Kopia repository password', 'Leave empty if you give a password file below.'),
            pwFileField('…or a file that holds the password', '/etc/kopia/password', 'Read on that server; the password is never sent to the dashboard.'),
            localOnly(), toolNote(), listLater());
        }
        return h('div', null, head,
          h('p', { class: 'muted' }, 'Connect to the place where the Kopia repository is. It is opened read-only.'),
          sf.el,
          pwField('Kopia repository password', 'The password you use with Kopia for this repository.'),
          toolNote(), scanButton());
      case 'borg':
        return h('div', null, head,
          field('Borg repository address', bind(input({ spellcheck: 'false', placeholder: 'ssh://u123456@u123456.your-storagebox.de:23/./backups   or   /mnt/backup/borg' }), I, 'borgRepository'),
            'The same address you use with borg (BORG_REPO). For BorgBase it looks like ssh://xxxx@xxxx.repo.borgbase.com/./repo.'),
          pwField('Borg passphrase', 'Leave empty if the repository is not encrypted or you give a passphrase file below.'),
          pwFileField('…or a file that holds the passphrase', '/root/.borg-passphrase', 'Read on that server; the passphrase is never sent to the dashboard.'),
          field('SSH key file (optional)', bind(input({ spellcheck: 'false', placeholder: '/root/.ssh/id_ed25519' }), I, 'borgSshKeyFile'), 'Only needed if the repository is on another server and borg normally uses a specific key.'),
          toolNote(), listLater());
      case 'cloud':
        return h('div', null, head,
          h('div', { class: 'banner info' }, 'Cloud drives are reached through rclone. Set up the drive once on the converting server with “rclone config” (choose Google Drive, Dropbox, OneDrive, …), then enter its name and folder here.'),
          field('rclone remote and folder', bind(input({ spellcheck: 'false', placeholder: 'gdrive:Backups/server1' }), I, 'cloudRemote'),
            'The name you gave the drive in rclone, a colon, then the folder with your backups. rclone-encrypted (crypt) remotes are decrypted automatically.'),
          details('Advanced', field('rclone settings file (optional)', bind(input({ spellcheck: 'false', placeholder: '/root/.config/rclone/rclone.conf' }), I, 'rcloneConfig'), 'Only if rclone\'s settings are not in the usual place.')),
          filesSecretBlock(), toolNote(), listLater());
      default:
        return h('div', null, head,
          h('p', { class: 'muted' }, 'Connect to the place where the old backups are. They are only read, never changed or deleted.'),
          sf.el, filesSecretBlock(), scanButton());
    }
  };

  const stepDest = () => {
    if (noScan() && !I.agentTouched && I.format !== 'cloud') { const other = agents.find((a) => !a.builtin); if (other) I.agentId = other.id; }
    const agent = agents.find((a) => a.id === I.agentId) || builtin;
    const hint = resticLocal() ? `Pick the server where restic runs — the one that has ${I.resticEnvFile} and ${I.resticPasswordFile}.`
      : kopiaLocal() ? `Pick the server where Kopia runs — the one that has ${I.kopiaConfigFile}.`
        : I.format === 'borg' ? 'Pick a server that has borg installed and can reach the repository.'
          : I.format === 'cloud' ? 'Pick the server where you set up the drive with rclone.'
            : repoTool() ? `The ${TOOL[I.format]} program must be installed on that server.` : null;
    return h('div', null,
      h('h2', null, 'Which server does the converting?'),
      h('p', { class: 'muted' }, 'It downloads the old backups, checks them and stores them again, encrypted, in BackupProof format.'),
      choices(agents.map((a) => ({ value: a.id, title: agentTitle(a), badge: a.builtin ? 'built in' : null, icon: 'computer', desc: [a.online ? 'Online' : 'Offline', osName(a.os)].join(' · ') })),
        { value: I.agentId, small: true, onPick: (v) => { I.agentId = v; I.agentTouched = true; } }),
      hint ? h('div', { class: 'banner info' }, hint) : null,
      h('h2', { class: 'mt' }, 'Where should the converted copies be stored?'),
      storageStep({
        repos, value: I.repoId, adding: I.addingRepo, agent,
        onPick: (v) => { if (v === 'new') I.addingRepo = true; else { I.addingRepo = false; I.repoId = v; } render(); },
        onAdded: async (id) => { repos = (await api('/repositories')) || []; I.repoId = id; I.addingRepo = false; render(); },
      }));
  };

  const stepConvert = () => {
    if (!I.name) autoName(); else nameInput.value = I.name;
    const what = I.format === 'borg' ? 'archives' : 'snapshots';
    return h('div', null,
      h('h2', null, 'Convert'),
      field('Name', nameInput, 'How it appears in your list of protected things.'),
      repoTool() ? [h('h3', null, 'Keep converting?'),
        choices([
          { value: 'nightly', title: `Convert new ${what} every night`, desc: 'At 3:00, after your existing backup has run.', badge: 'recommended', badgeCls: 'ok' },
          { value: 'once', title: 'Only once', desc: 'Convert what is there now. You can click “Convert again” later.' },
        ], { value: I.schedule, small: true, onPick: (v) => { I.schedule = v; } }),
        h('p', { class: 'hint' }, `Each night's new ${what.slice(0, -1)} is converted and restore-tested, so you get proof without changing your existing backup.`)] : null,
      h('h3', null, 'Test a restore'),
      choices(DRILL_CHOICES, { value: I.drill, small: true, onPick: (v) => { I.drill = v; } }),
      h('div', { class: 'banner info' }, 'Converted copies keep their original dates. Your old backups are only read, never changed or deleted. Click “Convert again” later to pick up anything new.'));
  };

  const validate = () => {
    if (I.step === 1) {
      importSpec();
      if (!noScan()) {
        if (!I.scan) throw new Error('Click “Look for backups” first.');
        if (!I.scan.found) throw new Error('No old backups were found at this place.');
      }
    }
    if (I.step === 2) {
      if (!I.agentId) throw new Error('Choose a server.');
      if (I.addingRepo) throw new Error('Save the new storage first (or pick an existing one).');
      if (!repos.some((r) => r.id === I.repoId)) throw new Error('Choose where to store the converted copies.');
    }
    if (I.step === 3 && !I.name.trim()) throw new Error('Give it a name.');
  };

  const convert = async () => {
    const spec = importSpec();
    const dr = DRILL_CHOICES.find((x) => x.value === I.drill);
    const imp = { format: spec.format, storage: spec.storage };
    for (const k of ['decrypt', 'unpack', 'grouping', 'opensslIter', 'resticEnvFile', 'resticPasswordFile', 'resticRepository', 'kopiaConfigFile', 'passwordFile', 'borgRepository', 'borgSshKeyFile']) if (spec[k] !== undefined) imp[k] = spec[k];
    const backupCron = repoTool() && I.schedule === 'nightly' ? '0 3 * * *' : 'manual';
    const secret = { importCredentials: spec.credentials };
    if (spec.password) secret.importPassword = spec.password;
    if (spec.privateKey) secret.importPrivateKey = spec.privateKey;
    const name = I.name.trim();
    const r = await post('/sources', {
      name, agentId: I.agentId, verifierId: null, repoId: I.repoId,
      spec: { name, kind: 'import', import: imp }, secret,
      backupCron, drillCron: dr.cron, retention: {}, proofMaxAgeHours: dr.maxAge, enabled: true,
    });
    let job = null, jerr = '';
    try { job = (await post(`/sources/${r.id}/run`, { kind: 'backup' })).jobId; } catch (e) { jerr = e.message; }
    fill(root,
      pageHead('Converting your old backups', 'This can take a while for large backups. You can leave this page — it carries on in the background.'),
      h('section', { class: 'card' },
        job ? jobProgress(r.id, job, name, true) : h('div', { class: 'banner bad' }, 'Couldn\'t start the conversion: ', jerr),
        h('div', { class: 'form-actions' }, h('a', { class: 'btn primary', href: `#/sources/${r.id}` }, 'Open ' + name))));
  };

  function render() {
    err.textContent = '';
    const body = [stepFormat, stepWhere, stepDest, stepConvert][I.step]();
    if (I.step === 1) showScan();
    const back = I.step > 0 ? btn('Back', () => { I.step--; render(); }) : h('a', { class: 'btn', href: '#/dashboard' }, 'Cancel');
    const next = I.step < 3
      ? btn('Next', () => { try { validate(); I.step++; render(); window.scrollTo(0, 0); } catch (e) { err.textContent = e.message; } }, 'primary')
      : btn('Convert', busy(async () => { try { validate(); } catch (e) { err.textContent = e.message; return; } await convert(); }), 'primary lg');
    fill(root,
      pageHead('Bring in your old backups', 'Convert backups made by other tools or scripts so they can be restore-tested and proven. Your old backups are only read, never changed or deleted.'),
      stepsBar(STEPS, I.step),
      h('section', { class: 'card wizard' }, body, err, h('div', { class: 'form-actions' }, back, next)));
  }
  if (!agents.length) {
    return h('div', null, pageHead('Bring in your old backups'), card(null, empty('No server is connected yet.', h('a', { class: 'btn primary', href: '#/agents' }, 'Connect a server'))));
  }
  render();
  return root;
}

// ------------------------------------------------------------- servers

function agentStatus(a) {
  if (a.revoked) return 'Disconnected (access removed)';
  if (a.online) return 'Online';
  return a.lastSeen ? `Offline since ${rel(a.lastSeen)}` : 'Never connected';
}

async function pageAgents() {
  const agents = agentOrder((await api('/agents')) || []);
  if (S.openConnect) { S.openConnect = false; if (canOperate()) setTimeout(() => connectModal(), 0); }
  return h('div', null,
    pageHead('Servers', 'The servers BackupProof backs up and runs restore tests on.', canOperate() ? btn('+ Connect a server', busy(connectModal), 'primary') : null),
    agents.length ? h('div', { class: 'cards' }, agents.map((a) => h('section', { class: 'card mini' + (a.revoked ? ' dim' : '') },
      h('div', { class: 'mini-head' }, h('span', { class: 'item-ico' }, icon('computer')),
        h('div', null, h('h2', null, agentTitle(a), a.builtin ? h('span', { class: 'pill' }, 'built in') : null),
          h('div', { class: 'small' }, h('span', { class: 'dot ' + (a.revoked ? '' : a.online ? 'on' : 'off') }), agentStatus(a)))),
      h('dl', { class: 'kv' },
        h('dt', null, 'System'), h('dd', null, osName(a.os)),
        h('dt', null, 'Can test databases'), h('dd', null, a.docker ? 'Yes' : 'No (needs Docker)'),
        !a.builtin ? [h('dt', null, 'Server name'), h('dd', null, a.hostname || '—')] : null),
      tech(h('dl', { class: 'kv' },
        h('dt', null, 'Name'), h('dd', null, a.name),
        h('dt', null, 'Host name'), h('dd', null, a.hostname || '—'),
        h('dt', null, 'Platform'), h('dd', null, a.os || '—'),
        h('dt', null, 'Version'), h('dd', null, a.version || '—'),
        h('dt', null, 'Key ID'), h('dd', null, h('code', { class: 'break' }, a.keyId || short(a.publicKey, 16))),
        h('dt', null, 'Connected'), h('dd', null, absTime(a.created)))),
      isAdmin() && !a.revoked && !a.builtin ? h('div', { class: 'form-actions' }, btn('Disconnect', busy(async () => {
        if (!(await confirmDlg(`Disconnect "${a.name}"? It immediately loses access and must be connected again with a new code. Its past proofs stay valid.`, 'Disconnect'))) return;
        await post(`/agents/${a.id}/revoke`); toast('Server disconnected', 'ok'); reload();
      }), 'sm danger')) : null)))
      : h('section', { class: 'card' }, empty('No servers connected yet.', canOperate() ? btn('Connect your first server', busy(connectModal), 'primary') : null)));
}

async function connectModal() {
  const before = new Set(((await api('/agents')) || []).map((a) => a.id));
  let st = await api('/status', { noAuthRedirect: true });
  let tok = await post('/agents/enroll-token');
  let tab = /Win/i.test(navigator.userAgent) ? 'windows' : /Mac/i.test(navigator.userAgent) ? 'macos' : 'linux';
  const body = h('div');
  const connected = h('div');
  const TABS = [['linux', 'Linux'], ['windows', 'Windows'], ['macos', 'macOS']];
  const STEP1 = {
    linux: 'Open a terminal on the other server.',
    windows: 'On the other server, open PowerShell as Administrator (right-click the Start button → “Terminal (Admin)” or “Windows PowerShell (Admin)”).',
    macos: 'Open Terminal on the other server (Applications → Utilities → Terminal).',
  };
  const render = () => {
    const cmd = (tok.commands && tok.commands[tab]) || tok.command;
    const urlBox = (() => {
      if (!st.publicUrlIsLocal) return null;
      if (!isAdmin()) return h('div', { class: 'banner warn' }, `Other servers can't reach this dashboard at ${st.publicUrl}. Ask an administrator to set the dashboard address in Settings.`);
      const u = input({ value: /^(localhost|127\.)/.test(location.hostname) ? '' : location.origin, placeholder: 'http://192.168.1.20:8420' });
      return h('div', { class: 'banner warn' },
        h('p', null, `Other servers can't reach this dashboard at ${st.publicUrl}. Enter the address they should use:`),
        h('div', { class: 'inline-field' }, field('Dashboard address', u, 'For example http://192.168.1.20:8420 or https://backup.example.com'),
          btn('Save', busy(async () => {
            await put('/settings/server', { publicUrl: u.value.trim() });
            st = await api('/status', { noAuthRedirect: true });
            S.status = { ...S.status, publicUrl: st.publicUrl, publicUrlIsLocal: st.publicUrlIsLocal };
            tok = await post('/agents/enroll-token');
            toast('Address saved — the command below is updated', 'ok');
            render();
          }), 'sm primary')));
    })();
    fill(body, 
      urlBox,
      h('div', { class: 'tabs', role: 'tablist' }, TABS.map(([k, l]) => h('button', { type: 'button', role: 'tab', class: 'tab' + (k === tab ? ' on' : ''), 'aria-selected': String(k === tab), onclick: () => { tab = k; render(); } }, l))),
      h('ol', { class: 'plain-steps' },
        h('li', null, STEP1[tab]),
        h('li', null, 'Paste this command and press Enter:', h('div', { class: 'copybox' }, h('pre', null, cmd), copyBtn(() => cmd))),
        h('li', null, 'It appears here within a minute.')),
      connected,
      h('p', { class: 'hint' }, `This connection code works once and expires ${tok.expires ? rel(tok.expires) : 'in 1 hour'}.`),
      tech(h('p', { class: 'small' }, 'Connection code:'), h('div', { class: 'copybox' }, h('pre', null, tok.token), copyBtn(() => tok.token)),
        h('p', { class: 'small' }, 'If the program is already installed:'), h('div', { class: 'copybox' }, h('pre', null, tok.command), copyBtn(() => tok.command))));
  };
  render();
  let timer = null;
  modal('Connect a server', body, [], { wide: true, onClose: () => { clearInterval(timer); reload(); } });
  fill(connected, h('div', { class: 'running' }, h('span', { class: 'spinner' }), 'Waiting for the server to connect…'));
  timer = setInterval(async () => {
    if (!body.isConnected) { clearInterval(timer); return; }
    try {
      const fresh = ((await api('/agents')) || []).filter((a) => !before.has(a.id));
      if (fresh.length) fill(connected, h('div', { class: 'banner ok' }, '✓ Connected: ', fresh.map((a) => a.name).join(', ')));
    } catch { /* ignore */ }
  }, 3000);
}

// -------------------------------------------- advanced (technical) form

const KINDS = [['files', 'Folders & files'], ['postgres', 'PostgreSQL'], ['mysql', 'MySQL / MariaDB'], ['mongodb', 'MongoDB'], ['sqlite', 'SQLite file'], ['command', 'Output of a command']];
const BACKUP_PRESETS = [['Every hour', '@hourly'], ['Every 6 hours', '@every 6h'], ['Every night', '0 2 * * *'], ['Every week', '0 3 * * 0']];
const DRILL_PRESETS = [['Every day', '0 5 * * *'], ['Every week', '0 4 * * 0'], ['Every month', '0 4 1 * *']];

async function pageSourceForm(id) {
  if (!canOperate()) return h('div', { class: 'banner warn' }, 'Your account can only view.');
  const [agents, repos, existing] = await Promise.all([
    api('/agents'), api('/repositories'), id ? api(`/sources/${id}`) : Promise.resolve(null),
  ]);
  const src = existing ? existing.source : {
    name: '', spec: { kind: 'files', drill: {} }, backupCron: '0 2 * * *', drillCron: '0 4 * * 0',
    retention: { keepDaily: 7, keepWeekly: 4, keepMonthly: 12, keepLastVerified: 1 }, proofMaxAgeHours: 192, enabled: true,
  };
  const sp = src.spec || {}, dr = sp.drill || {}, ret = src.retention || {};
  const liveAgents = agentOrder((agents || []).filter((a) => !a.revoked));
  const kinds = sp.kind === 'import' ? [...KINDS, ['import', 'Imported backups']] : KINDS;

  const f = {
    name: input({ value: src.name, required: true, disabled: !!id }),
    kind: select(kinds, sp.kind),
    paths: h('textarea', { rows: 3, placeholder: '/var/www\n/etc/nginx' }, (sp.paths || []).join('\n')),
    excludes: h('textarea', { rows: 2, placeholder: '*.tmp\n/var/www/cache' }, (sp.excludes || []).join('\n')),
    sqlitePath: input({ value: (sp.paths || [])[0] || '', placeholder: '/var/lib/app/app.db' }),
    host: input({ value: sp.host || '', placeholder: '127.0.0.1' }),
    port: input({ type: 'number', min: 0, max: 65535, value: sp.port || '' }),
    user: input({ value: sp.user || '' }),
    database: input({ value: sp.database || '' }),
    password: input({ type: 'password', autocomplete: 'new-password', placeholder: id ? 'unchanged — leave empty to keep' : '' }),
    uri: input({ type: 'password', autocomplete: 'off', placeholder: sp.uri === '(redacted)' ? 'saved — leave empty to keep' : 'mongodb://user:pass@host:27017/db' }),
    container: input({ value: sp.container || '', placeholder: 'optional container name' }),
    wpConfig: input({ value: sp.wpConfig || '', placeholder: '/var/www/site/wp-config.php' }),
    globals: checkbox('Also back up users and roles (pg_dumpall --globals-only)', sp.globals),
    command: input({ value: sp.command || '', placeholder: 'e.g. redis-cli --rdb -' }),
    preHook: input({ value: sp.preHook || '' }),
    postHook: input({ value: sp.postHook || '' }),
    agent: select([['', '— choose a server —'], ...liveAgents.map((a) => [a.id, `${agentTitle(a)} (${a.hostname || osName(a.os)})`])], src.agentId || ''),
    verifier: select([['', 'The same server'], ...liveAgents.map((a) => [a.id, `${agentTitle(a)} (${a.hostname || osName(a.os)})`])], src.verifierId || ''),
    repo: select([['', '— choose storage —'], ...(repos || []).map((r) => [r.id, `${r.name} (${repoKind(r.backend).label})`])], src.repoId || ''),
    backupCron: input({ value: src.backupCron, required: true }),
    drillCron: input({ value: src.drillCron, required: true }),
    maxAge: input({ type: 'number', min: 1, value: src.proofMaxAgeHours || 192 }),
    enabled: checkbox('Active (backups and restore tests run on schedule)', src.enabled !== false),
    keepLast: input({ type: 'number', min: 0, value: ret.keepLast || '' }),
    keepDaily: input({ type: 'number', min: 0, value: ret.keepDaily || '' }),
    keepWeekly: input({ type: 'number', min: 0, value: ret.keepWeekly || '' }),
    keepMonthly: input({ type: 'number', min: 0, value: ret.keepMonthly || '' }),
    keepYearly: input({ type: 'number', min: 0, value: ret.keepYearly || '' }),
    keepLastVerified: input({ type: 'number', min: 0, value: ret.keepLastVerified || '' }),
    expectPaths: h('textarea', { rows: 2 }, (dr.expectPaths || []).join('\n')),
    minFiles: input({ type: 'number', min: 0, value: dr.minFiles || '' }),
    image: input({ value: dr.image || '', placeholder: 'default: chosen automatically' }),
    drillCommand: input({ value: dr.command || '', placeholder: 'runs with RESTORE_DIR set; exit 0 = pass' }),
    tolerance: input({ type: 'number', min: 0, max: 1, step: '0.01', value: dr.rowCountTolerance || '', placeholder: '0.2' }),
    testDumps: checkbox('Test database dumps found in the backup (needs Docker)', !dr.skipDumps, 'PostgreSQL dumps inside the backed-up files are loaded into a test database and checked.'),
  };

  const presets = (target, lst) => h('div', { class: 'presets' }, lst.map(([l, v]) => btn(l, () => { target.value = v; target.dispatchEvent(new Event('input')); }, 'sm')));
  const cronHint = (el) => { const s = h('span', { class: 'hint' }); const u = () => { s.textContent = 'Means: ' + cronWords(el.value); }; el.addEventListener('input', u); u(); return s; };

  const assertBox = h('div');
  const addAssert = (a = { name: '', sql: '' }) => {
    const n = input({ value: a.name, placeholder: 'name', class: 'a-name' });
    const q = input({ value: a.sql, placeholder: 'SELECT count(*) > 0 FROM users', class: 'a-sql' });
    const row = h('div', { class: 'assert-row' }, n, q, btn('Remove', () => row.remove(), 'sm danger'));
    assertBox.append(row);
  };
  (dr.assertions || []).forEach(addAssert);

  const isDb = (k) => ['postgres', 'mysql', 'mongodb'].includes(k);
  const kindBox = h('div');
  const assertFs = h('div', null, h('h3', null, 'Your own database checks'),
    h('p', { class: 'hint' }, 'Each SQL query must return a single true/non-zero value on the restored database.'),
    assertBox, btn('+ Add check', () => addAssert(), 'sm'));
  const renderKind = () => {
    const k = f.kind.value;
    const dbRow = h('div', { class: 'row' }, field('Server address', f.host), field('Port', f.port), field('Username', f.user), field('Database name', f.database, k === 'mongodb' ? 'Optional; empty backs up all databases.' : null));
    add(clear(kindBox), [
      k === 'files' ? [field('Folders', f.paths, 'One full folder path per line.'), field('Skip these files', f.excludes, 'One pattern per line, e.g. *.tmp')] : null,
      k === 'postgres' || k === 'mysql' ? [dbRow, h('div', { class: 'row' }, field('Password', f.password), field('Docker container', f.container, 'Run the backup tools inside this container (no password needed).'))] : null,
      k === 'mysql' ? field('WordPress settings file', f.wpConfig, 'Optional: read the login from this wp-config.php automatically.') : null,
      k === 'postgres' ? f.globals.el : null,
      k === 'mongodb' ? [field('Connection address (URI)', f.uri, 'Either an address like this, or fill in the fields below.'), dbRow, h('div', { class: 'row' }, field('Password', f.password), field('Docker container', f.container))] : null,
      k === 'sqlite' ? field('Database file', f.sqlitePath, 'Full path of the SQLite database file.') : null,
      k === 'command' ? field('Command', f.command, 'Whatever it prints is saved as the backup.') : null,
      k === 'import' ? h('div', { class: 'banner info' }, `Imported from ${sp.import ? sp.import.format : 'another tool'} at ${sp.import ? repoUrl(sp.import.storage) : '—'}. To import from a different place, use Import.`) : null,
    ]);
    assertFs.classList.toggle('hidden', !(isDb(k) || k === 'sqlite') || k === 'mongodb');
  };
  f.kind.addEventListener('change', renderKind);

  const err = h('div', { class: 'form-error' });
  const form = h('form', {
    onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      const k = f.kind.value;
      if (!f.agent.value) { err.textContent = 'Choose a server.'; return; }
      if (!f.repo.value) { err.textContent = 'Choose the backup storage.'; return; }
      const spec = { ...sp, name: f.name.value.trim(), kind: k, preHook: f.preHook.value.trim(), postHook: f.postHook.value.trim() };
      for (const key of ['paths', 'excludes', 'host', 'port', 'user', 'database', 'container', 'globals', 'command', 'password', 'uri', 'wpConfig']) delete spec[key];
      if (k !== 'import') delete spec.import;
      if (k === 'files') { spec.paths = lines(f.paths.value); spec.excludes = lines(f.excludes.value); }
      if (k === 'sqlite') spec.paths = [f.sqlitePath.value.trim()].filter(Boolean);
      if (isDb(k)) Object.assign(spec, { host: f.host.value.trim(), port: num(f.port.value), user: f.user.value.trim(), database: f.database.value.trim(), container: f.container.value.trim() });
      if (k === 'mysql' && f.wpConfig.value.trim()) spec.wpConfig = f.wpConfig.value.trim();
      if (k === 'postgres') spec.globals = f.globals.cb.checked;
      if (k === 'command') spec.command = f.command.value.trim();
      const secret = {};
      if (isDb(k) && f.password.value) secret.password = f.password.value;
      if (k === 'mongodb') {
        if (f.uri.value) { secret.uri = f.uri.value; spec.uri = '(redacted)'; }
        else if (sp.uri === '(redacted)') spec.uri = '(redacted)';
      }
      spec.drill = {
        ...dr,
        assertions: [...assertBox.querySelectorAll('.assert-row')].map((r) => ({ name: r.querySelector('.a-name').value.trim(), sql: r.querySelector('.a-sql').value.trim() })).filter((a) => a.sql),
        expectPaths: lines(f.expectPaths.value),
        minFiles: num(f.minFiles.value),
        image: f.image.value.trim(),
        command: f.drillCommand.value.trim(),
        rowCountTolerance: num(f.tolerance.value),
        skipDumps: !f.testDumps.cb.checked,
      };
      const body = {
        ...(id ? { id } : {}),
        name: spec.name,
        agentId: Number(f.agent.value),
        verifierId: f.verifier.value ? Number(f.verifier.value) : null,
        repoId: Number(f.repo.value),
        spec,
        backupCron: f.backupCron.value.trim(),
        drillCron: f.drillCron.value.trim(),
        proofMaxAgeHours: num(f.maxAge.value),
        enabled: f.enabled.cb.checked,
        retention: {
          ...ret,
          keepLast: num(f.keepLast.value), keepDaily: num(f.keepDaily.value), keepWeekly: num(f.keepWeekly.value),
          keepMonthly: num(f.keepMonthly.value), keepYearly: num(f.keepYearly.value), keepLastVerified: num(f.keepLastVerified.value),
        },
        secret,
      };
      const b = form.querySelector('button[type=submit]'); b.disabled = true;
      try {
        if (id) { await put(`/sources/${id}`, body); toast('Saved', 'ok'); location.hash = `#/sources/${id}`; }
        else { const r = await post('/sources', body); toast('Created', 'ok'); location.hash = `#/sources/${r.id}`; }
      } catch (ex) { if (ex.status !== 401) err.textContent = ex.message; b.disabled = false; }
    },
  },
  h('fieldset', null, h('legend', null, 'What\'s protected'),
    h('div', { class: 'row' }, field('Name', f.name, id ? 'The name can\'t be changed (it is part of the stored proofs).' : 'Unique, e.g. “Billing database”.'), field('Type', f.kind)),
    kindBox,
    h('div', { class: 'row' }, field('Run before the backup', f.preHook, 'Optional command, e.g. to pause an app.'), field('Run after the backup', f.postHook, 'Runs even if the backup failed.'))),
  h('fieldset', null, h('legend', null, 'Where'),
    h('div', { class: 'row' }, field('Server', f.agent, 'The server that has the data.'), field('Backup storage', f.repo, 'Where encrypted copies are kept.')),
    field('Test restores on a different server', f.verifier, 'Optional. Stronger proof: restore tests run on another server that only has access to the storage.'),
    !liveAgents.length ? h('p', { class: 'hint' }, 'No servers connected yet. ', h('a', { href: '#/agents' }, 'Connect one')) : null,
    !(repos || []).length ? h('p', { class: 'hint' }, 'No storage yet. ', h('a', { href: '#/storage/new' }, 'Add storage')) : null),
  h('fieldset', null, h('legend', null, 'How often'),
    h('div', { class: 'row' },
      h('div', null, field('Back up', f.backupCron, 'A cron expression, “@every 6h”, or “manual”.'), cronHint(f.backupCron), presets(f.backupCron, BACKUP_PRESETS)),
      h('div', null, field('Test a restore', f.drillCron, 'When to restore into a safe place and check it.'), cronHint(f.drillCron), presets(f.drillCron, DRILL_PRESETS))),
    h('div', { class: 'row' }, field('Warn if no passed restore test for (hours)', f.maxAge, 'After this long, the item shows “Needs attention”.')),
    f.enabled.el),
  h('fieldset', null, h('legend', null, 'Keep old copies'),
    h('div', { class: 'row' }, field('Last', f.keepLast), field('Daily', f.keepDaily), field('Weekly', f.keepWeekly),
      field('Monthly', f.keepMonthly), field('Yearly', f.keepYearly), field('Newest tested', f.keepLastVerified, 'Copies with a passed restore test that are never removed.')),
    h('p', { class: 'hint' }, 'Leave all empty to keep everything.')),
  h('fieldset', null, h('legend', null, 'Restore test settings'),
    h('div', { class: 'row' }, field('Minimum number of files', f.minFiles, 'Fail if fewer files come back.'), field('Allowed row difference', f.tolerance, 'Between 0 and 1 (default 0.2 = 20%).')),
    field('Files that must be present', f.expectPaths, 'One per line.'),
    h('div', { class: 'row' }, field('Test environment image', f.image), field('Your own check command', f.drillCommand)),
    f.testDumps.el,
    assertFs),
  err,
  h('div', { class: 'form-actions' },
    h('a', { class: 'btn', href: id ? `#/sources/${id}` : '#/protect' }, 'Cancel'),
    h('button', { type: 'submit', class: 'btn primary' }, id ? 'Save changes' : 'Create')));
  renderKind();
  return h('div', null,
    h('div', { class: 'small' }, h('a', { href: id ? `#/sources/${id}` : '#/protect' }, '← Back')),
    pageHead(id ? `Edit ${src.name}` : 'Protect something (advanced)', 'All settings. For most things the simple “Protect something” wizard is easier.'),
    h('section', { class: 'card' }, form));
}

// --------------------------------------------------------- proof history

function isoDay(d) { return new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 10); }

async function pageProofs() {
  const LIMIT = 100;
  const [proofs, ledger] = await Promise.all([api('/proofs?limit=100'), api(`/ledger?${qs({ from: S.ledgerFrom, limit: LIMIT })}`)]);
  const PH = ['When', 'Item', 'What', 'Result', 'Time to restore', ''];

  const verifyOut = h('div');
  const verify = busy(async () => {
    const r = await api('/ledger/verify');
    fill(verifyOut, h('div', { class: 'banner ' + (r.ok ? 'ok' : 'bad') },
      h('strong', null, r.ok ? '✓ Nothing has been changed or removed. ' : '✗ The proof history has been tampered with or damaged. '),
      `${plural(r.entries, 'record')} checked.`, r.error ? h('div', null, r.error) : null,
      r.head ? tech(h('code', { class: 'break' }, r.head)) : null));
  });

  const LH = ['#', 'Time', 'Kind', 'Subject', 'Envelope digest', 'Hash'];
  const rows = ledger || [];
  const pager = h('div', { class: 'pager' },
    h('span', { class: 'muted small' }, rows.length ? `#${rows[0].seq} – #${rows[rows.length - 1].seq}` : ''),
    btn('← Earlier', () => { S.ledgerFrom = Math.max(1, S.ledgerFrom - LIMIT); reload(); }, 'sm'),
    btn('Later →', () => { S.ledgerFrom = rows[rows.length - 1].seq + 1; reload(); }, 'sm'));
  pager.children[1].disabled = S.ledgerFrom <= 1;
  pager.children[2].disabled = rows.length < LIMIT;

  const to = new Date(), from = new Date(Date.now() - 90 * 86400000);
  const fFrom = input({ type: 'date', value: isoDay(from) }), fTo = input({ type: 'date', value: isoDay(to) });
  const packLink = h('a', { class: 'btn primary', download: 'backupproof-evidence.json' }, 'Download (for checking by software)');
  const reportLink = h('a', { class: 'btn', target: '_blank', rel: 'noopener' }, 'Open printable report');
  const upd = () => {
    const q = qs({ from: fFrom.value, to: fTo.value });
    packLink.href = '/api/evidence?' + q;
    packLink.setAttribute('download', `backupproof-evidence-${fFrom.value}-to-${fTo.value}.json`);
    reportLink.href = '/api/evidence/report?' + q;
  };
  fFrom.addEventListener('input', upd); fTo.addEventListener('input', upd); upd();

  return h('div', null,
    pageHead('Proof history', 'Every backup and restore test leaves a proof: a tamper-proof record that every backup and restore test really happened.',
      h('a', { class: 'btn', href: '#/keys' }, 'Public keys')),
    h('section', { class: 'card' }, cardHead('Proof report for auditors'),
      h('p', { class: 'muted small' }, 'Everything that happened in a period — for auditors, insurers or compliance reviews.'),
      h('div', { class: 'row' }, field('From', fFrom), field('To', fTo)),
      h('div', { class: 'btns' }, reportLink, packLink)),
    h('section', { class: 'card' }, cardHead('Recent proofs'),
      (proofs || []).length ? table(PH, proofs.map((p) => proofRow(p, PH, true))) : empty('No proofs yet. They appear after the first backup.')),
    h('section', { class: 'card' }, cardHead('Is the history intact?', btn('Check now', verify, 'primary sm')),
      h('p', { class: 'muted small' }, 'Each record is chained to the one before it, so any change or deletion is detected.'),
      verifyOut,
      tech(rows.length ? table(LH, rows.map((e) => tr([
        '#' + e.seq, timeEl(e.time), h('span', { class: 'badge' }, e.kind),
        h('div', { class: 'break' }, e.subject, e.detail ? h('div', { class: 'sub' }, e.detail) : null),
        h('code', { title: e.envelopeDigest || '' }, short(e.envelopeDigest, 12)),
        h('code', { title: `hash ${e.hash}\nprev ${e.prev}` }, short(e.hash, 12)),
      ], LH))) : empty('No records in this range.'), pager)));
}

async function pageKeys() {
  const k = await api('/public/keys');
  const all = [...(k.server ? [{ ...k.server, name: k.server.name || 'server', role: 'this dashboard' }] : []), ...(k.agents || []).map((a) => ({ ...a, role: 'server' }))];
  const H = ['Name', 'Signs as', 'Key ID', 'Public key (Ed25519)'];
  return h('div', null,
    h('div', { class: 'small' }, h('a', { href: '#/proofs' }, '← Proof history')),
    pageHead('Public keys', 'Auditors use these to check proofs on their own, without trusting this server. The list is also public at /api/public/keys.'),
    h('section', { class: 'card' }, table(H, all.map((x) => tr([x.name, x.role, h('code', { class: 'break' }, x.keyid || '—'), h('code', { class: 'break small' }, String(x.public || ''))], H)))),
    k.bpkeys ? h('section', { class: 'card' }, cardHead('Keys file', copyBtn(() => k.bpkeys)), h('pre', null, k.bpkeys)) : null);
}

// ---------------------------------------------------------------- alerts

async function pageAlerts() {
  const [open, all] = await Promise.all([api('/alerts?open=1'), api('/alerts?open=0')]);
  setAlertCount(open || []);
  const H = ['When', 'What', 'Message', 'About', 'Status'];
  const row = (a) => tr([
    timeEl(a.created), ALERT_WORDS[a.kind] || String(a.kind || '').replace(/-/g, ' '), h('span', { class: 'break' }, a.message),
    a.sourceId ? h('a', { href: `#/sources/${a.sourceId}` }, 'Open item') : a.agentId ? h('a', { href: '#/agents' }, 'Servers') : '—',
    a.resolved ? h('span', null, 'Fixed ', timeEl(a.resolved)) : h('span', { class: 'pill bad' }, 'Open'),
  ], H);
  const history = (all || []).filter((a) => a.resolved);
  return h('div', null,
    pageHead('Alerts', 'Alerts go away by themselves once the problem is fixed.'),
    h('section', { class: 'card' }, cardHead(`Open (${(open || []).length})`), (open || []).length ? table(H, open.map(row)) : empty('All good — nothing needs your attention.')),
    h('section', { class: 'card' }, cardHead('Earlier'), history.length ? table(H, history.map(row)) : empty('No earlier alerts.')));
}

// -------------------------------------------------------------- settings

async function pageSettings() {
  const parts = [pageHead('Settings')];
  if (isAdmin()) {
    const [notify, tsa, users, st] = await Promise.all([api('/settings/notify'), api('/settings/tsa'), api('/users'), api('/status', { noAuthRedirect: true })]);
    parts.push(serverCard(st || {}), notifyCard(notify || {}), usersCard(users || []), tsaCard(tsa || { urls: [] }));
  }
  parts.push(passwordCard());
  return h('div', null, parts);
}

function serverCard(st) {
  const u = input({ type: 'url', value: st.publicUrl || '', placeholder: 'https://backup.example.com' });
  return card('Dashboard address',
    st.publicUrlIsLocal ? h('div', { class: 'banner warn' }, 'Other servers can\'t reach “localhost”. Set the address they should use, or connecting servers won\'t work.') : null,
    field('Address other servers use to reach this dashboard', u, 'For example https://backup.example.com or http://192.168.1.20:8420. Used in the “Connect a server” commands.'),
    h('div', { class: 'form-actions' }, btn('Save', busy(async () => {
      const r = await put('/settings/server', { publicUrl: u.value.trim() });
      if (r && r.publicUrl) u.value = r.publicUrl;
      toast('Address saved', 'ok');
    }), 'primary')));
}

function notifyCard(n) {
  const f = {
    webhookUrl: input({ type: 'url', value: n.webhookUrl || '', placeholder: 'https://hooks.slack.com/…' }),
    smtpHost: input({ value: n.smtpHost || '' }), smtpPort: input({ type: 'number', value: n.smtpPort || 587 }),
    smtpUser: input({ value: n.smtpUser || '', autocomplete: 'off' }),
    smtpPass: input({ type: 'password', autocomplete: 'new-password', placeholder: 'unchanged — leave empty to keep' }),
    from: input({ value: n.from || '', placeholder: 'backupproof@example.com' }), to: input({ value: n.to || '', placeholder: 'me@example.com' }),
    heartbeatUrl: input({ type: 'url', value: n.heartbeatUrl || '', placeholder: 'https://hc-ping.com/…' }),
  };
  const form = h('form', {
    onsubmit: busy(async (e) => {
      e.preventDefault();
      await put('/settings/notify', {
        webhookUrl: f.webhookUrl.value.trim(), smtpHost: f.smtpHost.value.trim(), smtpPort: num(f.smtpPort.value), smtpUser: f.smtpUser.value.trim(),
        smtpPass: f.smtpPass.value, from: f.from.value.trim(), to: f.to.value.trim(), heartbeatUrl: f.heartbeatUrl.value.trim(),
      });
      f.smtpPass.value = '';
      toast('Saved', 'ok');
    }),
  },
  h('p', { class: 'muted small' }, 'Get told when a backup or restore test fails.'),
  h('h3', null, 'By email'),
  h('div', { class: 'row' }, field('To', f.to, 'Your email address (several: separate with commas).'), field('From', f.from)),
  h('div', { class: 'row' }, field('Mail server', f.smtpHost, 'From your email provider, e.g. smtp.gmail.com'), field('Port', f.smtpPort, 'Usually 587.'), field('Username', f.smtpUser), field('Password', f.smtpPass)),
  h('h3', null, 'By chat'),
  field('Webhook address', f.webhookUrl, 'Slack, Microsoft Teams, ntfy, Discord… paste the incoming-webhook URL.'),
  details('Advanced', field('Heartbeat address', f.heartbeatUrl, 'Pinged regularly while BackupProof is healthy, so a service like healthchecks.io can tell you if BackupProof itself stops.')),
  h('div', { class: 'form-actions' },
    btn('Send a test', busy(async () => { const r = await post('/settings/notify/test'); toast(r && r.ok === false ? 'The test failed' : 'Test sent — check your inbox or chat', r && r.ok === false ? 'bad' : 'ok'); })),
    h('button', { type: 'submit', class: 'btn primary' }, 'Save')),
  h('p', { class: 'hint' }, 'Save before sending a test.'));
  return card('Alerts by email or chat', form);
}

function tsaCard(t) {
  const ta = h('textarea', { rows: 3, placeholder: 'https://freetsa.org/tsr' }, (t.urls || []).join('\n'));
  return card('Independent timestamps',
    h('p', { class: 'muted small' }, 'An outside timestamp service confirms when each proof was made, so nobody can back-date it — not even this server. One address per line; leave empty to turn off.'),
    details('Show timestamp services', field('Timestamp service addresses (RFC 3161)', ta),
      h('div', { class: 'form-actions' }, btn('Save', busy(async () => { await put('/settings/tsa', { urls: lines(ta.value) }); toast('Saved', 'ok'); }), 'primary'))));
}

function usersCard(users) {
  const H = ['Username', 'Can', 'Added', ''];
  const ROLE_WORDS = { admin: 'everything (administrator)', operator: 'set up and run backups', auditor: 'only view and download proofs' };
  const u = input({ autocomplete: 'off' });
  const p = input({ type: 'password', autocomplete: 'new-password', minlength: 10 });
  const r = select(Object.entries(ROLE_WORDS).reverse().map(([k, v]) => [k, v[0].toUpperCase() + v.slice(1)]), 'operator');
  const form = h('form', {
    onsubmit: busy(async (e) => {
      e.preventDefault();
      if (p.value.length < 10) { toast('The password must be at least 10 characters.', 'bad'); return; }
      await post('/users', { username: u.value.trim(), password: p.value, role: r.value });
      toast('Person added', 'ok'); reload();
    }),
  }, h('h3', null, 'Add a person'), h('div', { class: 'row' }, field('Username', u), field('Password', p, 'At least 10 characters.'), field('They can', r)),
  h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn primary' }, 'Add')));
  return card('People who can sign in',
    table(H, users.map((x) => tr([x.username, ROLE_WORDS[x.role] || x.role, timeEl(x.created),
      x.id === S.user.id ? h('span', { class: 'muted small' }, 'you') : btn('Remove', busy(async () => {
        if (!(await confirmDlg(`Remove "${x.username}"? They will no longer be able to sign in.`, 'Remove'))) return;
        await del(`/users/${x.id}`); toast('Removed', 'ok'); reload();
      }), 'sm danger')], H))),
    form);
}

function passwordCard() {
  const p1 = input({ type: 'password', autocomplete: 'new-password', minlength: 10 });
  const p2 = input({ type: 'password', autocomplete: 'new-password' });
  const form = h('form', {
    onsubmit: busy(async (e) => {
      e.preventDefault();
      if (p1.value.length < 10) { toast('The password must be at least 10 characters.', 'bad'); return; }
      if (p1.value !== p2.value) { toast('The two passwords are different.', 'bad'); return; }
      await post('/users/password', { password: p1.value });
      p1.value = p2.value = '';
      toast('Password changed', 'ok');
    }),
  }, h('div', { class: 'row' }, field('New password', p1, 'At least 10 characters.'), field('Type it again', p2)),
  h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn primary' }, 'Change password')));
  return card(`Your sign-in password (${S.user.username})`, form);
}

// ------------------------------------------------------------------ boot

async function boot() {
  try {
    S.status = await api('/status', { noAuthRedirect: true });
  } catch (err) {
    fill(document.getElementById('app'), h('div', { class: 'auth' }, h('div', { class: 'banner bad' }, 'Can\'t reach the BackupProof server: ', err.message)));
    return;
  }
  S.csrf = S.status.csrf || '';
  S.user = S.status.user || null;
  if (!S.user) renderAuth();
  else renderShell();
}

window.addEventListener('hashchange', () => route());
boot();
