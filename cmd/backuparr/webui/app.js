const appSelect = document.getElementById('appSelect');
const backendSelect = document.getElementById('backendSelect');
const refreshBtn = document.getElementById('refreshBtn');
const backupSelectedBtn = document.getElementById('backupSelectedBtn');
const backupAllBtn = document.getElementById('backupAllBtn');
const statusEl = document.getElementById('status');
const tbody = document.querySelector('#backupsTable tbody');

let apps = [];

function setStatus(message) {
  statusEl.textContent = message || '';
}

function setBusy(isBusy) {
  refreshBtn.disabled = isBusy;
  backupSelectedBtn.disabled = isBusy;
  backupAllBtn.disabled = isBusy;
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
    tr.appendChild(keyTd);
    tr.appendChild(actionTd);
    tbody.appendChild(tr);
  });

  setStatus(`${backups.length} backup(s)`);
}

async function triggerBackup(payload) {
  setBusy(true);
  try {
    setStatus('Running backup...');

    const res = await fetch('/api/backup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });

    const body = await res.json().catch(() => ({}));
    if (!res.ok && res.status !== 207) {
      throw new Error(body.error || 'backup failed');
    }

    const results = body.results || [];
    const ok = results.filter(r => r.ok).length;
    const failed = results.length - ok;
    setStatus(`Backup complete: ${ok} succeeded, ${failed} failed`);

    await loadBackups();
  } finally {
    setBusy(false);
  }
}

async function init() {
  try {
    await loadApps();
    await loadBackups();
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
