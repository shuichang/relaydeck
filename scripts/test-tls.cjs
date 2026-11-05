'use strict';

// Exercise the shipped serve command over real HTTPS. Certificate rotation must
// preserve the authenticated session and an already-established forwarding
// connection, and a selected certificate must survive a process restart.
const assert = require('assert/strict');
const cp = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const https = require('https');
const net = require('net');
const os = require('os');
const path = require('path');
const { once } = require('events');

const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
let child;
let childExit;
let childLog = '';
let echo;
let forwarded;
let tempRoot;
let activeAgent;
let cookie = '';

async function freePort() {
  const listener = net.createServer();
  listener.listen(0, '127.0.0.1');
  await once(listener, 'listening');
  const port = listener.address().port;
  await new Promise(resolve => listener.close(resolve));
  return port;
}

async function stopChild() {
  if (!child || child.exitCode !== null || child.signalCode !== null) return;
  child.kill();
  await Promise.race([childExit, delay(3000)]);
  if (child.exitCode === null && child.signalCode === null) {
    child.kill('SIGKILL');
    await childExit;
  }
}

function command(binary, args, input) {
  const result = cp.spawnSync(binary, args, {
    encoding: 'utf8', input, windowsHide: true, timeout: 30000,
  });
  if (result.error) throw result.error;
  if (result.status !== 0) throw new Error(`Command ${args[0]} failed: ${result.stderr.slice(-500)}`);
}

function request(origin, method, route, body) {
  return new Promise((resolve, reject) => {
    const raw = body === undefined ? undefined : JSON.stringify(body);
    const req = https.request(origin + route, {
      method, agent: activeAgent, timeout: 3000,
      headers: {
        'Content-Type': 'application/json', 'X-RelayDeck-CSRF': '1', Origin: origin,
        ...(cookie ? { Cookie: cookie } : {}),
      },
    }, res => {
      let text = '';
      res.on('data', data => text += data);
      res.on('end', () => {
        if (res.headers['set-cookie']) cookie = res.headers['set-cookie'][0].split(';')[0];
        let json;
        try { json = JSON.parse(text); } catch { return reject(new Error(`Invalid JSON: ${route}`)); }
        resolve({ status: res.statusCode, json, raw: text });
      });
    });
    req.on('error', reject);
    req.on('timeout', () => req.destroy(new Error(`HTTPS timeout: ${route}`)));
    req.end(raw);
  });
}

async function start(binary, dataDir, certFile, keyFile, port, origin) {
  childLog = '';
  child = cp.spawn(binary, [
    'serve', '--data', dataDir, '--listen', `127.0.0.1:${port}`,
    '--tls-cert', certFile, '--tls-key', keyFile,
    '--engine-binary', path.join(tempRoot, 'missing-engine'),
  ], { stdio: ['ignore', 'pipe', 'pipe'], windowsHide: true });
  childExit = once(child, 'exit');
  child.stdout.on('data', data => childLog += data);
  child.stderr.on('data', data => childLog += data);
  for (let attempt = 0; attempt < 80; attempt++) {
    if (child.exitCode !== null) throw new Error(`Serve exited early: ${childLog.slice(-800)}`);
    try {
      if ((await request(origin, 'GET', '/healthz')).status === 200) return;
    } catch {}
    await delay(100);
  }
  throw new Error(`HTTPS server did not become ready: ${childLog.slice(-800)}`);
}

function echoRequest(socket, payload) {
  return new Promise((resolve, reject) => {
    const expected = Buffer.from(payload);
    let received = Buffer.alloc(0);
    const timer = setTimeout(() => finish(new Error('Established forwarding connection stopped responding')), 3000);
    function finish(error) {
      clearTimeout(timer);
      socket.off('data', onData);
      socket.off('error', onError);
      socket.off('close', onClose);
      if (error) reject(error); else resolve();
    }
    function onData(chunk) {
      received = Buffer.concat([received, chunk]);
      if (received.length >= expected.length) {
        try { assert(received.equals(expected)); finish(); } catch (error) { finish(error); }
      }
    }
    const onError = error => finish(error);
    const onClose = () => finish(new Error('Certificate update closed a forwarding connection'));
    socket.on('data', onData).once('error', onError).once('close', onClose);
    socket.write(expected);
  });
}

