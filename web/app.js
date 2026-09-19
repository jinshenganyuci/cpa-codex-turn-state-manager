'use strict';
const base = '/v0/management/codex-turn-state-manager';
const $ = id => document.getElementById(id);
let key = '', keySource = '', busy = false, saving = false, selectionDirty = false, selectionRevision = 0;
const loginStorageKey = 'cpa-turn-state-manager.auth.v1';
const panelStorageKey = 'cli-proxy-auth';
const storagePrefix = 'enc::v1::';
// Match CPA's reversible storage encoding. This is obfuscation, not encryption.
function storageMask() {
  return new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent);
}
function readStored(name) {
  try {
    let raw = localStorage.getItem(name);
    if (!raw || raw.length > 65536) return null;
    if (raw.startsWith(storagePrefix)) {
      const bytes = Uint8Array.from(atob(raw.slice(storagePrefix.length)), c => c.charCodeAt(0));
      const mask = storageMask();
      raw = new TextDecoder().decode(bytes.map((value, index) => value ^ mask[index % mask.length]));
    }
    return JSON.parse(raw);
  } catch { return null; }
}
function storedAuth(name) {
  const stored = readStored(name), state = stored?.state || stored;
  if (!state || typeof state.apiBase !== 'string' || typeof state.managementKey !== 'string') return '';
  try {
    const url = new URL(state.apiBase);
    if (url.origin !== location.origin || url.username || url.password) return '';
    if (!['/', '/v0/management', '/v0/management/'].includes(url.pathname)) return '';
    return state.managementKey.trim();
  } catch { return ''; }
}
function rememberLogin() {
  if (keySource !== 'manual' || !key) return;
  try {
    const raw = JSON.stringify({apiBase: location.origin, managementKey: key});
    const mask = storageMask();
    const bytes = new TextEncoder().encode(raw).map((value, index) => value ^ mask[index % mask.length]);
    localStorage.setItem(loginStorageKey, storagePrefix + btoa(String.fromCharCode(...bytes)));
    keySource = 'saved';
  } catch { /* Keep the current login working if browser storage is unavailable. */ }
}
function loginVisibility(hidden) {
  $('login').hidden = hidden;
  $('changeLogin').hidden = !hidden;
}
function clearLogin(forget = true) {
  if (forget) {
    try { localStorage.removeItem(loginStorageKey); } catch { /* Storage may be disabled. */ }
  }
  key = ''; keySource = ''; selectionDirty = false; selectionRevision = 0;
  $('key').value = ''; $('modelChoices').replaceChildren(); $('credentialChoices').replaceChildren();
  $('accounts').replaceChildren(); $('history').replaceChildren();
  $('priorityStatus').textContent = ''; $('historyPersistence').textContent = ''; $('saveSelection').disabled = true;
  $('modeLabel').textContent = '等待连接'; $('valid').textContent = '—';
  $('target292').textContent = '—'; $('target332').textContent = '—';
  $('selectionSummary').textContent = '等待连接'; $('guardStatus').textContent = '等待请求处理状态。';
  loginVisibility(false);
}
async function restoreLogin() {
  const candidates = [{source: 'saved', value: storedAuth(loginStorageKey)}, {source: 'panel', value: storedAuth(panelStorageKey)}];
  const tried = new Set();
  for (const candidate of candidates) {
    if (!candidate.value || tried.has(candidate.value)) continue;
    tried.add(candidate.value); key = candidate.value; keySource = candidate.source;
    loginVisibility(true); notice('正在自动连接…');
    const connected = await refresh();
    // Keep credentials across network failures; the normal poll retries later.
    if (connected || key) return;
  }
  loginVisibility(false);
  if (!tried.size) notice('首次连接后会自动记住；自动沿用 CPA 已保存的登录。');
}
const modeNames = {observe: '只观察', replace_only: '仅替换 312', force: '优先使用有效缓存'};
const selectionErrors = {
  selection_changed_reload_before_saving: '其他窗口已修改选择，请重新载入后再保存。',
  selection_persistence_not_enabled: '当前实例没有启用勾选范围的持久保存。',
  credential_not_available: '凭据已停用或移除，请重新载入选择。',
  credential_changed_reload_before_saving: '凭据身份已变化，请重新载入后重新勾选。',
  model_not_available: '所选模型已不在候选列表，请重新载入。',
  model_out_of_credential_scope: '所选模型不在该凭据允许的模型范围内。',
  selection_save_failed: '保存失败，当前生效的选择没有改变。',
  probe_already_running: '该组合正在获取，无需重复提交。',
  probe_retry_later: '该组合尚在重试间隔或退避期，本次没有排队，请稍后重试。'
};
function notice(text) { $('notice').textContent = text; }
async function api(path, body) {
  if (!key) throw new Error('请先连接 CPA。');
  const requestKey = key;
  const response = await fetch(base + path, {
    method: body === undefined ? 'GET' : 'POST',
    headers: {Authorization: 'Bearer ' + requestKey, 'Content-Type': 'application/json'},
    body: body === undefined ? undefined : JSON.stringify(body), cache: 'no-store'
  });
  const data = await response.json().catch(() => ({}));
  if (requestKey !== key) throw new Error('登录已切换。');
  if (response.status === 401 || response.status === 403) {
    clearLogin(keySource !== 'panel');
    throw new Error('登录已失效，请重新连接。');
  }
  if (!response.ok) throw new Error(selectionErrors[data.error] || '请求失败：' + (data.error || response.status));
  return data;
}
function cell(row, text, cls) {
  const td = document.createElement('td');
  if (cls) { const span = document.createElement('span'); span.className = cls; span.textContent = text; td.append(span); }
  else td.textContent = text;
  row.append(td);
  return td;
}
function time(value) { return !value || value.startsWith('0001') ? '—' : new Date(value).toLocaleString(); }
function planName(value) {
  return ({pro: 'Pro', plus: 'Plus', free: 'Free', team: 'Team', business: 'Business', enterprise: 'Enterprise', edu: 'Edu', go: 'Go'})[value] || '未知套餐';
}
function credentialCell(row, entry, fallback) {
  const profile = entry.email ? entry : fallback || entry;
  const td = cell(row, profile.email || entry.credential_label || profile.label || entry.account);
  td.className = 'credential-cell'; td.title = entry.account || '';
  const details = document.createElement('small'); details.className = 'credential-detail';
  details.textContent = planName(profile.plan_type); td.append(details);
}
function sourceName(value) { return ({business: '业务观测', probe: '后台探测'})[value] || '旧记录'; }
function stateCell(row, length, source, valid, expires) {
  const td = cell(row, valid ? length + ' · ' + sourceName(source) : '暂无有效值');
  if (valid && expires) {const note=document.createElement('small');note.className='credential-detail';note.textContent='到期 '+time(expires);td.append(note);}
  return td;
}
function selectedValues(container) {
  return Array.from($(container).querySelectorAll('input:checked')).filter(input => !input.disabled).map(input => input.value);
}
function updateSelectionSummary() {
  const accounts = selectedValues('credentialChoices').length;
  const models = selectedValues('modelChoices').length;
  $('selectionSummary').textContent = accounts + ' 个凭据 × ' + models + ' 个模型';
}
function choice(container, value, label, checked, disabled, detail) {
  const wrapper = document.createElement('label'); wrapper.className = 'scope-option';
  const input = document.createElement('input'); input.type = 'checkbox'; input.value = value;
  input.checked = checked; input.disabled = disabled; input.setAttribute('aria-label', label);
  input.addEventListener('change', () => {
    selectionDirty = true; $('saveSelection').disabled = false;
    $('selectionNote').textContent = '有未保存的勾选，点击“保存勾选”后生效。'; updateSelectionSummary();
  });
  const text = document.createElement('span'); text.textContent = label;
  if (detail) { const small = document.createElement('small'); small.textContent = detail; text.append(small); }
  wrapper.append(input, text); $(container).append(wrapper);
}
function renderSelection(selection) {
  if (!selection || selectionDirty || saving || selection.revision < selectionRevision) return;
  selectionRevision = selection.revision;
  $('modelChoices').replaceChildren(); $('credentialChoices').replaceChildren();
  const disabled = !selection.required || !selection.persistent;
  for (const model of selection.model_options || []) choice('modelChoices', model, model, selection.models.includes(model), disabled);
  for (const account of selection.accounts || []) choice('credentialChoices', account.account, account.email || account.label,
    account.selected, disabled || account.disabled, planName(account.plan_type) + ' · ' + account.label + (account.disabled ? ' · 已停用' : ''));
  if (!(selection.accounts || []).length) {
    const message = document.createElement('p'); message.className = 'selection-empty'; message.textContent = '暂无可勾选的 Codex 凭据。'; $('credentialChoices').append(message);
  }
  $('saveSelection').disabled = true;
  $('selectionNote').textContent = disabled ? '当前实例未启用勾选控制。' : '勾选已保存，重启后仍有效。取消勾选会停止获取与替换，保留已有档案。';
  updateSelectionSummary();
}
async function refresh() {
  if (!key || busy) return;
  busy = true;
  try {
    const data = await api('/status');
    rememberLogin(); loginVisibility(true);
    if (data.selection && data.selection.revision < selectionRevision) return;
    $('modeLabel').textContent = modeNames[data.mode] + (data.dry_run ? ' · 模拟' : '');
    $('valid').textContent = data.valid_count;
    $('guardStatus').textContent = data.blocking_active ? '无有效 292 / 332 时拦截已勾选请求（503）；后台继续获取，成功后自动放行。' : data.block_without_state ? '拦截已配置；切回强制模式并关闭模拟后生效。' : '无有效 292 / 332 时正常放行；后台继续获取，成功后自动使用缓存。';
    $('priorityStatus').textContent = (data.prefer_292 ? '292 首选 · 332 保底并继续寻找 292' : '使用已验收的状态值')
      + (data.require_model_match ? ' · 模型一致才入库' : '') + (data.standby_enabled ? ' · 提前准备备用值' : '');
    $('historyPersistence').textContent = data.persistence_error ? '保存失败：'+data.persistence_error : data.history_persistent ? '历史已启用持久化 · 每 5 秒刷新' : '每 5 秒刷新 · 最多 200 条';
    const history = data.history || [];
    $('target292').textContent = history.filter(x => x.length === 292 && x.success && (!x.acceptance || ['accepted','ok'].includes(x.acceptance))).length;
    $('target332').textContent = history.filter(x => x.length === 332 && x.success && (!x.acceptance || ['accepted','ok'].includes(x.acceptance))).length;
    $('effort').textContent = data.reasoning_effort;
    if (document.activeElement !== $('mode')) $('mode').value = data.mode;
    if (document.activeElement !== $('dry')) $('dry').checked = data.dry_run;
    renderSelection(data.selection);
    const profiles = new Map((data.selection?.accounts || []).map(account => [account.account, account]));
    $('accounts').replaceChildren();
    for (const e of data.entries) {
      const row = document.createElement('tr');
      credentialCell(row, e, profiles.get(e.account)); cell(row, e.model); cell(row, e.selected ? '已勾选' : '未勾选');
      stateCell(row,e.state_length,e.source,e.valid,e.expires_at);
      stateCell(row,e.standby?.length,e.standby?.source,e.standby?.valid,e.standby?.expires_at);
      cell(row, !e.selected ? '未启用' : e.valid ? time(e.expires_at) : '暂无有效缓存');
      cell(row, e.next_refresh_at ? (e.probing ? '刷新中' : time(e.next_refresh_at)) : '未安排');
      cell(row, !e.selected ? '已停止获取' : e.probing ? '探测中' : e.last_probe || '尚未探测');
      const td = cell(row, ''); const button = document.createElement('button'); button.textContent = '探测一次';
      button.disabled = !data.probe_enabled || e.probing || !e.selected;
      button.addEventListener('click', async () => {
        button.disabled = true;
        let message = '';
        try { await api('/probe', {account: e.account, model: e.model}); message = '已开始获取，其他模型和凭据可同时获取。'; }
        catch (err) { message = err.message; }
        await refresh();
        if (message) notice(message);
      });
      td.append(button); $('accounts').append(row);
    }
    $('history').replaceChildren();
    for (const e of history.slice().reverse()) {
      const row = document.createElement('tr');
      cell(row, time(e.at)); credentialCell(row, e, profiles.get(e.account));
      const requestModel = cell(row, e.requested_model || e.model || '未提供');
      if (e.model && e.model !== e.requested_model) requestModel.title = '执行模型：' + e.model;
      const actualModel = cell(row, e.action === 'blocked' ? '未发送上游' : e.response_model || '未提供');
      actualModel.title = e.response_model ? '响应报告字段：' + (e.response_model_source || 'model') : '未读取到可确认的上游模型字段';
      if (e.response_model && e.model && e.response_model !== e.model) actualModel.className = 'model-different';
      cell(row, e.reasoning || '—'); cell(row, e.length || '未返回');
      const recovered=({active:'已入库',upgraded_292:'已升级为 292',standby:'已收为备用',duplicate:'重复值',older:'较旧值',lower_priority:'保留优先值'})[e.cache_action];
      const acceptance=({model_mismatch:'模型不一致，未入库',model_evidence_missing:'缺少模型证据',state_rejected:'状态未通过验收',accepted:'验收通过',ok:'验收通过'})[e.acceptance];
      cell(row,recovered ? sourceName(e.state_source)+' · '+recovered : acceptance || '—');
      cell(row, ({observe: '观察', probe: '探测', replaced: '已替换', would_replace: '模拟替换', blocked: '已拦截'})[e.action] || e.action);
      cell(row, e.action === 'blocked' ? '未发送上游' : e.success ? '完整成功' : '未成功', 'badge' + (e.success ? '' : ' failed')); $('history').append(row);
    }
    notice('已连接 · 代理策略：' + '使用各凭据设置中的代理'
      + (data.continuous ? ' · 各组合并行获取，重试间隔 ' + data.retry_seconds + ' 秒' : '') + ' · 更新时间 ' + new Date().toLocaleTimeString());
    return true;
  } catch (err) { notice(err.message); }
  finally { busy = false; }
}
$('login').addEventListener('submit', event => {
  event.preventDefault(); if (busy) return;
  key = $('key').value.trim(); keySource = 'manual'; $('key').value = '';
  selectionDirty = false; selectionRevision = 0; refresh();
});
$('changeLogin').addEventListener('click', () => {
  clearLogin(); notice('输入新的管理密钥，连接成功后自动记住。'); $('key').focus();
});
$('refresh').addEventListener('click', refresh);
$('reloadSelection').addEventListener('click', () => { if (saving) return; selectionDirty = false; refresh(); });
$('selectionForm').addEventListener('submit', async event => {
  event.preventDefault(); if (!key || saving || !selectionDirty) return;
  const body = {revision: selectionRevision, models: selectedValues('modelChoices'), accounts: selectedValues('credentialChoices')};
  saving = true; $('saveSelection').disabled = true;
  const inputs = Array.from($('selectionForm').querySelectorAll('input'));
  const disabled = inputs.map(input => input.disabled); inputs.forEach(input => { input.disabled = true; });
  try {
    const result = await api('/selection', body);
    selectionDirty = false; saving = false; renderSelection(result.selection);
    notice('勾选已保存并生效。'); await refresh();
  } catch (err) {
    inputs.forEach((input, index) => { input.disabled = disabled[index]; });
    $('selectionNote').textContent = err.message; $('saveSelection').disabled = false;
  } finally { saving = false; }
});
$('cancel').addEventListener('click', async () => {
  try { await api('/cancel', {}); notice('已取消当前探测；勾选范围内的自动获取会按配置继续。'); }
  catch (err) { notice(err.message); }
});
$('settings').addEventListener('submit', async event => {
  event.preventDefault();
  try { await api('/mode', {mode: $('mode').value, dry_run: $('dry').checked}); await refresh(); }
  catch (err) { notice(err.message); }
});
setInterval(() => { if (!document.hidden) refresh(); }, 5000);
window.addEventListener('storage', event => {
  if (keySource === 'panel' && (event.key === panelStorageKey || event.key === null) && storedAuth(panelStorageKey) !== key) {
    clearLogin(false); notice('CPA 登录已更改，请重新连接。');
  }
});
restoreLogin();
