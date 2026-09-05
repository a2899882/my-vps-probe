const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { compile } = require('@vue/compiler-dom');
const root = path.resolve(__dirname, '../..');

function loadUI(file, fetch) {
  const html = fs.readFileSync(path.join(root, 'server', file), 'utf8');
  const scripts = [...html.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/g)].map(m => m[1]).filter(s => s.trim());
  let api;
  const storage = { getItem() { return null; }, setItem() {}, removeItem() {} };
  const sandbox = {
    Vue: { ref: value => ({ value }), reactive: x => x, computed: f => ({ get value() { return f(); } }), watch() {}, onMounted() {}, onUnmounted() {}, nextTick: () => Promise.resolve(), createApp: options => ({ mount() { api = options.setup(); } }) },
    localStorage: storage, sessionStorage: storage, window: {},
    document: { getElementById: () => null, documentElement: { dataset: {} } },
    matchMedia: () => ({ matches: false }), setTimeout() {}, clearTimeout() {}, setInterval() {}, clearInterval() {},
    fetch, TextEncoder, TextDecoder, Uint8Array, btoa, atob, console,
    crypto: require('node:crypto').webcrypto,
    location: { origin: 'https://synthetic.example' }, confirm: () => true
  };
  vm.createContext(sandbox);
  for (const code of scripts) vm.runInContext(code, sandbox);
  return api;
}

for (const file of ['admin.html', 'index.html']) {
  test(`${file} Vue template and scripts compile`, () => {
    const html = fs.readFileSync(path.join(root, 'server', file), 'utf8');
    const template = html.match(/<body[^>]*>([\s\S]*?)<script>/)[1];
    const errors = [];
    const { code } = compile(template, { onError: e => errors.push(e) });
    assert.deepEqual(errors, []);
    new Function('Vue', code);
    loadUI(file, async () => { throw Error('unexpected request'); });
  });
}

test('notification configuration round-trip preserves false switches and zero duration', async () => {
  let saved;
  let config = { site_name: 'Test', admin_user: 'admin', nodes: [], ping_tasks: [], telegram: { enabled: false } };
  const api = loadUI('admin.html', async (url, options = {}) => {
    if (url === '/api/admin/config') {
      if (options.method === 'POST') { saved = JSON.parse(options.body); config = saved; }
      return { ok: true, json: async () => JSON.parse(JSON.stringify(config)) };
    }
    assert.equal(url, '/api/admin/runtime');
    return { ok: true, json: async () => ({ nodes: {} }) };
  });
  api.loginUser.value = 'admin'; api.loginPass.value = 'synthetic-password';
  await api.doLogin();
  assert.equal(api.authed.value, true);
  assert.equal(api.cfg.telegram.resources.cpu.enabled, false);
  api.cfg.telegram.notify_offline = false;
  api.cfg.telegram.notify_recovery = false;
  api.cfg.telegram.resources.cpu = { enabled: true, threshold: 97, duration_seconds: 0 };
  api.cfg.telegram.repeat_minutes = 0;
  api.cfg.telegram.excluded_node_ids = ['demo'];
  await api.saveConfig();
  assert.equal(saved.telegram.notify_offline, false);
  assert.equal(saved.telegram.notify_recovery, false);
  assert.equal(saved.telegram.resources.cpu.threshold, 97);
  assert.equal(api.cfg.telegram.resources.cpu.duration_seconds, 0);
  assert.equal(api.cfg.telegram.repeat_minutes, 0);
  assert.deepEqual([...api.cfg.telegram.excluded_node_ids], ['demo']);
});

test('Ping display distinguishes missing data, timeout, partial failure, and zero ms', () => {
  const api = loadUI('index.html');
  const p = { history_60: [null, -1, 0, 15], history_loss_60: [null, 100, 0, 50], history_start: 1788580800, sample_minutes: 3, has_current: true, current_delay: 0 };
  assert.equal(api.barClass(null, p, 0), 'missing');
  assert.equal(api.barClass(-1, p, 1), 'r');
  assert.equal(api.barClass(0, p, 2), 'g');
  assert.equal(api.barClass(15, p, 3), 'y');
  assert.equal(api.pingValue(p), '0 ms');
  assert.match(api.barTitle(null, p, 0), /未收到探测上报/);
  assert.equal(api.normalizedBars(p).length, 60);
  assert.equal(api.lossText({ sample_minutes: 0 }), '--');
});
