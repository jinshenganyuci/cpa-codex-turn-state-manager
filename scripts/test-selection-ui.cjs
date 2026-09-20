'use strict';
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {chromium} = require(process.env.CPA_PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '..');
const prefix = '/v0/resource/plugins/codex-turn-state-manager/';
const credential = 'credential-a';
let revision = 0, models = [], accounts = [], saves = 0, probeCalls = 0;
const modelOptions = ['gpt-6-astra', 'gpt-5.6-sol', 'gpt-5.6-luna'];
function status() {
  return {version: '0.3.2', probe_parallel: true, prefer_292: true, require_model_match: true, standby_enabled: true, history_persistent: true,
 block_without_state: false, blocking_active: false, mode: 'force', probe_enabled: true, continuous: true, retry_seconds: 15,
    reasoning_effort: 'medium', proxy_mode: 'credential', dry_run: false, valid_count: 0,
    selection: {required: true, persistent: true, revision, models, model_options: modelOptions,
      accounts: [{account: credential, label: '演示凭据 A', email: 'demo@example.test', plan_type: 'plus', disabled: false, selected: accounts.includes(credential)}]},
    entries: modelOptions.map(model => ({account: credential, model, selected: accounts.includes(credential) && models.includes(model),
      state_length: 292, source:'probe', standby:{length:292,source:'business',valid:true,expires_at:'2026-09-20T00:00:00Z'}, last_observed_length: 0, valid: true, pending: '', probing: false, last_probe: '', next_refresh_at: null})), history: [{at: '2026-09-19T00:00:00Z', account: credential, model: 'gpt-6-astra', action: 'blocked', success: false, length: 0}, {at: '2026-09-19T00:00:01Z', account: credential, requested_model: 'gpt-6-astra', model: 'gpt-6-astra', response_model: 'gpt-5.6-luna', response_model_source: 'response.model', acceptance:'model_mismatch', action: 'replaced', success: true, length: 332}, {at:'2026-09-19T00:00:02Z',account:credential,model:'gpt-6-astra',response_model:'gpt-6-astra',action:'replaced',success:true,length:292,acceptance:'accepted',cache_action:'standby',state_source:'business'},
    {at:'2026-09-19T00:00:03Z',account:credential,model:'gpt-6-astra',action:'probe',success:true,length:292,acceptance:'ok',proxy_id:'deleted-proxy',proxy_label:'Novproxy 以色列轮换',exit_ip:'203.0.113.24'},
    {at:'2026-09-19T00:00:04Z',account:credential,model:'gpt-6-astra',action:'probe',success:false,length:0,proxy_label:'旧记录'}]};
}
const server = http.createServer((request, response) => {
  const name = request.url.startsWith(prefix) ? request.url.slice(prefix.length) : '';
  const files = {status: ['index.html', 'text/html'], 'app.js': ['app.js', 'text/javascript'], 'app.css': ['app.css', 'text/css']};
  if (!files[name]) { response.writeHead(404); response.end(); return; }
  response.writeHead(200, {'Content-Type': files[name][1]}); response.end(fs.readFileSync(path.join(root, 'web', files[name][0])));
});
(async () => {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const browser = await chromium.launch({executablePath: process.env.CPA_CHROMIUM_EXECUTABLE || undefined, headless: true, args: ['--no-sandbox']});
  try {
    const page = await browser.newPage({viewport: {width: 1440, height: 1100}});
    const errors = []; page.on('pageerror', error => errors.push(error.message));
    await page.route('**/v0/management/codex-turn-state-manager/**', async route => {
      const request = route.request();
      if (request.headers().authorization !== 'Bearer local-ui-test') { await route.fulfill({status: 401, json: {error: 'unauthorized'}}); return; }
      if(request.url().endsWith('/probe')) { probeCalls++; await route.fulfill({status:202,json:{started:true}}); return; }
      if (request.url().endsWith('/selection')) {
        const body = request.postDataJSON();
        if (body.revision !== revision) { await route.fulfill({status: 409, json: {error: 'selection_changed_reload_before_saving'}}); return; }
        models = body.models; accounts = body.accounts; revision++; saves++;
        await route.fulfill({json: {ok: true, selection: status().selection}}); return;
      }
      await route.fulfill({json: status()});
    });
    const login = async () => {
      await page.locator('#key').fill('local-ui-test'); await page.getByRole('button', {name: '连接', exact: true}).click();
      await page.getByRole('checkbox', {name: 'gpt-6-astra', exact: true}).waitFor();
    };
    const save = async () => {
      await page.getByRole('button', {name: '保存勾选', exact: true}).click();
      await page.waitForFunction(() => document.getElementById('selectionNote').textContent.startsWith('勾选已保存'));
    };
    await page.goto(`http://127.0.0.1:${server.address().port}${prefix}status`); await login();
    assert.equal(await page.getByRole('checkbox').count(), 5);
    assert.match(await page.locator('#guardStatus').textContent(), /正常放行/);
    assert.equal(await page.locator('#accounts button').count(), 0);
    assert.equal(await page.locator('#history tr').count(), 0);
    await page.getByRole('checkbox', {name: 'gpt-6-astra', exact: true}).check();
    await page.getByRole('checkbox', {name: 'gpt-5.6-sol', exact: true}).check();
    await page.getByRole('checkbox', {name: 'demo@example.test', exact: true}).check();
    await page.getByRole('button', {name: '刷新', exact: true}).click();
    assert.equal(await page.getByRole('checkbox', {name: 'gpt-5.6-sol', exact: true}).isChecked(), true);
    await save(); assert.deepEqual(models, ['gpt-6-astra', 'gpt-5.6-sol']); assert.deepEqual(accounts, [credential]);
    await page.getByRole('button', {name: '刷新', exact: true}).click();
    assert.match(await page.locator('#history').textContent(), /已拦截.*未发送上游/);
    assert.match(await page.locator('#credentialChoices').textContent(), /demo@example.test.*Plus/);
    assert.match(await page.locator('#history').textContent(), /demo@example.test.*Plus.*gpt-6-astra.*gpt-5.6-luna/);
    assert.equal(await page.locator('#history .model-different').count(), 1);
    assert.equal(await page.getByRole('columnheader', {name: '实际响应模型', exact: true}).count(), 1);
    assert.match(await page.locator('#priorityStatus').textContent(), /292 首选.*有效 292 \/ 332 均暂停探测.*模型一致才入库/);
    assert.match(await page.locator('#accounts').textContent(), /292 · 业务观测/);
    assert.match(await page.locator('#history').textContent(), /业务观测 · 已收为备用/);
    assert.match(await page.locator('#history').textContent(), /模型不一致，未入库/);
    assert.equal(await page.locator('#target332').textContent(), '0');
    const exitCell = page.locator('#history .request-exit').filter({hasText:'Novproxy 以色列轮换'});
    assert.equal(await exitCell.locator('.exit-ip').textContent(),'203.0.113.24');
    assert.equal(await page.locator('#history .request-exit').filter({hasText:'旧记录'}).locator('.exit-ip').textContent(),'IP 未获取');
    assert.equal(await exitCell.locator('img,script').count(),0);
    await page.setViewportSize({width:390,height:844});
    assert.equal(await exitCell.evaluate(el=>el.querySelector('small').getBoundingClientRect().top >= el.querySelector('div').getBoundingClientRect().bottom),true);
    await exitCell.screenshot({path:path.join(root,'dist','exit-ip-layout-0.3.2.png')});
    await page.setViewportSize({width:1440,height:1100});

    assert.equal(await page.getByRole('heading',{name:'代理池健康',exact:true}).count(),0);
    assert.match(await page.locator('#notice').textContent(),/使用各凭据设置中的代理.*各组合并行获取/);

    await page.getByRole('button',{name:'刷新',exact:true}).click();
    await page.locator('#accounts tr').filter({hasText:'gpt-6-astra'}).getByRole('button',{name:'探测一次',exact:true}).click();
    assert.equal(probeCalls,1);
    await page.reload();
    await page.waitForFunction(() => document.getElementById('notice').textContent.startsWith('已连接'));
    assert.equal(await page.locator('#login').isVisible(), false);
    assert.equal(await page.getByRole('checkbox', {name: 'gpt-5.6-sol', exact: true}).isChecked(), true);
    await page.getByRole('checkbox', {name: 'gpt-5.6-sol', exact: true}).uncheck(); await save();
    assert.deepEqual(models, ['gpt-6-astra']);
    await page.getByRole('checkbox', {name: 'demo@example.test', exact: true}).uncheck(); await save(); assert.deepEqual(accounts, []);
    await page.getByRole('checkbox', {name: 'gpt-5.6-sol', exact: true}).check(); revision++;
    await page.getByRole('button', {name: '保存勾选', exact: true}).click();
    await page.waitForFunction(() => document.getElementById('selectionNote').textContent.includes('其他窗口'));
    assert.deepEqual(models, ['gpt-6-astra']);
    await page.getByRole('button', {name: '重新载入选择', exact: true}).click();
    await page.waitForFunction(() => !document.querySelector('#modelChoices input[value="gpt-5.6-sol"]').checked);
    await page.setViewportSize({width: 390, height: 844});
    assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({passed: true, saves, checks: ['egress_ip_below_name', 'historical_proxy_label', 'missing_ip_placeholder', 'unchecked_rows_hidden', '292_priority', 'business_standby', 'admission_display', 'credential_proxy_only', 'parallel_acquisition_notice', 'manual_start', 'email_and_plan', 'request_and_response_models', 'default_passthrough_status', 'blocked_history', 'checkboxes', 'draft_survives_poll', 'save', 'reload', 'deselect_model', 'deselect_credential', 'stale_revision', 'mobile_layout', 'no_console_errors']}));
  } finally { await browser.close(); await new Promise(resolve => server.close(resolve)); }
})().catch(error => { console.error(error); server.close(); process.exitCode = 1; });
