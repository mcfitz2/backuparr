const appSelect = document.getElementById('appSelect');
const backendSelect = document.getElementById('backendSelect');
const refreshBtn = document.getElementById('refreshBtn');
const backupSelectedBtn = document.getElementById('backupSelectedBtn');
const backupAllBtn = document.getElementById('backupAllBtn');
const statusEl = document.getElementById('status');
const logsSectionEl = document.getElementById('backupLogsSection');
const logsEl = document.getElementById('backupLogs');
const tbody = document.querySelector('#backupsTable tbody');

let apps = [];
let activeSocket = null;

const retentionGrid = document.getElementById('retentionGrid');

function updateRetentionPanel() {
  const app = apps.find(a => a.name === selectedApp());
  retentionGrid.innerHTML = '';
  if (!app) return;

  const r = app.retention || {};
  const items = [
    { label: 'Last',    value: r.keepLast,    cls: 'latest' },
    { label: 'Hourly',  value: r.keepHourly,  cls: 'hourly' },
    { label: 'Daily',   value: r.keepDaily,   cls: 'daily' },
    { label: 'Weekly',  value: r.keepWeekly,  cls: 'weekly' },
    { label: 'Monthly', value: r.keepMonthly, cls: 'monthly' },
    { label: 'Yearly',  value: r.keepYearly,  cls: 'yearly' },
  ];

  items.forEach(({ label, value, cls }) => {
    const card = document.createElement('div');
    card.className = `retention-card retention-${cls}`;
    const num = document.createElement('span');
    num.className = 'retention-value';
    num.textContent = value || 0;
    const lbl = document.createElement('span');
    lbl.className = 'retention-label';
    lbl.textContent = label;
    card.appendChild(num);
    card.appendChild(lbl);
    retentionGrid.appendChild(card);
  });
}

function setStatus(message) {
  statusEl.textContent = message || '';
}

function setBusy(isBusy) {
  refreshBtn.disabled = isBusy;
  backupSelectedBtn.disabled = isBusy;
  backupAllBtn.disabled = isBusy;
}

function showLogsSection(show) {
  logsSectionEl.classList.toggle('hidden', !show);
}

function setLogs(lines) {
  logsEl.textContent = (lines || []).join('\n');
  logsEl.scrollTop = logsEl.scrollHeight;
}

function summarizeResults(job) {
  const results = job.results || [];
  const ok = results.filter(r => r.ok).length;
  const failed = results.length - ok;
  return { ok, failed, total: results.length };
}

