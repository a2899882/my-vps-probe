const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const { compile } = require('@vue/compiler-dom');
const root = path.resolve(__dirname, '../..');

function loadUI(file, fetch, options = {}) {
  const html = fs.readFileSync(path.join(root, 'server', file), 'utf8');
  const scripts = [...html.matchAll(/<script(?:\s[^>]*)?>([\s\S]*?)<\/script>/g)].map(m => m[1]).filter(s => s.trim());
  let api;
  const storage = { getItem() { return null; }, setItem() {}, removeItem() {} };
  const sandbox = {
    Vue: { shallowRef: value => ({ value }), ref: value => ({ value }), reactive: x => x, computed: f => ({ get value() { return f(); } }), watch() {}, onMounted() {}, onUnmounted() {}, nextTick: () => Promise.resolve(), createApp: options => ({ mount() { api = options.setup(); } }) },
    localStorage: storage, sessionStorage: storage, window: {},
    document: { getElementById: () => null, documentElement: { dataset: {} } },
    matchMedia: () => ({ matches: false }), setTimeout() {}, clearTimeout() {}, setInterval() {}, clearInterval() {},
    fetch, TextEncoder, TextDecoder, Uint8Array, btoa, atob, console, AbortController,
    crypto: require('node:crypto').webcrypto,
    location: { origin: 'https://synthetic.example' }, confirm: () => true
  };
  Object.assign(sandbox, options.globals || {});
  Object.assign(sandbox.Vue, options.vue || {});
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

function trendUI(fetch) {
  const charts = [], elements = new Map();
  const api = loadUI('index.html', fetch, { globals: {
    requestAnimationFrame: callback => setTimeout(callback, 0), cancelAnimationFrame: clearTimeout,
    setTimeout: (callback, delay) => delay ? 0 : setTimeout(callback, 0),
    document: { body: { style: {} }, hidden: false, documentElement: { dataset: {} }, getElementById(id) { if (!elements.has(id)) elements.set(id, { id }); return elements.get(id); } },
    window: { echarts: { init(element) { const chart = { element, options: [], disposed: false, resize() {}, dispose() { this.disposed = true; }, setOption(option) { this.options.push(option); }, dispatchAction() {} }; charts.push(chart); return chart; } } }
  } });
  return { api, charts };
}

test('rapid trend switching ignores old results, caches ranges, and disposes on close', async () => {
  const pending = [];
  const { api, charts } = trendUI((url, options) => new Promise(resolve => pending.push({ url, signal: options.signal, resolve })));
  api.detailNode.value = { id: 'n' };
  const slow = api.loadTrend(1);
  const fast = api.loadTrend(4);
  assert.equal(pending.length, 2);
  assert.equal(pending[0].signal.aborted, true);
  const point = { ts: 1788580800, time: '2026-09-05 05:20:00', step_seconds: 60, cpu_usage: 4 };
  pending[1].resolve({ ok: true, json: async () => [point] });
  await fast;
  pending[0].resolve({ ok: true, json: async () => [{ ...point, cpu_usage: 99 }] });
  await slow;
  assert.equal(api.resourcePoints.value[0].cpu_usage, 4);
  assert.equal(charts.length, 6);
  await api.loadTrend(4);
  assert.equal(pending.length, 2, 'cached range refetched');
  assert.equal(charts.length, 6, 'canvas instances recreated');
  assert.equal(charts.every(c => c.options.length === 2), true);
  const closing = api.loadTrend(24);
  api.closeDetail();
  assert.equal(pending[2].signal.aborted, true);
  pending[2].resolve({ ok: true, json: async () => [point] });
  await closing;
  assert.equal(api.detailNode.value, null);
  assert.equal(charts.every(c => c.disposed), true);
  assert.equal(charts.length, 6, 'closed modal recreated charts');
});

test('Ping trend retains zero ms, partial loss and whole-node gaps', async () => {
  const { api, charts } = trendUI(async () => ({ ok: true, json: async () => [
    { ts: 1788580800, step_seconds: 60, target: 'TCP', delay: 0, loss: 25, samples: 1 },
    { ts: 1788580920, step_seconds: 60, target: 'TCP', delay: 10, loss: 0, samples: 1 }
  ] }));
  api.detailNode.value = { id: 'n' }; api.trendMode.value = 'ping';
  await api.loadTrend(.25);
  const option = charts[0].options[0];
  assert.deepEqual([...option.series[0].data], [0, null, 10], 'smoothing filled an outage or lost zero latency');
  assert.equal(api.pingLegend.value.TCP.loss, 12.5);
});

test('admin batch calibration uses saved billing revisions and preserves zero', async () => {
  let payload;
  const config = { site_name: 'Test', admin_user: 'admin', nodes: [{ id: 'n', name: 'N', token: 'test-token' }], ping_tasks: [] };
  const api = loadUI('admin.html', async (url, options = {}) => {
    if (url === '/api/admin/config') return { ok: true, json: async () => config };
    if (url === '/api/admin/runtime') return { ok: true, json: async () => ({ nodes: {} }) };
    assert.equal(url, '/api/admin/traffic');
    if (options.method === 'POST') { payload = JSON.parse(options.body); return { ok: true, json: async () => ({ updated: 1 }) }; }
    return { ok: true, json: async () => ({ nodes: { n: { used: 10737418240, cycle_key: '2026-09-01', revision: 4 } } }) };
  });
  api.loginUser.value = 'admin'; api.loginPass.value = 'test-password'; await api.doLogin();
  await api.openTraffic(api.cfg.nodes);
  assert.equal(api.trafficEditor.amount, '10', 'trailing zero in whole GB amount removed');
  api.trafficEditor.mode = 'zero';
  await api.saveTraffic();
  assert.equal(payload.adjustments[0].used_gb, '0');
  assert.equal(payload.adjustments[0].revision, 4);
  assert.equal(payload.adjustments[0].expected_cycle, '2026-09-01');
  assert.equal(api.trafficEditor.open, false);
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

test('80-node Vue rendering keeps clock, theme and detail updates out of card subtrees', async t => {
  // Vue's real renderer with an in-memory host tests VNode updates without a browser.
  const vue = require('vue');
  const api = loadUI('index.html', undefined, { vue: { ref: vue.ref, shallowRef: vue.shallowRef, computed: vue.computed } });
  const html = fs.readFileSync(path.join(root, 'server/index.html'), 'utf8');
  const { code } = compile(html.match(/<body[^>]*>([\s\S]*?)<script>/)[1], { prefixIdentifiers: true });
  const render = new Function('Vue', code)({ ...vue, vModelText: {}, vModelSelect: {} });
  const makeNode = (type, text = '') => ({ type, text, parent: null, children: [], props: {}, style: {} });
  let created = 0;
  const renderer = vue.createRenderer({
    createElement(type) { created++; return makeNode(type); },
    createText: text => makeNode('#text', text), createComment: text => makeNode('#comment', text),
    setText(node, text) { node.text = text; }, setElementText(node, text) { node.text = text; node.children = []; },
    parentNode: node => node.parent,
    nextSibling: node => node.parent?.children[node.parent.children.indexOf(node) + 1] || null,
    patchProp(node, key, previous, next) { node.props[key] = next; },
    insert(node, parent, anchor = null) { if (node.parent) node.parent.children.splice(node.parent.children.indexOf(node), 1); const index = anchor ? parent.children.indexOf(anchor) : parent.children.length; parent.children.splice(index, 0, node); node.parent = parent; },
    remove(node) { if (node.parent) node.parent.children.splice(node.parent.children.indexOf(node), 1); node.parent = null; }
  });
  api.servers.value = Array.from({ length: 80 }, (_, i) => ({ id: 'n'+i, name: 'Node '+i, expire_date: '2030/01/01|1000|1', traffic_limit_gb: 1000, status: { is_online: true, cpu_cores: 2, mem_total: 1073741824, disk_total: 10737418240, card_ping_statuses: Array.from({ length: 3 }, (_, j) => ({ target_name: 'TCP '+j, history_60: Array(60).fill(16), history_loss_60: Array(60).fill(0), history_start: 1788580800, sample_minutes: 60, has_current: true, current_delay: 16, current_at: 1788584400 })) } }));
  api.pageSize.value = 80;
  let stripsRendered = 0, titlesFormatted = 0;
  const originalStyle = api.barStyle, originalTitle = api.barTitle;
  api.barStyle = p => { stripsRendered++; return originalStyle(p); };
  api.barTitle = (...args) => { titlesFormatted++; return originalTitle(...args); };
  const container = makeNode('root');
  const app = renderer.createApp({ setup: () => api, render });
  const begin = performance.now(); app.mount(container); const initialMs = performance.now() - begin;
  const initialElements = created;
  assert.equal(stripsRendered, 240);
  assert.equal(titlesFormatted, 0, 'minute tooltip formatted during render');
  stripsRendered = 0;
  const updateStart = performance.now();
  for (let i = 0; i < 5; i++) { api.now.value = '12:00:0'+i; await vue.nextTick(); }
  const clockMs = (performance.now() - updateStart) / 5;
  api.theme.value = 'light'; await vue.nextTick();
  api.detailNode.value = api.servers.value[0]; await vue.nextTick();
  api.chartHours.value = 4; api.trendMode.value = 'ping'; await vue.nextTick();
  assert.equal(stripsRendered, 0, 'unrelated interaction rerendered the 80 cards');
  api.detailNode.value = null; api.pageSize.value = 20; await vue.nextTick();
  api.pageSize.value = 80; await vue.nextTick();
  assert.equal(stripsRendered, 180, 'changing page size rerendered retained cards');
  const all = []; const visit = node => { all.push(node); node.children.forEach(visit); }; visit(container);
  assert.equal(all.filter(n => n.props.class === 'bars').length, 240);
  assert.equal(all.filter(n => n.props.class === 'bar').length, 0);
  t.diagnostic(`80 nodes / 240 strips / ${initialElements} host elements; initial Vue render ${initialMs.toFixed(1)} ms; clock update ${clockMs.toFixed(2)} ms (in-memory host, not browser paint)`);
  app.unmount();
});
