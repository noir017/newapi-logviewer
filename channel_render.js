// Render check for the channel column against the live deployment.
//   node channel_render.js 9223 http://192.168.0.185:7071/logviewer/
// Confirms the chip is actually laid out in the DOM - not merely present in the
// source - and reports what each rung of the fallback produced on real rows.
const WebSocket = require('ws');
const [, , port, url] = process.argv;

(async () => {
  const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
  const page = targets.find(t => t.type === 'page');
  const ws = new WebSocket(page.webSocketDebuggerUrl, { perMessageDeflate: false });
  let id = 0;
  const pending = new Map();
  ws.on('message', m => {
    const d = JSON.parse(m);
    if (pending.has(d.id)) { pending.get(d.id)(d.result); pending.delete(d.id); }
  });
  const send = (method, params = {}) => new Promise(res => {
    const i = ++id;
    pending.set(i, res);
    ws.send(JSON.stringify({ id: i, method, params }));
  });
  await new Promise(r => ws.on('open', r));

  await send('Page.enable');
  await send('Runtime.enable');
  await send('Page.navigate', { url });
  await new Promise(r => setTimeout(r, 5000));

  const expr = `(() => {
    const chips = [...document.querySelectorAll('.item .tag.ch')];
    const rows = [...document.querySelectorAll('.item')];
    return JSON.stringify({
      rows: rows.length,
      chips: chips.length,
      // laid out, not just in the DOM
      visible: chips.filter(c => c.getBoundingClientRect().width > 0).length,
      clipped: chips.filter(c => c.scrollWidth > c.clientWidth + 1).length,
      samples: chips.slice(0, 8).map(c => ({
        text: c.textContent,
        title: c.getAttribute('title'),
        w: Math.round(c.getBoundingClientRect().width),
      })),
      // rows with no chip at all: calls that never reached a channel
      chipless: rows.filter(r => !r.querySelector('.tag.ch'))
        .slice(0, 3).map(r => r.querySelector('.i2')?.textContent.slice(0, 40)),
      // the tag row must stay on one line per row, or the list gets noisy
      rowHeights: [...new Set(rows.map(r => Math.round(r.getBoundingClientRect().height)))],
    });
  })()`;
  const r = await send('Runtime.evaluate', { expression: expr, returnByValue: true });
  console.log(JSON.stringify(JSON.parse(r.result.value), null, 2));
  ws.close();
  process.exit(0);
})();
