'use strict';
const http = require('node:http');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {chromium} = require(process.env.CPA_PLAYWRIGHT_MODULE || 'playwright');
const root = path.resolve(__dirname, '..');
const prefix = '/v0/resource/plugins/codex-turn-state-manager/';
const ownKey = 'cpa-turn-state-manager.auth.v1';
const panelKey = 'cli-proxy-auth';
const validKey = 'local-ui-login-test';
const fixture = {version: '0.3.4', mode: 'force', dry_run: false, blocking_active: true, valid_count: 0,
  entries: [], history: [], proxy_mode: 'credential', reasoning_effort: 'medium', continuous: true, retry_seconds: 7,
  selection: {required: true, persistent: true, revision: 1, models: [], model_options: [], accounts: []}};
let serviceStatus = 200, requestCount = 0;
const errors = [], checks = [];
const server = http.createServer((req, res) => {
  if (req.url === '/parent') {
    res.writeHead(200, {'Content-Type': 'text/html'}); res.end(`<iframe src="${prefix}status"></iframe>`); return;
  }
  if (req.url === '/v0/management/codex-turn-state-manager/status') {
    requestCount++;
    const code = req.headers.authorization === 'Bearer ' + validKey ? serviceStatus : 401;
    res.writeHead(code, {'Content-Type': 'application/json'}); res.end(JSON.stringify(code === 200 ? fixture : {error: 'test_error'})); return;
  }
  const files = {status: ['index.html', 'text/html'], 'app.js': ['app.js', 'text/javascript'], 'app.css': ['app.css', 'text/css']};
  const file = files[req.url.startsWith(prefix) ? req.url.slice(prefix.length) : ''];
  if (!file) {res.writeHead(404); res.end(); return;}
  res.writeHead(200, {'Content-Type': file[1], 'Cache-Control': 'no-store'}); res.end(fs.readFileSync(path.join(root, 'web', file[0])));
});
(async () => {
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const origin = `http://127.0.0.1:${server.address().port}`, url = origin + prefix + 'status';
  const browser = await chromium.launch({executablePath: process.env.CPA_CHROMIUM_EXECUTABLE || undefined, headless: true, args: ['--no-sandbox']});
  const context = await browser.newContext();
  const page = await context.newPage(); page.on('pageerror', e => errors.push(e.message));
  const connected = async target => {
    await target.waitForFunction(() => document.querySelector('#notice').textContent.startsWith('已连接'));
    assert.equal(await target.locator('#login').isVisible(), false);
  };
  const seed = async (name, state, encoded = true) => page.evaluate(({name, state, encoded}) => {
    let value = JSON.stringify(state);
    if (encoded) {
      const mask = new TextEncoder().encode('cli-proxy-api-webui::secure-storage|' + location.host + '|' + navigator.userAgent);
      const bytes = new TextEncoder().encode(value).map((b, i) => b ^ mask[i % mask.length]);
      value = 'enc::v1::' + btoa(String.fromCharCode(...bytes));
    }
    localStorage.setItem(name, value);
  }, {name, state, encoded});
  try {
    await page.goto(url); await page.locator('#login').waitFor({state: 'visible'});
    assert.equal(requestCount, 0); checks.push('fresh_browser_requires_initial_login');
    await page.locator('#key').fill('invalid-local-test'); await page.getByRole('button', {name: '连接', exact: true}).click();
    await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('失效'));
    assert.equal(await page.evaluate(name => localStorage.getItem(name), ownKey), null); checks.push('invalid_key_not_saved');
    await page.locator('#key').fill(validKey); await page.getByRole('button', {name: '连接', exact: true}).click(); await connected(page);
    assert.equal(await page.locator('#key').inputValue(), '');
    const saved = await page.evaluate(name => localStorage.getItem(name), ownKey);
    assert.ok(saved.startsWith('enc::v1::')); assert.equal(saved.includes(validKey), false);
    assert.equal(page.url().includes(validKey), false); checks.push('verified_login_saved_and_form_hidden');
    await page.reload(); await connected(page); checks.push('reload_auto_connects');
    const reopened = await context.newPage(); await reopened.goto(url); await connected(reopened); await reopened.close(); checks.push('new_tab_auto_connects');
    const restored = await browser.newContext({storageState: await context.storageState()});
    const restoredPage = await restored.newPage(); await restoredPage.goto(url); await connected(restoredPage); await restored.close(); checks.push('browser_storage_restore_auto_connects');
    serviceStatus = 503; await page.reload(); await page.waitForFunction(() => document.querySelector('#notice').textContent.includes('请求失败'));
    assert.equal(await page.evaluate(name => localStorage.getItem(name), ownKey), saved);
    serviceStatus = 200; await page.getByRole('button', {name: '刷新', exact: true}).click(); await connected(page); checks.push('network_failure_preserves_login');
    serviceStatus = 401; await page.reload(); await page.locator('#login').waitFor({state: 'visible'});
    assert.equal(await page.evaluate(name => localStorage.getItem(name), ownKey), null); serviceStatus = 200; checks.push('revoked_login_cleared');
    await seed(panelKey, {state: {apiBase: origin, managementKey: validKey, rememberPassword: true}, version: 0});
    await page.reload(); await connected(page);
    assert.equal(await page.evaluate(name => localStorage.getItem(name), ownKey), null); checks.push('cpa_obfuscated_login_inherited_without_copy');
    const parent = await context.newPage(); await parent.goto(origin + '/parent');
    await parent.frameLocator('iframe').locator('#guardStatus').filter({hasText: '503'}).waitFor();
    assert.equal(await parent.frameLocator('iframe').locator('#login').isVisible(), false); checks.push('cpa_iframe_has_no_second_login');
    await page.evaluate(name => localStorage.removeItem(name), panelKey);
    await parent.frameLocator('iframe').locator('#login').waitFor({state: 'visible'}); await parent.close(); checks.push('inherited_cpa_logout_clears_plugin_session');
    await seed(panelKey, {state: {apiBase: 'https://different-server.invalid', managementKey: validKey}}, false);
    const count = requestCount; await page.reload(); await page.locator('#login').waitFor({state: 'visible'});
    assert.equal(requestCount, count); checks.push('different_origin_key_ignored');
    await page.evaluate(name => localStorage.setItem(name, 'enc::v1::broken'), panelKey);
    await page.reload(); await page.locator('#login').waitFor({state: 'visible'}); checks.push('malformed_storage_falls_back');
    await seed(panelKey, {state: {apiBase: origin, managementKey: validKey}}, false);
    await page.reload(); await connected(page); checks.push('cpa_plain_storage_compatible');
    await page.getByRole('button', {name: '更换密钥', exact: true}).click(); await page.locator('#login').waitFor({state: 'visible'}); checks.push('change_login_available');
    assert.deepEqual(errors, []);
    console.log(JSON.stringify({passed: true, checks, page_errors: 0}));
  } finally {await browser.close(); await new Promise(resolve => server.close(resolve));}
})().catch(error => {console.error(error); server.close(); process.exitCode = 1;});
