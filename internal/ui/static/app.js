'use strict';

// aileron-ui frontend: poll the API, render builds/clones, submit manifests,
// open consoles. Deliberately dependency-free vanilla JS.

const REFRESH_MS = 4000;

function showError(msg) {
  document.getElementById('err').textContent = msg || '';
}

function badgeClass(phase) {
  if (phase === 'Succeeded' || phase === 'Ready') return 'ok';
  if (phase === 'Failed') return 'fail';
  return 'warn';
}

function fmtTime(t) {
  if (!t) return '';
  try { return new Date(t).toLocaleString(); } catch (_) { return t; }
}

function el(tag, attrs, children) {
  const e = document.createElement(tag);
  if (attrs) for (const k in attrs) {
    if (k === 'class') e.className = attrs[k];
    else if (k === 'text') e.textContent = attrs[k];
    else e.setAttribute(k, attrs[k]);
  }
  (children || []).forEach((c) => e.appendChild(c));
  return e;
}

function consoleButton(c) {
  const b = el('button', { class: 'console', text: 'console: ' + c.vmName });
  b.addEventListener('click', () => {
    const q = new URLSearchParams({ ns: c.namespace, vmi: c.vmi, name: c.vmName });
    window.open('/console.html?' + q.toString(), '_blank', 'noopener');
  });
  return b;
}

// cloneButton creates a VirtualMachineClone from a build's template. The
// clone's templateName is the build name by convention; the template only
// exists once the build has succeeded.
function cloneButton(buildName) {
  const b = el('button', { class: 'clone', text: 'clone' });
  b.addEventListener('click', async () => {
    const name = prompt('Name for the new clone of "' + buildName + '":', buildName + '-clone');
    if (!name) return;
    const manifest =
      'apiVersion: ruddervirt.io/v1alpha1\n' +
      'kind: VirtualMachineClone\n' +
      'metadata:\n' +
      '  name: ' + name + '\n' +
      'spec:\n' +
      '  templateName: ' + buildName + '\n';
    try {
      const res = await fetch('/api/clones', {
        method: 'POST',
        headers: { 'Content-Type': 'application/yaml' },
        body: manifest,
      });
      if (!res.ok) throw new Error(await errText(res));
      refresh();
    } catch (e) { alert('clone failed: ' + e.message); }
  });
  return b;
}

function deleteButton(kind, name) {
  const b = el('button', { class: 'danger', text: 'delete' });
  b.addEventListener('click', async () => {
    if (!confirm('Delete ' + kind + ' "' + name + '"?')) return;
    try {
      const res = await fetch('/api/' + kind + '/' + encodeURIComponent(name), { method: 'DELETE' });
      if (!res.ok && res.status !== 404) throw new Error(await errText(res));
      refresh();
    } catch (e) { showError('delete failed: ' + e.message); }
  });
  return b;
}

// logsButton opens the coordinator (provisioner) logs for a build VM in a new
// tab. The endpoint streams text/plain.
function logsButton(buildName, vmName) {
  const b = el('button', { class: 'logs', text: 'logs: ' + vmName });
  b.addEventListener('click', () => {
    const url = '/api/builds/' + encodeURIComponent(buildName) +
      '/logs?vm=' + encodeURIComponent(vmName);
    window.open(url, '_blank', 'noopener');
  });
  return b;
}

// buildProvisioners renders a collapsible per-VM provisioner status section
// (one row per step) plus a logs button per VM. Returns null when the build
// has no VM statuses yet.
function buildProvisioners(buildName, vms) {
  if (!vms || vms.length === 0) return null;
  const det = el('details', { class: 'provs' }, [el('summary', { text: 'provisioners' })]);
  vms.forEach((vm) => {
    const head = el('div', { class: 'prov-vm' }, [
      el('span', { class: 'name', text: vm.name }),
      el('span', { class: 'badge ' + badgeClass(vm.phase), text: vm.phase || '?' }),
    ]);
    head.appendChild(logsButton(buildName, vm.name));
    det.appendChild(head);

    (vm.provisioners || []).forEach((p) => {
      const label = (p.name || ('#' + p.index)) + ' (' + p.type + ')';
      const row = el('div', { class: 'prov-row' }, [
        el('span', { class: 'badge ' + badgeClass(p.status), text: p.status || '?' }),
        el('span', { text: label }),
      ]);
      if (p.duration) row.appendChild(el('span', { class: 'time', text: p.duration }));
      det.appendChild(row);
      if (p.message) det.appendChild(el('div', { class: 'msg prov-msg', text: p.message }));
    });
    if (!vm.provisioners || vm.provisioners.length === 0) {
      det.appendChild(el('div', { class: 'empty', text: 'no provisioner results yet' }));
    }
  });
  return det;
}

