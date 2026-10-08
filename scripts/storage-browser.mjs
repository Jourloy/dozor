// Browser checks use a loopback fixture and never connect to a recorder.
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {readFile, mkdtemp} from 'node:fs/promises';
import {tmpdir} from 'node:os';
import {join, extname, resolve} from 'node:path';
import {fileURLToPath} from 'node:url';
import {createRequire} from 'node:module';
const require = createRequire(new URL('../../monorepo-frontend/apps/dozor/package.json', import.meta.url));
const {chromium, expect} = require('@playwright/test');
const root = fileURLToPath(new URL('../internal/dozor/web/', import.meta.url));
const output = await mkdtemp(join(tmpdir(), 'dozor-local-storage-'));
const state = {policy: {max_disk_usage_percent: 80}, loadFailure: 0, saveFailure: 0, saves: 0, delay: 0};
const server = createServer(async (request, response) => {
  try {
    const path = new URL(request.url, 'http://fixture.local').pathname;
    const json = (value, status = 200) => {response.writeHead(status, {'Content-Type': 'application/json'}); response.end(JSON.stringify(value));};
    if (path === '/api/v1/auth') return json({authenticated: true, setup_required: false, csrf: 'fixture-csrf'});
    if (path === '/api/v1/settings') return json({timezone: 'Europe/Moscow', cameras: [], disk_uuid: 'fixture-disk', s3: {enabled: false, bytes_per_second: 2048, prefix: 'dozor'}, auto_update: false, release_url: ''});
    if (path === '/api/v1/status') return json({version: 'test', disk_ready: true, disk_used: 65, disk_total: 100, cameras: [], queue: 0, notices: []});
    if (path === '/api/v1/availability') return json({start: Date.now()-60000, end: Date.now(), cameras: []});
    if (path === '/api/v1/storage-policy') {
      if (request.method === 'GET') {
        const value = {...state.policy};
        if (state.delay) await new Promise(resolve => setTimeout(resolve,state.delay));
        return state.loadFailure ? json({error:'fixture secret'},state.loadFailure) : json(value);
      }
      if (request.headers['x-csrf-token'] !== 'fixture-csrf') return json({error:'CSRF'},403);
      state.saves++;
      if (state.saveFailure) return json({error:'fixture secret'},state.saveFailure);
      let raw=''; for await (const chunk of request) raw+=chunk;
      const policy=JSON.parse(raw);
      assert.deepEqual(Object.keys(policy),['max_disk_usage_percent']);
      if (!Number.isInteger(policy.max_disk_usage_percent) || policy.max_disk_usage_percent<1 || policy.max_disk_usage_percent>99) return json({error:'invalid'},400);
      state.policy=policy;
      return json(policy);
    }
    const file=resolve(root,'.'+(path==='/'?'/index.html':path));
    if (!file.startsWith(root)) {response.writeHead(404);return response.end();}
    const bytes=await readFile(file);
    const type={'.js':'text/javascript','.css':'text/css','.html':'text/html','.svg':'image/svg+xml','.webp':'image/webp','.woff2':'font/woff2'}[extname(file)] || 'application/octet-stream';
    response.writeHead(200,{'Content-Type':type});response.end(bytes);
  } catch { response.writeHead(404);response.end(); }
});
await new Promise(resolve=>server.listen(0,'127.0.0.1',resolve));
const origin=`http://127.0.0.1:${server.address().port}`;
let browser;
let page;
try {
  browser=await chromium.launch({headless:true,args:['--disable-gpu']});
  page=await browser.newPage({viewport:{width:1280,height:900},locale:'ru-RU'});
  const errors=[];page.on('pageerror',error=>errors.push(error.message));
  await page.goto(origin+'/#settings');
  const section=page.getByRole('region',{name:'Хранение записей на диске',exact:true});
  const input=section.getByLabel('Лимит заполнения диска, %',{exact:true});
  const save=section.getByRole('button',{name:'Сохранить лимит',exact:true});
  const refresh=async()=>{
    const response=page.waitForResponse(r=>new URL(r.url()).pathname==='/api/v1/storage-policy');
    await page.evaluate(()=>document.dispatchEvent(new Event('visibilitychange')));
    await response;
  };
  await expect(input).toHaveValue('80');await expect(save).toBeDisabled();
  for(const invalid of ['0','100','-1','1.5','']){
    await input.fill(invalid);await expect(save).toBeDisabled();await expect(input).toHaveAttribute('aria-invalid','true');
  }
  assert.equal(state.saves,0);
  await input.fill('65');await refresh();await expect(input).toHaveValue('65');
  await save.click();await expect(section.getByText('Лимит сохранён. Камеры продолжают запись.',{exact:true})).toBeVisible();
  assert.equal(state.policy.max_disk_usage_percent,65);
  await page.reload();await expect(input).toHaveValue('65');
  state.policy={max_disk_usage_percent:70};await refresh();await expect(input).toHaveValue('70');
  await input.fill('75');state.policy={max_disk_usage_percent:60};await refresh();await expect(input).toHaveValue('75');
  state.saveFailure=503;await save.click();await expect(section.getByText(/Не удалось подтвердить сохранение лимита/)).toBeVisible();
  await expect(input).toHaveValue('75');assert.equal(state.policy.max_disk_usage_percent,60);
  assert.doesNotMatch(await section.textContent(),/fixture secret/);
  state.saveFailure=0;await save.click();await expect(save).toBeDisabled();
  // Stale background GET cannot undo a later PUT.
  await input.fill('77');state.delay=500;
  const pending=page.waitForRequest(r=>new URL(r.url()).pathname==='/api/v1/storage-policy'&&r.method()==='GET');
  await page.evaluate(()=>document.dispatchEvent(new Event('visibilitychange')));await pending;
  await save.click();await expect(input).toHaveValue('77');await expect(save).toBeDisabled();
  await page.waitForTimeout(650);await expect(input).toHaveValue('77');state.delay=0;
  state.loadFailure=503;await refresh();await expect(input).toBeDisabled();
  state.loadFailure=0;await section.getByRole('button',{name:'Повторить',exact:true}).click();await expect(input).toBeEnabled();
  await page.goto(origin+'/#overview');state.policy={max_disk_usage_percent:85};
  await page.goto(origin+'/#settings');await expect(input).toHaveValue('85');
  for(const width of [1280,375]){
    await page.setViewportSize({width,height:900});await section.scrollIntoViewIfNeeded();
    assert(await page.evaluate(()=>document.documentElement.scrollWidth<=innerWidth));
    await section.screenshot({path:join(output,`local-storage-${width}.png`)});
  }
  assert.deepEqual(errors,[]);
  console.log('ok local storage default, validation, CSRF, persistence, polling, stale GET, failures and mobile layout');
} catch(error) {
  await page?.screenshot({path:join(output,'failure.png'),fullPage:true}).catch(()=>{});
  throw error;
} finally {
  await browser?.close();
  server.closeAllConnections();
  await new Promise(resolve=>server.close(resolve));
  console.log('Screenshots: '+output);
}