function removeOwnedTemp() {
  if (!tempRoot) return;
  const resolved = fs.realpathSync.native(tempRoot);
  const parent = fs.realpathSync.native(os.tmpdir());
  const same = value => process.platform === 'win32' ? value.toLowerCase() : value;
  assert(same(path.dirname(resolved)) === same(parent));
  assert(path.basename(resolved).startsWith('relaydeck-tls-'));
  fs.rmSync(resolved, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
}

(async () => {
  const binary = process.env.RELAYDECK_BINARY;
  assert(binary && path.isAbsolute(binary), 'RELAYDECK_BINARY must be an absolute binary path');
  assert(fs.statSync(binary).isFile());
  tempRoot = fs.mkdtempSync(path.join(os.tmpdir(), 'relaydeck-tls-'));
  const dataDir = path.join(tempRoot, 'data');
  const certFile = path.join(tempRoot, 'default.crt');
  const keyFile = path.join(tempRoot, 'default.key');
  const newCertFile = path.join(tempRoot, 'new.crt');
  const newKeyFile = path.join(tempRoot, 'new.key');
  const password = crypto.randomBytes(24).toString('base64url');
  command(binary, ['init', '--data', dataDir, '--username', 'admin', '--password-stdin'], password + '\n');
  for (const [cert, key] of [[certFile, keyFile], [newCertFile, newKeyFile]]) {
    command(binary, ['certificate', '--cert', cert, '--key', key, '--hosts', 'localhost,127.0.0.1,::1']);
  }
  const cert = fs.readFileSync(certFile);
  const nextCert = fs.readFileSync(newCertFile);
  const nextKey = fs.readFileSync(newKeyFile, 'utf8');
  activeAgent = new https.Agent({ ca: cert, keepAlive: false });
  const port = await freePort();
  const origin = `https://127.0.0.1:${port}`;
  await start(binary, dataDir, certFile, keyFile, port, origin);
  assert.equal((await request(origin, 'POST', '/api/login', { username: 'admin', password })).status, 200);
  const initial = await request(origin, 'GET', '/api/settings/tls');
  assert.equal(initial.status, 200);
  assert.equal(initial.json.available, true);
  assert.equal(initial.json.mode, 'default');

  echo = net.createServer(socket => {
    socket.on('error', () => {});
    socket.pipe(socket);
  });
  echo.listen(0, '127.0.0.1');
  await once(echo, 'listening');
  const forwardPort = await freePort();
  const rules = await request(origin, 'PUT', '/api/rules', { rules: [{
    id: 'tls-rotation-forward', name: 'TLS rotation live connection', enabled: true,
    network: 'tcp', listen: `127.0.0.1:${forwardPort}`,
    target: `127.0.0.1:${echo.address().port}`, max_connections: 8, udp_timeout: 30,
  }] });
  assert.equal(rules.status, 200);
  forwarded = net.connect(forwardPort, '127.0.0.1');
  await once(forwarded, 'connect');
  await echoRequest(forwarded, 'before-certificate-rotation');
  const pid = child.pid;
  const updated = await request(origin, 'PUT', '/api/settings/tls', {
    mode: 'pem', certificate_pem: nextCert.toString(), private_key_pem: nextKey,
  });
  assert.equal(updated.status, 200);
  assert.notEqual(updated.json.certificate.fingerprint_sha256, initial.json.certificate.fingerprint_sha256);
  assert(!updated.raw.includes('PRIVATE KEY'));
  assert(!updated.raw.includes('private_key_pem'));
  assert.equal(child.pid, pid);
  await echoRequest(forwarded, 'after-certificate-rotation');
  activeAgent.destroy();
  activeAgent = new https.Agent({ ca: nextCert, keepAlive: false });
  assert.equal((await request(origin, 'GET', '/api/status')).status, 200, 'rotation lost the session');
  const fingerprint = updated.json.certificate.fingerprint_sha256;
  const invalid = await request(origin, 'PUT', '/api/settings/tls', {
    mode: 'pem', certificate_pem: nextCert.toString(), private_key_pem: 'bad private key',
  });
  assert.equal(invalid.status, 400);
  const unchanged = await request(origin, 'GET', '/api/settings/tls');
  assert.equal(unchanged.json.certificate.fingerprint_sha256, fingerprint);
  const exported = await request(origin, 'GET', '/api/config/export');
  assert(!exported.raw.includes('private_key_pem') && !exported.raw.includes('PRIVATE KEY'));
  forwarded.destroy();
  forwarded = undefined;
  await stopChild();
  fs.unlinkSync(certFile);
  fs.unlinkSync(keyFile);
  cookie = '';
  await start(binary, dataDir, certFile, keyFile, port, origin);
  assert.equal((await request(origin, 'POST', '/api/login', { username: 'admin', password })).status, 200);
  const restarted = await request(origin, 'GET', '/api/settings/tls');
  assert.equal(restarted.json.certificate.fingerprint_sha256, fingerprint);
  assert.equal(restarted.json.mode, 'pem');
  console.log('PASS: real HTTPS hot rotation, unchanged process/session/forwarding connection, invalid update rollback, private export isolation, and restart without default files');
})().catch(error => {
  console.error(error.message);
  process.exitCode = 1;
}).finally(async () => {
  if (forwarded) forwarded.destroy();
  if (activeAgent) activeAgent.destroy();
  await stopChild();
  if (echo) await new Promise(resolve => echo.close(resolve));
  removeOwnedTemp();
});