function renderItem(kind, it) {
  const head = el('div', { class: 'head' }, [
    el('span', { class: 'name', text: it.name }),
    el('span', { class: 'badge ' + badgeClass(it.phase), text: it.phase || 'Unknown' }),
    el('span', { class: 'time', text: fmtTime(it.completionTime || it.startTime) }),
  ]);
  const actions = el('div', { class: 'actions' }, []);
  (it.consoles || []).forEach((c) => actions.appendChild(consoleButton(c)));
  if (kind === 'builds' && it.phase === 'Succeeded') {
    actions.appendChild(cloneButton(it.name));
  }
  actions.appendChild(deleteButton(kind, it.name));

  const kids = [head];
  if (it.templateName) kids.push(el('div', { class: 'msg', text: 'template: ' + it.templateName }));
  if (it.message) kids.push(el('div', { class: 'msg', text: it.message }));
  if (kind === 'builds') {
    const provs = buildProvisioners(it.name, it.vms);
    if (provs) kids.push(provs);
  }
  kids.push(actions);
  return el('div', { class: 'item' }, kids);
}

function renderGradeItem(it) {
  const head = el('div', { class: 'head' }, [
    el('span', { class: 'name', text: it.name }),
    el('span', { class: 'badge ' + badgeClass(it.phase), text: it.phase || 'Unknown' }),
    el('span', { class: 'time', text: fmtTime(it.completedAt || it.startedAt) }),
  ]);
  const kids = [head];
  if (it.targetNamespace) kids.push(el('div', { class: 'msg', text: 'target ns: ' + it.targetNamespace }));
  if (it.message) kids.push(el('div', { class: 'msg', text: it.message }));
  (it.vms || []).forEach((vm) => {
    let line = vm.name + ': ' + (vm.phase || '?');
    if (vm.message) line += ' - ' + vm.message;
    kids.push(el('div', { class: 'msg', text: line }));
  });
  kids.push(el('div', { class: 'actions' }, [deleteButton('grades', it.name)]));
  return el('div', { class: 'item' }, kids);
}

function renderList(containerId, items, renderer) {
  const c = document.getElementById(containerId);
  c.innerHTML = '';
  if (!items || items.length === 0) {
    c.appendChild(el('div', { class: 'empty', text: 'none' }));
    return;
  }
  items.forEach((it) => c.appendChild(renderer(it)));
}

async function errText(res) {
  try { const j = await res.json(); return j.error || res.statusText; }
  catch (_) { return res.statusText || ('HTTP ' + res.status); }
}

async function fetchList(path) {
  const res = await fetch(path);
  if (!res.ok) throw new Error(await errText(res));
  return res.json();
}

async function refresh() {
  try {
    const [builds, clones, grades] = await Promise.all([
      fetchList('/api/builds'),
      fetchList('/api/clones'),
      fetchList('/api/grades'),
    ]);
    renderList('builds', builds, (it) => renderItem('builds', it));
    renderList('clones', clones, (it) => renderItem('clones', it));
    renderList('grades', grades, renderGradeItem);
    showError('');
  } catch (e) {
    showError('refresh failed: ' + e.message);
  }
}

// setFormStatus writes persistent submit feedback into a per-form status line.
// It is independent of the global #err / polling refresh, which would otherwise
// clear a submit error within one refresh tick.
function setFormStatus(statusId, msg, ok) {
  const e = document.getElementById(statusId);
  e.textContent = msg || '';
  e.className = 'form-status' + (msg ? (ok ? ' ok' : ' fail') : '');
}

function wireSubmit(btnId, textId, kind, statusId) {
  document.getElementById(btnId).addEventListener('click', async () => {
    const body = document.getElementById(textId).value;
    setFormStatus(statusId, 'submitting...', true);
    try {
      const res = await fetch('/api/' + kind, {
        method: 'POST',
        headers: { 'Content-Type': 'application/yaml' },
        body,
      });
      if (!res.ok) throw new Error(await errText(res));
      const created = await res.json();
      setFormStatus(statusId, 'created ' + (created.name || ''), true);
      refresh();
    } catch (e) { setFormStatus(statusId, 'submit failed: ' + e.message, false); }
  });
}

document.getElementById('build-yaml').value = window.SAMPLE_BUILD || '';
document.getElementById('grade-yaml').value = window.SAMPLE_GRADE || '';
wireSubmit('build-submit', 'build-yaml', 'builds', 'build-status');
wireSubmit('grade-submit', 'grade-yaml', 'grades', 'grade-status');

refresh();
setInterval(refresh, REFRESH_MS);
