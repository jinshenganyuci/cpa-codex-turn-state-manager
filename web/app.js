'use strict';
const base = '/v0/management/codex-turn-state-manager';
const $ = id => document.getElementById(id);
let poolData = {enabled:false, revision:0, entries:[]}, poolSaving = false, editingProxy = '', proxyEditRevision = 0;
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
  $('accounts').replaceChildren(); $('history').replaceChildren(); $('proxyRows').replaceChildren();
  $('proxyForm').reset(); $('proxyForm').hidden = true; $('poolSummary').textContent = '等待连接'; $('togglePool').disabled = true; $('addProxy').disabled = true;
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
  proxy_pool_changed_reload: '其他窗口已修改代理池，请关闭编辑后重新载入。',
  proxy_pool_save_failed: '代理池保存失败，配置没有变更。',
  invalid_proxy_url: '代理地址无效，请填写完整的 SOCKS5、HTTP 或 HTTPS 地址。',
  invalid_proxy_label: '代理名称过长或包含换行。',
  proxy_already_exists: '这个代理地址已经存在。',
  proxy_not_found: '代理已删除，请刷新列表。',
  proxy_test_running: '此代理正在测试。',
  proxy_test_busy: '同时最多测试 4 个代理，请稍后再试。',
  proxy_pool_unavailable: '代理池暂无可用出口，等待启用或冷却结束。',
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
    $('priorityStatus').textContent = (data.prefer_292 ? '292 首选 · 有效 292 / 332 均暂停探测' : '使用已验收的状态值')
      + (data.require_model_match ? ' · 模型一致才入库' : '') + (data.standby_enabled ? ' · 提前准备备用值' : '');
    $('historyPersistence').textContent = data.persistence_error ? '保存失败：'+data.persistence_error : data.history_persistent ? '历史已启用持久化 · 每 5 秒刷新' : '每 5 秒刷新 · 最多 200 条';
    const selectedAccounts = new Set((data.selection?.accounts || []).filter(a => a.selected).map(a => a.account));
    const history = (data.history || []).filter(e => !data.selection?.required || selectedAccounts.has(e.account) && (data.selection.models || []).includes(e.model));
    $('target292').textContent = history.filter(x => x.length === 292 && x.success && (!x.acceptance || ['accepted','ok'].includes(x.acceptance))).length;
    $('target332').textContent = history.filter(x => x.length === 332 && x.success && (!x.acceptance || ['accepted','ok'].includes(x.acceptance))).length;
    $('effort').textContent = data.reasoning_effort;
    if (document.activeElement !== $('mode')) $('mode').value = data.mode;
    if (document.activeElement !== $('dry')) $('dry').checked = data.dry_run;
    renderSelection(data.selection);
    renderPool(data.proxy_pool || {enabled:false,revision:0,entries:[]});
    const profiles = new Map((data.selection?.accounts || []).map(account => [account.account, account]));
    $('accounts').replaceChildren();
    for (const e of data.entries.filter(entry => entry.selected)) {
      const row = document.createElement('tr');
      credentialCell(row, e, profiles.get(e.account)); cell(row, e.model); cell(row, e.selected ? '已勾选' : '未勾选');
      stateCell(row,e.state_length,e.source,e.valid,e.expires_at);
      stateCell(row,e.standby?.length,e.standby?.source,e.standby?.valid,e.standby?.expires_at);
      cell(row, !e.selected ? '未启用' : e.valid ? time(e.expires_at) : '暂无有效缓存');
      cell(row, e.next_refresh_at ? (e.probing ? '刷新中' : time(e.next_refresh_at)) : '未安排');
      cell(row, !e.selected ? '已停止获取' : e.probing ? '探测中' : selectionErrors[e.last_probe] || e.last_probe || '尚未探测');
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
    if (!$('accounts').children.length) { const row = document.createElement('tr'); cell(row, '暂无已保存勾选的凭据与模型，请在上方选择并保存。').colSpan = 9; $('accounts').append(row); }
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
      const exit = cell(row, ''); exit.className = 'request-exit';
      const exitName = document.createElement('div');
      exitName.textContent = e.action === 'probe' ? (e.proxy_label || poolData.entries.find(p => p.id === e.proxy_id)?.label || (e.proxy_id ? '已移除代理' : '凭据代理')) : 'CPA 凭据代理';
      exit.append(exitName);
      if (e.action === 'probe') {
        const ip = document.createElement('small'); ip.className = 'credential-detail exit-ip';
        ip.textContent = e.exit_ip || 'IP 未获取';
        ip.title = e.exit_ip ? '该次探测同一连接观测到的出口 IP' : '历史记录未采集，或未能通过同一连接确认出口 IP';
        exit.append(ip);
      }
      cell(row, ({observe: '观察', probe: '探测', replaced: '已替换', would_replace: '模拟替换', blocked: '已拦截'})[e.action] || e.action);
      cell(row, e.action === 'blocked' ? '未发送上游' : e.success ? '完整成功' : '未成功', 'badge' + (e.success ? '' : ' failed')); $('history').append(row);
    }
    notice('已连接 · 代理策略：' + (data.proxy_mode === 'pool' ? '插件获取走代理池；业务使用各凭据设置中的代理' : '使用各凭据设置中的代理')
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

function proxyResult(e) {
  if (e.testing) return '测试中…';
  const test = e.test || {};
  if (!test.at || test.at.startsWith('0001-')) return '未测试';
  const label = {reachable:'可达',upstream_forbidden:'上游拒绝访问',proxy_auth_failed:'代理认证失败',upstream_unavailable:'上游暂不可用',test_timeout_or_cancelled:'超时或已取消',connection_failed:'连接失败'}[test.result] || test.result;
  return label + (test.http_status ? ' · HTTP ' + test.http_status : '') + ' · ' + test.latency_ms + ' ms';
}
function showProxyEditor(e) {
  editingProxy = e?.id || ''; proxyEditRevision = poolData.revision;
  $('proxyForm').reset(); $('proxyFormTitle').textContent = e ? '编辑代理' : '添加代理';
  $('proxyLabel').value = e?.label || ''; $('proxyEnabled').checked = e?.enabled ?? true;
  $('proxyRotating').checked = e?.rotating ?? false; $('proxyURL').required = !e;
  $('proxyURL').placeholder = e ? '留空保留现有地址和认证' : 'socks5://用户名:密码@主机:端口';
  $('proxyForm').hidden = false; $('proxyLabel').focus();
}
async function poolAction(action, body, message) {
  if (poolSaving) return;
  poolSaving = true;
  try {
    await api('/proxy-pool/' + action, {revision:poolData.revision,...body});
    await refresh(); $('poolNotice').textContent = message;
  } catch (err) { $('poolNotice').textContent = err.message; }
  finally {poolSaving=false;renderPool(poolData);}
}
function renderPool(data) {
  poolData=data;
  const now=Date.now(), available=data.entries.filter(e => e.enabled && (!e.cooldown_until || Date.parse(e.cooldown_until)<=now)).length;
  $('poolSummary').textContent = data.enabled ? '代理池已启用 · '+available+'/'+data.entries.length+' 可用' : '获取使用凭据代理';
  $('togglePool').textContent=data.enabled?'停用代理池':'启用代理池';
  $('togglePool').disabled=!key||poolSaving;
  $('addProxy').disabled=!key||poolSaving;
  if(data.persistence_error) $('poolNotice').textContent='代理池保存失败，请检查状态目录的写入权限。';
  $('proxyRows').replaceChildren();
  for(const e of data.entries) {
    const row=document.createElement('tr'), name=cell(row,'');name.className='proxy-address';name.textContent=e.label;
    const address=document.createElement('small');address.className='credential-detail';address.textContent=e.address+(e.authenticated?' · 已保存认证':'')+(e.rotating?' · 轮换出口':'');name.append(address);
    const cooling=Date.parse(e.cooldown_until)>now;
    cell(row,!e.enabled?'已停用':cooling?'冷却至 '+time(e.cooldown_until):'可用');
    cell(row,e.attempts||0);cell(row,(e.success_292||0)+' / '+(e.success_332||0));cell(row,e.last_result||'尚未获取');cell(row,proxyResult(e));
    const actions=cell(row,''),wrap=document.createElement('div');wrap.className='proxy-actions';actions.append(wrap);
    const button=(label,run,disabled=false,danger=false)=>{const b=document.createElement('button');b.type='button';b.textContent=label;b.disabled=poolSaving||disabled;if(danger)b.className='danger';b.addEventListener('click',run);wrap.append(b);};
    button('测试',()=>poolAction('test',{id:e.id},'正在测试 '+e.label+'；测试结果会自动更新。'),e.testing);
    button('编辑',()=>showProxyEditor(e));
    button(e.enabled?'停用':'启用',()=>poolAction('save',{id:e.id,label:e.label,enabled:!e.enabled,rotating:e.rotating},'代理状态已保存。'));
    if(cooling) button('解除冷却',()=>poolAction('reset',{id:e.id},'冷却已解除。'));
    button('删除',()=>{if(window.confirm('删除代理“'+e.label+'”？后续获取将不再使用它。'))poolAction('delete',{id:e.id},'代理已删除。');},false,true);
    $('proxyRows').append(row);
  }
  if(!data.entries.length){const row=document.createElement('tr');cell(row,data.enabled?'代理池为空，后台获取已暂停。请添加并启用代理。':'尚未添加代理。添加并启用代理池后，仅插件获取改走池中出口。').colSpan=7;$('proxyRows').append(row);}
}
$('addProxy').addEventListener('click',()=>showProxyEditor(null));
$('togglePool').addEventListener('click',()=>poolAction('mode',{enabled:!poolData.enabled},poolData.enabled?'代理池已停用，后续获取使用凭据代理。':'代理池已启用，仅影响插件获取。'));
$('cancelProxyEdit').addEventListener('click',()=>{$('proxyForm').reset();$('proxyForm').hidden=true;editingProxy='';});
$('proxyForm').addEventListener('submit',async event=>{
 event.preventDefault();if(poolSaving)return;poolSaving=true;$('saveProxy').disabled=true;
 const body={revision:proxyEditRevision,id:editingProxy,label:$('proxyLabel').value.trim(),url:$('proxyURL').value.trim(),enabled:$('proxyEnabled').checked,rotating:$('proxyRotating').checked};
 try {
  await api('/proxy-pool/save',body);$('proxyForm').reset();$('proxyForm').hidden=true;editingProxy='';
  await refresh();$('poolNotice').textContent='代理已保存。'+(poolData.enabled?'后续获取会按池中可用出口轮询。':'可以先测试连通性，再点击“启用代理池”。');
 }catch(err){$('poolNotice').textContent=err.message;}
 finally{poolSaving=false;$('saveProxy').disabled=false;renderPool(poolData);}
});
