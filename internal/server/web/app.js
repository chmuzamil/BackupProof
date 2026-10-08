// BackupProof dashboard. Vanilla ES module, no build step, CSP-safe:
// every DOM node is created through h() and all server text goes through
// textContent, never innerHTML. Visible wording is written for people who
// are not backup experts (see VOCABULARY notes next to each page).

'use strict';

const S = {
  status: null, user: null, csrf: '', gen: 0, timers: [], openAlerts: 0, detailJob: null, importPrefill: null, openConnect: false,
  uid: 0, dirty: false, curHash: '', stepHandler: null,
};

// ---------------------------------------------------------------- helpers

function h(tag, attrs, ...kids) {
  const el = document.createElement(tag);
  if (tag === 'code' || tag === 'pre') el.setAttribute('translate', 'no');
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

const uid = (p = 'u') => `${p}-${++S.uid}`;
const srOnly = (text) => h('span', { class: 'sr-only' }, text);
// mark shows ✓/✗ to the eye and "passed"/"failed" to screen readers.
const mark = (ok, words = ok ? 'passed' : 'failed') => [h('span', { 'aria-hidden': 'true' }, ok ? '✓' : '✗'), srOnly(words)];
// glyph is a decorative ✓/✗ prefix (the words next to it carry the meaning).
const glyph = (ok) => h('span', { class: 'glyph', 'aria-hidden': 'true' }, ok ? '✓ ' : '✗ ');
// glyphLabel turns "Restore tested ✓" into text plus a hidden check mark.
function glyphLabel(label) {
  const m = /^(.*?)\s*([✓✗])$/.exec(label || '');
  return m ? [m[1], h('span', { 'aria-hidden': 'true' }, ' ' + m[2])] : label;
}
const FOCUSABLE = 'a[href], button:not([disabled]), input:not([disabled]):not([type=hidden]), select:not([disabled]), textarea:not([disabled]), summary, [tabindex]:not([tabindex="-1"])';

// Errors go to the assertive region so screen readers announce them at once.
function toast(msg, kind = '') {
  const box = document.getElementById(kind === 'bad' ? 'toasts-alert' : 'toasts') || document.getElementById('toasts');
  const t = h('div', { class: 'toast ' + kind }, msg);
  box.append(t);
  setTimeout(() => t.remove(), kind === 'bad' ? 9000 : 4500);
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
  let r;
  try { r = await fetch('/api' + path, init); }
  catch { throw new ApiError('Can’t reach the BackupProof server. Check that it is running and your network is connected, then try again.', 0); }
  const txt = await r.text();
  let data = null;
  try { data = txt ? JSON.parse(txt) : null; } catch { data = txt; }
  if (r.status === 401 && !opts.noAuthRedirect) {
    S.user = null;
    renderAuth();
    throw new ApiError('Your session has expired. Please sign in again.', 401);
  }
  if (!r.ok) {
    throw new ApiError((data && data.error) || (r.status >= 500
      ? `The server ran into a problem (error ${r.status}). Try again in a moment; if it keeps happening, check the BackupProof server log.`
      : `The request was refused (error ${r.status} ${r.statusText}). Reload the page and try again.`), r.status);
  }
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

const NUM = new Intl.NumberFormat();
const nf = (n) => NUM.format(n);
const plural = (n, unit) => `${nf(n)} ${unit}${n === 1 ? '' : 's'}`;
const RTF = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
const LIST_AND = new Intl.ListFormat(undefined, { style: 'long', type: 'conjunction' });
const LIST_UNIT = new Intl.ListFormat(undefined, { style: 'long', type: 'unit' });
const CLOCK = new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' });
// clock(2, 0) → "2:00 AM" (or "02:00" where people use a 24-hour clock).
const clock = (hr, min = 0) => CLOCK.format(new Date(2000, 0, 2, hr, min));
const unitFmt = {};
function unitNum(n, unit, display = 'long') {
  const k = unit + display;
  unitFmt[k] = unitFmt[k] || new Intl.NumberFormat(undefined, { style: 'unit', unit, unitDisplay: display, maximumFractionDigits: 1 });
  return unitFmt[k].format(n);
}

// rel renders a time the way people say it: "3 hours ago", "in 5 minutes".
function rel(t) {
  const d = toDate(t);
  if (!d) return '—';
  const s = Math.round((d - Date.now()) / 1000);
  const a = Math.abs(s);
  if (a < 45) return s < 0 ? 'just now' : 'in a moment';
  const units = [[60, 'minute', 3600], [3600, 'hour', 86400 * 2], [86400, 'day', 86400 * 60], [86400 * 30, 'month', 86400 * 730], [86400 * 365, 'year', Infinity]];
  for (const [size, name, max] of units) {
    if (a < max) return RTF.format(Math.sign(s) * Math.max(1, Math.round(a / size)), name);
  }
  return '—';
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
  const digits = i === 0 || v >= 10 ? 0 : 1;
  return new Intl.NumberFormat(undefined, { maximumFractionDigits: digits }).format(v) + ' ' + u[i];
}

// dur is the compact technical form ("3m 5s"); durWords is for plain summaries.
function dur(ms) {
  if (ms == null || isNaN(ms)) return '—';
  ms = Number(ms);
  const n = (v, u) => unitNum(v, u, 'narrow');
  if (ms < 1000) return n(Math.round(ms), 'millisecond');
  const s = ms / 1000;
  if (s < 60) return n(s < 10 ? Math.round(s * 10) / 10 : Math.round(s), 'second');
  const m = Math.floor(s / 60);
  if (m < 60) return `${n(m, 'minute')} ${n(Math.round(s % 60), 'second')}`;
  const hr = Math.floor(m / 60);
  if (hr < 48) return `${n(hr, 'hour')} ${n(m % 60, 'minute')}`;
  return `${n(Math.floor(hr / 24), 'day')} ${n(hr % 24, 'hour')}`;
}

function durWords(ms) {
  if (ms == null || isNaN(ms)) return '—';
  const s = Math.round(Number(ms) / 1000);
  if (s < 1) return 'less than a second';
  if (s < 60) return unitNum(s, 'second');
  const m = Math.floor(s / 60);
  if (m < 60) return LIST_UNIT.format([unitNum(m, 'minute'), s % 60 ? unitNum(s % 60, 'second') : null].filter(Boolean));
  const hr = Math.floor(m / 60);
  return LIST_UNIT.format([unitNum(hr, 'hour'), m % 60 ? unitNum(m % 60, 'minute') : null].filter(Boolean));
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
function joinWords(arr) { return LIST_AND.format(arr.map(String)); }
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
  sun: ['M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8z', 'M12 2v2', 'M12 20v2', 'M4.9 4.9l1.4 1.4', 'M17.7 17.7l1.4 1.4', 'M2 12h2', 'M20 12h2', 'M4.9 19.1l1.4-1.4', 'M17.7 6.3l1.4-1.4'],
  moon: ['M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z'],
  auto: ['M12 3a9 9 0 1 0 0 18z', 'M12 3a9 9 0 0 1 0 18'],
  chevron: ['M6 9l6 6 6-6'],
  list: ['M9 6h11', 'M9 12h11', 'M9 18h11', 'M4.5 6h.01', 'M4.5 12h.01', 'M4.5 18h.01'],
  info: ['M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18z', 'M12 11v5', 'M12 8h.01'],
  menu: ['M4 7h16', 'M4 12h16', 'M4 17h16'],
  close: ['M6 6l12 12', 'M18 6L6 18'],
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

const JOB_WORDS = { backup: 'Backup', drill: 'Restore test', check: 'Storage health check', maintain: 'Cleanup', copy: 'Copy to second storage', restore: 'Restore', 'restore-db': 'Database restore' };
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
  if (k === 'docker') return { label: spec.docker && spec.docker.project ? 'Docker app' : spec.docker && (spec.docker.containers || []).length ? 'Docker container' : 'Docker volumes', icon: 'box' };
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
  '0 2 * * *': `every night at ${clock(2)}`,
  '@every 6h': 'every 6 hours',
  '0 3 * * 0': `every Sunday at ${clock(3)}`,
  '0 5 * * *': `every day at ${clock(5)}`,
  '0 4 * * *': `every day at ${clock(4)}`,
  '0 4 * * 0': `every Sunday at ${clock(4)}`,
  '0 4 1 * *': `on the 1st of every month at ${clock(4)}`,
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
  if (m && Number(m[2]) < 24 && Number(m[1]) < 60) return `every day at ${clock(Number(m[2]), Number(m[1]))}`;
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
  const dd = /^database-dump:\s*(.*)$/.exec(name || '');
  if (dd) return `Loaded ${dd[1]} into a test database`;
  const di = /^database-dump-integrity:\s*(.*)$/.exec(name || '');
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

// statusPill is a real button that shows what the status means in a small
// note right below it (instead of a tooltip that disappears).
function statusPill(status, reason) {
  const s = STATUS[status] || { cls: '', label: status || 'Unknown', help: '' };
  const text = reason ? s.help || reason : s.help;
  if (!text) return h('span', { class: 'pill ' + s.cls }, glyphLabel(s.label));
  const id = uid('pill');
  const note = h('span', { class: 'pill-note', id, hidden: true }, text);
  const b = h('button', {
    type: 'button', class: 'pill pill-btn ' + s.cls, 'aria-expanded': 'false', 'aria-controls': id,
    onclick: (e) => { e.preventDefault(); const open = note.hidden; note.hidden = !open; b.setAttribute('aria-expanded', String(open)); },
    onkeydown: (e) => { if (e.key === 'Escape' && !note.hidden) { e.stopPropagation(); note.hidden = true; b.setAttribute('aria-expanded', 'false'); } },
  }, glyphLabel(s.label), icon('info', 'pill-i'), srOnly(' – what does this mean?'));
  return [b, note];
}

function statePill(state) {
  const cls = { succeeded: 'ok', failed: 'bad', running: 'warn', queued: '' }[state] ?? '';
  return h('span', { class: 'pill ' + cls }, glyphLabel(STATE_WORDS[state] || state || '—'));
}

function passPill(passed) { return h('span', { class: 'pill ' + (passed ? 'ok' : 'bad') }, passed ? 'Passed' : 'Failed'); }

function btn(label, onclick, cls = '', attrs = {}) { return h('button', { type: 'button', class: 'btn ' + cls, onclick, ...attrs }, label); }

// busy runs an async action. On a button it disables that button; used as a
// form's onsubmit it disables the form's submit button. While it runs the
// button shows a spinner and "Working…"/"Saving…". Errors are toasted.
function busy(fn, label) {
  return async (e) => {
    const el = e && e.currentTarget;
    const b = el && el.tagName === 'FORM' ? el.querySelector('button[type=submit]') : el && el.tagName === 'BUTTON' ? el : null;
    if (b && b.disabled) return;
    let kids = null, hadFocus = false, w = 0;
    if (b) {
      hadFocus = document.activeElement === b;
      kids = [...b.childNodes];
      w = b.offsetWidth;
      b.disabled = true;
      b.setAttribute('aria-busy', 'true');
      b.style.minWidth = w + 'px';
      fill(b, h('span', { class: 'spinner sm', 'aria-hidden': 'true' }), label || (el.tagName === 'FORM' ? 'Saving…' : 'Working…'));
    }
    try { await fn(e); } catch (err) { if (err.status !== 401) toast(err.message, 'bad'); } finally {
      if (b) {
        fill(b, kids);
        b.disabled = false;
        b.removeAttribute('aria-busy');
        b.style.minWidth = '';
        if (hadFocus && b.isConnected && document.activeElement === document.body) b.focus({ preventScroll: true });
      }
    }
  };
}

function describe(el, id) {
  if (!el || !el.setAttribute) return;
  const cur = (el.getAttribute('aria-describedby') || '').split(' ').filter((x) => x && x !== id);
  el.setAttribute('aria-describedby', [...cur, id].join(' '));
}

function field(label, control, hint) {
  let hintEl = null;
  if (hint) { hintEl = h('span', { class: 'hint', id: uid('hint') }, hint); describe(control, hintEl.id); }
  return h('label', { class: 'field' }, h('span', { class: 'field-label' }, label), control, hintEl);
}

// fieldError shows a message right under a field, marks it invalid, and
// moves focus there. clearErrors removes all of them inside a container.
function fieldError(control, msg) {
  const f = control.closest('.field') || control.parentNode;
  let e = f.querySelector('.field-error');
  if (!e) { e = h('span', { class: 'field-error', id: uid('err'), role: 'alert' }); f.append(e); }
  e.textContent = msg;
  control.setAttribute('aria-invalid', 'true');
  describe(control, e.id);
  control.focus({ preventScroll: true });
  control.scrollIntoView({ block: 'center', behavior: motionOK() ? 'smooth' : 'auto' });
  return false;
}
function clearErrors(root) {
  root.querySelectorAll('.field-error').forEach((e) => e.remove());
  root.querySelectorAll('[aria-invalid]').forEach((c) => c.removeAttribute('aria-invalid'));
}

const motionOK = () => !window.matchMedia('(prefers-reduced-motion: reduce)').matches;

// errBox is a form-level error line that is announced when it changes.
function errBox() { return h('div', { class: 'form-error', role: 'alert', tabindex: '-1' }); }
// showErr writes into an errBox and brings it into view.
function showErr(box, msg) {
  box.textContent = msg;
  if (!msg) return;
  box.scrollIntoView({ block: 'center', behavior: motionOK() ? 'smooth' : 'auto' });
  box.focus({ preventScroll: true });
}

// input builds a text field. Extra keys: code: true turns off spellcheck and
// auto-capitalisation (for hosts, usernames, paths, keys).
function input(attrs = {}) {
  const { code, ...rest } = attrs;
  if (code) Object.assign(rest, { spellcheck: 'false', autocapitalize: 'off', autocorrect: 'off' });
  return h('input', { type: 'text', ...rest });
}

function select(options, value, attrs = {}) {
  return h('select', attrs, options.map(([v, l]) => h('option', { value: String(v), selected: String(v) === String(value ?? '') }, l)));
}

function checkbox(label, checked, hint) {
  const cb = h('input', { type: 'checkbox', checked });
  const hid = hint ? uid('hint') : null;
  if (hid) describe(cb, hid);
  return { el: h('label', { class: 'check' }, cb, h('span', null, label, hint ? h('span', { class: 'hint block', id: hid }, hint) : null)), cb };
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
  return h('div', { class: 'page-head' }, h('div', { class: 'page-title' }, h('h1', { tabindex: '-1' }, title), sub ? h('p', { class: 'muted lede' }, sub) : null), right || null);
}

function focusHeading(root, sel) {
  const el = root.querySelector(sel);
  if (!el) return;
  if (!el.hasAttribute('tabindex')) el.setAttribute('tabindex', '-1');
  el.focus({ preventScroll: true });
}

function backLink(href, label) {
  return h('div', { class: 'back small' }, h('a', { href }, h('span', { 'aria-hidden': 'true' }, '← '), label));
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

function copyBtn(getText, label = 'Copy', ariaLabel) {
  const b = btn(label, async () => {
    await copyText(getText());
    fill(b, 'Copied ', h('span', { 'aria-hidden': 'true' }, '✓'));
    setTimeout(() => { b.textContent = label; }, 1500);
  }, 'sm', { 'aria-label': ariaLabel || null });
  return b;
}

function download(filename, text) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain;charset=utf-8' }));
  const a = h('a', { href: url, download: filename, class: 'offscreen' });
  document.body.append(a);
  a.click();
  setTimeout(() => { a.remove(); URL.revokeObjectURL(url); }, 1000);
}

// Open dialogs, topmost last. Only the topmost one reacts to Escape and Tab;
// everything behind it is inert.
const MODALS = [];

function syncInert() {
  const app = document.getElementById('app');
  if (app) app.inert = MODALS.length > 0;
  MODALS.forEach((m, i) => { m.bg.inert = i !== MODALS.length - 1; });
  document.body.classList.toggle('modal-open', MODALS.length > 0);
}

document.addEventListener('keydown', (e) => {
  const top = MODALS[MODALS.length - 1];
  if (!top) return;
  if (e.key === 'Escape') { e.preventDefault(); top.close(); return; }
  if (e.key !== 'Tab') return;
  const items = [...top.dlg.querySelectorAll(FOCUSABLE)].filter((x) => x.offsetParent !== null || x === document.activeElement);
  if (!items.length) { e.preventDefault(); top.heading.focus(); return; }
  const first = items[0], last = items[items.length - 1];
  const inside = top.dlg.contains(document.activeElement);
  if (e.shiftKey && (document.activeElement === first || !inside || document.activeElement === top.heading)) { e.preventDefault(); last.focus(); }
  else if (!e.shiftKey && (document.activeElement === last || !inside)) { e.preventDefault(); first.focus(); }
});

// modal opens a dialog. opts: wide, closeLabel, onClose, initialFocus
// ('close' for the close button, or an element; default: the heading).
// lastFocus remembers the control that last had focus, so a dialog opened by
// a busy (temporarily disabled) button can still return focus to it.
document.addEventListener('focusin', (e) => { if (e.target !== document.body) S.lastFocus = e.target; });

function modal(title, body, actions = [], opts = {}) {
  const ae = document.activeElement;
  const trigger = ae && ae !== document.body ? ae : S.lastFocus;
  let closed = false;
  const id = uid('dlg');
  const heading = h('h2', { id, tabindex: '-1' }, title);
  const closeBtn = btn(opts.closeLabel || 'Close', () => close());
  const dlg = h('div', { class: 'modal' + (opts.wide ? ' wide' : ''), role: opts.alert ? 'alertdialog' : 'dialog', 'aria-modal': 'true', 'aria-labelledby': id },
    heading, body, h('div', { class: 'modal-foot' }, actions, closeBtn));
  let downOnBg = false;
  const bg = h('div', {
    class: 'modal-bg',
    onmousedown: (e) => { downOnBg = e.target === bg; },
    onclick: (e) => { if (e.target === bg && downOnBg) close(); },
  }, dlg);
  const entry = { bg, dlg, heading, close: () => close() };
  const close = () => {
    if (closed) return;
    closed = true;
    const i = MODALS.indexOf(entry);
    if (i >= 0) MODALS.splice(i, 1);
    bg.remove();
    syncInert();
    if (trigger && trigger.isConnected && trigger.focus) trigger.focus({ preventScroll: true });
    if (opts.onClose) opts.onClose();
  };
  MODALS.push(entry);
  document.body.append(bg);
  syncInert();
  const start = opts.initialFocus === 'close' ? closeBtn : opts.initialFocus || heading;
  start.focus({ preventScroll: true });
  return close;
}

// confirmDlg asks a yes/no question. Destructive actions (the default) get a
// red confirm button, and focus starts on Cancel so Enter never destroys.
function confirmDlg(msg, okLabel = 'Confirm', { danger = true, title = 'Please confirm' } = {}) {
  return new Promise((resolve) => {
    let ok = false;
    const close = modal(title, msg instanceof Node ? msg : h('p', null, msg), [btn(okLabel, () => { ok = true; close(); }, danger ? 'danger solid' : 'primary')],
      { closeLabel: 'Cancel', alert: true, initialFocus: 'close', onClose: () => resolve(ok) });
  });
}

// every runs fn on an interval while the tab is visible; the timer stops when
// the page changes.
function every(ms, fn) { const id = setInterval(() => { if (!document.hidden) fn(); }, ms); S.timers.push(id); return id; }

// choices renders big clickable cards. Options: {value,title,desc,icon,badge,extra,disabled,notranslate}.
// Single choice is a radio group with one tab stop: arrow keys move between
// cards, Space/Enter picks one. label names the group for screen readers.
function choices(options, { value, onPick, multi = false, small = false, label = '' } = {}) {
  let sel = multi ? new Set(value || []) : value;
  const key = label || uid('ch');
  const wrap = h('div', { class: 'choices' + (small ? ' small' : ''), role: multi ? 'group' : 'radiogroup', 'aria-label': label || null, 'data-key': key });
  const btns = options.map((o) => {
    const b = h('button', {
      type: 'button', class: 'choice', disabled: o.disabled, role: multi ? 'checkbox' : 'radio', 'data-v': String(o.value),
      onclick: () => {
        if (multi) { if (sel.has(o.value)) sel.delete(o.value); else sel.add(o.value); } else sel = o.value;
        paint();
        if (onPick) onPick(multi ? [...sel] : sel, o);
        // A pick often re-renders the step; keep keyboard focus on the same card.
        if (!b.isConnected) {
          setTimeout(() => {
            const n = [...document.querySelectorAll('.choices[data-key]')].find((x) => x.dataset.key === key);
            const c = n && [...n.querySelectorAll('.choice')].find((x) => x.dataset.v === String(o.value));
            if (c) c.focus({ preventScroll: true });
          }, 0);
        }
      },
    },
    o.icon ? h('span', { class: 'choice-ico' }, icon(o.icon)) : null,
    h('span', { class: 'choice-body' },
      h('span', { class: 'choice-title', translate: o.notranslate ? 'no' : null }, o.title, o.badge ? [' ', h('span', { class: 'pill' + (o.badgeCls ? ' ' + o.badgeCls : '') }, o.badge)] : null),
      o.desc ? h('span', { class: 'choice-desc' }, o.desc) : null,
      o.extra || null));
    b.pvValue = o.value;
    return b;
  });
  const paint = () => {
    btns.forEach((b) => {
      const on = multi ? sel.has(b.pvValue) : sel === b.pvValue;
      b.classList.toggle('on', on);
      b.setAttribute('aria-checked', String(on));
    });
    if (!multi) {
      const enabled = btns.filter((b) => !b.disabled);
      const stop = enabled.find((b) => b.pvValue === sel) || enabled[0];
      btns.forEach((b) => { b.tabIndex = b === stop ? 0 : -1; });
    }
  };
  if (!multi) {
    wrap.addEventListener('keydown', (e) => {
      const enabled = btns.filter((b) => !b.disabled);
      const i = enabled.indexOf(document.activeElement);
      if (i < 0) return;
      let j = -1;
      if (e.key === 'ArrowRight' || e.key === 'ArrowDown') j = (i + 1) % enabled.length;
      else if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') j = (i - 1 + enabled.length) % enabled.length;
      else if (e.key === 'Home') j = 0;
      else if (e.key === 'End') j = enabled.length - 1;
      if (j < 0) return;
      e.preventDefault();
      enabled.forEach((b) => { b.tabIndex = -1; });
      enabled[j].tabIndex = 0;
      enabled[j].focus();
    });
  }
  add(wrap, btns);
  paint();
  return wrap;
}

function stepsBar(labels, cur) {
  return h('ol', { class: 'wsteps', 'aria-label': 'Steps' }, labels.map((l, i) => h('li', { class: i < cur ? 'done' : i === cur ? 'cur' : '', 'aria-current': i === cur ? 'step' : null },
    h('span', { class: 'n', 'aria-hidden': 'true' }, i < cur ? '✓' : String(i + 1)),
    srOnly(i < cur ? `Step ${i + 1}, completed: ` : `Step ${i + 1}: `),
    h('span', { class: 'l' }, l))));
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

// pickFolder opens a folder browser for this server’s own disks.
function pickFolder(start = '') {
  return new Promise((resolve) => {
    let chosen = null, cur = '', up = '';
    const listBox = h('div', { class: 'picker-list' });
    const pathEl = h('code', { class: 'break' });
    const upBtn = btn([h('span', { 'aria-hidden': 'true' }, '↑ '), 'Up'], () => go(up), 'sm', { 'aria-label': 'Up one folder' });
    const pickBtn = btn('Choose this folder', () => { chosen = cur; close(); }, 'primary');
    const go = async (p) => {
      const refocus = listBox.contains(document.activeElement) || document.activeElement === upBtn;
      fill(listBox, h('p', { class: 'muted', role: 'status' }, 'Loading…'));
      const after = () => {
        if (!refocus) return;
        const t = listBox.querySelector('.picker-item') || (upBtn.disabled ? pickBtn : upBtn);
        if (t && !t.disabled) t.focus({ preventScroll: true });
      };
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
        after();
      } catch (e) { fill(listBox, h('p', { class: 'form-error', role: 'alert' }, e.message), h('p', null, btn('Try again', () => go(p), 'sm'))); }
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
  const user = input({ name: 'username', autocomplete: 'username', required: true, code: true });
  const pass = input({ name: 'password', type: 'password', autocomplete: setup ? 'new-password' : 'current-password', required: true, minlength: setup ? 10 : null });
  const pass2 = input({ name: 'password2', type: 'password', autocomplete: 'new-password', required: true });
  const needCode = setup && S.status.setupCodeRequired;
  const code = input({ name: 'setup-code', autocomplete: 'one-time-code', code: true, required: true, placeholder: 'ABCD-EF23…' });
  const err = errBox();
  err.textContent = errMsg || '';
  const form = h('form', {
    onsubmit: busy(async (e) => {
      e.preventDefault();
      err.textContent = '';
      clearErrors(form);
      if (setup && pass.value.length < 10) return fieldError(pass, 'The password must be at least 10 characters.');
      if (setup && pass.value !== pass2.value) return fieldError(pass2, 'The passwords do not match. Type the same password twice.');
      if (needCode && !code.value.trim()) return fieldError(code, 'Enter the setup code shown when BackupProof was installed.');
      try {
        const body = { username: user.value.trim(), password: pass.value };
        if (needCode) body.setupCode = code.value.trim();
        const r = await api(setup ? '/setup' : '/login', { method: 'POST', body, noAuthRedirect: true });
        if (r && r.needCode) { renderCodeStep(r.challenge); return; }
        S.user = r.user; S.csrf = r.csrf || S.csrf;
        if (S.status) S.status.setupRequired = false;
        if (!location.hash || location.hash === '#/') history.replaceState(null, '', '#/dashboard');
        renderShell();
      } catch (ex) {
        if (needCode && ex.status === 403) return fieldError(code, ex.message);
        showErr(err, ex.status === 401 ? 'Wrong username or password. Check both and try again.' : ex.message);
      }
    }, setup ? 'Creating…' : 'Signing in…'),
  },
  needCode ? field('Setup code', code, 'Shown by the installer (and saved in setup-code.txt in the data folder). It makes sure only you can create this account.') : null,
  field('Username', user),
  field('Password', pass, setup ? 'At least 10 characters. You will use it to sign in to this dashboard.' : null),
  setup ? field('Type the password again', pass2) : null,
  err,
  h('button', { type: 'submit', class: 'btn primary block lg' }, setup ? 'Create my account' : 'Sign in'));
  app.append(h('main', { class: 'auth', id: 'main', tabindex: '-1' },
    h('div', { class: 'card' },
      brand(),
      h('h1', { class: 'auth-title' }, setup ? 'Create your administrator account' : 'Sign in'),
      h('p', { class: 'muted' }, setup ? 'This account manages backups, storage and the people who can sign in.' : 'Use the account an administrator gave you.'),
      form),
    h('p', { class: 'muted small auth-foot' }, 'Auditors: ', h('a', { href: '/api/public/keys', target: '_blank', rel: 'noopener' }, 'public keys for checking proofs'),
      S.status && S.status.version ? ` · ${verLabel()}` : '')));
  document.title = (setup ? 'Create account' : 'Sign in') + ' · BackupProof';
  if (window.matchMedia('(pointer: fine)').matches) user.focus();
}

// Theme: "auto" follows the device; "light" and "dark" are fixed. Saved in
// this browser only.
const THEMES = [['auto', 'Automatic (follow this device)', 'auto'], ['light', 'Light', 'sun'], ['dark', 'Dark', 'moon']];
function savedTheme() { try { return localStorage.getItem('bp-theme') || 'auto'; } catch { return 'auto'; } }
function applyTheme(t) {
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
  const dark = t === 'dark' || (t !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.querySelectorAll('meta[name="theme-color"]').forEach((m) => { m.content = dark ? '#08112a' : '#0e1a33'; m.removeAttribute('media'); });
}
applyTheme(savedTheme());
window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => applyTheme(savedTheme()));

function themeSwitch() {
  const wrap = h('div', { class: 'theme-switch', role: 'group', 'aria-label': 'Colour theme' });
  const paint = () => wrap.querySelectorAll('button').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.t === savedTheme())));
  for (const [t, label, ic] of THEMES) {
    wrap.append(h('button', { type: 'button', 'data-t': t, 'aria-label': label + ' theme', title: label,
      onclick: () => { try { localStorage.setItem('bp-theme', t); } catch { /* private mode: applies until reload */ } applyTheme(t); paint(); } }, icon(ic)));
  }
  paint();
  return wrap;
}

// userMenu is the signed-in person, top right, with Settings and Sign out.
function userMenu() {
  const roleWords = { admin: 'Administrator', operator: 'Can set up backups', auditor: 'View only' }[S.user.role] || S.user.role;
  const id = uid('usermenu');
  const btnEl = h('button', { type: 'button', class: 'user-btn', 'aria-haspopup': 'true', 'aria-expanded': 'false', 'aria-controls': id },
    h('span', { class: 'avatar', 'aria-hidden': 'true' }, (S.user.username || '?').slice(0, 1).toUpperCase()),
    h('span', { class: 'user-name' }, S.user.username), icon('chevron', 'user-chev'));
  const panel = h('div', { class: 'user-panel', id, hidden: true },
    h('div', { class: 'user-head' }, h('div', { class: 'who' }, S.user.username), h('div', { class: 'role' }, roleWords)),
    h('a', { href: '#/settings', class: 'user-item', onclick: () => setOpen(false) }, 'Settings and security'),
    btn('Sign out', busy(logout, 'Signing out…'), 'user-item'));
  const wrap = h('div', { class: 'user-menu' }, btnEl, panel);
  const setOpen = (open, focus) => {
    panel.hidden = !open;
    btnEl.setAttribute('aria-expanded', String(open));
    if (open) panel.querySelector('a').focus();
    else if (focus) btnEl.focus();
  };
  btnEl.addEventListener('click', () => setOpen(panel.hidden));
  wrap.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !panel.hidden) { e.stopPropagation(); setOpen(false, true); } });
  document.addEventListener('click', (e) => { if (!panel.hidden && !wrap.contains(e.target)) setOpen(false); });
  wrap.addEventListener('focusout', (e) => { if (!panel.hidden && e.relatedTarget && !wrap.contains(e.relatedTarget)) setOpen(false); });
  return wrap;
}

