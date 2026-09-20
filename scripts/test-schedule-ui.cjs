'use strict';
const http=require('node:http'),fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require(process.env.CPA_PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve(__dirname,'..'),prefix='/v0/resource/plugins/codex-turn-state-manager/';
let revision=0,advance=30,retry=7,failSave=false,saves=0;
const errors=[],checks=[];
const status=()=>({version:'0.3.3',mode:'force',valid_count:1,probe_enabled:true,continuous:true,retry_seconds:retry,refresh_before_seconds:advance*60,reasoning_effort:'medium',proxy_mode:'credential',schedule:{revision,advance_minutes:advance,retry_seconds:retry,default_advance_minutes:30,default_retry_seconds:7,persistent:true},selection:{required:true,persistent:true,revision:0,models:['gpt-6-astra'],model_options:['gpt-6-astra'],accounts:[{account:'demo',email:'demo@example.test',selected:true}]},entries:[{account:'demo',model:'gpt-6-astra',selected:true,state_length:332,valid:true,expires_at:'2026-09-20T12:21:42Z',next_refresh_at:new Date(Date.parse('2026-09-20T12:21:42Z')-advance*60000).toISOString()}],history:[]});
const server=http.createServer((req,res)=>{const names={status:['index.html','text/html'],'app.js':['app.js','text/javascript'],'app.css':['app.css','text/css']};const file=names[req.url.startsWith(prefix)?req.url.slice(prefix.length):''];if(!file){res.writeHead(404);res.end();return;}res.writeHead(200,{'Content-Type':file[1]});res.end(fs.readFileSync(path.join(root,'web',file[0])));});
(async()=>{
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const browser=await chromium.launch({executablePath:process.env.CPA_CHROMIUM_EXECUTABLE||undefined,headless:true,args:['--no-sandbox']});
 try{
  const page=await browser.newPage({viewport:{width:1440,height:1050},timezoneId:'Asia/Shanghai',locale:'zh-CN'});page.on('pageerror',e=>errors.push(e.message));
  await page.route('**/v0/management/codex-turn-state-manager/**',async route=>{
   const req=route.request();if(req.headers().authorization!=='Bearer schedule-test'){await route.fulfill({status:401,json:{error:'unauthorized'}});return;}
   if(req.url().endsWith('/status')){await route.fulfill({json:status()});return;}
   if(req.url().endsWith('/schedule')){
    const body=req.postDataJSON();
    if(body.revision!==revision){await route.fulfill({status:409,json:{error:'schedule_changed_reload'}});return;}
    if(failSave){await route.fulfill({status:500,json:{error:'schedule_save_failed'}});return;}
    advance=body.advance_minutes;retry=body.retry_seconds;revision++;saves++;
    await route.fulfill({json:{ok:true,schedule:status().schedule}});return;
   }
   await route.fulfill({status:404,json:{error:'not_found'}});
  });
  await page.goto('http://127.0.0.1:'+server.address().port+prefix+'status');await page.locator('#key').fill('schedule-test');await page.getByRole('button',{name:'连接',exact:true}).click();
  await page.locator('#scheduleNote').filter({hasText:'当前：'}).waitFor();
  assert.equal(await page.locator('#advanceMinutes').inputValue(),'30');assert.equal(await page.locator('#retrySeconds').inputValue(),'7');assert.match(await page.locator('#accounts').textContent(),/19:51:42/);checks.push('defaults_and_expected_refresh');
  await page.locator('#advanceMinutes').fill('10');await page.locator('#retrySeconds').fill('11');await page.getByRole('button',{name:'刷新',exact:true}).click();
  assert.equal(await page.locator('#advanceMinutes').inputValue(),'10');assert.equal(await page.locator('#retrySeconds').inputValue(),'11');checks.push('poll_preserves_unsaved_inputs');
  await page.locator('#saveSchedule').click();await page.locator('#scheduleNote').filter({hasText:'到期前 10 分钟'}).waitFor();assert.equal(advance,10);assert.equal(retry,11);await page.locator('#accounts').filter({hasText:'20:11:42'}).waitFor();checks.push('save_updates_schedule');
  await page.reload();await page.locator('#scheduleNote').filter({hasText:'到期前 10 分钟'}).waitFor();assert.equal(await page.locator('#retrySeconds').inputValue(),'11');checks.push('reload_restores_values');
  await page.locator('#advanceMinutes').fill('20');revision++;await page.locator('#saveSchedule').click();await page.locator('#scheduleNote').filter({hasText:'其他窗口'}).waitFor();assert.equal(advance,10);assert.equal(await page.locator('#advanceMinutes').inputValue(),'20');await page.locator('#reloadSchedule').click();await page.waitForFunction(()=>document.querySelector('#advanceMinutes').value==='10');checks.push('stale_edit_rejected_and_reload');
  await page.locator('#retrySeconds').fill('0');const previous=saves;await page.locator('#saveSchedule').click();assert.equal(saves,previous);assert.equal(await page.locator('#retrySeconds').evaluate(el=>el.validity.valid),false);checks.push('input_range_validation');
  await page.locator('#retrySeconds').fill('9');failSave=true;await page.locator('#saveSchedule').click();await page.locator('#scheduleNote').filter({hasText:'保存失败'}).waitFor();assert.equal(retry,11);failSave=false;checks.push('failed_save_retains_active_values');
  await page.locator('#resetSchedule').click();await page.locator('#scheduleNote').filter({hasText:'到期前 30 分钟'}).waitFor();assert.equal(advance,30);assert.equal(retry,7);assert.equal(await page.locator('#retrySeconds').inputValue(),'7');checks.push('restore_defaults_persists');
  await page.setViewportSize({width:390,height:844});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));await page.locator('.schedule-panel').screenshot({path:path.join(root,'dist','schedule-ui-0.3.3.png')});assert.deepEqual(errors,[]);checks.push('mobile_layout_and_no_errors');
  console.log(JSON.stringify({passed:true,saves,checks}));
 }finally{await browser.close();await new Promise(resolve=>server.close(resolve));}
})().catch(e=>{console.error(e);server.close();process.exitCode=1});
