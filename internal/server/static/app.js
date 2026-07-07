// Shared helpers for the Central Backup GUI.

function el(id) { return document.getElementById(id); }

async function api(method, url, body) {
  const opts = { method, headers: { 'X-Requested-With': 'fetch' } };
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(url, opts);
  if (res.status === 401 && !location.pathname.startsWith('/login')) {
    location.href = '/login';
    throw new Error('signed out');
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

async function logout() {
  try { await api('POST', '/api/admin/logout'); } catch {}
  location.href = '/login';
}

function esc(s) {
  return String(s ?? '').replace(/[&<>"']/g, c =>
    ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

function fmtBytes(n) {
  n = Number(n) || 0;
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return (i === 0 ? n : n.toFixed(1)) + ' ' + units[i];
}

function fmtTime(unix) {
  if (!unix) return '—';
  const d = new Date(unix * 1000);
  const diff = (Date.now() - d.getTime()) / 1000;
  if (diff >= 0 && diff < 60) return 'just now';
  if (diff >= 0 && diff < 3600) return Math.floor(diff / 60) + ' min ago';
  if (diff >= 0 && diff < 86400) return Math.floor(diff / 3600) + ' h ago';
  return d.toLocaleString();
}

function fmtDuration(run) {
  const start = run.StartedAt || run.CreatedAt;
  const end = run.FinishedAt || (run.Status === 'running' ? Math.floor(Date.now() / 1000) : 0);
  if (!start || !end || end < start) return '—';
  const s = end - start;
  if (s < 60) return s + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm ' + (s % 60) + 's';
  return Math.floor(s / 3600) + 'h ' + Math.floor((s % 3600) / 60) + 'm';
}

function statusDot(state) {
  return `<span class="dot ${state}"></span>`;
}

function statusBadge(status) {
  return `<span class="badge ${esc(status)}">${esc(status)}</span>`;
}

// fillRows replaces a table's tbody with rows of pre-escaped cell HTML.
function fillRows(tableID, rows, emptyMsg) {
  const tbody = document.querySelector('#' + tableID + ' tbody');
  if (!rows.length) {
    const cols = document.querySelectorAll('#' + tableID + ' thead th').length;
    tbody.innerHTML = `<tr><td colspan="${cols}" class="muted">${esc(emptyMsg || 'Nothing here yet.')}</td></tr>`;
    return;
  }
  tbody.innerHTML = rows.map(cells =>
    '<tr>' + cells.map(c => `<td>${c}</td>`).join('') + '</tr>').join('');
}

let toastTimer;
function toast(msg) {
  const t = el('toast');
  t.textContent = msg;
  t.classList.add('show');
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => t.classList.remove('show'), 4000);
}