// renderCodeStep is the second sign-in step for accounts with two-factor on.
function renderCodeStep(challenge) {
  const app = clear(document.getElementById('app'));
  const code = input({ name: 'one-time-code', autocomplete: 'one-time-code', inputmode: 'text', code: true, required: true, placeholder: '123456…', spellcheck: 'false' });
  const err = errBox();
  const form = h('form', {
    onsubmit: busy(async (e) => {
      e.preventDefault();
      err.textContent = '';
      if (!code.value.trim()) return fieldError(code, 'Enter the 6-digit code from your authenticator app.');
      try {
        const r = await api('/login', { method: 'POST', body: { challenge, code: code.value.trim() }, noAuthRedirect: true });
        S.user = r.user; S.csrf = r.csrf || S.csrf;
        if (!location.hash || location.hash === '#/') history.replaceState(null, '', '#/dashboard');
        renderShell();
      } catch (ex) {
        if (/password again/i.test(ex.message)) { renderAuth(ex.message); return; }
        showErr(err, ex.message);
        code.select();
      }
    }, 'Checking…'),
  },
  field('Code', code, 'From your authenticator app. Lost your phone? Enter one of your recovery codes instead.'),
  err,
  h('button', { type: 'submit', class: 'btn primary block lg' }, 'Sign in'),
  h('p', { class: 'small center' }, h('a', { href: '#/', onclick: (e) => { e.preventDefault(); renderAuth(); } }, 'Use a different account')));
  app.append(h('main', { class: 'auth', id: 'main', tabindex: '-1' },
    h('div', { class: 'card' }, brand(), h('h1', { class: 'auth-title' }, 'Enter your code'),
      h('p', { class: 'muted' }, 'This account uses two-factor sign-in.'), form)));
  document.title = 'Enter your code · BackupProof';
  code.focus();
}

