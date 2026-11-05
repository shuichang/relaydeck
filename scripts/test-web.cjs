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

function deferred() {
  let resolve;
  let reject;
  const promise = new Promise((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function enginePayload(overrides = {}) {
  return {
    enabled: false,
    available: true,
    running: false,
    version: 'sing-box version 1.14.4',
    error: '',
    config: { log: { level: 'info' } },
    logs: [],
    installation: installationPayload(),
    ...overrides,
  };
}

function installationPayload(overrides = {}) {
  return {
    supported: true,
    version: '1.14.4',
    phase: 'idle',
    running: false,
    downloaded_bytes: 0,
    total_bytes: 0,
    error: '',
    ...overrides,
  };
}

function tlsCertificatePayload(overrides = {}) {
  return {
    subject: 'CN=relay.example.com',
    issuer: 'CN=Fixture CA, O=RelayDeck QA',
    dns_names: ['relay.example.com'],
    ip_addresses: ['203.0.113.10'],
    not_before: '2026-01-01T00:00:00Z',
    not_after: '2027-01-01T00:00:00Z',
    fingerprint_sha256: 'AA:BB:CC:DD:EE:FF:00:11:22:33:44:55:66:77:88:99',
    self_signed: false,
    valid_now: true,
    ...overrides,
  };
}

function tlsSettingsPayload(overrides = {}) {
  return {
    available: true,
    mode: 'files',
    certificate: tlsCertificatePayload(),
    cert_path: '/etc/letsencrypt/live/relay.example.com/fullchain.pem',
    key_path: '/etc/letsencrypt/live/relay.example.com/privkey.pem',
    auto_reload_seconds: 60,
    reload_error: '',
    updated_at: '2026-11-05T08:00:00Z',
    message: '',
    ...overrides,
  };
}

async function computedFontSize(locator) {
  return await locator.evaluate(node => getComputedStyle(node).fontSize);
}

async function overflowDiagnostics(page) {
  return await page.evaluate(() => {
    const viewportWidth = window.innerWidth;
    const allElements = [...document.querySelectorAll('body *')];
    const overflow = allElements
      .map(node => {
        const rect = node.getBoundingClientRect();
        const style = getComputedStyle(node);
        return {
          tag: node.tagName,
          id: node.id,
          className: typeof node.className === 'string' ? node.className : '',
          text: (node.textContent || '').trim().slice(0, 80),
          left: Math.round(rect.left * 10) / 10,
          right: Math.round(rect.right * 10) / 10,
          documentRight: Math.round((rect.right + window.scrollX) * 10) / 10,
          width: Math.round(rect.width * 10) / 10,
          position: style.position,
          display: style.display,
        };
      })
      .filter(entry => entry.display !== 'none' && entry.documentRight > viewportWidth + 0.5)
      .sort((left, right) => right.documentRight - left.documentRight)
      .slice(0, 30);
    const wideBoxes = allElements
      .map(node => {
        const rect = node.getBoundingClientRect();
        return {
          tag: node.tagName,
          id: node.id,
          className: typeof node.className === 'string' ? node.className : '',
          width: Math.round(rect.width * 10) / 10,
          left: Math.round(rect.left * 10) / 10,
          right: Math.round(rect.right * 10) / 10,
          scrollWidth: node.scrollWidth,
          clientWidth: node.clientWidth,
        };
      })
      .filter(entry => entry.width > viewportWidth + 0.5 || entry.scrollWidth > viewportWidth + 0.5)
      .sort((left, right) => Math.max(right.width, right.scrollWidth) - Math.max(left.width, left.scrollWidth))
      .slice(0, 30);
    return {
      innerWidth: viewportWidth,
      outerWidth: window.outerWidth,
      scrollX: window.scrollX,
      activeElement: document.activeElement
        ? `${document.activeElement.tagName}#${document.activeElement.id}.${document.activeElement.className}`
        : '',
      documentScrollWidth: document.documentElement.scrollWidth,
      bodyScrollWidth: document.body.scrollWidth,
      overflow,
      wideBoxes,
    };
  });
}

async function settleFrames(page) {
  await page.evaluate(() => new Promise(resolve => {
    requestAnimationFrame(() => requestAnimationFrame(resolve));
  }));
}

async function capturePage(page, name) {
  await page.locator('#toast-region button').evaluateAll(buttons => {
    buttons.forEach(button => button.click());
  });
  await page.evaluate(() => window.scrollTo({ top: 0, left: 0, behavior: 'instant' }));
  await settleFrames(page);
  await page.screenshot({ path: artifactPath(name), fullPage: true, animations: 'disabled' });
}

async function checkEngineEnabled(page) {
  const checkbox = page.locator('#engine-enabled');
  if (!(await checkbox.isChecked())) {
    await page.getByText('配置保存后启用内核', { exact: true }).click();
  }
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
  const consoleMessages = [];
  const privateKeyMarker = 'QA-PRIVATE-KEY-MUST-NEVER-PERSIST';
  page.on('pageerror', error => errors.push(error.message));
  page.on('console', message => consoleMessages.push(message.text()));

  await page.goto(origin, { waitUntil: 'networkidle' });
  await page.locator('#login-username').fill(username);
  await page.locator('#login-password').fill(password);
  await page.locator('#login-submit').click();
  await page.locator('#app-view').waitFor({ state: 'visible' });
  await page.waitForFunction(() => document.getElementById('browser-panel-rtt-value').textContent.includes('ms'));

  let controlledTLS = tlsSettingsPayload({
    available: false,
    mode: 'default',
    certificate: null,
    cert_path: '',
    key_path: '',
    auto_reload_seconds: 0,
    message: '本地 HTTP 预览不支持在线管理 TLS 证书。',
  });
  let nextTLSGet;
  let nextTLSPut;
  let nextTLSReload;
  const fulfillTLS = (route, payload, status = 200) => route.fulfill({
    status,
    contentType: 'application/json',
    body: JSON.stringify(payload),
  });
  await page.route('**/api/settings/tls', async route => {
    const request = route.request();
    if (request.method() === 'GET') {
      if (nextTLSGet) {
        const handler = nextTLSGet;
        nextTLSGet = undefined;
        await handler(route);
        return;
      }
      await fulfillTLS(route, controlledTLS);
      return;
    }
    if (request.method() === 'PUT') {
      const body = request.postDataJSON();
      if (nextTLSPut) {
        const handler = nextTLSPut;
        nextTLSPut = undefined;
        await handler(route, body);
        return;
      }
      controlledTLS = tlsSettingsPayload({ mode: body.mode });
      await fulfillTLS(route, controlledTLS);
      return;
    }
    await route.continue();
  });
  await page.route('**/api/settings/tls/reload', async route => {
    if (route.request().method() !== 'POST') return route.continue();
    if (nextTLSReload) {
      const handler = nextTLSReload;
      nextTLSReload = undefined;
      await handler(route, route.request().postDataJSON());
      return;
    }
    await fulfillTLS(route, controlledTLS);
  });

  nextTLSGet = async route => {
    await fulfillTLS(route, { error: 'fixture settings session expired' }, 401);
  };
  await page.locator('[data-route="settings"]').click();
  await page.locator('#login-view').waitFor({ state: 'visible' });
  assert.match(await page.locator('#login-error').textContent(), /登录状态已失效/);
  report.settingsUnauthorizedExits = 'passed';

  await page.locator('#login-username').fill(username);
  await page.locator('#login-password').fill(password);
  await page.locator('#login-submit').click();
  await page.locator('#app-view').waitFor({ state: 'visible' });
  await page.locator('#settings-unavailable').waitFor({ state: 'visible' });
  assert.equal(await page.locator('#settings-main').isVisible(), false);
  assert.match(await page.locator('#settings-unavailable-message').textContent(), /本地 HTTP 预览/);
  report.settingsUnavailable = 'passed';

  controlledTLS = tlsSettingsPayload();
  await page.locator('#settings-refresh').click();
  await page.locator('#settings-main').waitFor({ state: 'visible' });
  assert.match(await page.locator('#tls-subject').textContent(), /CN=relay\.example\.com/);
  assert.match(await page.locator('#tls-issuer').textContent(), /Fixture CA/);
  assert.equal(await page.locator('#tls-validity-badge').textContent(), '有效');
  assert.match(await page.locator('#tls-dns-sans').textContent(), /relay\.example\.com/);
  assert.match(await page.locator('#tls-ip-sans').textContent(), /203\.0\.113\.10/);
  assert.match(await page.locator('#tls-auto-reload').textContent(), /每 60 秒/);
  assert.match(await page.locator('#tls-cert-path').inputValue(), /fullchain\.pem/);
  assert.match(await page.locator('.tls-browser-note').textContent(), /SAN 匹配的域名或 IP/);
  assert.equal(await computedFontSize(page.locator('.tls-summary dd').first()), '14px');
  report.settingsCertificateDetails = 'passed';

  await page.locator('#tls-cert-path').fill('/srv/draft/fullchain.pem');
  await page.locator('#tls-draft-badge').waitFor({ state: 'visible' });
  nextTLSGet = async route => {
    await fulfillTLS(route, { error: 'fixture settings read failure' }, 503);
  };
  await page.locator('#settings-refresh').click();
  await page.waitForFunction(() => !document.getElementById('settings-alert').hidden);
  assert.match(await page.locator('#settings-alert').textContent(), /fixture settings read failure/);
  assert.equal(await page.locator('#tls-cert-path').inputValue(), '/srv/draft/fullchain.pem');

  controlledTLS = tlsSettingsPayload({
    certificate: tlsCertificatePayload({ subject: 'CN=refreshed.example.com' }),
  });
  await page.locator('#settings-refresh').click();
  await page.waitForFunction(() => document.getElementById('tls-subject').textContent.includes('refreshed.example.com'));
  assert.equal(await page.locator('#tls-cert-path').inputValue(), '/srv/draft/fullchain.pem');
  report.settingsDraftSurvivesLoadFailureAndRefresh = 'passed';

  await page.getByText('粘贴 PEM', { exact: true }).click();
  await page.locator('#tls-certificate-pem').fill('-----BEGIN CERTIFICATE-----\nQA-CERT\n-----END CERTIFICATE-----');
  await page.locator('#tls-private-key-pem').fill(`-----BEGIN PRIVATE KEY-----\n${privateKeyMarker}\n-----END PRIVATE KEY-----`);
  let failedPemBody;
  nextTLSPut = async (route, body) => {
    failedPemBody = body;
    await fulfillTLS(route, { error: 'invalid certificate fixture' }, 400);
  };
  await page.locator('#tls-save').click();
  await page.locator('#tls-form-error').waitFor({ state: 'visible' });
  assert.match(await page.locator('#tls-form-error').textContent(), /invalid certificate fixture/);
  assert.equal(failedPemBody.mode, 'pem');
  assert.match(await page.locator('#tls-private-key-pem').inputValue(), new RegExp(privateKeyMarker));

  const uploadedCertificate = '-----BEGIN CERTIFICATE-----\nUPLOADED-CERT\n-----END CERTIFICATE-----\n';
  const uploadedPrivateKey = `-----BEGIN PRIVATE KEY-----\n${privateKeyMarker}-UPLOADED\n-----END PRIVATE KEY-----\n`;
  await page.locator('#tls-certificate-file').setInputFiles({
    name: 'certificate.pem',
    mimeType: 'text/plain',
    buffer: Buffer.from(uploadedCertificate),
  });
  await page.locator('#tls-private-key-file').setInputFiles({
    name: 'private-key.pem',
    mimeType: 'text/plain',
    buffer: Buffer.from(uploadedPrivateKey),
  });
  await page.waitForFunction(
    markers => document.getElementById('tls-certificate-pem').value.includes(markers.cert)
      && document.getElementById('tls-private-key-pem').value.includes(markers.key),
    { cert: 'UPLOADED-CERT', key: `${privateKeyMarker}-UPLOADED` },
  );

  let successfulPemBody;
  nextTLSPut = async (route, body) => {
    successfulPemBody = body;
    controlledTLS = tlsSettingsPayload({
      mode: 'pem',
      certificate: tlsCertificatePayload({ subject: 'CN=pem.example.com', issuer: 'CN=QA Issuer' }),
      cert_path: '',
      key_path: '',
      auto_reload_seconds: 0,
    });
    await fulfillTLS(route, controlledTLS);
  };
  await page.locator('#tls-save').click();
  await page.waitForFunction(() => document.getElementById('tls-subject').textContent.includes('pem.example.com'));
  assert.equal(successfulPemBody.mode, 'pem');
  assert.equal(successfulPemBody.certificate_pem, uploadedCertificate);
  assert.equal(successfulPemBody.private_key_pem, uploadedPrivateKey);
  assert.equal(await page.locator('#tls-certificate-pem').inputValue(), '');
  assert.equal(await page.locator('#tls-private-key-pem').inputValue(), '');
  assert.equal(await page.locator('#tls-certificate-file').evaluate(node => node.files.length), 0);
  assert.equal(await page.locator('#tls-private-key-file').evaluate(node => node.files.length), 0);
  assert.equal(await page.locator('#tls-reload').isDisabled(), true);
  const browserStorage = await page.evaluate(() => Object.values(localStorage).join('\n'));
  assert.doesNotMatch(browserStorage, new RegExp(privateKeyMarker));
  assert.doesNotMatch(await page.locator('body').textContent(), new RegExp(privateKeyMarker));
  report.settingsPemFailureRetainsDraftAndSuccessClearsSecrets = 'passed';

  await page.getByText('自签证书', { exact: true }).click();
  await page.locator('#tls-self-signed-hosts').fill('relay.example.com\n203.0.113.10\n::1');
  await page.locator('#tls-self-signed-days').fill('90');
  let selfSignedBody;
  nextTLSPut = async (route, body) => {
    selfSignedBody = body;
    controlledTLS = tlsSettingsPayload({
      mode: 'self_signed',
      certificate: tlsCertificatePayload({
        subject: 'CN=relay.example.com',
        issuer: 'CN=relay.example.com',
        dns_names: ['relay.example.com'],
        ip_addresses: ['203.0.113.10', '::1'],
        self_signed: true,
      }),
      cert_path: '/var/lib/relaydeck/tls/fullchain.pem',
      key_path: '/var/lib/relaydeck/tls/privkey.pem',
      auto_reload_seconds: 0,
    });
    await fulfillTLS(route, controlledTLS);
  };
  await page.locator('#tls-save').click();
  await page.waitForFunction(() => document.getElementById('tls-mode-badge').textContent === '自签证书');
  assert.deepEqual(selfSignedBody, {
    mode: 'self_signed',
    hosts: ['relay.example.com', '203.0.113.10', '::1'],
    days: 90,
  });
  report.settingsSelfSigned = 'passed';

  await page.getByText('服务器文件', { exact: true }).click();
  await page.locator('#tls-cert-path').fill('/etc/letsencrypt/live/relay.example.com/fullchain.pem');
  await page.locator('#tls-key-path-input').fill('/etc/letsencrypt/live/relay.example.com/privkey.pem');
  let filesBody;
  nextTLSPut = async (route, body) => {
    filesBody = body;
    controlledTLS = tlsSettingsPayload();
    await fulfillTLS(route, controlledTLS);
  };
  await page.locator('#tls-save').click();
  await page.waitForFunction(() => document.getElementById('tls-auto-reload').textContent.includes('60 秒'));
  assert.deepEqual(filesBody, {
    mode: 'files',
    cert_path: '/etc/letsencrypt/live/relay.example.com/fullchain.pem',
    key_path: '/etc/letsencrypt/live/relay.example.com/privkey.pem',
  });
  assert.equal(await page.locator('#tls-reload').isDisabled(), false);
  await page.locator('#tls-key-path-input').fill('/srv/pending/private.key');
  let reloadBody;
  nextTLSReload = async (route, body) => {
    reloadBody = body;
    controlledTLS = tlsSettingsPayload({ updated_at: '2026-11-05T09:00:00Z' });
    await fulfillTLS(route, controlledTLS);
  };
  await page.locator('#tls-reload').click();
  await page.waitForFunction(() => document.querySelector('#toast-region').textContent.includes('文件证书已重新载入'));
  assert.deepEqual(reloadBody, {});
  assert.equal(await page.locator('#tls-key-path-input').inputValue(), '/srv/pending/private.key');
  assert.equal(await page.locator('#tls-draft-badge').isVisible(), true);
  report.settingsFilesReload = 'passed';

  await capturePage(page, 'settings-desktop.png');
  await page.setViewportSize({ width: 390, height: 844 });
  await delay(300);
  const settingsMobileOverflow = await overflowDiagnostics(page);
  assert.equal(
    settingsMobileOverflow.documentScrollWidth > settingsMobileOverflow.innerWidth,
    false,
    JSON.stringify(settingsMobileOverflow),
  );
  await capturePage(page, 'settings-mobile.png');
  await page.setViewportSize({ width: 1440, height: 1050 });
  await delay(300);
  report.settingsMobileOverflow = false;

  await page.getByText('启动默认', { exact: true }).click();
  let defaultBody;
  nextTLSPut = async (route, body) => {
    defaultBody = body;
    controlledTLS = tlsSettingsPayload({ mode: 'default', certificate: null, cert_path: '', key_path: '', auto_reload_seconds: 60 });
    await fulfillTLS(route, controlledTLS);
  };
  await page.locator('#tls-save').click();
  await page.waitForFunction(() => document.getElementById('tls-mode-badge').textContent === '启动默认');
  assert.deepEqual(defaultBody, { mode: 'default' });

  await page.getByText('粘贴 PEM', { exact: true }).click();
  await page.locator('#tls-private-key-pem').fill(`-----BEGIN PRIVATE KEY-----\n${privateKeyMarker}-LOGOUT\n-----END PRIVATE KEY-----`);
  await page.locator('#tls-private-key-file').setInputFiles({
    name: 'logout-key.pem',
    mimeType: 'text/plain',
    buffer: Buffer.from(`-----BEGIN PRIVATE KEY-----\n${privateKeyMarker}-FILE\n-----END PRIVATE KEY-----\n`),
  });
  await page.waitForFunction(
    marker => document.getElementById('tls-private-key-pem').value.includes(marker),
    `${privateKeyMarker}-FILE`,
  );
  await page.evaluate(marker => {
    const original = File.prototype.text;
    File.prototype.text = function () {
      if (this.name === 'delayed-logout-key.pem') {
        return new Promise(resolve => { window.releaseDelayedTLSFile = () => resolve(marker); });
      }
      return original.call(this);
    };
    window.restoreTLSFileReader = () => { File.prototype.text = original; };
  }, `${privateKeyMarker}-DELAYED-OLD-SESSION`);
  await page.locator('#tls-private-key-file').setInputFiles({
    name: 'delayed-logout-key.pem', mimeType: 'text/plain', buffer: Buffer.from('deferred private key'),
  });
  await page.waitForFunction(() => typeof window.releaseDelayedTLSFile === 'function');
  await page.locator('#logout-button').click();
  await page.locator('#login-view').waitFor({ state: 'visible' });
  assert.equal(await page.locator('#tls-private-key-pem').inputValue(), '');
  assert.equal(await page.locator('#tls-private-key-file').evaluate(node => node.files.length), 0);
  report.settingsLogoutClearsSecrets = 'passed';

  await page.locator('#login-username').fill(username);
  await page.locator('#login-password').fill(password);
  await page.locator('#login-submit').click();
  await page.locator('#app-view').waitFor({ state: 'visible' });
  await page.locator('#settings-main').waitFor({ state: 'visible' });
  await page.evaluate(() => {
    window.releaseDelayedTLSFile();
    window.restoreTLSFileReader();
    delete window.releaseDelayedTLSFile;
    delete window.restoreTLSFileReader;
  });
  await settleFrames(page);
  assert.equal(await page.locator('#tls-private-key-pem').inputValue(), '');
  assert.equal(await page.locator('#tls-private-key-file').evaluate(node => node.files.length), 0);
  report.settingsLateFileReadCannotRestoreLoggedOutSecrets = 'passed';
  await page.locator('[data-route="rules"]').click();
  await page.unroute('**/api/settings/tls/reload');
  await page.unroute('**/api/settings/tls');

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
  const ruleTypography = await page.evaluate(() => {
    const size = selector => getComputedStyle(document.querySelector(selector)).fontSize;
    return {
      body: getComputedStyle(document.body).fontSize,
      nav: size('.nav-item'),
      button: size('.button'),
      header: size('.rules-table th'),
      cell: size('.rules-table td'),
      name: size('.rule-name strong'),
      endpoint: size('.endpoint'),
      limit: size('.limit-list'),
      runtime: size('.runtime-state'),
      probe: size('.rule-probe-heading'),
    };
  });
  assert.ok(Number.parseFloat(ruleTypography.body) >= 15);
  assert.ok(Number.parseFloat(ruleTypography.nav) >= 14);
  assert.ok(Number.parseFloat(ruleTypography.button) >= 14);
  assert.ok(Number.parseFloat(ruleTypography.header) >= 12);
  assert.ok(Number.parseFloat(ruleTypography.cell) >= 14);
  assert.ok(Number.parseFloat(ruleTypography.name) >= 16);
  assert.ok(Number.parseFloat(ruleTypography.endpoint) >= 14);
  assert.ok(Number.parseFloat(ruleTypography.limit) >= 12);
  assert.ok(Number.parseFloat(ruleTypography.runtime) >= 12);
  assert.ok(Number.parseFloat(ruleTypography.probe) >= 12);
  assert.equal(await page.evaluate(() => {
    const cell = document.querySelector('#rules-body tr td:last-child');
    const buttons = [...cell.querySelectorAll('button')].map(node => node.getBoundingClientRect());
    const bounds = cell.getBoundingClientRect();
    return buttons.length === 3
      && buttons.every(box => box.left >= bounds.left && box.right <= bounds.right + 0.5)
      && buttons.every((box, index) => index === 0 || box.left >= buttons[index - 1].right - 0.5);
  }), true);
  report.ruleTypography = 'passed';

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

  let ruleHistoryPayload = [{
    ...saved[0],
    running: true,
    error: '',
    error_network: '',
    last_error: 'TCP upstream reset fixture',
    last_error_at: '2026-11-05T01:00:00Z',
    last_recovery_at: '2026-11-05T01:01:00Z',
    last_error_network: 'tcp',
  }];
  await page.route('**/api/rules', async route => {
    if (route.request().method() !== 'GET') return route.continue();
    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify(ruleHistoryPayload),
    });
  });
  await page.locator('#rules-refresh').click();
  await page.waitForFunction(() => document.getElementById('rules-body').textContent.includes('最近错误（TCP）'));
  assert.match(await page.locator('#rules-body .runtime-state').textContent(), /^运行中$/);
  assert.doesNotMatch(await page.locator('#rules-body .runtime').textContent(), /当前错误/);
  assert.doesNotMatch(await page.locator('#rules-body .runtime-state').getAttribute('class'), /bad|has-error/);
  assert.match(await page.locator('#rules-body .rule-history').textContent(), /已恢复/);
  report.ruleRecoveredHistory = 'passed';

  ruleHistoryPayload = [{
    ...saved[0],
    running: true,
    error: 'UDP target reply timeout fixture',
    error_network: 'udp',
    last_error: 'older TCP reset fixture',
    last_error_at: '2026-11-05T01:00:00Z',
    last_recovery_at: '2026-11-05T01:01:00Z',
    last_error_network: 'tcp',
  }];
  await page.locator('#rules-refresh').click();
  await page.waitForFunction(() => document.getElementById('rules-body').textContent.includes('当前错误（UDP）'));
  assert.match(await page.locator('#rules-body .runtime-state').textContent(), /^运行中 · 当前错误$/);
  assert.doesNotMatch(await page.locator('#rules-body .runtime-state').textContent(), /启动异常|已停用/);
  assert.match(await page.locator('#rules-body .rule-current-error').textContent(), /UDP[\s\S]*reply timeout/);
  report.ruleCurrentAndHistoryNetworksSeparated = 'passed';

  ruleHistoryPayload = [{
    ...saved[0],
    listen: '[2001:db8:1234:5678:90ab:cdef:1234:5678]:443',
    running: true,
    error: '',
    error_network: '',
    last_error: 'Target reset after address replacement; historical detail remains intentionally long to verify wrapping.',
    last_error_at: '2026-11-05T03:00:00Z',
    last_recovery_at: '2026-11-05T02:00:00Z',
    last_error_network: 'tcp',
  }];
  await page.locator('#rules-refresh').click();
  await page.waitForFunction(() => document.getElementById('rules-body').textContent.includes('历史记录，当前目标需重新检测'));
  assert.doesNotMatch(await page.locator('#rules-body .rule-history').textContent(), /已恢复/);
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  report.ruleStaleRecoveryNotClaimed = 'passed';

  ruleHistoryPayload = [{
    ...saved[0],
    running: true,
    error: '',
    error_network: '',
    last_error: 'UDP reply timeout fixture',
    last_error_at: '2026-11-05T04:00:00Z',
    last_recovery_at: '',
    last_error_network: 'udp',
  }];
  await page.locator('#rules-refresh').click();
  await page.waitForFunction(() => document.getElementById('rules-body').textContent.includes('UDP 恢复尚待真实回包确认'));
  assert.doesNotMatch(await page.locator('#rules-body .rule-history').textContent(), /已恢复/);
  report.ruleUdpRecoveryNotInvented = 'passed';
  await page.unroute('**/api/rules');
  await page.locator('#rules-refresh').click();
  await page.waitForFunction(() => document.getElementById('rules-body').textContent.includes('TCP 连接'));

  await capturePage(page, 'rules-desktop.png');
  await page.setViewportSize({ width: 390, height: 844 });
  await delay(300);
  await capturePage(page, 'rules-mobile.png');
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  assert.ok(Number.parseFloat(await computedFontSize(page.locator('#rules-body .rule-name strong').first())) >= 16);
  assert.ok(Number.parseFloat(await computedFontSize(page.locator('#rules-body .endpoint').first())) >= 14);
  assert.equal(await page.evaluate(() => {
    const row = document.querySelector('#rules-body tr');
    const buttons = [...row.querySelectorAll('.row-actions button')].map(node => node.getBoundingClientRect());
    const bounds = row.getBoundingClientRect();
    return buttons.every(box => box.left >= bounds.left && box.right <= bounds.right + 0.5)
      && buttons.every((box, index) => index === 0 || box.left >= buttons[index - 1].right - 0.5);
  }), true);
  await page.locator('[data-action="new-rule"]:visible').first().click();
  await capturePage(page, 'draft-mobile.png');
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await page.locator('#rule-cancel-x').click();
  report.mobileOverflow = false;

  await page.setViewportSize({ width: 1440, height: 1050 });
  await page.locator('[data-route="engine"]').click();
  await page.waitForFunction(() => document.querySelectorAll('#engine-template option').length === 11);
  assert.equal(await page.locator('#engine-template option').nth(1).getAttribute('value'), 'direct-passthrough');

  // A: the real isolated preview has no engine binary. Exercise the native
  // unavailable state before installing controlled API fixtures.
  await page.context().grantPermissions(['clipboard-read', 'clipboard-write'], { origin });
  await page.waitForFunction(() => document.getElementById('engine-available').textContent.includes('未安装'));
  await page.locator('#engine-install-help').waitFor({ state: 'visible' });
  assert.equal(await page.locator('#engine-install-help').isVisible(), true);
  await page.locator('.engine-install-manual summary').click();
  await page.locator('#engine-install-command').waitFor({ state: 'visible' });
  const installCommand = (await page.locator('#engine-install-command').textContent()).trim();
  assert.match(installCommand, /curl\s+-fsSL/);
  assert.match(installCommand, /install-sing-box\.sh/);
  assert.match(installCommand, /\|\s*(?:sudo\s+)?bash/);
  assert.equal(await page.locator('#engine-enabled').isDisabled(), true);
  assert.equal(await page.locator('#engine-save').isDisabled(), true);
  await page.locator('#engine-template').selectOption('direct-passthrough');
  assert.equal(await page.locator('#engine-apply-template').isDisabled(), false);
  const nativeRecheck = page.waitForRequest(request =>
    request.method() === 'GET' && new URL(request.url()).pathname === '/api/engine',
  );
  await page.locator('#engine-recheck').click();
  await nativeRecheck;
  await page.waitForFunction(() => document.getElementById('engine-available').textContent.includes('未安装'));
  await page.locator('#engine-copy-install').click();
  await page.waitForFunction(async expected => {
    try {
      return (await navigator.clipboard.readText()).trim() === expected;
    } catch {
      return false;
    }
  }, installCommand);
  report.engineMissingKernelGuide = 'passed';

  let controlledEngine = enginePayload();
  let nextEngineGet;
  let nextEnginePut;
  let nextEngineInstall;
  const fulfillEngine = (route, payload) => route.fulfill({
    status: 200,
    contentType: 'application/json',
    body: JSON.stringify(payload),
  });
  const fulfillInstallation = (route, installation) => route.fulfill({
    status: 202,
    contentType: 'application/json',
    body: JSON.stringify(installation),
  });
  await page.route('**/api/engine', async route => {
    const request = route.request();
    if (request.method() === 'GET') {
      if (nextEngineGet) {
        const handler = nextEngineGet;
        nextEngineGet = undefined;
        await handler(route);
        return;
      }
      await fulfillEngine(route, controlledEngine);
      return;
    }
    if (request.method() === 'PUT') {
      const body = request.postDataJSON();
      if (nextEnginePut) {
        const handler = nextEnginePut;
        nextEnginePut = undefined;
        await handler(route, body);
        return;
      }
      controlledEngine = enginePayload({
        enabled: Boolean(body.enabled),
        running: Boolean(body.enabled),
        config: body.config,
      });
      await fulfillEngine(route, controlledEngine);
      return;
    }
    await route.continue();
  });
  await page.route('**/api/engine/install', async route => {
    if (route.request().method() !== 'POST') return route.continue();
    const body = route.request().postDataJSON();
    if (nextEngineInstall) {
      const handler = nextEngineInstall;
      nextEngineInstall = undefined;
      await handler(route, body);
      return;
    }
    await fulfillInstallation(route, installationPayload());
  });

  async function refreshControlledEngine(expectedStatus) {
    const response = page.waitForResponse(candidate =>
      candidate.request().method() === 'GET' && new URL(candidate.url()).pathname === '/api/engine',
    );
    await page.locator('#engine-recheck:visible, #engine-refresh:visible').first().click();
    await response;
    await settleFrames(page);
    if (expectedStatus) {
      await page.waitForFunction(
        expected => document.getElementById('engine-available').textContent.includes(expected),
        expectedStatus,
      );
    }
  }

  // One-click install: the first mocked POST fails and must become retryable.
  // The retry returns 202 in checking state, then the next GET publishes the
  // completed installation without downloading a real binary.
  controlledEngine = enginePayload({
    available: false,
    version: '',
    config: {},
    installation: installationPayload(),
  });
  await refreshControlledEngine('未安装');
  const installDraft = JSON.stringify({
    installDraft: { preserve: true },
    log: { level: 'debug' },
  }, null, 2);
  await page.locator('#engine-config').fill(installDraft);
  assert.equal(await page.locator('#engine-save').isDisabled(), true);
  await page.setViewportSize({ width: 390, height: 844 });
  await delay(300);
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), false);
  await capturePage(page, 'engine-install-mobile.png');
  report.engineMobileOverflow = false;
  await page.setViewportSize({ width: 1440, height: 1050 });
  await delay(300);
  await capturePage(page, 'engine-install-desktop.png');
  const installButton = page.getByRole('button', { name: /安装|重试/ }).first();
  await installButton.waitFor({ state: 'visible' });

  const firstInstallStarted = deferred();
  nextEngineInstall = async (route, body) => {
    firstInstallStarted.resolve(body);
    const failed = installationPayload({
      phase: 'failed',
      running: false,
      error: 'fixture install failure',
    });
    controlledEngine = enginePayload({
      available: false,
      version: '',
      config: {},
      installation: failed,
    });
    await fulfillInstallation(route, failed);
  };
  await installButton.click();
  assert.deepEqual(await firstInstallStarted.promise, {});
  await page.getByText('fixture install failure', { exact: false }).first().waitFor({ state: 'visible' });
  await installButton.waitFor({ state: 'visible' });
  assert.equal(await installButton.isDisabled(), false);

  const secondInstallStarted = deferred();
  const releaseSecondInstall = deferred();
  nextEngineInstall = async (route, body) => {
    secondInstallStarted.resolve(body);
    await releaseSecondInstall.promise;
    const total = 32 * 1024 * 1024;
    controlledEngine = enginePayload({
      available: true,
      version: 'sing-box version 1.14.4',
      config: {},
      installation: installationPayload({
        phase: 'complete',
        running: false,
        downloaded_bytes: total,
        total_bytes: total,
      }),
    });
    await fulfillInstallation(route, installationPayload({
      phase: 'checking',
      running: true,
      downloaded_bytes: total,
      total_bytes: total,
    }));
  };
  await installButton.click();
  assert.deepEqual(await secondInstallStarted.promise, {});
  report.engineInstallFailureRetry = 'passed';
  releaseSecondInstall.resolve();
  await page.waitForFunction(() => document.getElementById('engine-available').textContent.includes('已安装'));
  await settleFrames(page);
  assert.equal(await page.locator('#engine-config').inputValue(), installDraft);
  assert.equal(await page.locator('#engine-enabled').isDisabled(), false);
  assert.equal(await page.locator('#engine-save').isDisabled(), false);
  assert.equal(await page.locator('#engine-template').isDisabled(), false);
  report.engineInstallCompletion = 'passed';

  // Preserve the existing template application coverage under the newly
  // available state. A already asserted the real missing-engine contract.
  controlledEngine = enginePayload();
  await refreshControlledEngine('已安装');
  await page.locator('#engine-template').selectOption('direct-passthrough');
  await page.locator('#engine-apply-template').click();
  await page.locator('#confirm-dialog').waitFor({ state: 'visible' });
  await page.locator('#confirm-action').click();
  const generic = JSON.parse(await page.locator('#engine-config').inputValue());
  assert.equal(generic.outbounds[0].type, 'direct');
  assert.equal(generic.inbounds[0].network, undefined);
  assert.match(await page.locator('.template-guidance').textContent(), /无需协议内核/);
  report.templates = 10;

  // B: a manual recheck moves the fixture from unavailable to available. The
  // editor draft must survive and save/enable controls must unlock.
  controlledEngine = enginePayload({ available: false, version: '', config: {} });
  await refreshControlledEngine('未安装');
  const bDraft = JSON.stringify({ bDraft: { transport: 'both' }, log: { level: 'debug' } }, null, 2);
  await page.locator('#engine-config').fill(bDraft);
  assert.equal(await page.locator('#engine-enabled').isDisabled(), true);
  assert.equal(await page.locator('#engine-save').isDisabled(), true);
  controlledEngine = enginePayload({ available: true, config: {} });
  await refreshControlledEngine('已安装');
  assert.equal(await page.locator('#engine-config').inputValue(), bDraft);
  assert.equal(await page.locator('#engine-enabled').isDisabled(), false);
  assert.equal(await page.locator('#engine-save').isDisabled(), false);
  assert.equal(await page.locator('#engine-recheck').isDisabled(), false);
  report.engineAvailableRecheck = 'passed';

  // C: repeated poll-shaped enabled=false responses cannot reset a dirty draft
  // or the user's unsaved enable choice.
  controlledEngine = enginePayload({ enabled: false, available: true });
  await refreshControlledEngine('已安装');
  const cDraft = JSON.stringify({ cDraft: { route: 'direct' }, log: { level: 'warn' } }, null, 2);
  await page.locator('#engine-config').fill(cDraft);
  await checkEngineEnabled(page);
  for (let attempt = 0; attempt < 2; attempt++) {
    await refreshControlledEngine('已安装');
    assert.equal(await page.locator('#engine-config').inputValue(), cDraft);
    assert.equal(await page.locator('#engine-enabled').isChecked(), true);
  }
  assert.match(await page.locator('#engine-save').textContent(), /保存并启用/);
  report.engineRefreshPreservesDraft = 'passed';

  // D: capture the exact save payload, hold the PUT open to observe disabled
  // controls, then prove the running response is rendered by the UI.
  const dDraft = JSON.stringify({ dDraft: { mode: 'udp' }, log: { level: 'debug' } }, null, 2);
  await page.locator('#engine-config').fill(dDraft);
  await checkEngineEnabled(page);
  const saveStarted = deferred();
  const releaseSave = deferred();
  const saveFinished = deferred();
  nextEnginePut = async (route, body) => {
    saveStarted.resolve(body);
    await releaseSave.promise;
    controlledEngine = enginePayload({
      enabled: true,
      running: true,
      config: body.config,
    });
    await fulfillEngine(route, controlledEngine);
    saveFinished.resolve();
  };
  await page.locator('#engine-save').click();
  const saveBody = await saveStarted.promise;
  assert.equal(saveBody.enabled, true);
  assert.deepEqual(saveBody.config, JSON.parse(dDraft));
  assert.equal(await page.locator('#engine-save').isDisabled(), true);
  assert.equal(await page.locator('#engine-enabled').isDisabled(), true);
  assert.equal(await page.locator('#engine-config').isDisabled(), true);
  assert.equal(await page.locator('#engine-apply-template').isDisabled(), true);
  assert.equal(await page.locator('#engine-template').isDisabled(), true);
  releaseSave.resolve();
  await saveFinished.promise;
  await page.waitForFunction(() => document.getElementById('engine-running').textContent.includes('运行中'));
  assert.equal(await page.locator('#engine-enabled').isChecked(), true);
  report.engineSaveRequestAndRunning = 'passed';

  // E: save while an older GET is still pending, then release enabled=false.
  // The completed PUT must win and the stale GET must not roll the UI back.
  const eSaved = { eSaved: { mode: 'running' }, log: { level: 'info' } };
  controlledEngine = enginePayload({ enabled: true, running: true, config: eSaved });
  await refreshControlledEngine('已安装');
  await page.waitForFunction(() => document.getElementById('engine-running').textContent.includes('运行中'));
  const eDraft = JSON.stringify({ eDraft: { mode: 'latest' }, log: { level: 'error' } }, null, 2);
  await page.locator('#engine-config').fill(eDraft);
  await checkEngineEnabled(page);
  const staleStarted = deferred();
  const releaseStale = deferred();
  const staleFinished = deferred();
  nextEngineGet = async route => {
    staleStarted.resolve();
    await releaseStale.promise;
    await fulfillEngine(route, enginePayload({
      enabled: false,
      running: false,
      config: eSaved,
    }));
    staleFinished.resolve();
  };
  await page.locator('#engine-recheck:visible, #engine-refresh:visible').first().click();
  await staleStarted.promise;
  const staleSaveFinished = deferred();
  nextEnginePut = async (route, body) => {
    controlledEngine = enginePayload({
      enabled: true,
      running: true,
      config: body.config,
    });
    await fulfillEngine(route, controlledEngine);
    staleSaveFinished.resolve(body);
  };
  await page.locator('#engine-save').click();
  const staleSaveBody = await staleSaveFinished.promise;
  assert.equal(staleSaveBody.enabled, true);
  await page.waitForFunction(() => document.getElementById('engine-running').textContent.includes('运行中'));
  releaseStale.resolve();
  await staleFinished.promise;
  await settleFrames(page);
  assert.equal(await page.locator('#engine-config').inputValue(), eDraft);
  assert.equal(await page.locator('#engine-enabled').isChecked(), true);
  assert.doesNotMatch(await page.locator('#engine-running').textContent(), /已停用/);
  report.engineStaleGetOrdering = 'passed';

  // F: stopping must use the last saved backend config, not the unsaved editor
  // draft, while the unsaved JSON remains in the form.
  const fSaved = { fSaved: { mode: 'saved' }, log: { level: 'info' } };
  controlledEngine = enginePayload({ enabled: true, running: true, config: fSaved });
  await refreshControlledEngine('已安装');
  await page.waitForFunction(() => document.getElementById('engine-running').textContent.includes('运行中'));
  const fDraft = JSON.stringify({ fDraft: { mode: 'unsaved' }, log: { level: 'error' } }, null, 2);
  await page.locator('#engine-config').fill(fDraft);
  await page.locator('#engine-dirty').waitFor({ state: 'visible' });
  const stopStarted = deferred();
  const stopFinished = deferred();
  nextEnginePut = async (route, body) => {
    stopStarted.resolve(body);
    controlledEngine = enginePayload({
      enabled: false,
      running: false,
      config: body.config,
    });
    await fulfillEngine(route, controlledEngine);
    stopFinished.resolve();
  };
  await page.locator('#engine-stop').click();
  await page.locator('#confirm-dialog').waitFor({ state: 'visible' });
  await page.locator('#confirm-action').click();
  const stopBody = await stopStarted.promise;
  assert.equal(stopBody.enabled, false);
  assert.deepEqual(stopBody.config, fSaved);
  await stopFinished.promise;
  await page.waitForFunction(() => document.getElementById('engine-running').textContent.includes('已停用'));
  assert.equal(await page.locator('#engine-config').inputValue(), fDraft);
  assert.equal(await page.locator('#engine-enabled').isChecked(), false);
  await page.locator('#engine-dirty').waitFor({ state: 'visible' });
  report.engineStopUsesSavedConfig = 'passed';

  await page.locator('[data-route="rules"]').click();
  await page.unroute('**/api/engine/install');
  await page.unroute('**/api/engine');

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
  assert.equal(consoleMessages.some(message => message.includes(privateKeyMarker)), false);
  report.privateKeyConsoleLeak = false;
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
