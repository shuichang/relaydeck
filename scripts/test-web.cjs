'use strict';

const assert = require('assert/strict');
const cp = require('child_process');
const crypto = require('crypto');
const dgram = require('dgram');
const fs = require('fs');
const net = require('net');
const os = require('os');
const path = require('path');
const { once } = require('events');

const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
const ownedTempRoots = new Set();
const report = {};

let artifactsDir = '';
let browser;
let echo;
let preview;
let previewExit;
let previewLog = '';
let udp;

function absoluteFileFromEnv(name) {
  const value = process.env[name];
  if (!value) throw new Error(`${name} must point to an absolute file path`);
  if (!path.isAbsolute(value)) throw new Error(`${name} must be absolute: ${value}`);
  const resolved = path.resolve(value);
  if (!fs.statSync(resolved, { throwIfNoEntry: false })?.isFile()) {
    throw new Error(`${name} is not a file: ${resolved}`);
  }
  return resolved;
}

function loadPlaywright() {
  const custom = process.env.RELAYDECK_PLAYWRIGHT;
  if (custom && !path.isAbsolute(custom)) {
    throw new Error(`RELAYDECK_PLAYWRIGHT must be absolute: ${custom}`);
  }
  return require(custom || 'playwright');
}

function prepareArtifacts() {
  const requested = process.env.RELAYDECK_UI_ARTIFACTS;
  if (requested) {
    if (!path.isAbsolute(requested)) {
      throw new Error(`RELAYDECK_UI_ARTIFACTS must be absolute: ${requested}`);
    }
    fs.mkdirSync(requested, { recursive: true });
    return path.resolve(requested);
  }
  return fs.mkdtempSync(path.join(os.tmpdir(), 'relaydeck-ui-artifacts-'));
}

function artifactPath(name) {
  return path.join(artifactsDir, name);
}

function initializeAdmin(binary, dataDir, username, password) {
  fs.mkdirSync(dataDir, { recursive: true, mode: 0o700 });
  const result = cp.spawnSync(binary, [
    'init', '--data', dataDir, '--username', username, '--password-stdin',
  ], {
    encoding: 'utf8',
    input: password + '\n',
    timeout: 30000,
    windowsHide: true,
  });
  if (result.error) throw result.error;
  if (result.status !== 0) {
    throw new Error(`relaydeck init failed (${result.status}): ${String(result.stderr || '').trim()}`);
  }
}

async function freePort(host = '127.0.0.1') {
  const server = net.createServer();
  server.listen(0, host);
  await once(server, 'listening');
  const port = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return port;
}

async function waitForPreview(origin) {
  for (let i = 0; i < 100; i++) {
    if (preview.exitCode !== null || preview.signalCode !== null) {
      throw new Error(
        `preview exited before startup: code=${preview.exitCode} signal=${preview.signalCode}\n${previewLog.slice(-1000)}`,
      );
    }
    try {
      const response = await fetch(origin + '/healthz', { signal: AbortSignal.timeout(1000) });
      if (response.ok) return;
    } catch {}
    await delay(100);
  }
  throw new Error(`preview startup failed: ${previewLog.slice(-1000)}`);
}