function verLabel() { const v = String(S.status.version).replace(/^backupproof\//, ''); return /^\d/.test(v) ? 'v' + v : v; }

// markSvg is the BackupProof mark, the same shield and check as backupproof.dev.
function markSvg() {
  const NS = 'http://www.w3.org/2000/svg';
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', '0 0 72 88');
  svg.setAttribute('class', 'mark');
  svg.setAttribute('aria-hidden', 'true');
  const shield = document.createElementNS(NS, 'path');
  shield.setAttribute('d', 'M36 0 70 12v28c0 22-15 39-34 46C17 79 2 62 2 40V12Z');
  shield.setAttribute('class', 'mark-shield');
  const check = document.createElementNS(NS, 'path');
  check.setAttribute('d', 'm21 43 11 11 20-22');
  check.setAttribute('class', 'mark-check');
  svg.append(shield, check);
  return svg;
}

// brand is the logo; with href it links there (Home, inside the dashboard).
function brand(href) {
  const kids = [h('span', { class: 'brand-mark' }, markSvg()), h('span', null, 'Backup', h('span', { class: 'brand-accent' }, 'Proof'))];
  return href
    ? h('a', { class: 'brand', href, translate: 'no', 'aria-label': 'BackupProof, go to Home' }, kids)
    : h('div', { class: 'brand', translate: 'no' }, kids);
}

const EXT = (href, label) => h('a', { href, target: '_blank', rel: 'noopener' }, label, h('span', { class: 'sr-only' }, ' (opens in a new tab)'));

// ----------------------------------------------------------------- shell

const NAV = [
  ['dashboard', 'Home', 'home'],
  ['protected', 'Protected', 'shield'],
  ['repositories', 'Storage', 'cloud'],
  ['agents', 'Servers', 'computer'],
  ['import', 'Import', 'import'],
  ['proofs', 'Proof history', 'history'],
  ['alerts', 'Alerts', 'bell'],
  ['activity', 'Activity', 'list'],
  ['settings', 'Settings', 'sliders'],
];

const NAV_ALIAS = { sources: 'protected', protect: 'protected', storage: 'repositories', keys: 'proofs' };

function renderShell() {
  const app = clear(document.getElementById('app'));
  app.className = '';
  const layout = h('div', { class: 'layout' });
  const mq = window.matchMedia('(max-width: 800px)');
  // On small screens the sidebar is a drawer: closed it is inert, so its
  // links can't be reached with Tab while off-screen.
  const setNav = (open, focus) => {
    layout.classList.toggle('nav-open', open);
    menuBtn.setAttribute('aria-expanded', String(open));
    menuBtn.setAttribute('aria-label', open ? 'Close menu' : 'Open menu');
    sidebar.inert = mq.matches && !open;
    if (open && focus) { const a = sidebar.querySelector('a[aria-current="page"]') || sidebar.querySelector('a'); if (a) a.focus({ preventScroll: true }); }
    if (!open && focus) menuBtn.focus({ preventScroll: true });
  };
  const navOpen = () => layout.classList.contains('nav-open');
  const menuBtn = h('button', { type: 'button', class: 'btn icon', 'aria-label': 'Open menu', 'aria-expanded': 'false', 'aria-controls': 'sidebar', onclick: () => setNav(!navOpen(), true) }, icon('menu'));
  const nav = h('nav', { class: 'nav', id: 'nav', 'aria-label': 'Main' }, NAV.map(([k, l, ic]) =>
    h('a', { href: '#/' + k, 'data-k': k, onclick: () => { if (navOpen()) setNav(false, false); } }, h('span', { class: 'nav-l' }, icon(ic), h('span', null, l)),
      k === 'alerts' ? h('span', { class: 'count hidden', id: 'alert-count' }) : null)));
  const sidebar = h('aside', { class: 'sidebar', id: 'sidebar' }, nav);
  const footer = h('footer', { class: 'app-foot' },
    h('span', { translate: 'no' }, 'BackupProof', S.status && S.status.version ? ' ' + verLabel() : ''),
    h('nav', { 'aria-label': 'BackupProof links' }, EXT('https://backupproof.dev', 'Website'), EXT('https://github.com/chmuzamil/BackupProof', 'GitHub')));
  add(layout, [
    h('header', { class: 'topbar' }, menuBtn, brand('#/dashboard'), h('div', { class: 'top-right' }, themeSwitch(), userMenu())),
    h('div', { class: 'scrim', onclick: () => setNav(false, true) }),
    sidebar,
    h('div', { class: 'content' }, h('main', { class: 'main', id: 'main', tabindex: '-1' }), footer),
  ]);
  layout.addEventListener('keydown', (e) => { if (e.key === 'Escape' && navOpen()) { e.stopPropagation(); setNav(false, true); } });
  const onMq = () => setNav(false, false);
  if (mq.addEventListener) mq.addEventListener('change', onMq);
  app.append(layout);
  setNav(false, false);
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
  if (document.hidden) return;
  try { setAlertCount((await api('/alerts?open=1')) || []); } catch { /* ignore */ }
}

function setAlertCount(alerts) {
  const el = document.getElementById('alert-count');
  if (!el) return;
  fill(el, nf(alerts.length), srOnly(alerts.length === 1 ? ' open alert' : ' open alerts'));
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
  [/^\/sources\/(\d+)\/restore$/, (m) => pageRestore(Number(m[1]))],
  [/^\/agents$/, pageAgents],
  [/^\/repositories$/, pageRepositories],
  [/^\/storage\/new$/, pageStorageNew],
  [/^\/proofs$/, pageProofs],
  [/^\/keys$/, pageKeys],
  [/^\/alerts$/, pageAlerts],
  [/^\/activity$/, pageActivity],
  [/^\/settings$/, pageSettings],
];

// hashParts splits "#/proofs?from=101" into its path and query parameters.
function hashParts(hash = location.hash) {
  const raw = (hash || '#/dashboard').slice(1) || '/dashboard';
  const [path, q] = raw.split('?');
  return { path: path || '/dashboard', params: new URLSearchParams(q || '') };
}

async function route(keepScroll) {
  if (!S.user) return;
  const main = document.getElementById('main');
  if (!main) return;
  stopTimers();
  S.stepHandler = null;
  const { path, params } = hashParts();
  S.curHash = location.hash;
  const top = path.split('/')[1];
  const navKey = NAV_ALIAS[top] || top;
  document.querySelectorAll('#nav a').forEach((a) => {
    const on = a.dataset.k === navKey;
    a.classList.toggle('active', on);
    if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
  });
  const gen = ++S.gen;
  // Remember which control had focus so a refresh can put it back.
  const focusables = [...main.querySelectorAll(FOCUSABLE)];
  const fIdx = keepScroll ? focusables.indexOf(document.activeElement) : -1;
  const fText = fIdx >= 0 ? document.activeElement.textContent : '';
  let node;
  try {
    const r = ROUTES.find(([re]) => re.test(path));
    node = r ? await r[1](path.match(r[0]), params) : h('div', null, pageHead('Page not found'), card(null, h('p', null, 'There is no page at this address. It may have moved.'), h('a', { class: 'btn primary', href: '#/dashboard' }, 'Go to Home')));
  } catch (err) {
    if (err.status === 401) return;
    node = h('div', null, pageHead('Couldn’t load this page'),
      h('div', { class: 'banner bad', role: 'alert' }, h('p', null, err.message),
        h('div', { class: 'btns' }, btn('Try again', () => route(), 'sm primary'), h('a', { class: 'btn sm', href: '#/dashboard' }, 'Go to Home'))));
  }
  if (gen !== S.gen) return;
  const y = window.scrollY;
  fill(main, node);
  S.dirty = false;
  const h1 = main.querySelector('h1');
  document.title = (h1 && h1.textContent.trim() ? h1.textContent.trim() + ' · ' : '') + 'BackupProof';
  if (keepScroll) {
    window.scrollTo(0, y);
    if (fIdx >= 0) {
      const again = [...main.querySelectorAll(FOCUSABLE)][fIdx];
      if (again && again.textContent === fText) again.focus({ preventScroll: true });
    }
  } else {
    window.scrollTo(0, 0);
    if (h1 && S.routed) h1.focus({ preventScroll: true });
    S.routed = true;
  }
}

const reload = () => route(true);

// Periodic refresh that never interrupts someone: it skips while a dialog is
// open, details are expanded, or keyboard focus is inside the page.
function autoRefresh() {
  if (document.hidden || MODALS.length || document.querySelector('#main details[open], #main .pill-note:not([hidden])')) return;
  const a = document.activeElement;
  const main = document.getElementById('main');
  if (a && a !== document.body && a !== main && main && main.contains(a)) return;
  reload();
}

// ---------------------------------------------------- unsaved-changes guard
// Pages with forms call guardDirty(root): typing marks the page dirty, and
// leaving it (link, Back button, closing the tab) asks first.

function guardDirty(root) {
  const mark = (e) => { if (e.target && e.target.matches && e.target.matches('input, textarea, select')) S.dirty = true; };
  root.addEventListener('input', mark);
  root.addEventListener('change', mark);
  return root;
}

const LEAVE_MSG = 'You have entered information on this page that isn’t saved yet. Leave without saving?';

window.addEventListener('beforeunload', (e) => { if (S.dirty) { e.preventDefault(); e.returnValue = ''; } });

// In-app links: ask before following them.
document.addEventListener('click', async (e) => {
  if (!S.dirty || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
  const a = e.target.closest && e.target.closest('a[href^="#/"]');
  if (!a || a.target) return;
  if (hashParts(a.getAttribute('href')).path === hashParts().path) return;
  e.preventDefault();
  if (await confirmDlg(LEAVE_MSG, 'Leave without saving', { title: 'Leave this page?' })) { S.dirty = false; location.hash = a.getAttribute('href'); }
}, true);

window.addEventListener('hashchange', async () => {
  const to = location.hash;
  if (!to.startsWith('#/')) { if (S.curHash) history.replaceState(null, '', S.curHash); return; }
  const next = hashParts(to), cur = hashParts(S.curHash);
  if (S.curHash && next.path === cur.path && S.stepHandler) { S.curHash = to; S.stepHandler(next.params); return; }
  if (S.dirty && S.curHash && next.path !== cur.path) {
    history.replaceState(null, '', S.curHash); // stay here until they decide
    if (!(await confirmDlg(LEAVE_MSG, 'Leave without saving', { title: 'Leave this page?' }))) return;
    S.dirty = false;
    location.hash = to;
    return;
  }
  route();
});

// wizardHistory lets the browser Back button step back through a wizard.
// Each step gets its own entry like #/protect?step=2.
function wizardHistory(base, getStep, setStep) {
  let depth = 0;
  const url = (n) => `#${base}?step=${n}`;
  history.replaceState(null, '', url(getStep()));
  S.curHash = location.hash;
  S.stepHandler = (params) => {
    const n = Number(params.get('step'));
    if (!Number.isFinite(n)) return;
    if (n < getStep()) { depth = Math.max(0, depth - 1); setStep(n); }
    else if (n > getStep()) { history.replaceState(null, '', url(getStep())); S.curHash = location.hash; }
  };
  return {
    forward(n) { depth++; history.pushState(null, '', url(n)); S.curHash = location.hash; setStep(n); },
    back(n) { if (depth > 0) history.back(); else { history.replaceState(null, '', url(n)); S.curHash = location.hash; setStep(n); } },
    done() { S.stepHandler = null; history.replaceState(null, '', `#${base}?step=done`); S.curHash = location.hash; },
  };
}

// ------------------------------------------------------------------ home

async function runJob(id, kind, after, isImport) {
  const r = await post(`/sources/${id}/run`, { kind });
  toast(`${jobWord(kind, isImport)} started. This can take a few minutes.`, 'ok');
  if (after) after(r.jobId);
  else reload();
}

function runButtons(id, running, after, withCheck, isImport, name) {
  if (!canOperate()) return null;
  const al = (verb) => (name ? { 'aria-label': `${verb}: ${name}` } : {});
  const backupWord = isImport ? 'Convert again' : 'Back up now';
  return h('div', { class: 'btns' },
    btn(backupWord, busy(() => runJob(id, 'backup', after, isImport), 'Starting…'), 'sm primary', al(backupWord)),
    btn('Test restore', busy(() => runJob(id, 'drill', after, isImport), 'Starting…'), 'sm', al('Test restore')),
    withCheck ? btn('Check storage health', busy(() => runJob(id, 'check', after, isImport), 'Starting…'), 'sm', al('Check storage health')) : null);
}

function runningLine(j, isImport) {
  if (!j) return null;
  const what = j.kind === 'drill' ? 'Testing the restore' : j.kind === 'check' ? 'Checking storage health' : isImport ? 'Converting' : 'Backing up';
  return h('div', { class: 'running', role: 'status' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), j.state === 'queued' ? `${what} — waiting for the server to start…` : `${what} now…`);
}

// The proof tape: one mark per day for the last TAPE_DAYS days, showing
// whether that day's newest restore test passed, only a backup was made,
// or something failed.
const TAPE_DAYS = 14;
const TAPE_WORDS = { tested: 'restore test passed', backup: 'backed up (not restore-tested that day)', failed: 'something failed', '': 'nothing ran' };

function dayStart(d) { return new Date(d.getFullYear(), d.getMonth(), d.getDate()); }

function proofTapes(proofs) {
  const start = dayStart(new Date());
  start.setDate(start.getDate() - (TAPE_DAYS - 1));
  const seen = new Map(); // sourceId → [{ drill, backup }] per day, newest proof wins
  for (const p of proofs || []) { // newest first
    const d = toDate(p.created);
    if (!p.sourceId || !d || d < start) continue;
    const i = Math.round((dayStart(d) - start) / 864e5);
    if (i < 0 || i >= TAPE_DAYS) continue;
    let days = seen.get(p.sourceId);
    if (!days) seen.set(p.sourceId, days = Array.from({ length: TAPE_DAYS }, () => ({})));
    const k = p.kind === 'drill' ? 'drill' : 'backup';
    if (!(k in days[i])) days[i][k] = !!p.passed;
  }
  const out = new Map();
  for (const [id, days] of seen) {
    out.set(id, days.map((x) => 'drill' in x ? (x.drill ? 'tested' : 'failed') : 'backup' in x ? (x.backup ? 'backup' : 'failed') : ''));
  }
  return out;
}

function proofTape(days) {
  days = days || Array(TAPE_DAYS).fill('');
  const n = (v) => days.filter((x) => x === v).length;
  const summary = `Last ${TAPE_DAYS} days: restore test passed on ${plural(n('tested'), 'day')}` + (n('failed') ? `, something failed on ${plural(n('failed'), 'day')}` : '');
  const today = dayStart(new Date());
  return h('div', { class: 'tape', role: 'img', 'aria-label': summary },
    days.map((v, i) => {
      const d = new Date(today);
      d.setDate(d.getDate() - (TAPE_DAYS - 1 - i));
      return h('span', { class: 'tape-m t-' + (v || 'none'), title: `${d.toLocaleDateString(undefined, { weekday: 'short', month: 'short', day: 'numeric' })}: ${TAPE_WORDS[v]}` });
    }));
}

function tapeLegend() {
  return h('div', { class: 'tape-legend', 'aria-hidden': 'true' },
    h('span', null, h('span', { class: 'tape-m t-tested' }), 'Restore test passed'),
    h('span', null, h('span', { class: 'tape-m t-backup' }), 'Backed up'),
    h('span', null, h('span', { class: 'tape-m t-failed' }), 'Failed'),
    h('span', null, h('span', { class: 'tape-m t-none' }), 'Nothing'));
}

async function removeItem(id, name, after) {
  if (!(await confirmDlg(`Stop protecting “${name}”? No new backups or restore tests will run. Backup copies already in storage and the proof history are kept.`, 'Remove', { title: 'Remove this item?' }))) return;
  await del(`/sources/${id}`);
  toast(`Removed “${name}”`, 'ok');
  after();
}

function itemCard(s, days, removable) {
  const src = s.source, k = kindInfo(src.spec), isImp = src.spec && src.spec.kind === 'import';
  const lb = s.lastBackup, ld = s.lastDrill;
  const st = STATUS[s.status] || { cls: '' };
  return h('article', { class: 'item ' + (st.cls ? 'is-' + st.cls : 'is-none') },
    h('div', { class: 'item-ico' }, icon(k.icon)),
    h('div', { class: 'item-main' },
      h('div', { class: 'item-title' }, h('a', { href: `#/sources/${src.id}` }, src.name), statusPill(s.status, s.reason)),
      h('div', { class: 'item-sub' }, [k.label, s.agentName ? 'on ' + s.agentName : '', s.repoName ? 'stored in ' + s.repoName : '', src.enabled === false ? 'paused' : ''].filter(Boolean).join(' · ')),
      s.reason ? h('p', { class: 'item-reason' }, s.reason) : null,
      h('div', { class: 'item-facts' },
        h('span', null, h('span', { class: 'fact-l' }, isImp ? 'Last conversion ' : 'Last backup '), lb ? [timeEl(lb.created), lb.passed === false ? h('span', { class: 'fact-bad' }, ' (failed)') : ''] : h('span', { class: 'muted' }, 'not yet')),
        h('span', null, h('span', { class: 'fact-l' }, 'Last restore test '), ld ? [h('span', { class: ld.passed ? 'fact-ok' : 'fact-bad' }, ld.passed ? 'passed ' : 'failed '), timeEl(ld.created)] : h('span', { class: 'muted' }, 'not yet'))),
      runningLine(s.running, isImp)),
    h('div', { class: 'item-tape' }, proofTape(days), h('span', { class: 'tape-cap' }, `Last ${TAPE_DAYS} days`)),
    h('div', { class: 'item-actions' }, runButtons(src.id, s.running, null, false, isImp, src.name),
      removable && isAdmin() ? btn('Remove', busy(() => removeItem(src.id, src.name, reload)), 'sm danger quiet', { 'aria-label': `Remove ${src.name}` }) : null));
}

function itemList(sources, tapes, removable) {
  return h('div', { class: 'items' }, sources.map((s) => itemCard(s, tapes && tapes.get(s.source.id), removable)));
}

function welcomeCard() {
  return h('section', { class: 'card welcome' },
    h('div', { class: 'welcome-ico' }, icon('shieldCheck')),
    h('h2', { class: 'big' }, 'Let’s protect your first thing'),
    h('p', { class: 'muted' }, 'BackupProof backs up your files, websites and databases, then regularly tests that each backup really restores — so you know it works before you need it.'),
    h('ol', { class: 'welcome-steps' }, ['Choose what', 'Choose where', 'How often', 'Done'].map((l) => h('li', null, l))),
    canOperate() ? h('div', { class: 'btns center' },
      h('a', { class: 'btn primary lg', href: '#/protect' }, 'Protect something'),
      h('a', { class: 'btn lg stack', href: '#/import' }, 'Import old backups', h('span', { class: 'btn-sub' }, 'from S3, B2, a disk or another server')))
      : h('p', { class: 'hint' }, 'Ask an administrator to set up the first backup.'));
}

async function pageDashboard() {
  const [d, proofs] = await Promise.all([api('/dashboard'), api('/proofs?limit=1000').catch(() => [])]);
  const sources = d.sources || [], agents = d.agents || [], alerts = d.alerts || [];
  const c = d.counts || {};
  setAlertCount(alerts);
  every(15000, autoRefresh);
  const head = pageHead('Home', 'Everything you protect, and whether its backups are proven to restore.',
    sources.length && canOperate() ? h('div', { class: 'btns' }, h('a', { class: 'btn primary', href: '#/protect' }, '+ Protect something')) : null);
  if (!sources.length) return h('div', null, head, welcomeCard());

  const total = c.sources ?? sources.length;
  const proven = c.proven ?? 0, failing = c.failing ?? 0;
  const items = (n) => plural(n, 'item');
  let tone, verdict;
  if (failing) { tone = 'bad'; verdict = `${items(failing)} ${failing === 1 ? 'has' : 'have'} a problem`; }
  else if (proven === total) { tone = 'ok'; verdict = total === 1 ? 'Your item is restore-tested' : `All ${nf(total)} items are restore-tested`; }
  else { tone = 'warn'; verdict = `${nf(proven)} of ${items(total)} ${proven === 1 ? 'is' : 'are'} restore-tested`; }
  const lastPass = sources.map((s) => s.lastDrill && s.lastDrill.passed && toDate(s.lastDrill.created)).filter(Boolean).sort((a, b) => b - a)[0];
  const nextTest = sources.map((s) => s.source.enabled !== false && toDate(s.source.nextDrill)).filter((x) => x && x > Date.now()).sort((a, b) => a - b)[0];
  const agentsTotal = c.agentsTotal ?? agents.length, online = c.agentsOnline ?? 0;
  const facts = [
    lastPass ? ['Last restore test passed ', timeEl(lastPass)] : 'No restore test has passed yet',
    nextTest ? ['Next one ', timeEl(nextTest)] : null,
    `${nf(online)} of ${plural(agentsTotal, 'server')} online`,
  ].filter(Boolean);
  const tiles = h('section', { class: 'verdict v-' + tone, 'aria-labelledby': 'verdict-h' },
    h('span', { class: 'verdict-seal', 'aria-hidden': 'true' }, icon(tone === 'ok' ? 'shieldCheck' : tone === 'bad' ? 'bell' : 'shield')),
    h('div', { class: 'verdict-text' },
      h('h2', { id: 'verdict-h' }, verdict),
      h('p', { class: 'verdict-facts' }, facts.map((f) => h('span', null, f)))));

  const banner = alerts.length ? h('div', { class: 'banner bad' },
    h('p', null, h('strong', null, `${nf(alerts.length)} thing${alerts.length > 1 ? 's need' : ' needs'} your attention`), ' · ', h('a', { href: '#/alerts' }, 'See all alerts')),
    h('ul', null, alerts.slice(0, 5).map((a) => h('li', null, a.message, ' ', h('span', { class: 'small' }, '(', rel(a.created), ')'))))) : null;

  return h('div', null, head, tiles, banner,
    h('section', { class: 'card ledger' }, cardHead('What’s protected', tapeLegend()), itemList(sources, proofTapes(proofs))));
}

async function pageProtected() {
  const [d, proofs] = await Promise.all([api('/dashboard'), api('/proofs?limit=1000').catch(() => [])]);
  const sources = d.sources || [];
  every(15000, autoRefresh);
  const actions = canOperate() ? h('div', { class: 'btns' },
    h('a', { class: 'btn', href: '#/import' }, 'Import old backups'),
    h('a', { class: 'btn primary', href: '#/protect' }, '+ Protect something')) : null;
  return h('div', null,
    pageHead('Protected', 'Everything BackupProof backs up and restore-tests.', actions),
    sources.length ? h('section', { class: 'card ledger' }, cardHead(plural(sources.length, 'item'), tapeLegend()), itemList(sources, proofTapes(proofs), true),
      h('div', { class: 'legend' }, h('h2', { class: 'sr-only' }, 'What the labels mean'), Object.values(STATUS).map((x) => h('span', null, h('span', { class: 'pill ' + x.cls }, glyphLabel(x.label)), ' ', x.help))))
      : welcomeCard());
}

// ---------------------------------------------------------- restore

// restoreProgress follows a restore job until it finishes.
function restoreProgress(jobId, what) {
  const line = h('div', { class: 'progress', role: 'status' });
  const pre = h('pre', { class: 'log', tabindex: '0', 'aria-label': 'Log' }, '');
  const el = h('div', { class: 'prog' }, line, details('Show details', pre));
  const gen = S.gen;
  const set = (cls, text, spin) => {
    line.className = 'progress ' + cls;
    fill(line, spin ? h('span', { class: 'spinner', 'aria-hidden': 'true' }) : h('span', { class: 'prog-ico', 'aria-hidden': 'true' }, cls === 'ok' ? '✓' : '✗'), h('span', null, text));
  };
  set('', 'Waiting for the server to start…', true);
  const tick = async () => {
    if (gen !== S.gen || !el.isConnected) return;
    try {
      const j = await api(`/jobs/${jobId}`);
      pre.textContent = (j.log || '(nothing written yet)') + (j.error ? '\nERROR: ' + j.error : '');
      if (j.state === 'queued') set('', 'Waiting for the server to start…', true);
      else if (j.state === 'running') set('', what + '…', true);
      else if (j.state === 'succeeded') {
        const res = j.result || {};
        set('ok', res.into ? 'Restored into ' + res.into + '.' : `Restored ${plural(res.files || 0, 'file')} (${bytes(res.bytes || 0)}). Every file was checked against the backup.`);
        return;
      } else { set('bad', 'The restore didn’t finish: ' + (j.error || 'see the details below.')); return; }
    } catch { /* try again */ }
    setTimeout(tick, 2000);
  };
  setTimeout(tick, 500);
  return el;
}

const yyyymmdd = (d = new Date()) => d.toISOString().slice(0, 10).replaceAll('-', '');

async function pageRestore(id) {
  if (!isAdmin()) return h('div', { class: 'banner warn' }, 'Only administrators can restore, because a restore can change files on a server.');
  const [st, rp, agents] = await Promise.all([api(`/sources/${id}`), api(`/sources/${id}/restore-points`), api('/agents')]);
  const src = st.source, points = rp.points || [];
  document.title = 'Restore ' + src.name + ' · BackupProof';
  const head = h('div', { class: 'page-head' }, h('div', { class: 'page-title' }, backLink(`#/sources/${id}`, src.name),
    h('h1', { tabindex: '-1' }, 'Restore ' + src.name),
    h('p', { class: 'muted lede' }, rp.database ? 'Put the database back, or load a copy next to it.' : 'Get files back: download them, put them back where they were, or restore to a new folder.')));
  if (!points.length) return h('div', null, head, card(null, empty('There is no finished backup of this item yet.')));

  const R = { snap: points[0].snapshotId, picks: new Set(), path: '' };
  const pointList = h('div', { class: 'restore-points', role: 'radiogroup', 'aria-label': 'Backup to restore' }, points.slice(0, 30).map((pt, i) => {
    const b = h('button', { type: 'button', role: 'radio', class: 'rp' + (i === 0 ? ' on' : ''), 'aria-checked': String(i === 0), onclick: () => {
      pointList.querySelectorAll('.rp').forEach((x) => { x.classList.remove('on'); x.setAttribute('aria-checked', 'false'); });
      b.classList.add('on'); b.setAttribute('aria-checked', 'true');
      R.snap = pt.snapshotId; R.picks.clear(); R.path = ''; if (browse) loadDir('');
    } },
    h('span', { class: 'rp-when' }, absTime(pt.created)), h('span', { class: 'rp-rel muted small' }, rel(pt.created)),
    pt.tested ? h('span', { class: 'pill ok' }, glyphLabel('Restore tested ✓')) : h('span', { class: 'pill' }, 'Backed up'));
    return b;
  }));

  const out = h('div');
  let browse = null, loadDir = null;

  if (rp.database) {
    // ---- databases
    const k = src.spec.kind, sqlite = k === 'sqlite';
    const orig = sqlite ? (src.spec.paths || [''])[0] : src.spec.database;
    const defName = sqlite ? orig.replace(/(\.[^./\\]+)?$/, `.restored-${yyyymmdd()}$1`) : `${orig || 'db'}_restored_${yyyymmdd()}`;
    const mode = { v: 'new' };
    const target = input({ name: 'db-target', code: true, value: defName, autocomplete: 'off' });
    const confirmName = input({ name: 'confirm-name', code: true, autocomplete: 'off', placeholder: orig + '…' });
    const replaceBox = h('div', { hidden: true }, h('div', { class: 'banner warn' }, sqlite
      ? 'The database file is replaced with the backup. Stop the app that uses it first, or it may keep writing to the old file.'
      : 'Tables in the backup replace the tables in the original database. Anything changed since this backup is lost.'),
    field(`Type “${orig}” to confirm`, confirmName));
    const newBox = h('div', null, field(sqlite ? 'New database file' : 'New database name', target, sqlite ? 'A full path on the server. The original file isn’t touched.' : 'Created on the same database server. The original isn’t touched.'));
    const modes = choices([
      { value: 'new', title: sqlite ? 'Restore to a new file' : 'Restore into a new database', desc: 'Safe: check the data before using it.', badge: 'recommended', badgeCls: 'ok' },
      { value: 'replace', title: 'Replace the original', desc: sqlite ? 'Overwrites ' + orig + '.' : 'Overwrites the tables in ' + orig + '.' },
    ], { value: 'new', small: true, label: 'How to restore', onPick: (v) => { mode.v = v; newBox.hidden = v !== 'new'; replaceBox.hidden = v !== 'replace'; } });
    const err = errBox();
    const go = btn('Restore the database', busy(async () => {
      err.textContent = '';
      const body = { snapshotId: R.snap };
      if (mode.v === 'replace') {
        if (confirmName.value.trim() !== orig) return fieldError(confirmName, `Type “${orig}” exactly to confirm.`);
        body.dbReplace = true;
      } else {
        if (!target.value.trim()) return fieldError(target, 'Enter a name.');
        body.dbTarget = target.value.trim();
      }
      const res = await post(`/sources/${id}/restore-db`, body);
      fill(out, restoreProgress(res.jobId, 'Restoring the database'));
    }, 'Starting…'), 'primary');
    const dumpLink = () => `/api/sources/${id}/download?` + qs({ snapshot: R.snap, path: rp.dumpPath });
    const dl = h('a', { class: 'btn', href: dumpLink(), onclick: (e) => { e.currentTarget.href = dumpLink(); } }, 'Download the backup file');
    return h('div', null, head,
      card('1. Choose a backup', pointList),
      card('2. Restore', modes, newBox, replaceBox, err, h('div', { class: 'form-actions start' }, go, dl), out));
  }

  // ---- files
  const itemAgent = agents.find((a) => a.id === src.agentId) || {};
  const win = (itemAgent.os || '').startsWith('windows');
  // Shown the way the server writes it: /srv/www or C:\Users.
  const shown = (p2) => win && /^[A-Za-z](\/|$)/.test(p2) ? p2[0] + ':\\' + p2.slice(2).replaceAll('/', '\\') : '/' + p2;
  const crumbs = h('nav', { class: 'crumbs', 'aria-label': 'Folder' });
  const list = h('div', { class: 'file-list' });
  const picked = h('p', { class: 'small picked', 'aria-live': 'polite' });
  const showPicked = () => fill(picked, R.picks.size ? [h('strong', null, plural(R.picks.size, 'selection')), ': ', [...R.picks].map(shown).join(', ')] : 'Nothing selected: everything in this backup.');
  let expanded = false;
  // auto: open single folders straight away, so browsing starts where the item's files are.
  loadDir = async (dir, auto = !dir) => {
    R.path = dir;
    fill(list, h('p', { class: 'muted' }, 'Loading…'));
    let res;
    try { res = await api(`/sources/${id}/files?` + qs({ snapshot: R.snap, path: dir })); }
    catch (e) { fill(list, h('div', { class: 'banner info' }, e.message)); fill(crumbs); return; }
    if (auto && res.entries && res.entries.length === 1 && res.entries[0].dir) return loadDir(res.entries[0].path, true);
    const parts = dir ? dir.split('/') : [];
    const crumb = (p2, i) => [h('span', { 'aria-hidden': 'true' }, ' / '), i === parts.length - 1 ? h('span', { 'aria-current': 'location' }, p2) : h('button', { type: 'button', class: 'linkish', onclick: () => loadDir(parts.slice(0, i + 1).join('/'), false) }, p2)];
    const long = parts.length > 4 && !expanded;
    fill(crumbs, h('button', { type: 'button', class: 'linkish', onclick: () => loadDir('', false) }, 'Backup'),
      long ? [crumb(parts[0], 0), h('span', { 'aria-hidden': 'true' }, ' / '), h('button', { type: 'button', class: 'linkish', 'aria-label': 'Show the whole path', onclick: () => { expanded = true; loadDir(dir, false); } }, '…'),
        parts.slice(-2).map((p2, j) => crumb(p2, parts.length - 2 + j))] : parts.map(crumb));
    fill(list, (res.entries || []).length ? res.entries.map((e) => {
      const cb = h('input', { type: 'checkbox', checked: R.picks.has(e.path), 'aria-label': 'Select ' + e.name, onchange: () => { if (cb.checked) R.picks.add(e.path); else R.picks.delete(e.path); showPicked(); } });
      return h('div', { class: 'file-row' }, cb,
        e.dir ? h('button', { type: 'button', class: 'linkish file-name', onclick: () => loadDir(e.path, false) }, icon('folder'), e.name)
          : h('span', { class: 'file-name' }, icon('file'), e.name),
        h('span', { class: 'muted small tnum' }, e.dir ? (e.files ? plural(e.files, 'file') + ' · ' : '') + bytes(e.size) : bytes(e.size)));
    }) : h('p', { class: 'muted' }, 'This folder is empty.'));
  };
  browse = h('div', null, crumbs, list, picked);
  showPicked();
  loadDir('');

  const zipLink = () => `/api/sources/${id}/download?` + new URLSearchParams([['snapshot', R.snap], ...[...R.picks].map((x) => ['path', x])]).toString();
  const dl = h('a', { class: 'btn', href: '#', onclick: (e) => { e.currentTarget.href = zipLink(); } }, 'Download as zip');

  const where = { v: 'original' };
  const live = agents.filter((a) => !a.revoked);
  const agentSel = select(live.map((a) => [String(a.id), agentTitle(a)]), String(src.agentId), { name: 'restore-server' });
  const folder = input({ name: 'restore-folder', code: true, autocomplete: 'off', value: (agents.find((a) => a.id === src.agentId) || {}).os?.startsWith('windows') ? `C:\\Restored\\${src.name}-${yyyymmdd()}` : `/root/restored/${src.name.replace(/[^\w.-]+/g, '-')}-${yyyymmdd()}` });
  const folderBox = h('div', { hidden: true, class: 'row' }, field('On server', agentSel), field('Folder', folder, 'Files keep their full path inside it, so nothing gets mixed up.'));
  const isDocker = src.spec.kind === 'docker';
  const origNote = h('div', { class: 'banner warn' }, isDocker
    ? 'Each volume’s contents are replaced with the backup, exactly as it was, and mounted folders are put back where they came from (files with the same names are replaced). The containers using them are stopped first and started again after. To get back part of a volume, restore to a new folder.'
    : 'Files with the same names are replaced with the backed-up versions. Other files are left alone. Owners and permissions are kept.');
  const whereChoice = choices([
    isDocker ? { value: 'original', title: 'Put the app’s data back', desc: 'Replace the volumes and mounted folders on ' + (agents.find((a) => a.id === src.agentId) ? agentTitle(agents.find((a) => a.id === src.agentId)) : 'the server') + ' with this backup.' }
      : { value: 'original', title: 'Where it came from', desc: 'Put files back in their original place on ' + (agents.find((a) => a.id === src.agentId) ? agentTitle(agents.find((a) => a.id === src.agentId)) : 'the server') + '.' },
    { value: 'folder', title: 'A new folder', desc: 'Restore next to the original, or onto another server.', badge: 'safest', badgeCls: 'ok' },
  ], { value: 'original', small: true, label: 'Where to restore', onPick: (v) => { where.v = v; folderBox.hidden = v !== 'folder'; origNote.hidden = v !== 'original'; } });
  const go = btn('Restore', busy(async () => {
    const body = { snapshotId: R.snap, paths: [...R.picks] };
    if (where.v === 'folder') {
      if (!folder.value.trim()) return fieldError(folder, 'Enter the folder to restore into.');
      body.folder = folder.value.trim(); body.agentId = Number(agentSel.value);
    } else if (!(await confirmDlg(isDocker
      ? `Put ${R.picks.size ? 'the chosen volumes and folders' : 'every volume and mounted folder in this backup'} back? Volumes’ current contents are deleted, files with the same names in mounted folders are replaced, and the containers using them restart.`
      : `Put ${R.picks.size ? plural(R.picks.size, 'selection') : 'everything in this backup'} back where it came from? Files with the same names are replaced.`, isDocker ? 'Put it back' : 'Restore', { title: 'Restore in place?' }))) return;
    const res = await post(`/sources/${id}/restore`, body);
    fill(out, restoreProgress(res.jobId, 'Restoring'));
  }, 'Starting…'), 'primary');

  return h('div', null, head,
    card('1. Choose a backup', pointList),
    card('2. Choose files', h('p', { class: 'muted small' }, 'Tick files or folders, or leave everything unticked to restore it all.'), browse),
    card('3. Get them back', h('div', { class: 'form-actions start' }, dl, h('span', { class: 'muted small' }, 'Downloads your selection through the dashboard.')),
      h('h3', null, 'Or restore on a server'), whereChoice, origNote, folderBox, h('div', { class: 'form-actions start' }, go), out));
}

// ---------------------------------------------------------- item detail

async function pageSource(id) {
  let st, jobs, proofs, repos;
  try {
    [st, jobs, proofs, repos] = await Promise.all([
      api(`/sources/${id}`), api(`/jobs?${qs({ source: id, limit: 50 })}`), api(`/proofs?${qs({ source: id, limit: 100 })}`),
      api('/repositories').catch(() => []),
    ]);
  } catch (e) {
    if (e.status !== 404) throw e;
    document.title = 'Item removed · BackupProof';
    return h('div', null, pageHead('Item removed'),
      card(null, empty('This item isn’t protected any more: it was removed. Its earlier backups and proofs are kept in storage and under Proof history.',
        h('div', { class: 'form-actions' }, h('a', { class: 'btn', href: '#/proofs' }, 'Proof history'), h('a', { class: 'btn primary', href: '#/protected' }, 'Go to Protected')))));
  }
  const src = st.source, k = kindInfo(src.spec), isImp = src.spec && src.spec.kind === 'import';
  if (S.detailJob && S.detailJob.source !== id) S.detailJob = null;
  const drill = st.lastDrill, dp = pred(drill), backup = st.lastBackup, bp = pred(backup);
  const statusInfo = STATUS[st.status] || { cls: '', label: st.status };
  if (st.running) every(5000, autoRefresh);
  const after = (jid) => { S.detailJob = { source: id, id: jid }; reload(); };

  const head = h('div', { class: 'page-head' },
    h('div', { class: 'page-title' },
      backLink('#/protected', 'Protected'),
      h('h1', { class: 'with-ico', tabindex: '-1' }, icon(k.icon), h('span', null, src.name)),
      h('p', { class: 'muted lede' }, k.label)),
    canOperate() ? h('div', { class: 'btns' },
      isAdmin() ? h('a', { class: 'btn primary', href: `#/sources/${id}/restore` }, 'Restore…') : null,
      h('a', { class: 'btn', href: `#/sources/${id}/edit` }, 'Edit (advanced)'),
      isAdmin() ? btn('Remove', busy(() => removeItem(id, src.name, () => { location.hash = '#/protected'; })), 'danger') : null) : null);

  const rto = drill ? (drill.rtoMs ?? dp.rtoMs) : null;
  const hero = h('section', { class: 'card hero ' + statusInfo.cls },
    h('div', { class: 'hero-status' }, h('h2', { class: 'hero-label' }, glyphLabel(statusInfo.label)), h('p', { class: 'hero-reason' }, st.reason || statusInfo.help || '')),
    runningLine(st.running, isImp),
    h('dl', { class: 'kv' },
      h('dt', null, isImp ? 'Last conversion' : 'Last backup'),
      h('dd', null, backup ? [timeEl(backup.created), backup.passed === false ? ' — failed' : '', bp.bytes != null ? ` · ${bytes(bp.bytes)}` : '', bp.entries != null ? ` · ${plural(bp.entries, 'file')}` : ''] : 'not yet'),
      h('dt', null, 'Last restore test'),
      h('dd', null, drill ? [h('span', { class: drill.passed ? 'fact-ok' : 'fact-bad' }, drill.passed ? 'passed ' : 'failed '), timeEl(drill.created)] : 'not yet'),
      h('dt', null, 'Time to restore'), h('dd', null, rto != null ? durWords(rto) : '—'),
      h('dt', null, isImp ? 'Converts' : 'Backs up'), h('dd', null, cronWords(src.backupCron), src.nextBackup && src.backupCron !== 'manual' ? [' (next ', timeEl(src.nextBackup), ')'] : ''),
      h('dt', null, 'Tests a restore'), h('dd', null, cronWords(src.drillCron), src.nextDrill ? [' (next ', timeEl(src.nextDrill), ')'] : ''),
      h('dt', null, 'Keeps'), h('dd', null, retentionWords(src.retention)),
      h('dt', null, 'Server'), h('dd', null, st.agentName || '—'),
      h('dt', null, 'Stored in'), h('dd', null, st.repoName || '—'),
      h('dt', null, 'Restore tests run on'), h('dd', null, st.verifierName ? `${st.verifierName} (a different server)` : 'the same server'),
      src.enabled === false ? [h('dt', null, 'Schedule'), h('dd', null, 'Paused — nothing runs automatically')] : null),
    h('div', { class: 'form-actions start' }, runButtons(id, st.running, after, true, isImp)));
  hero.setAttribute('aria-label', 'Status');

  const checks = dp.checks || [];
  const rootsMatch = dp.restoredRoot && dp.restoredRoot === dp.expectedRoot;
  const drillCard = card('What the last restore test checked', drill ? h('div', null,
    h('p', { class: 'muted' }, `The backup copy from ${day(dp.snapshotTime || drill.created)} was restored into a safe, separate place and checked. `,
      drill.passed ? 'Everything passed.' : 'Something did not pass — see below.'),
    checks.length ? h('ul', { class: 'checks' }, checks.map((c) => h('li', null,
      h('span', { class: 'ico ' + (c.passed ? 'ok' : 'bad') }, mark(c.passed)),
      h('div', null, h('div', null, checkWords(c.name)), (!c.passed || c.name === 'database-dumps') && c.detail ? h('div', { class: 'detail' }, c.detail) : null),
      h('span', { class: 'muted small' }, c.durationMs ? durWords(c.durationMs) : '')))) : h('p', { class: 'muted' }, 'No checks were recorded.'),
    tech(h('dl', { class: 'kv' },
      h('dt', null, 'Backup copy ID'), h('dd', null, h('code', { class: 'break' }, drill.snapshotId || '—')),
      h('dt', null, 'Restore time (RTO)'), h('dd', null, dur(rto)),
      h('dt', null, 'Test environment'), h('dd', null, dp.sandbox || '—'),
      h('dt', null, 'Tested by'), h('dd', null, dp.verifier || drill.signer || '—'),
      h('dt', null, 'Expected root'), h('dd', null, h('code', { class: 'break' }, dp.expectedRoot || '—')),
      h('dt', null, 'Restored root'), h('dd', null, h('code', { class: 'break' }, dp.restoredRoot || '—'), ' ', rootsMatch ? h('span', { class: 'pill ok' }, h('span', { 'aria-hidden': 'true' }, '✓ '), 'match') : h('span', { class: 'pill bad' }, h('span', { 'aria-hidden': 'true' }, '✗ '), 'differ'))),
    h('h3', null, 'Raw checks'),
    h('ul', { class: 'checks' }, checks.map((c) => h('li', null,
      h('span', { class: 'ico ' + (c.passed ? 'ok' : 'bad') }, mark(c.passed)),
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
      btn('Show details', () => { S.detailJob = { source: id, id: j.id }; showJobLog(logBox, id, j.id, isImp); logBox.scrollIntoView({ behavior: motionOK() ? 'smooth' : 'auto', block: 'nearest' }); }, 'sm', { 'aria-label': `Show details: ${jobWord(j.kind, isImp)}, ${absTime(j.created)}` }),
    ], JH))) : empty('Nothing has run yet.'),
    logBox);
  if (S.detailJob) showJobLog(logBox, id, S.detailJob.id, isImp);
  else if (st.running) showJobLog(logBox, id, st.running.id, isImp);

  const PH = ['When', 'What', 'Result', 'Time to restore', ''];
  const proofsCard = h('section', { class: 'card' }, cardHead('Proof history'),
    h('p', { class: 'muted small' }, 'Each backup and restore test leaves a proof: a tamper-proof record that it really happened.'),
    (proofs || []).length ? details(`Show ${plural(proofs.length, 'proof')}`, table(PH, proofs.map((p) => proofRow(p, PH, false)))) : empty('No proofs yet.'));

  // Second copy (3-2-1): another storage that gets a copy of every backup.
  const lastCopy = (proofs || []).find((p) => p.kind === 'copy' && p.passed);
  const copyRepo = src.copyRepoId ? (repos || []).find((r) => r.id === src.copyRepoId) : null;
  const copySel = select([['', 'No second copy'], ...(repos || []).filter((r) => r.id !== src.repoId).map((r) => [String(r.id), r.name])], src.copyRepoId ? String(src.copyRepoId) : '', { name: 'copy-storage', 'aria-label': 'Second storage' });
  const copyCard = h('section', { class: 'card' }, cardHead('Second copy'),
    h('p', { class: 'muted small' }, 'Keep a copy of every backup in another storage, ideally somewhere else entirely, such as a different cloud. Each copy gets its own signed proof.'),
    copyRepo ? h('p', null, 'Copies go to ', h('strong', null, copyRepo.name), '. ', lastCopy ? ['Last copied ', timeEl(lastCopy.created), '.'] : h('span', { class: 'muted' }, 'Not copied yet.')) : null,
    isAdmin() && !(repos || []).some((r) => r.id !== src.repoId) ? h('p', { class: 'hint' }, 'To keep a second copy, first ', h('a', { href: '#/storage/new' }, 'add another storage'), ', for example a different cloud provider.') : null,
    isAdmin() && (repos || []).some((r) => r.id !== src.repoId) ? h('div', { class: 'form-actions start' }, copySel, btn('Save', busy(async () => {
      await put(`/sources/${id}/copy`, { repoId: copySel.value ? Number(copySel.value) : null });
      toast(copySel.value ? 'Second copy turned on. The newest backup is being copied now.' : 'Second copy turned off', 'ok');
      reload();
    }), 'sm')) : null);
  return h('div', null, head, hero, h('div', { class: 'grid2' }, drillCard, backupCard), copyCard, jobsCard, proofsCard);
}

async function showJobLog(box, sourceId, jobId, isImport) {
  const gen = S.gen;
  const tok = {};
  box.pvTok = tok; // a newer viewer in the same box cancels this one
  const pre = h('pre', { class: 'log', tabindex: '0', 'aria-label': 'Log' }, 'Loading…');
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
    } catch (err) {
      if (timer) clearInterval(timer);
      timer = null;
      pre.textContent = 'Couldn’t load the details: ' + err.message;
      if (box.isConnected) box.append(h('p', null, btn('Try again', () => showJobLog(box, sourceId, jobId, isImport), 'sm')));
    }
  };
  await tick();
}

function proofRow(p, headers, withSource) {
  const rto = p.rtoMs != null ? p.rtoMs : null;
  const what = p.kind === 'drill' ? 'Restore test' : p.kind === 'backup' ? 'Backup' : p.kind === 'copy' ? 'Copy in second storage' : p.kind;
  const ctx = `${what} proof${p.sourceName ? ' for ' + p.sourceName : ''}, ${absTime(p.created)}`;
  const cells = [
    timeEl(p.created),
    what,
    passPill(p.passed),
    rto != null ? durWords(rto) : '—',
    h('div', { class: 'btns' }, btn('View', busy(() => viewProof(p.id), 'Opening…'), 'sm', { 'aria-label': 'View ' + ctx }),
      h('a', { class: 'btn sm', href: `/api/proofs/${p.id}/bundle`, download: `proof-${p.id}.json`, 'aria-label': 'Download ' + ctx }, 'Download')),
  ];
  if (withSource) cells.splice(1, 0, p.sourceId ? h('a', { href: `#/sources/${p.sourceId}` }, p.sourceName) : p.sourceName || '—');
  return tr(cells, headers);
}

function hostOf(u) { try { return new URL(u).host; } catch { return u || ''; } }

async function viewProof(id) {
  const p = await api(`/proofs/${id}`);
  const env = p.envelope || {};
  const what = p.kind === 'drill' ? 'Restore test' : p.kind === 'backup' ? 'Backup' : p.kind === 'copy' ? 'Copy in second storage' : p.kind;
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
    h('pre', { class: 'log', tabindex: '0', 'aria-label': 'Proof contents' }, JSON.stringify(pred(p), null, 2)))),
  [h('a', { class: 'btn primary', href: `/api/proofs/${p.id}/bundle`, download: `proof-${p.id}.json` }, 'Download proof file')]);
}

