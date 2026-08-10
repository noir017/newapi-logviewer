// Behavioural test for the channel column's fallback chain, run against the
// real ui.html source rather than a copy of it.
//
//   node channel_test.js
//
// The chain - name, then upstream address, then bare id, then nothing - is the
// whole feature: New API logs only the id, and the address cannot stand in when
// many channels share a host. Each rung is pinned here.
const fs = require('fs');
const assert = require('assert');

const html = fs.readFileSync(__dirname + '/ui.html', 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];

function extract(name) {
  const re = new RegExp(`function ${name}\\(r\\)\\{[\\s\\S]*?\\n\\}`);
  const m = script.match(re);
  if (!m) throw new Error(`${name}() not found in ui.html`);
  return m[0];
}

const esc = s => String(s).replace(/[&<>"']/g, c =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const sandbox = new Function('esc',
  `${extract('chanChip')}\n${extract('chanLabel')}\nreturn {chanChip, chanLabel};`)(esc);
const { chanChip, chanLabel } = sandbox;

let failed = 0;
function check(what, fn) {
  try { fn(); console.log('  ✓ ' + what); }
  catch (e) { failed++; console.log('  ✗ ' + what + '\n      ' + e.message); }
}

console.log('chanChip — list row');

check('prefers the resolved name over the address', () => {
  const h = chanChip({ channel_id: 9, channel: 'nvidia-nim-3682', upstream: 'integrate.api.nvidia.com' });
  assert.ok(h.includes('>nvidia-nim-3682<'), h);
  assert.ok(!h.includes('>integrate.api.nvidia.com<'), h);
});

check('falls back to the upstream host with no name', () => {
  const h = chanChip({ channel_id: 9, upstream: 'integrate.api.nvidia.com' });
  assert.ok(h.includes('>integrate.api.nvidia.com<'), h);
});

check('falls back to the bare id with neither', () => {
  assert.ok(chanChip({ channel_id: 9 }).includes('>#9<'));
});

// A request rejected by the distributor ("No available channel for model X")
// never reached a channel. Labelling it would be inventing data.
check('renders nothing when no channel was ever chosen', () => {
  assert.strictEqual(chanChip({ channel_id: null, model: '' }), '');
  assert.strictEqual(chanChip({}), '');
});

check('channel id 0 is a channel, not a missing one', () => {
  assert.ok(chanChip({ channel_id: 0 }).includes('>#0<'));
});

check('title carries id, name and address together', () => {
  const h = chanChip({ channel_id: 9, channel: 'nvidia-nim-3682', upstream: 'integrate.api.nvidia.com' });
  const title = h.match(/title="([^"]*)"/)[1];
  assert.ok(title.includes('#9') && title.includes('nvidia-nim-3682')
    && title.includes('integrate.api.nvidia.com'), title);
});

check('escapes a hostile channel name', () => {
  const h = chanChip({ channel_id: 1, channel: '<img src=x onerror=alert(1)>' });
  assert.ok(!h.includes('<img'), h);
  assert.ok(h.includes('&lt;img'), h);
});

console.log('chanLabel — detail pane');

check('shows the id even when the name resolves', () => {
  assert.strictEqual(chanLabel({ channel_id: 6, channel_name: 'modelscope' }), '#6 modelscope');
});

check('shows the id alone when the name does not', () => {
  assert.strictEqual(chanLabel({ channel_id: 6 }), '#6');
});

check('shows a dash when there is no channel', () => {
  assert.strictEqual(chanLabel({ channel_id: null }), '—');
});

console.log(failed ? `\n${failed} failed` : '\nall passed');
process.exit(failed ? 1 : 0);