function formatBytes(bytes) {
  if (bytes == null) return '-';
  if (bytes < 1024) return `${bytes} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let value = bytes / 1024;
  let i = 0;
  while (value >= 1024 && i < units.length - 1) {
    value /= 1024;
    i++;
  }
  return `${value.toFixed(1)} ${units[i]}`;
}

function selectedApp() {
  return appSelect.value;
}

function selectedBackend() {
  return backendSelect.value;
}

function updateBackends() {
  const app = apps.find(a => a.name === selectedApp());
  backendSelect.innerHTML = '';
  (app?.backends || []).forEach(name => {
    const opt = document.createElement('option');
    opt.value = name;
    opt.textContent = name;
    backendSelect.appendChild(opt);
  });
  updateRetentionPanel();
}

async function loadApps() {
  const res = await fetch('/api/apps');
  if (!res.ok) throw new Error('failed to load apps');
  const data = await res.json();
  apps = data.apps || [];

  appSelect.innerHTML = '';
  apps.forEach(app => {
    const opt = document.createElement('option');
    opt.value = app.name;
    opt.textContent = `${app.name} (${app.appType})`;
    appSelect.appendChild(opt);
  });

  updateBackends();
}

async function deleteBackup(key) {
  if (!confirm(`Delete backup?\n\n${key}`)) return;

  const params = new URLSearchParams({
    app: selectedApp(),
    backend: selectedBackend(),
    key,
  });

  const res = await fetch(`/api/backups?${params.toString()}`, { method: 'DELETE' });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || 'delete failed');
  }

  await loadBackups();
}

async function loadBackups() {
  const app = selectedApp();
  const backend = selectedBackend();
  if (!app || !backend) {
    tbody.innerHTML = '';
    return;
  }

  setStatus('Loading backups...');
  const params = new URLSearchParams({ app, backend });
  const res = await fetch(`/api/backups?${params.toString()}`);
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || 'failed to load backups');
  }

  const data = await res.json();
  const backups = data.backups || [];

  tbody.innerHTML = '';
  backups.forEach(b => {
    const tr = document.createElement('tr');

    const fileTd = document.createElement('td');
    fileTd.textContent = b.FileName || b.fileName || '-';

    const sizeTd = document.createElement('td');
    sizeTd.textContent = formatBytes(b.Size ?? b.size);

    const createdTd = document.createElement('td');
    const created = b.CreatedAt || b.createdAt;
    createdTd.textContent = created ? new Date(created).toLocaleString() : '-';

    const retentionTd = document.createElement('td');
    const buckets = b.retentionBuckets || [];
    if (buckets.length > 0) {
      buckets.forEach(label => {
        const badge = document.createElement('span');
        badge.className = `badge badge-${label}`;
        badge.textContent = label;
        retentionTd.appendChild(badge);
      });
    } else {
      const badge = document.createElement('span');
      badge.className = 'badge badge-prunable';
      badge.textContent = 'prunable';
      retentionTd.appendChild(badge);
    }

    const keyTd = document.createElement('td');
    keyTd.className = 'key';
    const key = b.Key || b.key;
    keyTd.textContent = key || '-';

    const actionTd = document.createElement('td');
    const delBtn = document.createElement('button');
    delBtn.className = 'danger';
    delBtn.textContent = 'Delete';
    delBtn.disabled = !key;
    delBtn.onclick = async () => {
      try {
        await deleteBackup(key);
      } catch (err) {
        setStatus(`Error: ${err.message}`);
      }
    };
    actionTd.appendChild(delBtn);

    tr.appendChild(fileTd);
    tr.appendChild(sizeTd);
    tr.appendChild(createdTd);
    tr.appendChild(retentionTd);
    tr.appendChild(keyTd);
    tr.appendChild(actionTd);
    tbody.appendChild(tr);
  });

  setStatus(`${backups.length} backup(s)`);
}

async function triggerBackup(payload) {
  setBusy(true);
  try {
    showLogsSection(true);
    setStatus('Starting backup...');
    setLogs([]);

    const res = await fetch('/api/backup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });

    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      throw new Error(body.error || 'backup failed');
    }

    if (!body.jobId) {
      throw new Error('backup job id missing');
    }

    const job = await streamJob(body.jobId);
    const summary = summarizeResults(job);
    setStatus(`Backup complete: ${summary.ok} succeeded, ${summary.failed} failed`);

    await loadBackups();
  } finally {
    setBusy(false);
  }
}

function renderJobUpdate(job) {
  if (job.error) {
    throw new Error(job.error);
  }

  setLogs(job.logs || []);

  if (job.running) {
    setStatus(`Backup running (${job.status})...`);
    return;
  }

  const summary = summarizeResults(job);
  setStatus(`Backup ${job.status}: ${summary.ok} succeeded, ${summary.failed} failed`);
}

async function pollJobUntilDone(jobId) {
  while (true) {
    const params = new URLSearchParams({ id: jobId });
    const res = await fetch(`/api/backup?${params.toString()}`);
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.error || 'failed to poll backup job');

    renderJobUpdate(body);
    if (!body.running) return body;

    await new Promise(resolve => setTimeout(resolve, 1000));
  }
}

function streamJob(jobId) {
  return new Promise((resolve, reject) => {
    const protocol = window.location.protocol === 'https:' ? 'wss' : 'ws';
    const url = `${protocol}://${window.location.host}/api/backup/ws?id=${encodeURIComponent(jobId)}`;

    let settled = false;
    let lastJob = null;
    const ws = new WebSocket(url);
    activeSocket = ws;

    ws.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data);
        renderJobUpdate(data);
        lastJob = data;
        if (data.running === false && !settled) {
          settled = true;
          ws.close();
          resolve(data);
        }
      } catch (err) {
        if (!settled) {
          settled = true;
          reject(err);
        }
      }
    };

    ws.onerror = () => {
      // handled by onclose fallback
    };

    ws.onclose = async () => {
      if (activeSocket === ws) {
        activeSocket = null;
      }
      if (settled) return;
      if (lastJob && lastJob.running === false) {
        settled = true;
        resolve(lastJob);
        return;
      }

      try {
        const finalJob = await pollJobUntilDone(jobId);
        if (!settled) {
          settled = true;
          resolve(finalJob);
        }
      } catch (err) {
        if (!settled) {
          settled = true;
          reject(err);
        }
      }
    };
  });
}

async function init() {
  try {
    showLogsSection(false);
    await loadApps();
    await loadBackups();
    setLogs([]);
  } catch (err) {
    setStatus(`Error: ${err.message}`);
  }
}

appSelect.addEventListener('change', async () => {
  updateBackends();
  await loadBackups();
});

backendSelect.addEventListener('change', loadBackups);
refreshBtn.addEventListener('click', loadBackups);
backupSelectedBtn.addEventListener('click', async () => {
  try {
    await triggerBackup({ app: selectedApp() });
  } catch (err) {
    setStatus(`Error: ${err.message}`);
  }
});

backupAllBtn.addEventListener('click', async () => {
  try {
    await triggerBackup({ all: true });
  } catch (err) {
    setStatus(`Error: ${err.message}`);
  }
});

init();