// ------------------------------------------------------- live progress
// Follows a backup (or conversion) job, then the restore test the server
// starts automatically after the first backup, in plain words.

function jobProgress(sourceId, jobId, name, isImport) {
  const line = h('div', { class: 'progress', role: 'status' });
  const pre = h('pre', { class: 'log', tabindex: '0', 'aria-label': 'Log' }, '');
  const el = h('div', { class: 'prog' }, h('div', { class: 'prog-name' }, h('a', { href: `#/sources/${sourceId}` }, name)), line, details('Show details', pre));
  const gen = S.gen;
  const started = Date.now();
  let phase = 'backup', cur = jobId, findStart = 0, first = true;
  const logs = {};
  let last = '';
  const set = (cls, text, spin, extra) => {
    if (last === cls + text) return; // don't re-announce the same words
    last = cls + text;
    line.className = 'progress ' + cls;
    fill(line, spin ? h('span', { class: 'spinner', 'aria-hidden': 'true' }) : h('span', { class: 'prog-ico', 'aria-hidden': 'true' }, cls === 'ok' ? '✓' : cls === 'bad' ? '✗' : '•'), h('span', null, text), extra || null);
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
          else if (j.state === 'failed') { set('bad', (isImport ? 'The conversion didn’t finish: ' : 'The backup didn’t finish: ') + (j.error || 'no reason was given. Open “Show details” below to read the log.')); return; }
          else if (j.state === 'succeeded') { phase = 'find'; findStart = Date.now(); set('', isImport ? 'Converted. Starting the restore test…' : 'Backed up. Starting the restore test…', true); }
        } else {
          if (j.state === 'queued' || j.state === 'running') set('', 'Testing the restore…', true);
          else {
            const st = await api(`/sources/${sourceId}`);
            if (j.state === 'succeeded' && st.status === 'proven') { set('ok', 'Protected and restore tested'); return; }
            set('bad', 'The restore test did not pass: ' + (j.error || st.reason || 'see details'), false, h('a', { href: `#/sources/${sourceId}`, class: 'small' }, 'Open the item to see what failed'));
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
    setTimeout(poll, 2000);
  };
  const poll = () => { if (document.hidden) { setTimeout(poll, 2000); return; } tick(); };
  setTimeout(poll, 300);
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
    path: inp({ name: 'path', code: true, value: ib.type === 'local' ? ib.path || '' : '', placeholder: mode === 'import' ? '/mnt/backups…' : '/mnt/backup-disk…' }),
    bucket: inp({ name: 'bucket', code: true, value: ib.bucket || '', autocomplete: 'off', placeholder: 'my-backups…' }),
    prefix: inp({ name: 'prefix', code: true, value: initial ? ib.prefix || '' : mode === 'repo' ? 'backupproof' : '', placeholder: mode === 'repo' ? 'backupproof…' : 'backups/server1…' }),
    accessKey: inp({ name: 'access-key', code: true, value: ic.accessKey || '', autocomplete: 'off' }),
    secretKey: inp({ name: 'secret-key', type: 'password', value: ic.secretKey || '', autocomplete: 'off' }),
    b2Endpoint: inp({ name: 'b2-endpoint', code: true, inputmode: 'url', value: type === 'b2' ? ihost : '', placeholder: 's3.us-west-004.backblazeb2.com…' }),
    s3Region: select([...S3_REGIONS.map((r) => [r, r]), ['_other', 'Other…']], type === 's3' ? (S3_REGIONS.includes(ib.region) ? ib.region : ib.region ? '_other' : 'us-east-1') : 'us-east-1', { name: 's3-region' }),
    s3RegionOther: inp({ name: 's3-region-other', code: true, value: type === 's3' && !S3_REGIONS.includes(ib.region) ? ib.region || '' : '', placeholder: 'ap-east-1…' }),
    r2Account: inp({ name: 'r2-account', code: true, value: type === 'r2' ? ihost.split('.')[0] : '', autocomplete: 'off' }),
    wasabiRegion: select(WASABI_REGIONS.map((r) => [r, r]), type === 'wasabi' ? ib.region : 'us-east-1', { name: 'wasabi-region' }),
    endpoint: inp({ name: 'endpoint', type: 'url', code: true, value: type === 'other' ? ib.endpoint || '' : '', placeholder: 'https://s3.example.com…' }),
    region: inp({ name: 'region', code: true, value: type === 'other' ? ib.region || '' : '', placeholder: 'us-east-1…' }),
    host: inp({ name: 'host', code: true, value: ib.host || '', placeholder: 'backup.example.com…' }),
    port: inp({ name: 'port', type: 'number', inputmode: 'numeric', min: 1, max: 65535, value: ib.port || 22 }),
    user: inp({ name: 'sftp-user', code: true, value: ib.user || '', autocomplete: 'off' }),
    sftpPass: inp({ name: 'sftp-password', type: 'password', value: ic.password || '', autocomplete: 'off' }),
    privateKey: ta({ name: 'private-key', rows: 3, placeholder: '-----BEGIN OPENSSH PRIVATE KEY-----…', spellcheck: 'false', autocapitalize: 'off', autocomplete: 'off' }, ic.privateKey),
    sftpPath: inp({ name: 'sftp-path', code: true, value: ib.type === 'sftp' ? ib.path || '' : '', placeholder: mode === 'repo' ? '/home/me/backupproof…' : '/backups…' }),
    hostKey: ta({ name: 'host-key', rows: 2, placeholder: 'backup.example.com ssh-ed25519 AAAA…', spellcheck: 'false', autocapitalize: 'off' }, ib.hostKey),
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
    field('Folder inside the bucket', f.prefix, mode === 'repo' ? 'Optional. Keeps BackupProof files tidy in their own folder.' : 'Where the old backups are, for example the folder your backup script uploads to. Leave empty if the backups are at the top of the bucket.'));

  const renderType = () => {
    const t = type;
    const drives = (inv.drives || []);
    add(clear(box), [
      t === 'local' ? [
        mode === 'repo' && drives.length ? [h('p', { class: 'label' }, 'Disks on ' + (agent ? agentTitle(agent) : 'the server')),
          choices(drives.map((d) => ({ value: d.path, title: d.label || d.path, desc: d.path, icon: 'disk', extra: d.total ? spaceBar(d.free, d.total) : null })),
            { value: f.path.value.trim(), small: true, label: 'Disks', onPick: (v) => { f.path.value = v; updWhere(); changed(); } })] : null,
        h('div', { class: 'inline-field' },
          field(mode === 'repo' ? 'Folder or disk' : 'Folder where the old backups are', f.path,
            builtin ? (mode === 'repo' ? 'Pick a disk above, browse, or type a folder path.' : 'Browse or type the folder path.') : 'Type the folder path on that server (for example D:\\Backups or /mnt/backup).'),
          browseBtn(f.path, () => f.path.value.trim())),
        where,
        mode === 'repo' ? h('div', { class: 'banner warn' }, 'Keep at least one copy somewhere else too (another disk or the cloud). A backup on the same disk won’t help if that disk breaks.') : null,
      ] : null,
      t === 'b2' ? [
        keyFields('Key ID', 'Application Key', 'From Backblaze: App Keys → Add a New Application Key.'),
        bucketRow(),
        field('Endpoint', f.b2Endpoint, 'Shown on the bucket page in Backblaze.'),
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
        h('div', { class: 'row' }, field('Endpoint (address)', f.endpoint, 'From your provider, e.g. https://fsn1.your-objectstorage.com'), field('Region', f.region, 'Often “us-east-1” if your provider doesn’t mention one.')),
        keyFields('Access key', 'Secret key'),
        bucketRow(),
      ] : null,
      t === 'sftp' ? [
        h('div', { class: 'row' }, field('Server address', f.host, 'Name or IP address of the server.'), field('Port', f.port, 'Usually 22.'), field('Username', f.user)),
        h('div', { class: 'row' }, field('Password', f.sftpPass, 'Leave empty if you use a private key. Kept encrypted on this server.'), field('Private key (optional)', f.privateKey, 'Paste the whole key file if you log in with a key.')),
        field(mode === 'repo' ? 'Folder on the server' : 'Folder where the old backups are', f.sftpPath, mode === 'repo' ? 'Created if it doesn’t exist.' : null),
        field('Server fingerprint', f.hostKey, 'Proves you’re talking to the right server. Run “ssh-keyscan your-server” on any server and paste one of the lines.'),
      ] : null,
    ]);
    updWhere();
  };

  const typePicker = choices(STORE_TYPES.map((o) => ({ ...o, desc: o.value === 'local' && mode === 'import' ? 'A folder or disk on this server.' : o.desc })),
    { value: type, small: true, label: 'Type of storage', onPick: (v) => { type = v; renderType(); changed(); } });
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
  // setLocation points the form at another folder of the same storage.
  const setLocation = (b) => {
    if (b.type === 'local') f.path.value = b.path || '';
    else if (b.type === 'sftp') f.sftpPath.value = b.path || '';
    else if (b.type === 's3') f.prefix.value = b.prefix || '';
  };
  return { el, collect, setLocation, kind: () => type, onChange: (fn) => listeners.push(fn) };
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
// title: optional heading shown above the form.
function storageChooser({ agent, onSaved, onCancel, title }) {
  const sf = storageFields({ mode: 'repo', agent });
  const name = input({ name: 'storage-name', required: true, autocomplete: 'off' });
  let nameTouched = false;
  name.addEventListener('input', () => { nameTouched = true; });
  const autoName = () => { if (!nameTouched) { const c = sf.collect(); name.value = c.label || name.value; } };
  sf.onChange(autoName);

  // ransomware protection
  let lock = '';
  const lockDays = input({ name: 'lock-days', type: 'number', inputmode: 'numeric', min: 1, value: 30 });
  const lockBox = h('div', null,
    choices([
      { value: '', title: 'Off', desc: 'Backups can be deleted normally.' },
      { value: 'GOVERNANCE', title: 'Protect for some days', desc: 'Nobody can delete or change backups for N days. An admin of the bucket can still lift it.' },
      { value: 'COMPLIANCE', title: 'Locked — nobody can delete', desc: 'Not even you, the bucket owner, or the provider, until the days are over. Use with care.' },
    ], { value: '', small: true, label: 'Ransomware protection', onPick: (v) => { lock = v; lockExtra.classList.toggle('hidden', !v); } }),
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
  const pwShow = h('code', { class: 'pw', 'aria-label': 'Encryption password' }, pw);
  const own1 = input({ name: 'encryption-password', type: 'password', autocomplete: 'new-password', minlength: 12 });
  const own2 = input({ name: 'encryption-password-2', type: 'password', autocomplete: 'new-password' });
  const saved = checkbox('I saved the password somewhere safe — without it nobody can restore', false);
  const kitBtn = btn('Download recovery kit', () => {
    const c = sf.collect();
    if (c.error) { toast(c.error, 'bad'); return; }
    const kindLabel = (STORE_TYPES.find((x) => x.value === sf.kind()) || {}).title || '';
    download(`backupproof-recovery-kit-${(name.value || 'storage').replace(/[^\w.-]+/g, '-')}.txt`, recoveryKit(name.value, kindLabel, c.backend, currentPw()));
  }, 'sm');
  const regen = btn('Make a new one', async () => {
    const ok = await confirmDlg('Make a new encryption password? If you already wrote down or downloaded the current one, that copy won’t work for this storage — you’ll need to save the new one instead.', 'Make a new one', { danger: false, title: 'Replace the password?' });
    if (!ok) return;
    pw = genPassword();
    pwShow.textContent = pw;
    saved.cb.checked = false;
    toast('New password made. Save it before you continue.', 'ok');
  }, 'sm');
  const autoBox = h('div', null,
    h('div', { class: 'pwbox' }, pwShow, h('div', { class: 'btns' }, copyBtn(() => pw, 'Copy', 'Copy the encryption password'), regen, kitBtn)),
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
  const result = h('div', { 'aria-live': 'polite' });
  const backendWithLock = (c) => {
    const b = { ...c.backend };
    if (lock && isS3Like(sf.kind())) { b.objectLockMode = lock; b.objectLockDays = num(lockDays.value) || 30; }
    return b;
  };
  const showTest = (r, c) => {
    fill(result, h('div', { class: 'banner ' + (r.ok ? (r.existing && r.passwordOk !== true ? 'warn' : 'ok') : 'bad') }, glyph(r.ok), r.message,
      r.ok ? null : h('p', { class: 'small' }, 'Check the details above, then click “Test connection” again.')));
    if (r.existing && r.passwordOk !== true) {
      result.append(h('p', { class: 'hint' }, 'This storage already contains BackupProof backups — enter its existing password instead.'));
      if (pwMode !== 'own') setMode('own');
    }
    if (r.oldBackups) {
      result.append(h('div', { class: 'callout' }, h('strong', null, `Found old ${r.oldBackups} backups here.`), ' ',
        'You can convert them so they are restore-tested and proven too. ',
        btn('Convert them', () => {
          S.importPrefill = { format: /restic/i.test(r.oldBackups) ? 'restic' : /kopia/i.test(r.oldBackups) ? 'kopia' : 'files', backend: c.backend, credentials: c.credentials };
          S.dirty = false;
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
    if (!quiet) fill(result, h('p', { class: 'running' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), 'Checking the connection…'));
    const r = await post('/repositories/test', { backend: backendWithLock(c), credentials: c.credentials, password: pwMode === 'own' ? own1.value : '' });
    showTest(r, c);
    return r;
  };
  const err = errBox();
  const save = async () => {
    err.textContent = '';
    clearErrors(el);
    const c = sf.collect();
    if (c.error) return showErr(err, c.error);
    if (!name.value.trim()) return fieldError(name, 'Give this storage a name.');
    if (pwMode === 'own') {
      if (own1.value.length < 12) return fieldError(own1, 'The password must be at least 12 characters.');
      if (own1.value !== own2.value) return fieldError(own2, 'The two passwords are different. Type the same password twice.');
    }
    if (!saved.cb.checked) return fieldError(saved.cb, 'Save the password first and tick this box — without it nobody can restore.');
    if (lock === 'COMPLIANCE' && isS3Like(sf.kind())) {
      const days = num(lockDays.value) || 30;
      const ok = await confirmDlg(`Lock every backup copy for ${plural(days, 'day')}? Nobody — not you, the bucket owner or the provider — can delete them until the days are over, and this can’t be undone.`, 'Lock the backups');
      if (!ok) return;
    }
    const r = await test(true);
    if (!r || !r.ok) return showErr(err, 'The connection test failed — read the message above, fix the details, and save again.');
    if (r.existing && r.passwordOk !== true) return showErr(err, 'This storage already has backups. Enter its existing password (the test must say it works).');
    const res = await post('/repositories', { name: name.value.trim(), backend: backendWithLock(c), password: currentPw(), credentials: c.credentials });
    toast('Storage added', 'ok');
    if (onSaved) onSaved(res.id);
  };

  const el = h('div', { class: 'storage-chooser' },
    title ? h('h3', null, title) : null,
    sf.el,
    field('Name', name, 'So you can recognise it later.'),
    lockSection,
    h('fieldset', null, h('legend', null, 'Encryption password'),
      h('p', { class: 'muted small' }, 'Everything is encrypted before it leaves the server. The storage provider can’t read your files — but you need this password to restore if this server is ever lost.'),
      autoBox, ownBox, modePick, saved.el),
    result, err,
    h('div', { class: 'form-actions' },
      onCancel ? btn('Cancel', onCancel) : null,
      btn('Test connection', busy(() => test(false), 'Testing…')),
      btn('Save storage', busy(save, 'Saving…'), 'primary')));
  autoName();
  showLock();
  return el;
}

async function pageStorageNew() {
  if (!canOperate()) return h('div', { class: 'banner warn' }, 'Your account can only view. Ask an administrator to add storage.');
  const agents = ((await api('/agents')) || []).filter((a) => !a.revoked);
  const agent = agents.find((a) => a.builtin) || agents[0] || null;
  return guardDirty(h('div', null,
    backLink('#/repositories', 'Storage'),
    pageHead('Add backup storage', 'Choose where encrypted backup copies are kept.'),
    h('section', { class: 'card' }, storageChooser({ agent, onSaved: () => { S.dirty = false; location.hash = '#/repositories'; }, onCancel: () => { location.hash = '#/repositories'; } }))));
}

// sizeChart draws storage size over time (one bar per health check).
function sizeChart(stats) {
  const NS = 'http://www.w3.org/2000/svg';
  const W = 160, H = 44, n = stats.length, gap = 3;
  const max = Math.max(...stats.map((x) => x.bytes), 1);
  const bw = Math.max(3, Math.min(14, (W - gap * (n - 1)) / Math.max(n, 1)));
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
  svg.setAttribute('class', 'size-chart');
  svg.setAttribute('role', 'img');
  const first = stats[0], last = stats[n - 1];
  svg.setAttribute('aria-label', n > 1 ? `Size grew from ${bytes(first.bytes)} on ${day(first.time)} to ${bytes(last.bytes)} on ${day(last.time)}` : `Size ${bytes(last.bytes)} on ${day(last.time)}`);
  stats.forEach((x, i) => {
    const h = Math.max(2, Math.round((x.bytes / max) * (H - 2)));
    const rect = document.createElementNS(NS, 'rect');
    rect.setAttribute('x', W - (n - i) * (bw + gap) + gap);
    rect.setAttribute('y', H - h);
    rect.setAttribute('width', bw);
    rect.setAttribute('height', h);
    rect.setAttribute('rx', 2);
    rect.setAttribute('class', x.ok ? 'bar' : 'bar bad');
    const t = document.createElementNS(NS, 'title');
    t.textContent = `${day(x.time)}: ${bytes(x.bytes)}${x.ok ? '' : ' (problems found)'}`;
    rect.append(t);
    svg.append(rect);
  });
  return svg;
}

function healthLine(stats) {
  const last = stats && stats[stats.length - 1];
  if (!last) return h('p', { class: 'small muted' }, 'No health check yet. One runs every week, or click “Check now”.');
  if (last.ok) return h('p', { class: 'small' }, h('span', { class: 'fact-ok' }, 'Health check passed '), timeEl(last.time), last.readBlobs ? ` · re-read ${plural(last.readBlobs, 'sample')}` : '');
  return h('p', { class: 'small' }, h('span', { class: 'fact-bad' }, 'Health check found problems '), timeEl(last.time),
    ` · ${plural(last.missing, 'missing piece')}, ${plural(last.corrupt, 'damaged piece')}. Run a restore test and check the storage provider.`);
}

async function pageRepositories() {
  const [repos0, sources, stats] = await Promise.all([api('/repositories'), api('/sources').catch(() => []), api('/repositories/stats').catch(() => ({}))]);
  const repos = repos0 || [];
  const usedBy = (id) => (sources || []).filter((x) => x.repoId === id);
  const removeRepo = async (r) => {
    const ok = await confirmDlg(h('div', null,
      h('p', null, `BackupProof will stop using “${r.name}”. The backups already in it are not deleted.`),
      h('p', null, h('strong', null, 'Keep its recovery kit.'), ' Its encryption password is removed from this dashboard, and without it nobody can open those backups or add this storage again.')),
    'Remove storage', { title: 'Remove this storage?' });
    if (!ok) return;
    await del(`/repositories/${r.id}`);
    toast(`Removed “${r.name}”`, 'ok');
    reload();
  };
  return h('div', null,
    pageHead('Storage', 'Where your encrypted backup copies are kept.', isAdmin() ? h('a', { class: 'btn primary', href: '#/storage/new' }, '+ Add storage') : null),
    repos.length ? h('div', { class: 'cards' }, repos.map((r) => {
      const k = repoKind(r.backend);
      const lw = lockWords(r.backend);
      return h('section', { class: 'card mini' },
        h('div', { class: 'mini-head' }, h('span', { class: 'item-ico' }, icon(k.icon)), h('div', null, h('h2', null, r.name), h('div', { class: 'muted small' }, k.label))),
        h('p', { class: 'break small' }, repoLocation(r.backend)),
        h('div', { class: 'btns' }, lw ? h('span', { class: 'pill ok' }, lw) : h('span', { class: 'pill' }, 'No ransomware protection'), h('span', { class: 'pill' }, 'Encrypted')),
        (() => {
          const users = usedBy(r.id);
          return h('p', { class: 'small used-by' }, users.length
            ? ['Used by ', joinWords(users.map((x) => x.name))]
            : h('span', { class: 'muted' }, 'Not used by any item'));
        })(),
        (() => {
          const st = (stats && stats[String(r.id)]) || [];
          const last = st[st.length - 1];
          const monthAgo = st.filter((x) => toDate(x.time) < Date.now() - 30 * 864e5).pop();
          return h('div', { class: 'storage-health' },
            last ? h('div', { class: 'size-row' },
              h('div', null, h('div', { class: 'size-now' }, bytes(last.bytes)),
                h('div', { class: 'small muted' }, monthAgo ? `${last.bytes >= monthAgo.bytes ? '+' : '−'}${bytes(Math.abs(last.bytes - monthAgo.bytes))} in 30 days` : `${plural(last.snapshots, 'backup copy')}`.replace('copys', 'copies'))),
              st.length > 1 ? sizeChart(st) : null) : null,
            healthLine(st),
            canOperate() && usedBy(r.id).length ? btn('Check now', busy(async () => {
              await post(`/repositories/${r.id}/check`);
              toast('Health check started. It reads back a sample of the stored data.', 'ok');
            }, 'Starting…'), 'sm') : null);
        })(),
        isAdmin() ? h('div', { class: 'form-actions start' }, usedBy(r.id).length
          ? h('p', { class: 'hint' }, 'To remove this storage, first remove the items that use it.')
          : btn('Remove', busy(() => removeRepo(r)), 'sm danger quiet', { 'aria-label': `Remove storage ${r.name}` })) : null,
        tech(h('dl', { class: 'kv' },
          h('dt', null, 'Address'), h('dd', null, h('code', { class: 'break' }, repoUrl(r.backend))),
          h('dt', null, 'Storage ID'), h('dd', null, h('code', { class: 'break' }, r.repoId || 'created on first backup')),
          h('dt', null, 'Added'), h('dd', null, absTime(r.created)))));
    })) : h('section', { class: 'card' }, empty('No backup storage yet. Add a disk, a cloud bucket or another server to keep your backups in.',
      isAdmin() ? h('a', { class: 'btn primary', href: '#/storage/new' }, 'Add storage') : h('p', { class: 'hint' }, 'Ask an administrator to add storage.'))));
}

// ------------------------------------------------------ protect wizard

const BACKUP_CHOICES = [
  { value: '@hourly', title: 'Every hour', desc: 'For things that change all day.' },
  { value: '0 2 * * *', title: 'Every night', desc: `At ${clock(2)}.`, badge: 'recommended', badgeCls: 'ok' },
  { value: '@every 6h', title: 'Every 6 hours', desc: '4 times a day.' },
  { value: '0 3 * * 0', title: 'Every week', desc: `Sundays at ${clock(3)}.` },
  { value: 'custom', title: 'Custom', desc: 'Advanced: your own schedule.' },
];
const DRILL_CHOICES = [
  { value: 'daily', title: 'Every day', desc: `At ${clock(5)}.`, cron: '0 5 * * *', maxAge: 26, words: 'every day' },
  { value: 'weekly', title: 'Every week', desc: `Sundays at ${clock(4)}.`, cron: '0 4 * * 0', maxAge: 192, words: 'every week', badge: 'recommended', badgeCls: 'ok' },
  { value: 'monthly', title: 'Every month', desc: `On the 1st at ${clock(4)}.`, cron: '0 4 1 * *', maxAge: 768, words: 'every month' },
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

function storageStep({ repos, value, adding, agent, onPick, onAdded, label = 'Backup storage' }) {
  const opts = repos.map((r) => {
    const k = repoKind(r.backend);
    return { value: r.id, title: r.name, desc: `${k.label} · ${repoLocation(r.backend)}`, icon: k.icon, badge: r.backend && r.backend.objectLockMode ? 'Ransomware protection' : null, badgeCls: 'ok' };
  });
  if (isAdmin()) opts.push({ value: 'new', title: 'Add new storage', desc: 'A disk, a cloud bucket (Backblaze B2, S3…) or another server.', icon: 'plus' });
  if (!isAdmin() && !repos.length) return h('div', { class: 'banner warn' }, 'There’s no backup storage yet. Ask an administrator to add one.');
  if (!isAdmin()) adding = false;
  return h('div', null,
    choices(opts, { value: adding ? 'new' : value, onPick, label }),
    adding ? h('div', { class: 'card inset' }, storageChooser({ agent, title: 'New storage', onSaved: onAdded, onCancel: repos.length ? () => onPick(value || repos[0].id) : null })) : null);
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
  const root = guardDirty(h('div'));
  const agent = () => agents.find((a) => a.id === W.agentId) || agents[0];
  const inv = () => inventory(agent());
  const err = errBox();

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
    if (W.what === 'docker') {
      if (W.dockerApp) return 'Docker: ' + W.dockerApp.replace(/^c:/, '');
      const v = [...(W.dockerVols || [])];
      return v.length ? 'Docker volumes: ' + (v.length > 2 ? `${v[0]}, ${v[1]} and ${v.length - 2} more` : joinWords(v)) : '';
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
  const nameInput = input({ name: 'item-name', autocomplete: 'off' });
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
    if (W.what === 'docker') {
      const vols = [...(W.dockerVols || [])];
      if (!W.dockerApp && !vols.length) throw new Error('Choose a Docker app or at least one volume.');
      const docker = { stop: W.dockerStop !== false, mounts: W.dockerMounts !== false };
      if (W.dockerApp && W.dockerApp.startsWith('c:')) docker.containers = [W.dockerApp.slice(2)];
      else if (W.dockerApp) docker.project = W.dockerApp;
      else docker.volumes = vols;
      return [{ name, spec: { kind: 'docker', docker } }];
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
    h('h2', { tabindex: '-1' }, 'Which server has the data?'),
    choices(agents.map((a) => ({
      value: a.id, title: agentTitle(a), badge: a.builtin ? 'built in' : null, icon: 'computer',
      desc: [a.online ? 'Online' : 'Offline', osName(a.os), a.builtin ? 'the server running this dashboard' : a.hostname].filter(Boolean).join(' · '),
    })), { value: W.agentId, label: 'Server', onPick: (v) => { W.agentId = v; W.folders = []; W.site = null; W.dbVal = null; autoName(); } }),
    h('p', { class: 'small' }, h('a', { href: '#/agents', onclick: () => { S.openConnect = true; } }, '+ Connect another server')));

  const foldersForm = () => {
    const sugg = (inv().items || []).filter((i) => i.kind === 'folder');
    const builtin = agent().builtin;
    const listBox = h('div');
    const renderCustom = () => {
      const custom = W.folders.filter((p) => !sugg.some((s) => s.path === p));
      fill(listBox, custom.length ? h('ul', { class: 'path-list', 'aria-label': 'Folders you added' }, custom.map((p) => h('li', null, icon('folder'), h('code', { class: 'break' }, p),
        btn('Remove', () => { W.folders = W.folders.filter((x) => x !== p); renderCustom(); autoName(); newIn.focus(); }, 'sm', { 'aria-label': 'Remove ' + p })))) : null);
    };
    const addPath = (p) => { p = String(p || '').trim(); if (p && !W.folders.includes(p)) W.folders.push(p); renderCustom(); autoName(); };
    const newIn = input({ name: 'folder-path', code: true, placeholder: agent().os && agent().os.startsWith('windows') ? 'C:\\Users\\Me\\Documents…' : '/home/me/documents…' });
    newIn.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); addPath(newIn.value); newIn.value = ''; } });
    const ex = bind(input({ name: 'excludes', code: true, placeholder: '*.tmp, cache…' }), W, 'excludes');
    renderCustom();
    return h('div', null,
      sugg.length ? [h('p', { class: 'label', id: 'sugg-label' }, 'Folders found on ' + agentTitle(agent())),
        h('div', { class: 'check-list', role: 'group', 'aria-labelledby': 'sugg-label' }, sugg.map((i) => {
          const cb = h('input', { type: 'checkbox', checked: W.folders.includes(i.path), onchange: () => { if (cb.checked) addPath(i.path); else { W.folders = W.folders.filter((x) => x !== i.path); autoName(); } } });
          return h('label', { class: 'check-card' }, cb, h('span', null, h('strong', null, i.label), h('span', { class: 'hint block break' }, i.path)));
        }))] : h('p', { class: 'hint' }, inv().collected ? 'No usual folders were found on this server — add one below.' : 'This server hasn’t reported its folders yet — add one below.'),
      listBox,
      h('div', { class: 'inline-field' },
        field('Add a folder path', newIn, builtin ? 'Type a path and press Enter, or browse.' : 'Type the folder path on that server and press Enter. (Browsing works only for this server.)'),
        btn('Add folder', () => { addPath(newIn.value); newIn.value = ''; newIn.focus(); }, 'sm'),
        builtin ? btn('Browse…', async () => { const p = await pickFolder(''); if (p) addPath(p); }, 'sm') : null),
      details('More options', field('Skip these files', ex, 'Separate with commas. Files and folders matching these names are not backed up, e.g. *.tmp, cache, node_modules.')));
  };

  const websiteForm = () => {
    const items = siteItems();
    const manual = bind(input({ name: 'site-path', code: true, placeholder: '/var/www/mysite…' }), W, 'sitePath', () => { W.site = null; autoName(); });
    const it = siteItem();
    const dbCb = checkbox('Also back up its database (found automatically)', W.siteDb, 'The login is read from the WordPress settings file, so you don’t need to type a password.');
    dbCb.cb.addEventListener('change', () => { W.siteDb = dbCb.cb.checked; });
    return h('div', null,
      items.length ? choices(items.map((i) => ({ value: i.path, title: i.label, desc: i.path, icon: 'globe', badge: i.kind === 'wordpress' ? 'WordPress' : null })),
        { value: W.site, label: 'Websites found', onPick: (v) => { W.site = v; W.sitePath = ''; W.nameTouched = false; autoName(); render(); } })
        : h('p', { class: 'hint' }, 'No websites were found on this server. Type the website’s folder below, or choose “Folders & files”.'),
      it && it.wpConfig ? dbCb.el : null,
      field(items.length ? 'Or type the website’s folder' : 'Website folder', manual, 'The folder that contains the website files.'));
  };

  // dockerForm: pick a Compose app or a container (its volumes and mounted
  // folders come along) or single volumes.
  const dockerForm = () => {
    const i = inv();
    if (!i.docker) return h('div', { class: 'banner info' }, 'Docker isn’t running on this server, or BackupProof can’t use it. Start Docker, wait a minute for the list to refresh, and try again.');
    const apps = i.dockerApps || [], vols = i.dockerVolumes || [];
    if (!W.dockerVols) W.dockerVols = new Set();
    if (W.dockerStop === undefined) W.dockerStop = true;
    if (W.dockerMounts === undefined) W.dockerMounts = true;
    const appKey = (a) => (a.standalone ? 'c:' : '') + a.name;
    const picked = apps.find((a) => appKey(a) === W.dockerApp);
    const mountsCb = checkbox('Include folders mounted into the containers', W.dockerMounts,
      picked && (picked.mounts || []).length ? 'On this server: ' + joinWords(picked.mounts.slice(0, 3)) + (picked.mounts.length > 3 ? ` and ${picked.mounts.length - 3} more` : '') + '.' : 'Folders on this server that the containers use, such as ./data in a Compose file.');
    mountsCb.cb.addEventListener('change', () => { W.dockerMounts = mountsCb.cb.checked; });
    const stop = checkbox('Stop the containers during each backup', W.dockerStop,
      'Recommended for apps with a database: files are copied while nothing writes to them. The containers are started again straight after, usually within a minute.');
    stop.cb.addEventListener('change', () => { W.dockerStop = stop.cb.checked; });
    const appChoices = apps.length ? choices(apps.map((a) => ({
      value: appKey(a), title: a.name, icon: 'box',
      desc: [a.standalone ? 'Container' : plural(a.containers.length, 'container'),
        a.volumes.length ? plural(a.volumes.length, 'volume') : 'no volumes',
        (a.mounts || []).length ? plural(a.mounts.length, 'mounted folder') : null,
        a.running ? null : 'stopped'].filter(Boolean).join(' · '),
      badge: a.database ? 'has a database' : null, badgeCls: 'warn',
    })), { value: W.dockerApp || '', small: true, label: 'Docker apps and containers', onPick: (v) => {
      W.dockerApp = v; W.dockerVols.clear();
      const a = apps.find((x) => appKey(x) === v);
      if (a && a.database) W.dockerStop = true;
      W.nameTouched = false; autoName(); render();
    } }) : h('p', { class: 'muted' }, 'No Docker apps or containers on this server.');
    const volList = vols.length ? h('div', { class: 'check-list' }, vols.map((v) => {
      const c = checkbox(v.name, W.dockerVols.has(v.name), v.usedBy.length ? 'Used by ' + joinWords(v.usedBy) : 'Not used by a container');
      c.cb.addEventListener('change', () => { if (c.cb.checked) { W.dockerVols.add(v.name); W.dockerApp = ''; } else W.dockerVols.delete(v.name); W.nameTouched = false; autoName(); render(); });
      return c.el;
    })) : h('p', { class: 'muted' }, 'No named volumes on this server. Folders mounted into containers come with their app above, or can be protected with Folders & files.');
    return h('div', null,
      h('h3', null, 'A Docker app or container'), h('p', { class: 'muted small' }, 'Backs up its volumes, the folders mounted into it, its containers’ settings and its Compose files.'), appChoices,
      details('Or choose single volumes', volList),
      W.dockerApp ? mountsCb.el : null,
      stop.el);
  };

  const databaseForm = () => {
    const dbs = inv().databases || [];
    const o = dbOpt();
    const opts = dbs.map((d, i) => ({
      value: 'inv:' + i, title: d.label, icon: 'db',
      desc: d.container ? 'The login is read from the container automatically.' : d.wpConfig ? 'The login is read from the WordPress settings automatically.' : d.needsLogin ? 'You’ll need its username and password.' : '',
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
      fields = field('Database file', bind(input({ name: 'sqlite-file', code: true, placeholder: '/var/lib/app/app.db…' }), d, 'file', autoName), 'The full path of the .db or .sqlite file.');
    } else if (o && o.inv && o.inv.container) {
      fields = [h('div', { class: 'banner ok' }, 'No password needed: the login is read from the container automatically.'),
        o.kind !== 'mongodb' ? field('Database name (optional)', bind(input({ name: 'db-name', code: true }), d, 'database'), 'Leave empty to detect it automatically, unless the container has several databases.') : null];
    } else if (o && o.inv && o.inv.wpConfig) {
      fields = h('div', { class: 'banner ok' }, 'No password needed: the login is read from the WordPress settings file.');
    } else if (o) {
      fields = [h('div', { class: 'row' },
        field('Server address', bind(input({ name: 'db-host', code: true }), d, 'host'), '127.0.0.1 means the same server.'),
        field('Port', bind(input({ name: 'db-port', type: 'number', inputmode: 'numeric', min: 1, max: 65535 }), d, 'port'), 'Usually ' + DB_DEFAULT_PORT[o.kind] + '.')),
      h('div', { class: 'row' },
        field('Username', bind(input({ name: 'db-user', code: true, autocomplete: 'off' }), d, 'user')),
        field('Password', bind(input({ name: 'db-password', type: 'password', autocomplete: 'off' }), d, 'password'), 'Kept encrypted on this server.'),
        field(o.kind === 'mongodb' ? 'Database name (optional)' : 'Database name', bind(input({ name: 'db-name', code: true }), d, 'database', autoName), o.kind === 'mongodb' ? 'Leave empty to back up all databases.' : 'The name of the database to back up.'))];
    }
    return h('div', null,
      dbs.length ? [h('p', { class: 'label' }, 'Databases found'), choices(opts, { value: W.dbVal, onPick: pick, label: 'Databases found' })] : h('p', { class: 'hint' }, 'No databases were found automatically. Choose the type below.'),
      h('p', { class: 'label' }, dbs.length ? 'Or enter one yourself' : 'Type of database'),
      choices(manual, { value: W.dbVal, onPick: pick, small: true, label: 'Type of database' }),
      fields);
  };

  const stepWhat = () => h('div', null,
    h('h2', { tabindex: '-1' }, 'What do you want to protect?'),
    choices([
      { value: 'folders', title: 'Folders & files', desc: 'Documents, photos, project folders…', icon: 'folder' },
      { value: 'website', title: 'Website', desc: 'A website’s files. WordPress sites can include their database.', icon: 'globe' },
      { value: 'database', title: 'Database', desc: 'PostgreSQL, MySQL/MariaDB, MongoDB or SQLite.', icon: 'db' },
      { value: 'docker', title: 'Docker app or volumes', desc: 'A Compose app or container with its volumes, mounted folders and settings, or single volumes.', icon: 'box' },
      { value: 'other', title: 'Something else (advanced)', desc: 'All settings: commands, hooks, custom checks.', icon: 'sliders' },
    ], { value: W.what, label: 'What to protect', onPick: (v) => { if (v === 'other') { location.hash = '#/sources/new'; return; } W.what = v; W.nameTouched = false; autoName(); render(); } }),
    W.what ? h('div', { class: 'subform' },
      W.what === 'folders' ? foldersForm() : W.what === 'website' ? websiteForm() : W.what === 'docker' ? dockerForm() : databaseForm(),
      field('Name', nameInput, 'How it appears in your list. You can change it.')) : null);

  const stepWhere = () => h('div', null,
    h('h2', { tabindex: '-1' }, 'Where should backups be stored?'),
    h('p', { class: 'muted' }, 'Backups are encrypted before they leave the server.'),
    storageStep({
      repos, value: W.repoId, adding: W.addingRepo, agent: agent(),
      onPick: (v) => { if (v === 'new') W.addingRepo = true; else { W.addingRepo = false; W.repoId = v; } render(); },
      onAdded: async (id) => { repos = (await api('/repositories')) || []; W.repoId = id; W.addingRepo = false; render(); },
    }));

  const stepWhen = () => {
    const custom = bind(input({ name: 'custom-schedule', code: true, placeholder: '30 1 * * *…' }), W, 'customCron');
    const others = agents.filter((a) => a.id !== W.agentId);
    const ver = checkbox('Test restores on a different server', W.useVerifier, 'Stronger proof: the restore test runs on another server that only has access to the storage.');
    const verSel = select([['', '— choose a server —'], ...others.map((a) => [a.id, agentTitle(a)])], W.verifierId);
    verSel.addEventListener('change', () => { W.verifierId = verSel.value; });
    ver.cb.addEventListener('change', () => { W.useVerifier = ver.cb.checked; render(); });
    return h('div', null,
      h('h2', { tabindex: '-1' }, 'How often?'),
      h('h3', null, 'Back up'),
      choices(BACKUP_CHOICES, { value: W.backup, small: true, label: 'Back up', onPick: (v) => { W.backup = v; render(); } }),
      W.backup === 'custom' ? field('Custom schedule', custom, 'A cron expression like “30 1 * * *” (1:30 every night) or “@every 12h”.') : null,
      h('h3', null, 'Test a restore'),
      h('p', { class: 'hint' }, 'BackupProof restores the latest backup into a safe, separate place and checks every file — so you know it really works.'),
      choices(DRILL_CHOICES, { value: W.drill, small: true, label: 'Test a restore', onPick: (v) => { W.drill = v; } }),
      h('h3', null, 'Keep old copies'),
      choices(KEEP_CHOICES, { value: W.keep, small: true, label: 'Keep old copies', onPick: (v) => { W.keep = v; } }),
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
    try { srcs = buildSources(); } catch (e) { return h('div', { class: 'banner bad', role: 'alert' }, e.message, ' Go back a step to fix it.'); }
    const { a, repo, dr, kp, cron } = plan();
    const whatWords = W.what === 'folders' ? joinWords(W.folders.map(folderLabel)) : srcs.length > 1 ? `${srcs[0].name} and its database` : srcs[0].name;
    const sentence = `Back up ${whatWords} on ${agentTitle(a)} ${cronWords(cron)} to “${repo.name}” (${repoKind(repo.backend).label}), test a restore ${dr.words}, and keep ${retentionWords(kp.retention)}.`;
    return h('div', null,
      h('h2', { tabindex: '-1' }, 'Review & protect'),
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
    S.dirty = false;
    nav.done();
    fill(root,
      pageHead('Done', 'Your first backup is running now. You can leave this page — it carries on in the background.'),
      h('section', { class: 'card' },
        created.map((c) => c.jobId ? jobProgress(c.id, c.jobId, c.name, false) : h('div', { class: 'banner bad' }, `${c.name}: couldn’t start the backup: ${c.error}. Open the item and click “Back up now” to try again.`)),
        h('div', { class: 'form-actions' },
          h('a', { class: 'btn', href: '#/protect' }, 'Protect something else'),
          h('a', { class: 'btn primary wrap', href: `#/sources/${created[0].id}` }, 'Open ' + created[0].name))));
    document.title = 'Done · BackupProof';
    focusHeading(root, 'h1');
  };

  let painted = false;
  function render() {
    err.textContent = '';
    const body = [stepComputer, stepWhat, stepWhere, stepWhen, stepReview][W.step]();
    const back = W.step > first ? btn('Back', () => nav.back(W.step - 1)) : h('a', { class: 'btn', href: '#/dashboard' }, 'Cancel');
    const next = W.step < 4
      ? btn(`Next: ${STEPS[W.step + 1]}`, () => { try { validate(); nav.forward(W.step + 1); } catch (e) { showErr(err, e.message); } }, 'primary')
      : btn('Protect now', busy(async () => { try { validate(); } catch (e) { showErr(err, e.message); return; } await protect(); }, 'Starting…'), 'primary lg');
    fill(root,
      pageHead('Protect something', 'A few simple questions. You can change everything later.'),
      stepsBar(STEPS.slice(first), W.step - first),
      h('section', { class: 'card wizard' }, body, err, h('div', { class: 'form-actions' }, back, next)));
  }
  // Moving between steps scrolls to the top and puts focus on the step's question.
  const nav = wizardHistory('/protect', () => W.step, (n) => {
    W.step = Math.max(first, Math.min(4, n));
    render();
    if (painted) { window.scrollTo(0, 0); focusHeading(root, '.wizard h2'); }
  });
  render();
  painted = true;
  return root;
}

// -------------------------------------------------------- import wizard

const IMPORT_FORMATS = [
  { value: 'files', title: 'Backup files in a bucket or folder', desc: 'Database dumps, .zip/.tar.gz archives, or encrypted files (.gpg, .enc, .age) made by any tool or script. Not sure what made your backups? Choose this: BackupProof recognises restic, Kopia and Borg backups and switches for you.', icon: 'file' },
  { value: 'restic', title: 'restic', notranslate: true, desc: 'A restic repository. The restic program must be installed on the server that converts.', icon: 'box' },
  { value: 'kopia', title: 'Kopia', notranslate: true, desc: 'A Kopia repository (on S3, B2, a disk or SFTP). The kopia program must be installed on the server that converts.', icon: 'box' },
  { value: 'borg', title: 'BorgBackup', notranslate: true, desc: 'A Borg repository, e.g. on another server over SSH, BorgBase or a Hetzner Storage Box. Needs the borg program.', icon: 'box' },
  { value: 'cloud', title: 'Google Drive, Dropbox, OneDrive…', desc: 'Backup files on a cloud drive (70+ services via rclone, including rclone-encrypted folders). Same options as backup files.', icon: 'cloud' },
];
const GROUPING = [
  ['auto', 'Automatically (one backup copy per day if file names contain dates)'],
  ['day', 'One copy per day'], ['folder', 'One copy per folder'], ['file', 'One copy per file'], ['all', 'Everything is one copy'],
];
const DECRYPT = [
  { value: 'auto', title: 'Not encrypted / not sure', desc: 'Detected automatically.' },
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
  // These are read on the converting server itself, so the dashboard can’t list them in advance.
  const noScan = () => resticLocal() || kopiaLocal() || I.format === 'borg' || I.format === 'cloud';
  const TOOL = { restic: 'restic', kopia: 'kopia', borg: 'borg', cloud: 'rclone' };
  const sf = storageFields({ mode: 'import', agent: builtin, initial: pre ? { backend: pre.backend, credentials: pre.credentials } : null });
  sf.onChange(() => { I.scan = null; clear(scanOut); });
  const root = guardDirty(h('div'));
  const err = errBox();
  const scanOut = h('div', { 'aria-live': 'polite' });
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
    if (I.switched) {
      scanOut.append(h('div', { class: 'banner info' }, h('strong', null, `These are ${I.switched.tool} backups.`), ' ',
        `They’re stored as a ${I.switched.tool} repository`, I.switched.folder ? ` in the “${I.switched.folder}” folder` : '',
        `, not as separate backup files, so BackupProof switched to the ${I.switched.tool} import for you.`));
    }
    if (!r) return;
    if (!r.found) {
      scanOut.append(h('div', { class: 'banner warn' }, sf.kind() === 'local'
        ? 'No old backups were found in this folder. Check that the folder path is right and that the backups are inside it, then look again.'
        : 'Connected, but no old backups were found here. Check the bucket and the “folder inside the bucket”, then look again.'));
      return;
    }
    scanOut.append(h('div', { class: 'banner ok' }, glyph(true), groupsSummary(r)),
      details(`Show the list (${r.groups.length})`, h('ul', { class: 'plain-list' }, r.groups.map((x) => h('li', null, groupLine(x))))));
    if (I.format === 'files') scanOut.append(h('p', { class: 'hint' }, 'So far the files have only been listed. If a password is wrong, you’ll see it when the conversion runs.'));
  };

  // switchTo moves to the import for the repository found where the person
  // looked for backup files: they don't need to know which tool made them.
  const switchTo = async (res) => {
    const repo = res.repository;
    if (repo.format === 'borg') {
      // Borg is read with the borg program over SSH or from a folder, not from a bucket.
      I.scan = null;
      fill(scanOut, h('div', { class: 'banner warn' }, h('strong', null, 'These are BorgBackup backups.'), ' Go back, choose BorgBackup, and enter the repository address you use with borg.'));
      return;
    }
    I.switched = { tool: repo.tool, folder: repo.prefix };
    I.format = repo.format;
    if (repo.format === 'restic') I.resticMode = 'manual';
    if (repo.format === 'kopia') I.kopiaMode = 'manual';
    sf.setLocation(res.storage);
    I.scan = null;
    render();
    if (I.password) await scan();
    else scanOut.append(h('p', null, `Enter the ${repo.tool} repository password above, then click “Look for backups”.`));
  };

  const scan = async () => {
    err.textContent = '';
    let spec;
    try { spec = importSpec(); } catch (e) { showErr(err, e.message); return; }
    if (repoTool() && !I.password) { showErr(err, `Enter the ${I.format === 'restic' ? 'restic' : 'Kopia'} repository password.`); return; }
    fill(scanOut, h('div', { class: 'running' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), 'Looking for backups… this can take a minute for big buckets.'));
    try {
      const res = await post('/import/scan', spec);
      if (res && res.repository) { await switchTo(res); return; }
      I.scan = res;
      showScan();
      autoName();
    } catch (e) {
      I.scan = null;
      if (!isAdmin() && /admin/i.test(e.message)) {
        I.scanSkipped = true;
        fill(scanOut, h('div', { class: 'banner info' }, 'Only an administrator can preview old backups. You can still continue — they are listed when the conversion starts.'));
        return;
      }
      showScan(); // keeps the "switched" note, if any
      scanOut.append(h('div', { class: 'banner bad' }, glyph(false), e.message));
    }
  };

  const nameInput = input({ name: 'item-name', autocomplete: 'off' });
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
    h('h2', { tabindex: '-1' }, 'What made your old backups?'),
    choices(IMPORT_FORMATS, { value: I.format, label: 'What made your old backups', onPick: (v) => { I.format = v; I.scan = null; I.switched = null; } }));

  const pwField = (label, help) => field(label, bind(input({ name: 'import-password', type: 'password', autocomplete: 'off' }), I, 'password', () => { I.scan = null; }), help);
  const pwFileField = (label, placeholder, help) => field(label, bind(input({ name: 'password-file', code: true, placeholder }), I, 'passwordFile'), help);
  const toolNote = () => h('p', { class: 'hint' }, `The server that converts needs the ${TOOL[I.format]} program installed (it already is if your backups are made there).`);
  const localOnly = () => h('div', { class: 'banner info' }, 'Nothing secret is sent to the dashboard — the server reads these files itself. In the next step, pick the server that has them.');
  const listLater = () => h('p', { class: 'muted' }, 'The backups are listed when the conversion starts.');

  const filesSecretBlock = () => {
    const key = bind(h('textarea', { name: 'private-key', rows: 4, spellcheck: 'false', autocapitalize: 'off', autocomplete: 'off', placeholder: 'AGE-SECRET-KEY-…' }), I, 'privateKey');
    const unpack = checkbox('Open archives (.zip, .tar.gz, .gz) so each file inside is checked', I.unpack);
    unpack.cb.addEventListener('change', () => { I.unpack = unpack.cb.checked; I.scan = null; });
    const grp = select(GROUPING, I.grouping, { name: 'grouping' });
    grp.addEventListener('change', () => { I.grouping = grp.value; I.scan = null; clear(scanOut); });
    const iter = bind(input({ name: 'openssl-iter', type: 'number', inputmode: 'numeric', min: 1, placeholder: '10000…' }), I, 'opensslIter');
    return h('div', null,
      h('h3', null, 'Are the files encrypted?'),
      h('p', { class: 'hint' }, 'BackupProof recognises GPG (.gpg/.pgp/.asc, also used by duplicity), OpenSSL (openssl enc, files starting with Salted__) and age (.age). Encryption done by the storage itself needs nothing extra.'),
      choices(DECRYPT, { value: I.decrypt, small: true, label: 'Are the files encrypted?', onPick: (v) => { I.decrypt = v; } }),
      pwField('Password used to encrypt them (if any)', 'Kept encrypted on this server and only used to read your old backups.'),
      details('I use a key file instead of a password', field('Private key', key, 'Paste your GPG private key (-----BEGIN PGP PRIVATE KEY BLOCK-----) or age key (AGE-SECRET-KEY-…).')),
      unpack.el,
      field('How are your backups organised?', grp, 'Decides how files are grouped into backup copies.'),
      details('Advanced', field('OpenSSL PBKDF2 iterations (only if you used -iter)', iter)));
  };

  const modeChoice = (key, localTitle, localDesc, manualDesc) => choices([
    { value: 'local', title: localTitle, badge: 'recommended', badgeCls: 'ok', desc: localDesc },
    { value: 'manual', title: 'Enter the repository details', desc: manualDesc },
  ], { value: I[key], small: true, label: 'How to connect', onPick: (v) => { I[key] = v; I.scan = null; render(); } });

  const scanButton = () => h('div', null, h('div', { class: 'form-actions start' }, btn('Look for backups', busy(scan, 'Looking…'), 'primary')), scanOut);

  const stepWhere = () => {
    const head = [h('h2', { tabindex: '-1' }, 'Where are they?')];
    switch (I.format) {
      case 'restic':
        head.push(modeChoice('resticMode', 'Use the restic settings already on that server', 'Best if a script already runs restic there (for example with /etc/restic/env).', 'Connect to the bucket or server where the restic repository is.'));
        if (resticLocal()) {
          return h('div', null, head,
            field('restic settings file', bind(input({ name: 'restic-env-file', code: true }), I, 'resticEnvFile'), 'The file your backup script loads with “set -a; . /etc/restic/env”; it contains the repository address and storage keys.'),
            field('restic password file', bind(input({ name: 'restic-password-file', code: true }), I, 'resticPasswordFile'), 'The file that holds the restic repository password.'),
            localOnly(), toolNote(), listLater());
        }
        return h('div', null, head,
          h('p', { class: 'muted' }, 'Connect to the place where the restic repository is. It is only read, never changed.'),
          sf.el,
          pwField('restic repository password', 'The password you use with restic for this repository.'),
          field('Repository address (optional)', bind(input({ name: 'restic-repository', code: true, placeholder: 'b2:my-bucket:server1…' }), I, 'resticRepository'),
            'Only if restic uses a native address like b2:my-bucket:server1 or sftp:user@host:/backups instead of the storage above.'),
          toolNote(), scanButton());
      case 'kopia':
        head.push(modeChoice('kopiaMode', 'Use the Kopia connection already on that server', 'Best if Kopia already runs there. It reuses Kopia’s own settings file.', 'Connect to the bucket, folder or SFTP server where the Kopia repository is.'));
        if (kopiaLocal()) {
          return h('div', null, head,
            field('Kopia settings file', bind(input({ name: 'kopia-config-file', code: true }), I, 'kopiaConfigFile'), 'Usually /root/.config/kopia/repository.config on Linux, or %APPDATA%\\kopia\\repository.config on Windows.'),
            pwField('Kopia repository password', 'Leave empty if you give a password file below.'),
            pwFileField('…or a file that holds the password', '/etc/kopia/password…', 'Read on that server; the password is never sent to the dashboard.'),
            localOnly(), toolNote(), listLater());
        }
        return h('div', null, head,
          h('p', { class: 'muted' }, 'Connect to the place where the Kopia repository is. It is opened read-only.'),
          sf.el,
          pwField('Kopia repository password', 'The password you use with Kopia for this repository.'),
          toolNote(), scanButton());
      case 'borg':
        return h('div', null, head,
          field('Borg repository address', bind(input({ name: 'borg-repository', code: true, placeholder: 'ssh://u123456@u123456.your-storagebox.de:23/./backups…' }), I, 'borgRepository'),
            'The same address you use with borg (BORG_REPO), or a folder like /mnt/backup/borg. For BorgBase it looks like ssh://xxxx@xxxx.repo.borgbase.com/./repo.'),
          pwField('Borg passphrase', 'Leave empty if the repository is not encrypted or you give a passphrase file below.'),
          pwFileField('…or a file that holds the passphrase', '/root/.borg-passphrase…', 'Read on that server; the passphrase is never sent to the dashboard.'),
          field('SSH key file (optional)', bind(input({ name: 'borg-ssh-key', code: true, placeholder: '/root/.ssh/id_ed25519…' }), I, 'borgSshKeyFile'), 'Only needed if the repository is on another server and borg normally uses a specific key.'),
          toolNote(), listLater());
      case 'cloud':
        return h('div', null, head,
          h('div', { class: 'banner info' }, 'Cloud drives are reached through rclone. Set up the drive once on the converting server with “rclone config” (choose Google Drive, Dropbox, OneDrive, …), then enter its name and folder here.'),
          field('rclone remote and folder', bind(input({ name: 'rclone-remote', code: true, placeholder: 'gdrive:Backups/server1…' }), I, 'cloudRemote'),
            'The name you gave the drive in rclone, a colon, then the folder with your backups. rclone-encrypted (crypt) remotes are decrypted automatically.'),
          details('Advanced', field('rclone settings file (optional)', bind(input({ name: 'rclone-config', code: true, placeholder: '/root/.config/rclone/rclone.conf…' }), I, 'rcloneConfig'), 'Only if rclone’s settings are not in the usual place.')),
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
      h('h2', { tabindex: '-1' }, 'Which server does the converting?'),
      h('p', { class: 'muted' }, 'It downloads the old backups, checks them and stores them again, encrypted, in BackupProof format.'),
      choices(agents.map((a) => ({ value: a.id, title: agentTitle(a), badge: a.builtin ? 'built in' : null, icon: 'computer', desc: [a.online ? 'Online' : 'Offline', osName(a.os)].join(' · ') })),
        { value: I.agentId, small: true, label: 'Server that converts', onPick: (v) => { I.agentId = v; I.agentTouched = true; } }),
      hint ? h('div', { class: 'banner info' }, hint) : null,
      h('h2', { class: 'mt' }, 'Where should the converted copies be stored?'),
      storageStep({
        repos, value: I.repoId, adding: I.addingRepo, agent, label: 'Storage for converted copies',
        onPick: (v) => { if (v === 'new') I.addingRepo = true; else { I.addingRepo = false; I.repoId = v; } render(); },
        onAdded: async (id) => { repos = (await api('/repositories')) || []; I.repoId = id; I.addingRepo = false; render(); },
      }));
  };

  const stepConvert = () => {
    if (!I.name) autoName(); else nameInput.value = I.name;
    const what = I.format === 'borg' ? 'archives' : 'snapshots';
    return h('div', null,
      h('h2', { tabindex: '-1' }, 'Convert'),
      field('Name', nameInput, 'How it appears in your list of protected things.'),
      repoTool() ? [h('h3', null, 'Keep converting?'),
        choices([
          { value: 'nightly', title: `Convert new ${what} every night`, desc: `At ${clock(3)}, after your existing backup has run.`, badge: 'recommended', badgeCls: 'ok' },
          { value: 'once', title: 'Only once', desc: 'Convert what is there now. You can click “Convert again” later.' },
        ], { value: I.schedule, small: true, label: 'Keep converting?', onPick: (v) => { I.schedule = v; } }),
        h('p', { class: 'hint' }, `Each night’s new ${what.slice(0, -1)} is converted and restore-tested, so you get proof without changing your existing backup.`)] : null,
      h('h3', null, 'Test a restore'),
      choices(DRILL_CHOICES, { value: I.drill, small: true, label: 'Test a restore', onPick: (v) => { I.drill = v; } }),
      h('div', { class: 'banner info' }, 'Converted copies keep their original dates. Your old backups are only read, never changed or deleted. Click “Convert again” later to pick up anything new.'));
  };

  const validate = () => {
    if (I.step === 1) {
      importSpec();
      if (!noScan() && !I.scanSkipped) {
        if (!I.scan) throw new Error('Click “Look for backups” first, so you can see what will be converted.');
        if (!I.scan.found) throw new Error('No old backups were found at this place. Check the details above and look again.');
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
    S.dirty = false;
    nav.done();
    fill(root,
      pageHead('Converting your old backups', 'This can take a while for large backups. You can leave this page — it carries on in the background.'),
      h('section', { class: 'card' },
        job ? jobProgress(r.id, job, name, true) : h('div', { class: 'banner bad' }, 'Couldn’t start the conversion: ', jerr, '. Open the item and click “Convert again” to retry.'),
        h('div', { class: 'form-actions' }, h('a', { class: 'btn primary wrap', href: `#/sources/${r.id}` }, 'Open ' + name))));
    document.title = 'Converting · BackupProof';
    focusHeading(root, 'h1');
  };

  let painted = false;
  function render() {
    err.textContent = '';
    const body = [stepFormat, stepWhere, stepDest, stepConvert][I.step]();
    if (I.step === 1) showScan();
    const back = I.step > 0 ? btn('Back', () => nav.back(I.step - 1)) : h('a', { class: 'btn', href: '#/dashboard' }, 'Cancel');
    const next = I.step < 3
      ? btn(`Next: ${STEPS[I.step + 1]}`, () => { try { validate(); nav.forward(I.step + 1); } catch (e) { showErr(err, e.message); } }, 'primary')
      : btn('Convert', busy(async () => { try { validate(); } catch (e) { showErr(err, e.message); return; } await convert(); }, 'Starting…'), 'primary lg');
    fill(root,
      pageHead('Bring in your old backups', 'Convert backups made by other tools or scripts so they can be restore-tested and proven. Your old backups are only read, never changed or deleted.'),
      stepsBar(STEPS, I.step),
      h('section', { class: 'card wizard' }, body, err, h('div', { class: 'form-actions' }, back, next)));
  }
  if (!agents.length) {
    return h('div', null, pageHead('Bring in your old backups'), card(null, empty('No server is connected yet.', h('a', { class: 'btn primary', href: '#/agents' }, 'Connect a server'))));
  }
  const nav = wizardHistory('/import', () => I.step, (n) => {
    I.step = Math.max(0, Math.min(3, n));
    render();
    if (painted) { window.scrollTo(0, 0); focusHeading(root, '.wizard h2'); }
  });
  render();
  painted = true;
  return root;
}

// ------------------------------------------------------------- servers

function agentStatus(a) {
  if (a.revoked) return 'Disconnected (access removed)';
  if (a.online) return 'Online';
  return a.lastSeen ? `Offline since ${rel(a.lastSeen)}` : 'Never connected';
}

// Speeds are shown in megabits per second (how internet links are sold) and
// stored as kilobytes per second.
const mbitToKB = (v) => Math.round((Number(v) || 0) * 1000 / 8);
const kbToMbit = (kb) => kb ? +(kb * 8 / 1000).toFixed(1) : 0;

function limitsWords(l) {
  const parts = [];
  if (l.uploadKBps) parts.push(`upload up to ${kbToMbit(l.uploadKBps)} Mbit/s`);
  if (l.downloadKBps) parts.push(`download up to ${kbToMbit(l.downloadKBps)} Mbit/s`);
  if (l.windowStart) parts.push(`scheduled jobs between ${l.windowStart} and ${l.windowEnd}`);
  if (l.concurrency) parts.push(l.concurrency === 1 ? 'one transfer at a time' : `${l.concurrency} transfers at once`);
  return parts.length ? parts.join(' · ') : 'No limits: full speed, any time';
}

function limitsBox(a) {
  const l = a.limits || {};
  const up = input({ name: 'upload-mbit', type: 'number', inputmode: 'decimal', min: 0, step: 'any', value: kbToMbit(l.uploadKBps) || '', placeholder: 'No limit…' });
  const down = input({ name: 'download-mbit', type: 'number', inputmode: 'decimal', min: 0, step: 'any', value: kbToMbit(l.downloadKBps) || '', placeholder: 'No limit…' });
  const from = h('input', { type: 'time', name: 'window-start', value: l.windowStart || '' });
  const to = h('input', { type: 'time', name: 'window-end', value: l.windowEnd || '' });
  const conc = input({ name: 'concurrency', type: 'number', inputmode: 'numeric', min: 0, max: 64, step: 1, value: l.concurrency || '', placeholder: 'Automatic…' });
  const mem = input({ name: 'max-inflight', type: 'number', inputmode: 'numeric', min: 16, max: 4096, step: 1, value: l.maxInflightMB || '', placeholder: '64…' });
  const form = h('form', {
    novalidate: true,
    onsubmit: busy(async (e) => {
      e.preventDefault();
      clearErrors(form);
      if (!!from.value !== !!to.value) return fieldError(from.value ? to : from, 'Give both a start and an end time, or leave both empty.');
      if (from.value && from.value === to.value) return fieldError(to, 'The end time must be different from the start time.');
      const c = Number(conc.value) || 0, m = Number(mem.value) || 0;
      if (c < 0 || c > 64 || !Number.isInteger(c)) return fieldError(conc, 'Enter a whole number from 1 to 64, or leave it empty.');
      if (m && (m < 16 || m > 4096)) return fieldError(mem, 'Enter 16 to 4096 MB, or leave it empty.');
      await put(`/agents/${a.id}/limits`, { uploadKBps: mbitToKB(up.value), downloadKBps: mbitToKB(down.value), windowStart: from.value, windowEnd: to.value, concurrency: c, maxInflightMB: m });
      S.dirty = false;
      toast('Limits saved for ' + agentTitle(a), 'ok');
      reload();
    }),
  },
  h('div', { class: 'row' }, field('Upload limit (Mbit/s)', up, 'For backups. Empty means no limit.'), field('Download limit (Mbit/s)', down, 'For restore tests and restores.')),
  h('div', { class: 'row' }, field('Run scheduled jobs from', from), field('until', to, 'In the dashboard server’s time zone. Crossing midnight is fine, for example 22:00 to 06:00.')),
  h('p', { class: 'hint' }, 'Outside the window, scheduled backups and restore tests wait for it to open. Jobs you start yourself always run straight away.'),
  details('Advanced: parallel transfers',
    h('div', { class: 'row' }, field('Transfers at once', conc, 'Empty adapts between 4 and 32 to what the link and the storage can take. 1 moves one piece at a time.'), field('Memory for transfers (MB)', mem, 'Most backup data held in memory at once. Empty means 64 MB.'))),
  h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn primary sm' }, 'Save limits')));
  return h('div', { class: 'limits' }, h('p', { class: 'small' }, h('span', { class: 'muted' }, 'Speed and timing: '), limitsWords(l)),
    canOperate() ? details('Change speed limits and time window', form) : null);
}

async function pageAgents() {
  const agents = agentOrder((await api('/agents')) || []);
  if (S.openConnect) { S.openConnect = false; if (isAdmin()) setTimeout(() => connectModal(), 0); }
  return h('div', null,
    pageHead('Servers', 'The servers BackupProof backs up and runs restore tests on.', isAdmin() ? btn('+ Connect a server', busy(connectModal, 'Preparing…'), 'primary') : null),
    agents.length ? h('div', { class: 'cards' }, agents.map((a) => h('section', { class: 'card mini' + (a.revoked ? ' dim' : '') },
      h('div', { class: 'mini-head' }, h('span', { class: 'item-ico' }, icon('computer')),
        h('div', null, h('h2', null, agentTitle(a), a.builtin ? h('span', { class: 'pill' }, 'built in') : null),
          h('div', { class: 'small' }, h('span', { class: 'dot ' + (a.revoked ? '' : a.online ? 'on' : 'off'), 'aria-hidden': 'true' }), agentStatus(a)))),
      h('dl', { class: 'kv' },
        h('dt', null, 'System'), h('dd', null, osName(a.os)),
        h('dt', null, 'Can test databases'), h('dd', null, a.docker ? 'Yes' : 'No (needs Docker)'),
        !a.builtin ? [h('dt', null, 'Server name'), h('dd', null, a.hostname || '—')] : null),
      !a.revoked ? limitsBox(a) : null,
      tech(h('dl', { class: 'kv' },
        h('dt', null, 'Name'), h('dd', null, a.name),
        h('dt', null, 'Host name'), h('dd', null, a.hostname || '—'),
        h('dt', null, 'Platform'), h('dd', null, a.os || '—'),
        h('dt', null, 'Version'), h('dd', { translate: 'no' }, a.version || '—'),
        h('dt', null, 'Key ID'), h('dd', null, h('code', { class: 'break' }, a.keyId || short(a.publicKey, 16))),
        h('dt', null, 'Connected'), h('dd', null, absTime(a.created)))),
      isAdmin() && !a.revoked && !a.builtin ? h('div', { class: 'form-actions' }, btn('Disconnect', busy(async () => {
        if (!(await confirmDlg(`Disconnect “${a.name}”? It immediately loses access and must be connected again with a new code. Its past proofs stay valid.`, 'Disconnect'))) return;
        await post(`/agents/${a.id}/revoke`); toast('Server disconnected', 'ok'); reload();
      }), 'sm danger', { 'aria-label': 'Disconnect ' + a.name })) : null,
      isAdmin() && a.revoked ? h('div', { class: 'form-actions' }, btn('Remove', busy(async () => {
        if (!(await confirmDlg(`Remove “${a.name}” from this list? Its signed proofs stay valid: its key is kept for checking them.`, 'Remove'))) return;
        await del(`/agents/${a.id}`); toast(`Removed “${a.name}”`, 'ok'); reload();
      }), 'sm danger quiet', { 'aria-label': 'Remove ' + a.name })) : null)))
      : h('section', { class: 'card' }, empty('No servers connected yet.', isAdmin() ? btn('Connect your first server', busy(connectModal, 'Preparing…'), 'primary') : h('p', { class: 'hint' }, 'Ask an administrator to connect a server.'))));
}

async function connectModal() {
  const before = new Set(((await api('/agents')) || []).map((a) => a.id));
  let st = await api('/status', { noAuthRedirect: true });
  let tok = await post('/agents/enroll-token');
  let tab = /Win/i.test(navigator.userAgent) ? 'windows' : /Mac/i.test(navigator.userAgent) ? 'macos' : 'linux';
  const urlArea = h('div');
  const tabArea = h('div');
  const connected = h('div', { 'aria-live': 'polite' });
  const expires = h('p', { class: 'hint' });
  const techArea = h('div');
  const TABS = [['linux', 'Linux'], ['windows', 'Windows'], ['macos', 'macOS']];
  const STEP1 = {
    linux: 'Open a terminal on the other server.',
    windows: 'On the other server, open PowerShell as Administrator (right-click the Start button → “Terminal (Admin)” or “Windows PowerShell (Admin)”).',
    macos: 'Open Terminal on the other server (Applications → Utilities → Terminal).',
  };
  const base = uid('tabs');
  // Tabs follow the WAI-ARIA tabs pattern: one tab stop, arrow keys switch.
  const renderTabs = (focusTab) => {
    const cmd = (tok.commands && tok.commands[tab]) || tok.command;
    const tabs = TABS.map(([k, l]) => h('button', {
      type: 'button', role: 'tab', id: `${base}-${k}`, class: 'tab' + (k === tab ? ' on' : ''), 'aria-selected': String(k === tab),
      'aria-controls': `${base}-panel`, tabindex: k === tab ? '0' : '-1', onclick: () => { tab = k; renderTabs(true); },
    }, l));
    const list = h('div', { class: 'tabs', role: 'tablist', 'aria-label': 'System of the other server' }, tabs);
    list.addEventListener('keydown', (e) => {
      const i = TABS.findIndex(([k]) => k === tab);
      let j = -1;
      if (e.key === 'ArrowRight') j = (i + 1) % TABS.length;
      else if (e.key === 'ArrowLeft') j = (i - 1 + TABS.length) % TABS.length;
      else if (e.key === 'Home') j = 0;
      else if (e.key === 'End') j = TABS.length - 1;
      if (j < 0) return;
      e.preventDefault();
      tab = TABS[j][0];
      renderTabs(true);
    });
    fill(tabArea, list,
      h('div', { role: 'tabpanel', id: `${base}-panel`, 'aria-labelledby': `${base}-${tab}` },
        h('ol', { class: 'plain-steps' },
          h('li', null, STEP1[tab]),
          h('li', null, 'Paste this command and press Enter:', h('div', { class: 'copybox' }, h('pre', null, cmd), copyBtn(() => cmd, 'Copy', 'Copy the command'))),
          h('li', null, 'It appears here within a minute.'))));
    if (focusTab) { const t = document.getElementById(`${base}-${tab}`); if (t) t.focus(); }
  };
  const renderRest = () => {
    expires.textContent = `This connection code works once and expires ${tok.expires ? rel(tok.expires) : 'in 1 hour'}.`;
    fill(techArea, tech(h('p', { class: 'small' }, 'Connection code:'), h('div', { class: 'copybox' }, h('pre', null, tok.token), copyBtn(() => tok.token, 'Copy', 'Copy the connection code')),
      h('p', { class: 'small' }, 'If the program is already installed:'), h('div', { class: 'copybox' }, h('pre', null, tok.command), copyBtn(() => tok.command, 'Copy', 'Copy the install-free command'))));
  };
  const renderUrl = () => {
    if (!st.publicUrlIsLocal) { clear(urlArea); return; }
    if (!isAdmin()) { fill(urlArea, h('div', { class: 'banner warn' }, `Other servers can’t reach this dashboard at ${st.publicUrl}. Ask an administrator to set the dashboard address in Settings.`)); return; }
    const u = input({ name: 'public-url', type: 'url', code: true, value: /^(localhost|127\.)/.test(location.hostname) ? '' : location.origin, placeholder: 'http://192.168.1.20:8420…' });
    fill(urlArea, h('div', { class: 'banner warn' },
      h('p', null, `Other servers can’t reach this dashboard at ${st.publicUrl}. Enter the address they should use:`),
      h('div', { class: 'inline-field' }, field('Dashboard address', u, 'For example http://192.168.1.20:8420 or https://backup.example.com'),
        btn('Save address', busy(async () => {
          await put('/settings/server', { publicUrl: u.value.trim() });
          st = await api('/status', { noAuthRedirect: true });
          S.status = { ...S.status, publicUrl: st.publicUrl, publicUrlIsLocal: st.publicUrlIsLocal };
          tok = await post('/agents/enroll-token');
          toast('Address saved — the command below is updated', 'ok');
          renderTabs(false); renderRest(); renderUrl();
        }, 'Saving…'), 'sm primary'))));
  };
  renderUrl(); renderTabs(false); renderRest();
  const body = h('div', null, urlArea, tabArea, connected, expires, techArea);
  let timer = null;
  modal('Connect a server', body, [], { wide: true, onClose: () => { clearInterval(timer); reload(); } });
  fill(connected, h('div', { class: 'running' }, h('span', { class: 'spinner', 'aria-hidden': 'true' }), 'Waiting for the server to connect…'));
  timer = setInterval(async () => {
    if (!body.isConnected) { clearInterval(timer); return; }
    if (document.hidden) return;
    try {
      const fresh = ((await api('/agents')) || []).filter((a) => !before.has(a.id));
      if (fresh.length && !connected.dataset.done) { connected.dataset.done = '1'; fill(connected, h('div', { class: 'banner ok' }, glyph(true), 'Connected: ', joinWords(fresh.map((a) => a.name)))); }
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
    name: input({ name: 'name', value: src.name, required: true, disabled: !!id, autocomplete: 'off' }),
    kind: select(kinds, sp.kind, { name: 'kind' }),
    paths: h('textarea', { name: 'paths', rows: 3, spellcheck: 'false', autocapitalize: 'off', placeholder: '/var/www…' }, (sp.paths || []).join('\n')),
    excludes: h('textarea', { name: 'excludes', rows: 2, spellcheck: 'false', autocapitalize: 'off', placeholder: '*.tmp…' }, (sp.excludes || []).join('\n')),
    sqlitePath: input({ name: 'sqlite-path', code: true, value: (sp.paths || [])[0] || '', placeholder: '/var/lib/app/app.db…' }),
    host: input({ name: 'db-host', code: true, value: sp.host || '', placeholder: '127.0.0.1…' }),
    port: input({ name: 'db-port', type: 'number', inputmode: 'numeric', min: 0, max: 65535, value: sp.port || '' }),
    user: input({ name: 'db-user', code: true, autocomplete: 'off', value: sp.user || '' }),
    database: input({ name: 'db-name', code: true, value: sp.database || '' }),
    password: input({ name: 'db-password', type: 'password', autocomplete: 'off' }),
    uri: input({ name: 'db-uri', type: 'password', autocomplete: 'off', placeholder: sp.uri === '(redacted)' ? '' : 'mongodb://user:pass@host:27017/db…' }),
    container: input({ name: 'container', code: true, value: sp.container || '' }),
    wpConfig: input({ name: 'wp-config', code: true, value: sp.wpConfig || '', placeholder: '/var/www/site/wp-config.php…' }),
    globals: checkbox('Also back up users and roles (pg_dumpall --globals-only)', sp.globals),
    command: input({ name: 'command', code: true, value: sp.command || '', placeholder: 'redis-cli --rdb -…' }),
    preHook: input({ name: 'pre-hook', code: true, value: sp.preHook || '' }),
    postHook: input({ name: 'post-hook', code: true, value: sp.postHook || '' }),
    agent: select([['', '— choose a server —'], ...liveAgents.map((a) => [a.id, `${agentTitle(a)} (${a.hostname || osName(a.os)})`])], src.agentId || ''),
    verifier: select([['', 'The same server'], ...liveAgents.map((a) => [a.id, `${agentTitle(a)} (${a.hostname || osName(a.os)})`])], src.verifierId || ''),
    repo: select([['', '— choose storage —'], ...(repos || []).map((r) => [r.id, `${r.name} (${repoKind(r.backend).label})`])], src.repoId || ''),
    backupCron: input({ name: 'backup-schedule', code: true, value: src.backupCron, required: true }),
    drillCron: input({ name: 'drill-schedule', code: true, value: src.drillCron, required: true }),
    maxAge: input({ name: 'max-age', type: 'number', inputmode: 'numeric', min: 1, value: src.proofMaxAgeHours || 192 }),
    enabled: checkbox('Active (backups and restore tests run on schedule)', src.enabled !== false),
    keepLast: input({ type: 'number', min: 0, value: ret.keepLast || '' }),
    keepDaily: input({ type: 'number', min: 0, value: ret.keepDaily || '' }),
    keepWeekly: input({ type: 'number', min: 0, value: ret.keepWeekly || '' }),
    keepMonthly: input({ type: 'number', min: 0, value: ret.keepMonthly || '' }),
    keepYearly: input({ type: 'number', min: 0, value: ret.keepYearly || '' }),
    keepLastVerified: input({ type: 'number', min: 0, value: ret.keepLastVerified || '' }),
    expectPaths: h('textarea', { name: 'expect-paths', rows: 2, spellcheck: 'false', autocapitalize: 'off' }, (dr.expectPaths || []).join('\n')),
    minFiles: input({ name: 'min-files', type: 'number', inputmode: 'numeric', min: 0, value: dr.minFiles || '' }),
    image: input({ name: 'image', code: true, value: dr.image || '' }),
    drillCommand: input({ name: 'drill-command', code: true, value: dr.command || '' }),
    tolerance: input({ name: 'tolerance', type: 'number', inputmode: 'decimal', min: 0, max: 1, step: '0.01', value: dr.rowCountTolerance || '', placeholder: '0.2…' }),
    testDumps: checkbox('Test database dumps found in the backup (needs Docker)', !dr.skipDumps, 'PostgreSQL dumps inside the backed-up files are loaded into a test database and checked.'),
  };

  const presets = (target, lst) => h('div', { class: 'presets' }, lst.map(([l, v]) => btn(l, () => { target.value = v; target.dispatchEvent(new Event('input')); }, 'sm')));
  const cronHint = (el) => { const s = h('span', { class: 'hint' }); const u = () => { s.textContent = 'Means: ' + cronWords(el.value); }; el.addEventListener('input', u); u(); return s; };

  const assertBox = h('div');
  let assertN = 0;
  const addAssert = (a = { name: '', sql: '' }) => {
    const i = ++assertN;
    const n = input({ name: 'check-name', value: a.name, placeholder: 'Users exist…', class: 'a-name', 'aria-label': `Check ${i}: name` });
    const q = input({ name: 'check-sql', code: true, value: a.sql, placeholder: 'SELECT count(*) > 0 FROM users…', class: 'a-sql', 'aria-label': `Check ${i}: SQL query` });
    const row = h('div', { class: 'assert-row' }, n, q, btn('Remove', () => { row.remove(); S.dirty = true; addBtn.focus(); }, 'sm danger', { 'aria-label': `Remove check ${i}` }));
    assertBox.append(row);
    return n;
  };
  (dr.assertions || []).forEach(addAssert);

  const isDb = (k) => ['postgres', 'mysql', 'mongodb'].includes(k);
  const kindBox = h('div');
  const addBtn = btn('+ Add check', () => addAssert().focus(), 'sm');
  const assertFs = h('div', null, h('h3', null, 'Your own database checks'),
    h('p', { class: 'hint' }, 'Each SQL query must return a single true/non-zero value on the restored database.'),
    assertBox, addBtn);
  const renderKind = () => {
    const k = f.kind.value;
    const dbRow = h('div', { class: 'row' }, field('Server address', f.host), field('Port', f.port), field('Username', f.user), field('Database name', f.database, k === 'mongodb' ? 'Optional; empty backs up all databases.' : null));
    add(clear(kindBox), [
      k === 'files' ? [field('Folders', f.paths, 'One full folder path per line.'), field('Skip these files', f.excludes, 'One pattern per line, e.g. *.tmp')] : null,
      k === 'postgres' || k === 'mysql' ? [dbRow, h('div', { class: 'row' }, field('Password', f.password, id ? 'Leave empty to keep the saved password.' : null), field('Docker container (optional)', f.container, 'Run the backup tools inside this container (no password needed).'))] : null,
      k === 'mysql' ? field('WordPress settings file', f.wpConfig, 'Optional: read the login from this wp-config.php automatically.') : null,
      k === 'postgres' ? f.globals.el : null,
      k === 'mongodb' ? [field('Connection address (URI)', f.uri, sp.uri === '(redacted)' ? 'Saved. Leave empty to keep it, or fill in the fields below instead.' : 'Either an address like this, or fill in the fields below.'), dbRow, h('div', { class: 'row' }, field('Password', f.password, id ? 'Leave empty to keep the saved password.' : null), field('Docker container (optional)', f.container))] : null,
      k === 'sqlite' ? field('Database file', f.sqlitePath, 'Full path of the SQLite database file.') : null,
      k === 'command' ? field('Command', f.command, 'Whatever it prints is saved as the backup.') : null,
      k === 'import' ? h('div', { class: 'banner info' }, `Imported from ${sp.import ? sp.import.format : 'another tool'} at ${sp.import ? repoUrl(sp.import.storage) : '—'}. To import from a different place, use Import.`) : null,
    ]);
    assertFs.classList.toggle('hidden', !(isDb(k) || k === 'sqlite') || k === 'mongodb');
  };
  f.kind.addEventListener('change', renderKind);

  const err = errBox();
  const form = h('form', {
    onsubmit: busy(async (e) => {
      e.preventDefault();
      err.textContent = '';
      clearErrors(form);
      const k = f.kind.value;
      if (!id && !f.name.value.trim()) return fieldError(f.name, 'Give it a name.');
      if (!f.agent.value) return fieldError(f.agent, 'Choose a server.');
      if (!f.repo.value) return fieldError(f.repo, 'Choose the backup storage.');
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
      try {
        if (id) { await put(`/sources/${id}`, body); S.dirty = false; toast('Changes saved', 'ok'); location.hash = `#/sources/${id}`; }
        else { const r = await post('/sources', body); S.dirty = false; toast('Created', 'ok'); location.hash = `#/sources/${r.id}`; }
      } catch (ex) { if (ex.status !== 401) showErr(err, ex.message); }
    }),
  },
  h('fieldset', null, h('legend', null, 'What’s protected'),
    h('div', { class: 'row' }, field('Name', f.name, id ? 'The name can’t be changed (it is part of the stored proofs).' : 'Unique, e.g. “Billing database”.'), field('Type', f.kind)),
    kindBox,
    h('div', { class: 'row' }, field('Run before the backup', f.preHook, 'Optional command, e.g. to pause an app.'), field('Run after the backup', f.postHook, 'Runs even if the backup failed.'))),
  h('fieldset', null, h('legend', null, 'Where'),
    h('div', { class: 'row' }, field('Server', f.agent, 'The server that has the data.'), field('Backup storage', f.repo, 'Where encrypted copies are kept.')),
    field('Test restores on a different server', f.verifier, 'Optional. Stronger proof: restore tests run on another server that only has access to the storage.'),
    !liveAgents.length ? h('p', { class: 'hint' }, 'No servers connected yet. ', h('a', { href: '#/agents' }, 'Connect one')) : null,
    !(repos || []).length ? h('p', { class: 'hint' }, 'No storage yet. ', isAdmin() ? h('a', { href: '#/storage/new' }, 'Add storage') : 'Ask an administrator to add one.') : null),
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
    h('button', { type: 'submit', class: 'btn primary' }, id ? 'Save changes' : 'Create item')));
  renderKind();
  guardDirty(form);
  return h('div', null,
    backLink(id ? `#/sources/${id}` : '#/protect', id ? 'Back to the item' : 'Back to Protect something'),
    pageHead(id ? `Edit “${src.name}”` : 'Protect something (advanced)', 'All settings. For most things the simple “Protect something” wizard is easier.'),
    h('section', { class: 'card' }, form));
}

// --------------------------------------------------------- proof history

function isoDay(d) { return new Date(d.getTime() - d.getTimezoneOffset() * 60000).toISOString().slice(0, 10); }

async function pageProofs(m, params) {
  const LIMIT = 100;
  const from = Math.max(1, Number(params && params.get('from')) || 1);
  const [proofs, ledger] = await Promise.all([api('/proofs?limit=100'), api(`/ledger?${qs({ from, limit: LIMIT })}`)]);
  const PH = ['When', 'Item', 'What', 'Result', 'Time to restore', ''];

  const verifyOut = h('div', { 'aria-live': 'polite' });
  const verify = busy(async () => {
    const r = await api('/ledger/verify');
    fill(verifyOut, h('div', { class: 'banner ' + (r.ok ? 'ok' : 'bad') },
      h('strong', null, glyph(r.ok), r.ok ? 'Nothing has been changed or removed. ' : 'The proof history has been tampered with or damaged. '),
      `${plural(r.entries, 'record')} checked.`, r.error ? h('div', null, r.error) : null,
      r.head ? tech(h('code', { class: 'break' }, r.head)) : null));
  }, 'Checking…');

  const LH = ['#', 'Time', 'Kind', 'Subject', 'Envelope digest', 'Hash'];
  const rows = ledger || [];
  // The ledger page is part of the address (#/proofs?from=101), so Back works.
  const earlier = from > 1 ? h('a', { class: 'btn sm', href: '#/proofs?from=' + Math.max(1, from - LIMIT) }, h('span', { 'aria-hidden': 'true' }, '← '), 'Earlier') : btn([h('span', { 'aria-hidden': 'true' }, '← '), 'Earlier'], null, 'sm', { disabled: true });
  const later = rows.length >= LIMIT ? h('a', { class: 'btn sm', href: '#/proofs?from=' + (rows[rows.length - 1].seq + 1) }, 'Later', h('span', { 'aria-hidden': 'true' }, ' →')) : btn(['Later', h('span', { 'aria-hidden': 'true' }, ' →')], null, 'sm', { disabled: true });
  const pager = h('nav', { class: 'pager', 'aria-label': 'Ledger pages' },
    h('span', { class: 'muted small tnum' }, rows.length ? `#${nf(rows[0].seq)} – #${nf(rows[rows.length - 1].seq)}` : ''),
    earlier, later);

  const to = new Date(), from90 = new Date(Date.now() - 90 * 86400000);
  const fFrom = input({ name: 'from', type: 'date', value: isoDay(from90) }), fTo = input({ name: 'to', type: 'date', value: isoDay(to) });
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
    pageHead('Proof history', null, h('a', { class: 'btn', href: '#/keys' }, 'Public keys')),
    h('section', { class: 'card' }, cardHead('Proof report for auditors'),
      h('p', { class: 'muted small' }, 'Everything that happened in a period — for auditors, insurers or compliance reviews.'),
      h('div', { class: 'row' }, field('From', fFrom), field('To', fTo)),
      h('div', { class: 'btns' }, reportLink, packLink)),
    h('section', { class: 'card' }, cardHead('Recent proofs'),
      (proofs || []).length ? table(PH, proofs.map((p) => proofRow(p, PH, true))) : empty('No proofs yet. They appear after the first backup.')),
    h('section', { class: 'card' }, cardHead('Is the history intact?', btn('Check the history', verify, 'primary sm')),
      h('p', { class: 'muted small' }, 'Each record is chained to the one before it, so any change or deletion is detected.'),
      verifyOut,
      ledgerDetails(rows.length ? table(LH, rows.map((e) => tr([
        '#' + e.seq, timeEl(e.time), h('span', { class: 'badge' }, e.kind),
        h('div', { class: 'break' }, e.subject, e.detail ? h('div', { class: 'sub' }, e.detail) : null),
        h('code', { title: e.envelopeDigest || '' }, short(e.envelopeDigest, 12)),
        h('code', { title: `hash ${e.hash}\nprev ${e.prev}` }, short(e.hash, 12)),
      ], LH))) : empty('No records in this range.'), pager)));
  function ledgerDetails(...kids) { const d = tech(...kids); if (params && params.has('from')) d.open = true; return d; }
}

async function pageKeys() {
  const k = await api('/public/keys');
  const all = [...(k.server ? [{ ...k.server, name: k.server.name || 'server', role: 'this dashboard' }] : []), ...(k.agents || []).map((a) => ({ ...a, role: 'server' }))];
  const H = ['Name', 'Signs as', 'Key ID', 'Public key (Ed25519)'];
  return h('div', null,
    backLink('#/proofs', 'Proof history'),
    pageHead('Public keys', 'Auditors use these to check proofs on their own, without trusting this server. The list is also public at /api/public/keys.'),
    h('section', { class: 'card' }, table(H, all.map((x) => tr([x.name, x.role, h('code', { class: 'break' }, x.keyid || '—'), h('code', { class: 'break small' }, String(x.public || ''))], H)))),
    k.bpkeys ? h('section', { class: 'card' }, cardHead('Keys file', copyBtn(() => k.bpkeys, 'Copy', 'Copy the keys file')), h('pre', null, k.bpkeys)) : null);
}

// ---------------------------------------------------------------- alerts

async function pageAlerts() {
  const [open, all] = await Promise.all([api('/alerts?open=1'), api('/alerts?open=0')]);
  setAlertCount(open || []);
  const H = ['When', 'What', 'Message', 'About', 'Status'];
  const row = (a) => tr([
    timeEl(a.created), ALERT_WORDS[a.kind] || String(a.kind || '').replace(/-/g, ' '), h('span', { class: 'break' }, a.message),
    a.sourceId ? h('a', { href: `#/sources/${a.sourceId}`, 'aria-label': 'Open the item for: ' + (a.message || 'this alert') }, 'Open item') : a.agentId ? h('a', { href: '#/agents' }, 'Servers') : '—',
    a.resolved ? h('span', null, 'Fixed ', timeEl(a.resolved)) : h('span', { class: 'pill bad' }, 'Open'),
  ], H);
  const history = (all || []).filter((a) => a.resolved);
  return h('div', null,
    pageHead('Alerts', 'Alerts go away by themselves once the problem is fixed.'),
    h('section', { class: 'card' }, cardHead(`Open (${nf((open || []).length)})`), (open || []).length ? table(H, open.map(row)) : empty('All good — nothing needs your attention.')),
    h('section', { class: 'card' }, cardHead('Earlier'), history.length ? table(H, history.map(row)) : empty('No earlier alerts.')));
}

// -------------------------------------------------------------- settings

// Plain words for the actions in the activity log.
const ACTION_WORDS = {
  setup: 'Created the first administrator', 'sign-in': 'Signed in', 'change-password': 'Changed their password',
  'reset-password': 'Reset a password', 'enable-2fa': 'Turned on two-factor sign-in', 'disable-2fa': 'Turned off two-factor sign-in',
  'reset-2fa': 'Reset two-factor sign-in', 'create-user': 'Added a person', 'delete-user': 'Removed a person',
  'create-token': 'Created an API token', 'delete-token': 'Deleted an API token', 'create-repository': 'Added storage',
  'delete-repository': 'Removed storage', 'save-source': 'Saved an item', 'delete-source': 'Removed an item',
  'run-backup': 'Started a backup', 'run-drill': 'Started a restore test', 'run-check': 'Started a storage health check',
  'create-enroll-token': 'Created a server connection code', enroll: 'Connected a server', 'revoke-agent': 'Disconnected a server',
  'update-notifications': 'Changed alert settings', 'update-public-url': 'Changed the dashboard address', 'update-tsa': 'Changed timestamp services',
  'export-evidence': 'Downloaded a proof report',
};

async function pageActivity() {
  const { params } = hashParts();
  const q = params.get('q') || '';
  const rows = (await api('/activity?' + qs({ q, limit: 50 }))) || [];
  const H = ['When', 'Who', 'What', 'Details'];
  const body = h('tbody');
  const addRows = (list) => list.forEach((a) => body.append(tr([timeEl(a.time), h('span', { class: 'break' }, a.actor), ACTION_WORDS[a.action] || a.action, h('span', { class: 'break small' }, a.detail || '—')], H)));
  addRows(rows);
  const more = h('div', { class: 'form-actions start' });
  let last = rows.length ? rows[rows.length - 1].seq : 0;
  const showMore = (n) => fill(more, n === 50 ? btn('Show older', busy(async () => {
    const next = (await api('/activity?' + qs({ q, limit: 50, before: last }))) || [];
    addRows(next);
    if (next.length) last = next[next.length - 1].seq;
    showMore(next.length);
  }, 'Loading…')) : null);
  showMore(rows.length);
  const search = input({ name: 'q', type: 'search', value: q, placeholder: 'Search by person, action or item…', autocomplete: 'off', 'aria-label': 'Search the activity' });
  const form = h('form', { class: 'search-row', role: 'search', onsubmit: (e) => { e.preventDefault(); location.hash = '#/activity' + (search.value.trim() ? '?' + qs({ q: search.value.trim() }) : ''); } },
    search, h('button', { type: 'submit', class: 'btn' }, 'Search'));
  const t = table(H, []);
  t.querySelector('tbody').replaceWith(body);
  return h('div', null,
    pageHead('Activity', 'Who did what, from sign-ins to changed settings. Every entry is part of the tamper-evident ledger.'),
    h('section', { class: 'card' }, form,
      rows.length ? t : empty(q ? `Nothing matches “${q}”.` : 'Nothing recorded yet.'),
      more));
}

async function pageSettings() {
  const parts = [pageHead('Settings')];
  if (isAdmin()) {
    const [notify, tsa, users, st] = await Promise.all([api('/settings/notify'), api('/settings/tsa'), api('/users'), api('/status', { noAuthRedirect: true })]);
    const tokens = await api('/tokens').catch(() => []);
    parts.push(serverCard(st || {}), notifyCard(notify || {}), usersCard(users || []), tokensCard(tokens || []), tsaCard(tsa || { urls: [] }));
  }
  parts.push(twoFactorCard(), passwordCard());
  return settingsGuard(h('div', null, parts));
}

function serverCard(st) {
  const u = input({ name: 'public-url', type: 'url', code: true, value: st.publicUrl || '', placeholder: 'https://backup.example.com…' });
  const form = h('form', {
    novalidate: true,
    onsubmit: busy(async (e) => {
      e.preventDefault();
      clearErrors(form);
      const v = u.value.trim();
      if (v && !/^https?:\/\/[^\s/]+/i.test(v)) return fieldError(u, 'Enter a full address that starts with http:// or https://, for example https://backup.example.com.');
      const r = await put('/settings/server', { publicUrl: v });
      if (r && r.publicUrl) u.value = r.publicUrl;
      S.dirty = false;
      toast('Address saved', 'ok');
    }),
  },
  st.publicUrlIsLocal ? h('div', { class: 'banner warn' }, 'Other servers can’t reach “localhost”. Set the address they should use, or connecting servers won’t work.') : null,
  field('Address other servers use to reach this dashboard', u, 'For example https://backup.example.com or http://192.168.1.20:8420. Used in the “Connect a server” commands.'),
  h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn primary' }, 'Save address')));
  return card('Dashboard address', form);
}

// notifyCard: every alert channel, and the weekly summary. Secrets are never
// shown again after saving; leaving one empty keeps it.
function notifyCard(resp) {
  const n = (resp && resp.settings) || resp || {};
  const saved = new Set((resp && resp.saved) || []);
  const clear = new Set();
  const txt = (name, value, attrs = {}) => input({ name, code: true, autocomplete: 'off', value: value || '', ...attrs });
  const secret = (key, name) => {
    const el = input({ name, type: 'password', autocomplete: 'off', placeholder: saved.has(key) ? 'Saved — leave empty to keep…' : '' });
    el.dataset.secret = key;
    return el;
  };
  const f = {
    to: input({ name: 'to', type: 'email', multiple: true, code: true, value: n.to || '', placeholder: 'me@example.com…' }),
    from: input({ name: 'from', type: 'email', code: true, value: n.from || '', placeholder: 'backupproof@example.com…' }),
    smtpHost: txt('smtp-host', n.smtpHost, { placeholder: 'smtp.gmail.com…' }),
    smtpPort: input({ name: 'smtp-port', type: 'number', inputmode: 'numeric', value: n.smtpPort || 587 }),
    smtpUser: txt('smtp-user', n.smtpUser),
    smtpPass: secret('smtpPass', 'smtp-password'),
    webhookUrl: txt('webhook', n.webhookUrl, { type: 'url', placeholder: 'https://hooks.slack.com/services/…' }),
    teamsUrl: txt('teams-url', n.teamsUrl, { type: 'url', placeholder: 'https://prod-00.westeurope.logic.azure.com/workflows/…' }),
    telegramToken: secret('telegramToken', 'telegram-token'),
    telegramChatId: txt('telegram-chat', n.telegramChatId, { placeholder: '-1001234567890…' }),
    ntfyUrl: txt('ntfy-url', n.ntfyUrl, { type: 'url', placeholder: 'https://ntfy.sh/my-backups…' }),
    ntfyToken: secret('ntfyToken', 'ntfy-token'),
    gotifyUrl: txt('gotify-url', n.gotifyUrl, { type: 'url', placeholder: 'https://gotify.example.com…' }),
    gotifyToken: secret('gotifyToken', 'gotify-token'),
    pushoverUser: secret('pushoverUser', 'pushover-user'),
    pushoverToken: secret('pushoverToken', 'pushover-token'),
    pagerDutyKey: secret('pagerDutyKey', 'pagerduty-key'),
    heartbeatUrl: txt('heartbeat', n.heartbeatUrl, { type: 'url', placeholder: 'https://hc-ping.com/…' }),
  };
  const weekly = checkbox('Send a weekly summary by email', !!n.weeklyReport);
  const day = select([[1, 'Monday'], [2, 'Tuesday'], [3, 'Wednesday'], [4, 'Thursday'], [5, 'Friday'], [6, 'Saturday'], [0, 'Sunday']].map(([v, l]) => [String(v), l]),
    String(n.weeklyReport ? n.reportDay : 1), { name: 'report-day' });
  const hour = select(Array.from({ length: 24 }, (_, i) => [String(i), clock(i)]), String(n.weeklyReport ? n.reportHour : 8), { name: 'report-hour' });

  // forget lets someone remove a saved secret (turning that channel off).
  const forget = (key) => saved.has(key) ? btn('Forget saved', (e) => { clear.add(key); e.currentTarget.replaceWith(h('span', { class: 'hint' }, 'Will be removed when you save.')); S.dirty = true; }, 'sm quiet') : null;
  const on = (...keys) => keys.some((k) => (f[k] && f[k].value && !f[k].dataset.secret) || saved.has(k)) ? h('span', { class: 'pill ok' }, 'On') : null;
  const channel = (title, state, ...body) => {
    const d = details(title, ...body);
    if (state) d.querySelector('summary').append(' ', state);
    d.classList.add('channel');
    return d;
  };

  const form = h('form', {
    novalidate: true,
    onsubmit: busy(async (e) => {
      e.preventDefault();
      clearErrors(form);
      for (const [el, what] of [[f.to, 'Enter valid email addresses, separated by commas, for example me@example.com.'], [f.from, 'Enter a valid email address, for example backupproof@example.com.'],
        [f.webhookUrl, 'Enter the full webhook address, starting with https://.'], [f.teamsUrl, 'Enter the full Workflows address, starting with https://.'],
        [f.ntfyUrl, 'Enter the full topic address, for example https://ntfy.sh/my-backups.'], [f.gotifyUrl, 'Enter your Gotify server address, starting with https://.'],
        [f.heartbeatUrl, 'Enter the full heartbeat address, starting with https://.']]) {
        if (!el.checkValidity()) { const d = el.closest('details'); if (d) d.open = true; return fieldError(el, what); }
      }
      if (weekly.cb.checked && !(f.smtpHost.value.trim() && f.to.value.trim())) return fieldError(f.to, 'The weekly summary is sent by email. Fill in the email settings above.');
      const body = { clear: [...clear], weeklyReport: weekly.cb.checked, reportDay: Number(day.value), reportHour: Number(hour.value), smtpPort: num(f.smtpPort.value) };
      for (const [k, el] of Object.entries(f)) if (k !== 'smtpPort') body[k] = el.dataset.secret ? el.value : el.value.trim();
      await put('/settings/notify', body);
      S.dirty = false;
      toast('Alert settings saved', 'ok');
      reload();
    }),
  },
  h('p', { class: 'muted' }, 'Get told when a backup or restore test fails, a backup is late, or a server goes quiet, and again when it’s fixed. Use as many channels as you like.'),
  channel('Email', on('smtpHost'),
    h('div', { class: 'row' }, field('To', f.to, 'Several addresses: separate them with commas.'), field('From', f.from)),
    h('div', { class: 'row' }, field('Mail server', f.smtpHost, 'From your email provider, for example smtp.gmail.com.'), field('Port', f.smtpPort, 'Usually 587.'), field('Username', f.smtpUser), field('Password', f.smtpPass)), forget('smtpPass')),
  channel('Slack, Discord or Mattermost', on('webhookUrl'),
    field('Incoming webhook address', f.webhookUrl, 'Create an incoming webhook in your chat app and paste its address.')),
  channel('Microsoft Teams', on('teamsUrl'),
    field('Workflows webhook address', f.teamsUrl, 'In Teams, add the Workflows app to a channel, choose “Post to a channel when a webhook request is received”, and paste the address it gives you.')),
  channel('Telegram', on('telegramToken'),
    h('div', { class: 'row' }, field('Bot token', f.telegramToken, 'From @BotFather in Telegram.'), field('Chat ID', f.telegramChatId, 'Add the bot to your chat; a group ID starts with -100.')), forget('telegramToken')),
  channel('ntfy', on('ntfyUrl'),
    h('div', { class: 'row' }, field('Topic address', f.ntfyUrl, 'ntfy.sh or your own server, with a hard-to-guess topic name.'), field('Access token (optional)', f.ntfyToken, 'Only for protected topics.')), forget('ntfyToken')),
  channel('Gotify', on('gotifyUrl'),
    h('div', { class: 'row' }, field('Server address', f.gotifyUrl), field('Application token', f.gotifyToken, 'Create an application in Gotify and copy its token.')), forget('gotifyToken')),
  channel('Pushover', on('pushoverToken'),
    h('div', { class: 'row' }, field('Your user key', f.pushoverUser, 'Shown on your Pushover dashboard.'), field('Application token', f.pushoverToken, 'Create an application in Pushover.')), forget('pushoverUser'), forget('pushoverToken')),
  channel('PagerDuty', on('pagerDutyKey'),
    field('Integration key', f.pagerDutyKey, 'Add an “Events API v2” integration to a service. Problems open an incident, which is resolved automatically when fixed.'), forget('pagerDutyKey')),
  channel('Heartbeat (advanced)', on('heartbeatUrl'),
    field('Heartbeat address', f.heartbeatUrl, 'Pinged every minute while BackupProof is healthy, so a service like healthchecks.io can tell you if BackupProof itself stops.')),
  h('h3', null, 'Weekly summary'),
  h('p', { class: 'muted small' }, 'One email a week: what was backed up and restore-tested, what failed, and anything that needs attention.'),
  weekly.el,
  h('div', { class: 'row' }, field('Day', day), field('Time', hour, 'In the server’s time zone.')),
  h('div', { class: 'form-actions' },
    btn('Send a test alert', busy(async () => {
      try { await post('/settings/notify/test'); toast('Test sent to every channel that’s set up. Check them.', 'ok'); }
      catch (ex) { toast('Some channels failed: ' + ex.message + '. Fix the details, save, and try again.', 'bad'); }
    }, 'Sending…')),
    btn('Send the summary now', busy(async () => {
      await post('/settings/report/send'); toast('Weekly summary sent. Check your inbox.', 'ok');
    }, 'Sending…')),
    h('button', { type: 'submit', class: 'btn primary' }, 'Save alert settings')),
  h('p', { class: 'hint' }, 'Save before sending a test.'));
  return card('Alerts', form);
}

function tsaCard(t) {
  const ta = h('textarea', { name: 'tsa-urls', rows: 3, inputmode: 'url', spellcheck: 'false', autocapitalize: 'off', placeholder: 'https://freetsa.org/tsr…' }, (t.urls || []).join('\n'));
  return card('Independent timestamps',
    h('p', { class: 'muted small' }, 'An outside timestamp service confirms when each proof was made, so nobody can back-date it — not even this server. One address per line; leave empty to turn off.'),
    details('Show timestamp services', field('Timestamp service addresses (RFC 3161)', ta),
      h('div', { class: 'form-actions' }, btn('Save timestamp services', busy(async () => { await put('/settings/tsa', { urls: lines(ta.value) }); S.dirty = false; toast('Timestamp services saved', 'ok'); }, 'Saving…'), 'primary'))));
}

function usersCard(users) {
  const H = ['Username', 'Can', 'Two-factor', 'Added', ''];
  const ROLE_WORDS = { admin: 'everything (administrator)', operator: 'set up and run backups', auditor: 'only view and download proofs' };
  const u = input({ name: 'new-username', code: true, autocomplete: 'off' });
  const p = input({ name: 'new-user-password', type: 'password', autocomplete: 'off', minlength: 10 });
  const r = select(Object.entries(ROLE_WORDS).reverse().map(([k, v]) => [k, v[0].toUpperCase() + v.slice(1)]), 'operator', { name: 'role' });
  const form = h('form', {
    novalidate: true,
    onsubmit: busy(async (e) => {
      e.preventDefault();
      clearErrors(form);
      if (!u.value.trim()) return fieldError(u, 'Enter a username.');
      if (p.value.length < 10) return fieldError(p, 'The password must be at least 10 characters.');
      await post('/users', { username: u.value.trim(), password: p.value, role: r.value });
      S.dirty = false;
      toast('Person added', 'ok'); reload();
    }, 'Adding…'),
  }, h('h3', null, 'Add a person'), h('div', { class: 'row' }, field('Username', u), field('Password', p, 'At least 10 characters.'), field('They can', r)),
  h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn primary' }, 'Add person')));
  return card('People who can sign in',
    table(H, users.map((x) => tr([h('span', { class: 'break' }, x.username), ROLE_WORDS[x.role] || x.role,
      x.twoFactor ? h('span', { class: 'pill ok' }, 'On') : h('span', { class: 'muted' }, 'Off'), timeEl(x.created),
      x.id === S.user.id ? h('span', { class: 'muted small' }, 'you') : h('div', { class: 'btns' },
        x.twoFactor ? btn('Reset two-factor', busy(async () => {
          if (!(await confirmDlg(`Turn off two-factor sign-in for “${x.username}”? Use this when they’ve lost their phone and recovery codes. They can set it up again after signing in.`, 'Turn it off'))) return;
          await post(`/users/${x.id}/2fa/reset`); toast('Two-factor sign-in turned off for ' + x.username, 'ok'); reload();
        }), 'sm', { 'aria-label': 'Reset two-factor for ' + x.username }) : null,
        btn('Remove', busy(async () => {
          if (!(await confirmDlg(`Remove “${x.username}”? They will no longer be able to sign in.`, 'Remove'))) return;
          await del(`/users/${x.id}`); toast('Removed', 'ok'); reload();
        }), 'sm danger', { 'aria-label': 'Remove ' + x.username }))], H))),
    form);
}

function passwordCard() {
  const cur = input({ name: 'current-password', type: 'password', autocomplete: 'current-password' });
  const p1 = input({ name: 'new-password', type: 'password', autocomplete: 'new-password', minlength: 10 });
  const p2 = input({ name: 'new-password-2', type: 'password', autocomplete: 'new-password' });
  const form = h('form', {
    novalidate: true,
    onsubmit: busy(async (e) => {
      e.preventDefault();
      clearErrors(form);
      if (!cur.value) return fieldError(cur, 'Enter your current password.');
      if (p1.value.length < 10) return fieldError(p1, 'The password must be at least 10 characters.');
      if (p1.value !== p2.value) return fieldError(p2, 'The two passwords are different. Type the same password twice.');
      try { await post('/users/password', { currentPassword: cur.value, password: p1.value }); }
      catch (ex) { if (ex.status === 403) return fieldError(cur, ex.message); throw ex; }
      S.dirty = false;
      toast('Password changed. Sign in with the new one.', 'ok');
      S.user = null; renderAuth();
    }),
  }, h('input', { type: 'text', name: 'username', autocomplete: 'username', value: S.user.username, hidden: true, readonly: true }), field('Current password', cur), h('div', { class: 'row' }, field('New password', p1, 'At least 10 characters.'), field('Type it again', p2)),
  h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn primary' }, 'Change password')));
  return card(`Your sign-in password (${S.user.username})`, form);
}

// qrSvg draws a QR code from rows of "0"/"1" sent by the server.
function qrSvg(rows) {
  const NS = 'http://www.w3.org/2000/svg', n = rows.length, q = 4;
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', `0 0 ${n + 2 * q} ${n + 2 * q}`);
  svg.setAttribute('class', 'qr');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', 'QR code to scan with your authenticator app');
  svg.setAttribute('shape-rendering', 'crispEdges');
  const bg = document.createElementNS(NS, 'rect');
  bg.setAttribute('width', n + 2 * q); bg.setAttribute('height', n + 2 * q); bg.setAttribute('fill', '#fff');
  let d = '';
  rows.forEach((row, y) => { for (let x = 0; x < row.length; x++) if (row[x] === '1') d += `M${x + q} ${y + q}h1v1h-1z`; });
  const path = document.createElementNS(NS, 'path');
  path.setAttribute('d', d); path.setAttribute('fill', '#000');
  svg.append(bg, path);
  return svg;
}

function recoveryCodesBox(codes) {
  const text = 'BackupProof recovery codes for ' + S.user.username + '\nEach works once, instead of a code from your authenticator app.\n\n' + codes.join('\n') + '\n';
  return h('div', null,
    h('p', null, h('strong', null, 'Save these recovery codes.'), ' If you lose your phone, each one lets you sign in once. They won’t be shown again.'),
    h('ul', { class: 'codes', translate: 'no' }, codes.map((c) => h('li', null, h('code', null, c)))),
    h('div', { class: 'btns' }, copyBtn(() => text, 'Copy codes'), btn('Download', () => download(`backupproof-recovery-codes-${S.user.username}.txt`, text), 'sm')));
}

function twoFactorCard() {
  const on = !!S.user.twoFactor;
  const setup = busy(async () => {
    const t = await post('/2fa/setup');
    const code = input({ name: 'one-time-code', autocomplete: 'one-time-code', inputmode: 'numeric', code: true, placeholder: '123456…' });
    const err = errBox();
    const body = h('div', { class: 'tfa-setup' },
      h('ol', { class: 'tfa-steps' },
        h('li', null, 'Open an authenticator app on your phone, such as Google Authenticator, Microsoft Authenticator, 1Password or Bitwarden.'),
        h('li', null, 'Scan this code with it.', h('div', { class: 'qr-wrap' }, qrSvg(t.qr)),
          details('Can’t scan? Enter this key instead', h('p', null, h('code', { class: 'break', translate: 'no' }, t.secret.replace(/(.{4})/g, '$1 ').trim())))),
        h('li', null, 'Type the 6-digit code the app shows.', field('Code', code))),
      err);
    let close;
    const confirm = btn('Turn on two-factor sign-in', busy(async () => {
      err.textContent = '';
      if (!/^\d{6}$/.test(code.value.trim())) return fieldError(code, 'Enter the 6 digits from the app.');
      try {
        const res = await post('/2fa/enable', { code: code.value.trim() });
        S.user.twoFactor = true;
        close();
        modal('Two-factor sign-in is on', recoveryCodesBox(res.recoveryCodes), [], { closeLabel: 'I’ve saved them', onClose: () => reload() });
      } catch (ex) { showErr(err, ex.message); }
    }, 'Checking…'), 'primary');
    close = modal('Set up two-factor sign-in', body, [confirm], { wide: true, initialFocus: code });
  }, 'Preparing…');
  const off = busy(async () => {
    const pw = input({ name: 'current-password', type: 'password', autocomplete: 'current-password' });
    const err = errBox();
    let close;
    const go = btn('Turn off', busy(async () => {
      try { await post('/2fa/disable', { password: pw.value }); S.user.twoFactor = false; close(); toast('Two-factor sign-in is off', 'ok'); reload(); }
      catch (ex) { showErr(err, ex.message); }
    }), 'danger solid');
    close = modal('Turn off two-factor sign-in?', h('div', null, h('p', null, 'Signing in will only need your password. Enter it to confirm.'), field('Your password', pw), err), [go], { initialFocus: pw });
  });
  return card('Two-factor sign-in',
    h('p', null, on
      ? [h('span', { class: 'pill ok' }, 'On'), ' Signing in needs your password and a code from your authenticator app.']
      : [h('span', { class: 'pill' }, 'Off'), ' Add a code from your phone to signing in, so a stolen password isn’t enough.']),
    h('div', { class: 'form-actions start' }, on ? btn('Turn off', off, 'danger') : btn('Set up two-factor sign-in', setup, 'primary')));
}

function tokensCard(tokens) {
  const H = ['Name', 'Can', 'Created by', 'Last used', 'Expires', ''];
  const ROLE = { admin: 'Everything', operator: 'Set up and run backups', auditor: 'Only view' };
  const name = input({ name: 'token-name', autocomplete: 'off', placeholder: 'CI deploys…' });
  const role = select([['auditor', 'Only view (monitoring, reports)'], ['operator', 'Set up and run backups'], ['admin', 'Everything except people and tokens']], 'auditor', { name: 'token-role' });
  const exp = select([['30', '30 days'], ['90', '90 days'], ['365', '1 year'], ['0', 'Never']], '90', { name: 'token-expiry' });
  const form = h('form', {
    novalidate: true,
    onsubmit: busy(async (e) => {
      e.preventDefault();
      clearErrors(form);
      if (!name.value.trim()) return fieldError(name, 'Give the token a name, so you know what uses it.');
      const res = await post('/tokens', { name: name.value.trim(), role: role.value, expiresDays: Number(exp.value) });
      S.dirty = false;
      modal('Copy your new token', h('div', null,
        h('p', null, 'This is the only time it’s shown. Store it where the script can read it, such as a secret in your CI.'),
        h('div', { class: 'pwbox' }, h('code', { class: 'break', translate: 'no' }, res.token), copyBtn(() => res.token, 'Copy', 'Copy the token')),
        h('p', { class: 'hint' }, 'Send it as a header: ', h('code', { translate: 'no' }, 'Authorization: Bearer ' + res.token.slice(0, 12) + '…'))),
      [], { closeLabel: 'Done', onClose: () => reload() });
    }, 'Creating…'),
  }, h('h3', null, 'Create a token'), h('div', { class: 'row' }, field('Name', name), field('It can', role), field('Expires after', exp)),
  h('div', { class: 'form-actions' }, h('button', { type: 'submit', class: 'btn primary' }, 'Create token')));
  return card('API tokens',
    h('p', { class: 'muted' }, 'For scripts and monitoring that use the BackupProof API. Tokens can’t manage people or other tokens.'),
    tokens.length ? table(H, tokens.map((t) => tr([h('span', { class: 'break' }, t.name, ' ', h('code', { class: 'muted small', translate: 'no' }, t.prefix + '…')), ROLE[t.role] || t.role, t.username,
      t.lastUsed ? timeEl(t.lastUsed) : h('span', { class: 'muted' }, 'never'), t.expires ? timeEl(t.expires) : 'never',
      btn('Delete', busy(async () => {
        if (!(await confirmDlg(`Delete the token “${t.name}”? Anything using it stops working straight away.`, 'Delete token'))) return;
        await del(`/tokens/${t.id}`); toast('Token deleted', 'ok'); reload();
      }), 'sm danger quiet', { 'aria-label': 'Delete token ' + t.name })], H))) : h('p', { class: 'muted' }, 'No tokens yet.'),
    form);
}

function settingsGuard(node) {
  return guardDirty(node);
}

// ------------------------------------------------------------------ boot

async function boot() {
  try {
    S.status = await api('/status', { noAuthRedirect: true });
  } catch (err) {
    const app = document.getElementById('app');
    app.className = '';
    fill(app, h('main', { class: 'auth', id: 'main', tabindex: '-1' }, brand(), h('h1', { class: 'auth-title' }, 'Can’t reach BackupProof'),
      h('div', { class: 'banner bad', role: 'alert' }, h('p', null, err.message), btn('Try again', () => boot(), 'sm primary'))));
    document.title = 'Can’t connect · BackupProof';
    return;
  }
  S.csrf = S.status.csrf || '';
  S.user = S.status.user || null;
  if (!S.user) renderAuth();
  else renderShell();
}

document.addEventListener('visibilitychange', () => { if (!document.hidden && S.user) refreshAlertCount(); });

const skip = document.querySelector('.skip-link');
if (skip) {
  skip.addEventListener('click', (e) => {
    e.preventDefault();
    const m = document.getElementById('main');
    if (m) { m.focus({ preventScroll: true }); m.scrollIntoView(); }
  });
}

boot();
