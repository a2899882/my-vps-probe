// Exercises a real server with synthetic WebSocket agents. Telegram stays disabled.
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const cp = require('node:child_process');
const assert = require('node:assert/strict');
const root = path.resolve(__dirname, '../..');
const binary = path.resolve(process.argv[2] || '/tmp/probe-server');
const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'probe-smoke-'));
const siteName = 'Synthetic-' + Date.now();
const origin = 'http://127.0.0.1:8080';
const auth = { Authorization: 'Basic ' + Buffer.from('admin:synthetic-password').toString('base64') };
const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
let server, exited, logs = '', sockets = [];
fs.symlinkSync(path.join(root, 'server'), path.join(dir, 'server'));
fs.writeFileSync(path.join(dir, 'config.json'), JSON.stringify({
  site_name: siteName, admin_user: 'admin', admin_pass: 'synthetic-password',
  nodes: [{ id: 'n1', name: 'Synthetic node', token: 'synthetic-token', expire_date: '2030/01/01|100|1' }, { id: 'n2', name: 'New offline node', token: 'synthetic-token-2', expire_date: '2030/01/01|100|1' }],
  ping_tasks: [{ name: 'TCP', host: '127.0.0.1:80' }], telegram: { enabled: false },
  history_days: 7, public_refresh_seconds: 3, agent_report_seconds: 3
}));
async function read(url) {
  const response = await fetch(origin + url, { headers: auth, signal: AbortSignal.timeout(3000) });
  assert.equal(response.status, 200, await response.clone().text());
  return response.json();
}
async function post(url, body) {
  return fetch(origin + url, { method: 'POST', headers: { ...auth, 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}
async function waitFor(fn, message) {
  for (let i = 0; i < 200; i++) { if (await fn()) return; await delay(25); }
  throw Error(message);
}
async function start() {
  server = cp.spawn(binary, [], { cwd: dir, env: { ...process.env, TZ: 'Asia/Shanghai' } });
  exited = new Promise(resolve => server.once('exit', resolve));
  server.stdout.on('data', b => logs += b); server.stderr.on('data', b => logs += b);
  server.on('error', e => { logs += e.message; });
  await waitFor(async () => {
    if (server.exitCode != null) throw Error('server exited: ' + logs);
    let r; try { r = await fetch(origin + '/api/status', { signal: AbortSignal.timeout(500) }); } catch { return false; }
    if (!r.ok) return false;
    const status = await r.json(); assert.equal(status.site_name, siteName, 'port 8080 belongs to another service'); return true;
  }, 'server did not start');
}
async function stop() {
  for (const ws of sockets) ws.close(); sockets = [];
  if (server?.exitCode == null) {
    server.kill('SIGTERM');
    const timer = setTimeout(() => server.kill('SIGKILL'), 5000);
    const code = await exited; clearTimeout(timer); assert.equal(code, 0, logs);
  }
}
async function connect(token) {
  const ws = new WebSocket(origin.replace('http', 'ws') + '/ws?token=' + token); sockets.push(ws);
  await new Promise((resolve, reject) => { ws.addEventListener('message', resolve, { once: true }); ws.addEventListener('error', reject, { once: true }); });
  return ws;
}
async function calibrate(id, amount) {
  const usage = (await read('/api/admin/traffic')).nodes[id];
  const response = await post('/api/admin/traffic', { adjustments: [{ node_id: id, expected_cycle: usage.cycle_key, revision: usage.revision, used_gb: amount }] });
  assert.equal(response.status, 200, await response.text());
}
(async () => {
  try {
    await start();
    assert.equal((await fetch(origin + '/api/admin/traffic')).status, 401);
    const config = await read('/api/admin/config');
    config.telegram.resources.cpu = { enabled: true, threshold: 91, duration_seconds: 0 };
    config.telegram.notify_expiry = false;
    assert.equal((await post('/api/admin/config', config)).status, 200);
    const saved = await read('/api/admin/config');
    assert.equal(saved.telegram.notify_expiry, false); assert.equal(saved.telegram.resources.cpu.duration_seconds, 0);
    const ws = await connect('synthetic-token');
    const sample = { cpu_cores: 2, cpu_usage: 95, mem_total: 100, mem_used: 92, disk_total: 100, disk_used: 91, net_in_transfer: 100, net_out_transfer: 100, ping_statuses: [{ target_name: 'TCP', current_delay: 16, history: [16] }] };
    ws.send(JSON.stringify(sample));
    await waitFor(async () => (await read('/api/status')).nodes[0].status.card_ping_statuses[0].has_current, 'first report was not received');
    const ping = (await read('/api/status')).nodes[0].status.card_ping_statuses[0];
    assert.equal(ping.history_60[59], 16); assert.equal(ping.history_60[58], null);
    await Promise.all(Array.from({ length: 20 }, async (_, i) => { ws.send(JSON.stringify({ ...sample, cpu_usage: i })); await read('/api/status'); }));
    await calibrate('n1', '1.5');
    ws.send(JSON.stringify({ ...sample, net_in_transfer: 200, net_out_transfer: 300 }));
    const expected = 1610612736 + 300;
    await waitFor(async () => (await read('/api/status')).nodes[0].month_used === expected, 'online calibration did not continue from its baseline');
    // Saving ordinary config must not overwrite the independent correction.
    assert.equal((await post('/api/admin/config', saved)).status, 200);
    assert.equal((await read('/api/status')).nodes[0].month_used, expected);
    await calibrate('n2', '2');
    const ws2 = await connect('synthetic-token-2');
    ws2.send(JSON.stringify({ ...sample, net_in_transfer: 5000000000, net_out_transfer: 5000000000 }));
    await waitFor(async () => (await read('/api/status')).nodes[1].status.net_in_transfer === 5000000000, 'new agent report missing');
    assert.equal((await read('/api/status')).nodes[1].month_used, 2147483648, 'old counters imported after offline correction');
    assert.deepEqual((await read('/api/admin/notifications')).items, [], 'test attempted a Telegram delivery');
    await stop();
    const persisted = JSON.parse(fs.readFileSync(path.join(dir, 'usage_state.json')));
    assert.equal(persisted.n1.Used ?? persisted.n1.used, expected);
    // Seed archived points with the standard-library SQLite driver, preserving real schema.
    cp.execFileSync('python3', ['-c', `import sqlite3,sys
d=sqlite3.connect(sys.argv[1])
for i in range(1,61):
 d.execute("INSERT INTO resource_history(timestamp,server_id,cpu_usage,mem_used,mem_total,disk_used,disk_total,swap_used,swap_total,load_1,net_in_speed,net_out_speed,tcp_connections,udp_connections) VALUES(datetime('now',?), 'n1',99,20,100,30,100,0,0,0.2,100,200,10,1)", ('-%d minutes'%i,))
d.commit()
assert d.execute('SELECT COUNT(*) FROM ping_history').fetchone()[0]>=1`, path.join(dir, 'data.db')]);
    await start();
    const status = await read('/api/status');
    assert.equal(status.nodes[0].month_used, expected); assert.equal(status.nodes[0].status.is_online, false);
    const history = await read('/api/resource_history?server_id=n1&hours=0.25');
    assert.ok(history.length > 0 && history.length <= 15); assert.equal(history[0].cpu_usage, 99);
    await calibrate('n1', '0');
    assert.equal((await read('/api/status')).nodes[0].month_used, 0);
    await stop();
    assert.ok(!logs.includes('DATA RACE'), logs);
    console.log('PASS: real HTTP/WebSocket reports, auth, Ping minutes, online/offline calibration, config preservation, restart persistence, 15-minute history, graceful shutdown. No Telegram messages sent.');
  } finally {
    await stop();
    fs.writeFileSync(path.join(dir, 'test.log'), logs);
  }
})().catch(e => { console.error(e); console.error('Synthetic test files:', dir); process.exitCode = 1; });
