'use strict';
const http=require('node:http'),fs=require('node:fs'),path=require('node:path'),assert=require('node:assert/strict');
const {chromium}=require(process.env.CPA_PLAYWRIGHT_MODULE||'playwright');
const root=path.resolve(__dirname,'..'),prefix='/v0/resource/plugins/codex-turn-state-manager/';
let revision=0,enabled=false,entries=[],serial=0,testRequests=0;
const urls=new Map(),checks=[];
const status=()=>({version:'0.3.4',mode:'force',valid_count:0,prefer_292:true,probe_enabled:true,continuous:true,retry_seconds:7,proxy_mode:enabled?'pool':'credential',reasoning_effort:'medium',selection:{required:true,persistent:true,revision:1,models:[],model_options:['gpt-6-astra'],accounts:[]},entries:[],history:[],proxy_pool:{enabled,revision,entries,persistent:true}});
const server=http.createServer((req,res)=>{const files={status:['index.html','text/html'],'app.js':['app.js','text/javascript'],'app.css':['app.css','text/css']};const file=files[req.url.startsWith(prefix)?req.url.slice(prefix.length):''];if(!file){res.writeHead(404);res.end();return;}res.writeHead(200,{'Content-Type':file[1]});res.end(fs.readFileSync(path.join(root,'web',file[0])));});
(async()=>{
 await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
 const browser=await chromium.launch({executablePath:process.env.CPA_CHROMIUM_EXECUTABLE||undefined,headless:true,args:['--no-sandbox']});
 try{
  const page=await browser.newPage({viewport:{width:1536,height:1100}}),errors=[];page.on('pageerror',e=>errors.push(e.message));
  await page.route('**/v0/management/codex-turn-state-manager/**',async route=>{
   const req=route.request();if(req.headers().authorization!=='Bearer pool-ui-test'){await route.fulfill({status:401,json:{error:'unauthorized'}});return;}
   const action=req.url().split('/').pop();
   if(action==='status'){await route.fulfill({json:status()});return;}
   const b=req.postDataJSON();
   if(action==='test'){testRequests++;entries.find(e=>e.id===b.id).test={at:new Date().toISOString(),http_status:401,connected:true,latency_ms:27,result:'reachable'};await route.fulfill({status:202,json:{started:true}});return;}
   if(b.revision!==revision){await route.fulfill({status:409,json:{error:'proxy_pool_changed_reload'}});return;}
   if(action==='mode')enabled=b.enabled;
   if(action==='save'){
    let e=entries.find(e=>e.id===b.id);
    if(!e){e={id:String(++serial),attempts:0,success_292:0,success_332:0};entries.push(e);}
    if(b.url)urls.set(e.id,b.url);
    const u=new URL(urls.get(e.id));e.authenticated=!!u.username;u.username='';u.password='';
    Object.assign(e,{address:u.toString(),label:b.label,enabled:b.enabled,rotating:b.rotating});
   }
   if(action==='delete')entries=entries.filter(e=>e.id!==b.id);
   revision++;await route.fulfill({json:{ok:true,proxy_pool:status().proxy_pool}});
  });
  await page.goto('http://127.0.0.1:'+server.address().port+prefix+'status');await page.locator('#key').fill('pool-ui-test');await page.getByRole('button',{name:'连接',exact:true}).click();
  await page.locator('#poolSummary').filter({hasText:'获取使用凭据代理'}).waitFor();checks.push('upgrade_default_keeps_credential_proxy');
  await page.getByRole('button',{name:'添加代理',exact:true}).click();await page.locator('#proxyLabel').fill('出口 A');await page.locator('#proxyURL').fill('socks5://demo-user:demo-pass@proxy.invalid:1080');
  await page.getByRole('button',{name:'刷新',exact:true}).click();assert.equal(await page.locator('#proxyURL').inputValue(),'socks5://demo-user:demo-pass@proxy.invalid:1080');checks.push('poll_preserves_draft');
  await page.getByRole('button',{name:'保存代理',exact:true}).click();await page.locator('#proxyRows tr').filter({hasText:'出口 A'}).waitFor();
  assert.equal(await page.locator('#proxyForm').isVisible(),false);assert.equal((await page.locator('body').innerText()).includes('demo-pass'),false);assert.equal(await page.locator('#proxyURL').inputValue(),'');checks.push('add_and_secret_hidden');
  await page.getByRole('button',{name:'启用代理池',exact:true}).click();await page.locator('#poolSummary').filter({hasText:'代理池已启用'}).waitFor();assert.ok(enabled);checks.push('pool_switch_persists');
  await page.getByRole('button',{name:'测试',exact:true}).click();await page.locator('#proxyRows').filter({hasText:'HTTP 401'}).waitFor();assert.equal(testRequests,1);checks.push('test_result_visible');
  await page.getByRole('button',{name:'编辑',exact:true}).click();assert.equal(await page.locator('#proxyURL').inputValue(),'');await page.locator('#proxyLabel').fill('<img src=x onerror=alert(1)>');await page.getByRole('button',{name:'保存代理',exact:true}).click();await page.locator('#proxyRows').filter({hasText:'<img src=x onerror=alert(1)>'}).waitFor();assert.equal(await page.locator('#proxyRows img').count(),0);assert.equal(urls.get('1'),'socks5://demo-user:demo-pass@proxy.invalid:1080');checks.push('edit_keeps_secret_and_escapes_label');
  await page.getByRole('button',{name:'停用',exact:true}).click();await page.locator('#poolSummary').filter({hasText:'0/1 可用'}).waitFor();checks.push('disable_proxy');
  await page.reload();await page.locator('#poolSummary').filter({hasText:'0/1 可用'}).waitFor();assert.equal(await page.locator('#login').isVisible(),false);checks.push('reload_restores_pool_and_login');
  await page.getByRole('button',{name:'编辑',exact:true}).click();await page.locator('#proxyLabel').fill('stale change');revision++;await page.getByRole('button',{name:'保存代理',exact:true}).click();await page.locator('#poolNotice').filter({hasText:'其他窗口'}).waitFor();assert.equal(entries[0].label,'<img src=x onerror=alert(1)>');await page.getByRole('button',{name:'取消',exact:true}).click();checks.push('stale_edit_rejected');
  await page.getByRole('button',{name:'刷新',exact:true}).click();page.once('dialog',d=>d.accept());await page.getByRole('button',{name:'删除',exact:true}).click();await page.locator('#proxyRows').filter({hasText:'代理池为空'}).waitFor();assert.equal(entries.length,0);assert.ok(enabled);checks.push('delete_does_not_enable_fallback');
  await page.setViewportSize({width:390,height:844});assert.ok(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));assert.deepEqual(errors,[]);checks.push('mobile_and_no_page_errors');
  console.log(JSON.stringify({passed:true,checks}));
 }finally{await browser.close();await new Promise(resolve=>server.close(resolve));}
})().catch(e=>{console.error(e);server.close();process.exitCode=1;});