async function pageAPI(page, method, route, body) {
  return await page.evaluate(async ({ method, route, body }) => {
    const res = await fetch(route, {
      method,
      headers: { 'Content-Type': 'application/json', 'X-RelayDeck-CSRF': '1' },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    return { status: res.status, body: await res.json() };
  }, { method, route, body });
}

async function waitTarget(page, text) {
  await page.waitForFunction(
    expected => document.getElementById('rule-target-probe-badge').textContent.includes(expected),
    text,
  );
}

async function stopOwnedPreview() {
  const child = preview;
  preview = undefined;
  if (!child || !previewExit) return;
  if (child.exitCode !== null || child.signalCode !== null) return;

  try { child.kill(); } catch {}
  await Promise.race([previewExit, delay(3000)]);
  if (child.exitCode === null && child.signalCode === null) {
    try { child.kill('SIGKILL'); } catch {}
    await Promise.race([previewExit, delay(2000)]);
  }
}

function samePath(left, right) {
  const normalize = value => process.platform === 'win32' ? value.toLowerCase() : value;
  return normalize(path.resolve(left)) === normalize(path.resolve(right));
}

function removeOwnedTempRoot(tempRoot) {
  if (!ownedTempRoots.has(tempRoot)) {
    throw new Error(`refusing to remove a directory this script did not create: ${tempRoot}`);
  }
  if (!path.isAbsolute(tempRoot) || !path.basename(tempRoot).startsWith('relaydeck-ui-')) {
    throw new Error(`refusing to remove an unrecognized temporary directory: ${tempRoot}`);
  }

  const realTemp = fs.realpathSync.native(os.tmpdir());
  const realRoot = fs.realpathSync.native(tempRoot);
  const expected = path.join(realTemp, path.basename(tempRoot));
  if (!samePath(realRoot, expected)) {
    throw new Error(`temporary directory resolved outside ${realTemp}: ${realRoot}`);
  }

  fs.rmSync(realRoot, {
    recursive: true,
    force: true,
    maxRetries: process.platform === 'win32' ? 8 : 2,
    retryDelay: 100,
  });
}

async function writeReport() {
  if (!artifactsDir) return;
  report.artifactsDirectory = artifactsDir;
  fs.writeFileSync(artifactPath('ui-result.json'), JSON.stringify(report, null, 2));
}

async function run() {
  const binary = absoluteFileFromEnv('RELAYDECK_BINARY');
  const playwright = loadPlaywright();
  const chrome = process.env.RELAYDECK_CHROMIUM
    ? absoluteFileFromEnv('RELAYDECK_CHROMIUM')
    : undefined;
  artifactsDir = prepareArtifacts();

  const tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'relaydeck-ui-'));
  ownedTempRoots.add(tempRoot);
  if (process.platform !== 'win32') fs.chmodSync(tempRoot, 0o700);
  const dataDir = path.join(tempRoot, 'data');
  const username = 'admin';
  const password = crypto.randomBytes(24).toString('base64url');
  initializeAdmin(binary, dataDir, username, password);

  echo = net.createServer(conn => {
    // Payload-free TCP probes intentionally reset their socket on close.
    conn.on('error', () => {});
    conn.pipe(conn);
  });
  echo.listen(0, '127.0.0.1');
  await once(echo, 'listening');
  const targetPort = echo.address().port;
  const target = `127.0.0.1:${targetPort}`;

  udp = dgram.createSocket('udp4');
  udp.on('message', (data, peer) => udp.send(data, peer.port, peer.address));
  udp.bind(targetPort, '127.0.0.1');
  await once(udp, 'listening');

  const ingress = await freePort('::1');
  const closed = await freePort('127.0.0.1');
  const previewPort = await freePort('127.0.0.1');
  const origin = `http://127.0.0.1:${previewPort}`;
  preview = cp.spawn(binary, [
    'serve', '--insecure-local', '--listen', `127.0.0.1:${previewPort}`,
    '--data', dataDir, '--engine-binary', path.join(tempRoot, 'missing-engine'),
  ], { stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
  previewExit = new Promise(resolve => preview.once('exit', (code, signal) => {
    resolve({ code, signal });
  }));
  preview.stdout.on('data', chunk => previewLog += chunk);
  preview.stderr.on('data', chunk => previewLog += chunk);
  await waitForPreview(origin);

  const launchOptions = { headless: true, args: ['--no-first-run'] };
  if (chrome) launchOptions.executablePath = chrome;
  browser = await playwright.chromium.launch(launchOptions);
  const page = await browser.newPage({ viewport: { width: 1440, height: 1050 } });
  page.setDefaultTimeout(12000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));

  await page.goto(origin, { waitUntil: 'networkidle' });
  await page.locator('#login-username').fill(username);
  await page.locator('#login-password').fill(password);
  await page.locator('#login-submit').click();
  await page.locator('#app-view').waitFor({ state: 'visible' });
  await page.waitForFunction(() => document.getElementById('browser-panel-rtt-value').textContent.includes('ms'));

  assert.equal((await pageAPI(page, 'PUT', '/api/rules', { rules: [] })).status, 200);
  await page.reload({ waitUntil: 'networkidle' });
  await page.locator('[data-route="rules"]').click();
  await page.locator('[data-action="new-rule"]:visible').first().click();
  // Let the dialog's scheduled autofocus finish before filling other fields.
  await delay(80);
  await page.locator('#rule-name').fill('双栈入口与目标检测');
  await page.locator('#rule-listen').fill(`[::1]:${ingress}`);
  await page.locator('#rule-target').fill(target);
  assert.equal(await page.locator('#rule-target').inputValue(), target);
  assert.equal(await page.locator('#rule-max-connections').inputValue(), '8192');
  assert.equal(await page.locator('#rule-max-connections').getAttribute('max'), '65536');
  await page.locator('#rule-probe').click();
  await waitTarget(page, 'TCP 可达');
  assert.match(await page.locator('#rule-listen-probe-badge').textContent(), /可绑定/);
  assert.match(await page.locator('#rule-target-probe-metric').textContent(), /TCP.+ms/);
  assert.match(await page.locator('#rule-target-probe-message').textContent(), /UDP 尚未验证/);
  assert.equal((await pageAPI(page, 'GET', '/api/rules')).body.length, 0);
  report.draftProbeWithoutSaving = 'passed';

  await page.locator('#rule-target').fill(`127.0.0.1:${closed}`);
  assert.match(await page.locator('#rule-target-probe-badge').textContent(), /重新|未检测|已失效/);
  await page.locator('#rule-probe').click();
  await waitTarget(page, '检测失败');
  assert.match(await page.locator('#rule-target-probe-message').textContent(), /握手失败/);
  report.refusedTargetDiagnosis = 'passed';

  await page.locator('#rule-target').fill(target);
  let delayed = false;
  await page.route('**/api/rules/probe', async route => {
    if (delayed) return route.continue();
    delayed = true;
    const response = await route.fetch();
    await delay(350);
    try { await route.fulfill({ response }); } catch {}
  });
  await page.locator('#rule-probe').click();
  await page.waitForFunction(() => document.getElementById('rule-target-probe-badge').textContent === '检测中…');
  await page.locator('#rule-target').fill(`127.0.0.1:${closed}`);
  await delay(550);
  assert.doesNotMatch(await page.locator('#rule-target-probe-badge').textContent(), /TCP 可达/);
  await page.unroute('**/api/rules/probe');
  report.staleAsyncResponseDiscarded = 'passed';

  await page.locator('#rule-target').fill(target);
  await page.locator('#rule-form').getByText('仅 UDP', { exact: true }).click();
  await page.locator('#rule-probe').click();
  await waitTarget(page, '未验证');
  assert.doesNotMatch(await page.locator('#rule-target-probe-metric').textContent(), /ms/);
  assert.match(await page.locator('#rule-target-probe-message').textContent(), /UDP/);
  report.udpHasNoInventedRTT = 'passed';
  await page.locator('#rule-form').getByText('TCP + UDP', { exact: true }).click();
  await page.locator('#rule-submit').click();
  await page.locator('#rule-dialog').waitFor({ state: 'hidden' });
  await page.locator('#rules-body').getByText('双栈入口与目标检测', { exact: true }).waitFor();
  await page.waitForFunction(() => document.getElementById('rules-body').textContent.includes('TCP 可达'));
  assert.match(await page.locator('#rules-body').textContent(), /8,192|8192/);
  assert.match(await page.locator('#rules-body').textContent(), /本机监听正常/);
  const saved = (await pageAPI(page, 'GET', '/api/rules')).body;
  assert.equal(saved.length, 1);
  assert.equal(saved[0].max_connections, 8192);
  assert.equal(saved[0].effective_max_connections, 8192);

  // Deterministically reorder requests from the list and edit dialog, which
  // share a cache. Cover both late success and late HTTP failure paths.
  for (const olderFails of [false, true]) {
    let requests = 0;
    let releaseOlder;
    let olderStarted;
    const gate = new Promise(resolve => releaseOlder = resolve);
    const started = new Promise(resolve => olderStarted = resolve);
    await page.route('**/api/rules/probe', async route => {
      const index = ++requests;
      if (index === 1) {
        olderStarted();
        await gate;
      }
      const fail = index === 1 ? olderFails : !olderFails;
      if (index === 1 && olderFails) {
        return route.fulfill({ status: 503, contentType: 'application/json', body: '{"error":"older-http-failure"}' });
      }
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          checked_at: new Date().toISOString(),
          target_address: target,
          listen: { status: 'listening', latency_ms: null, message: 'local fixture' },
          target: {
            status: fail ? 'error' : 'reachable',
            latency_ms: fail ? null : 27,
            message: index === 1 ? 'older-result' : 'newer-result',
          },
        }),
      });
    });
    await page.locator('#rules-body .probe-mini-action').click();
    await started;
    await page.locator('#rules-body button[aria-label="编辑规则"]').click();
    await delay(80);
    await page.locator('#rule-probe').click();
    await waitTarget(page, olderFails ? 'TCP 可达' : '检测失败');
    await page.locator('#rule-cancel-x').click();
    releaseOlder();
    await page.waitForFunction(() => !document.querySelector('#rules-body .probe-mini-action').disabled);
    await page.locator('#rules-body button[aria-label="编辑规则"]').click();
    await delay(80);
    await waitTarget(page, olderFails ? 'TCP 可达' : '检测失败');
    assert.equal(await page.locator('#rule-target-probe-message').textContent(), 'newer-result');
    await page.locator('#rule-cancel-x').click();
    await page.unroute('**/api/rules/probe');
  }
  report.crossViewCacheOrdering = 'passed: late success and late HTTP failure';
  await page.locator('#rules-body .probe-mini-action').click();
  await page.waitForFunction(() => !document.querySelector('#rules-body .probe-mini-action').disabled);

  const tcpReply = await new Promise((resolve, reject) => {
    const conn = net.connect({ host: '::1', port: ingress }, () => conn.write('browser-QA-TCP'));
    conn.setTimeout(3000, () => conn.destroy(new Error('TCP echo timed out')));
    conn.once('error', reject);
    conn.once('data', data => { resolve(data.toString()); conn.destroy(); });
  });
  assert.equal(tcpReply, 'browser-QA-TCP');
  const udpReply = await new Promise((resolve, reject) => {
    const conn = dgram.createSocket('udp6');
    const timeout = setTimeout(() => { conn.close(); reject(new Error('UDP echo timed out')); }, 3000);
    conn.once('error', reject);
    conn.once('message', data => { clearTimeout(timeout); conn.close(); resolve(data.toString()); });
    conn.send(Buffer.from('browser-QA-UDP'), ingress, '::1');
  });
  assert.equal(udpReply, 'browser-QA-UDP');
  report.savedRuleAutoProbeAndTCPUDP = 'passed';

  await page.route('**/api/rules', async route => {
    if (route.request().method() !== 'GET') return route.continue();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(saved.map(rule => ({ ...rule, network: 'udp', udp_sessions: 8192 }))),
    });
  });
  await page.locator('#rules-refresh').click();
  await page.locator('#rules-body .limit-usage.full').waitFor();
  assert.match(await page.locator('#rules-body .limit-usage.full').textContent(), /UDP 会话.+已达上限/);
  report.udpLimitWarning = 'passed';
  await page.unroute('**/api/rules');
  await page.locator('#rules-refresh').click();
  await page.waitForFunction(() => document.getElementById('rules-body').textContent.includes('TCP 连接'));

  await page.screenshot({ path: artifactPath('rules-desktop.png'), fullPage: true });
  await page.setViewportSize({ width: 390, height: 844 });
  await delay(300);
  await page.screenshot({ path: artifactPath('rules-mobile.png'), fullPage: true });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await page.locator('[data-action="new-rule"]:visible').first().click();
  await page.screenshot({ path: artifactPath('draft-mobile.png'), fullPage: true });
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await page.locator('#rule-cancel-x').click();
  report.mobileOverflow = false;

  await page.setViewportSize({ width: 1440, height: 1050 });
  await page.locator('[data-route="engine"]').click();
  await page.waitForFunction(() => document.querySelectorAll('#engine-template option').length === 11);
  assert.equal(await page.locator('#engine-template option').nth(1).getAttribute('value'), 'direct-passthrough');
  await page.locator('#engine-template').selectOption('direct-passthrough');
  await page.locator('#engine-apply-template').click();
  const generic = JSON.parse(await page.locator('#engine-config').inputValue());
  assert.equal(generic.outbounds[0].type, 'direct');
  assert.equal(generic.inbounds[0].network, undefined);
  assert.match(await page.locator('.template-guidance').textContent(), /无需协议内核/);
  report.templates = 10;

  await page.locator('[data-route="rules"]').click();
  // Exercise the new upper bound through the real form and persistence API.
  await page.locator('#rules-body button[aria-label="编辑规则"]').first().click();
  await delay(80);
  await page.locator('#rule-max-connections').fill('65536');
  await page.locator('#rule-submit').click();
  await page.locator('#rule-dialog').waitFor({ state: 'hidden' });
  assert.equal((await pageAPI(page, 'GET', '/api/rules')).body[0].max_connections, 65536);
  report.newMaximumAccepted = 65536;

  assert.deepEqual(errors, []);
  report.browserErrors = errors;
  assert.equal((await pageAPI(page, 'PUT', '/api/rules', { rules: [] })).status, 200);
  await writeReport();
  console.log(JSON.stringify(report, null, 2));
}

run().catch(async error => {
  console.error(error.stack || error);
  report.error = String(error.stack || error);
  if (browser && artifactsDir) {
    const pages = browser.contexts().flatMap(context => context.pages());
    if (pages[0]) {
      await pages[0].screenshot({ path: artifactPath('ui-error.png'), fullPage: true }).catch(() => {});
    }
  }
  await writeReport().catch(writeError => console.error(writeError.stack || writeError));
  process.exitCode = 1;
}).finally(async () => {
  if (browser) await browser.close().catch(() => {});
  await stopOwnedPreview();
  if (udp) {
    try { udp.close(); } catch {}
  }
  if (echo) {
    try { await new Promise(resolve => echo.close(resolve)); } catch {}
  }
  for (const tempRoot of ownedTempRoots) {
    try {
      removeOwnedTempRoot(tempRoot);
    } catch (error) {
      console.error(error.stack || error);
      process.exitCode = 1;
    }
  }
});
