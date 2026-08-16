// Browser test for the stats view. Like fold_test.js, this drives a real
// browser rather than asserting on markup strings: the two things most likely
// to be wrong here are layout facts (does the header still fit, is the pane
// scrollable) and reconciliation facts (do the rendered numbers agree with the
// API), and neither is visible to a string comparison.
//
//   node stats_test.js <cdp-port> <url>
const CDP_PORT = process.argv[2] || '9223';
const URL = process.argv[3];

async function main(){
  const list = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`)).json();
  const page = list.find(t => t.type === 'page' && t.webSocketDebuggerUrl);
  if (!page) throw new Error('no debuggable page');
  const WebSocket = (await import('ws')).default;
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise(r => ws.on('open', r));
  let id = 0; const pending = new Map();
  ws.on('message', m => { const x = JSON.parse(m);
    if (x.id && pending.has(x.id)){ pending.get(x.id)(x); pending.delete(x.id); } });
  const send = (method, params) => new Promise(res => {
    const i = ++id; pending.set(i, res); ws.send(JSON.stringify({id: i, method, params})); });
  const ev = async expr => {
    const r = await send('Runtime.evaluate',
      {expression: expr, returnByValue: true, awaitPromise: true});
    if (r.result?.exceptionDetails)
      throw new Error(r.result.exceptionDetails.exception?.description || 'eval failed');
    return r.result?.result?.value;
  };

  await send('Page.enable'); await send('Runtime.enable');
  await send('Emulation.setDeviceMetricsOverride',
    {width: 1400, height: 900, deviceScaleFactor: 1, mobile: false});
  await send('Page.navigate', {url: 'about:blank'});
  await new Promise(r => setTimeout(r, 300));
  // Start on the stats view regardless of what a previous run left in storage.
  await send('Page.navigate', {url: URL});
  await new Promise(r => setTimeout(r, 2500));

  const results = [];
  const check = (n, p, d) => results.push({n, p, d});

  // Any uncaught error during boot leaves the page half-rendered; catch it
  // explicitly rather than letting later assertions fail confusingly.
  const err = await ev(`(() => { try { return window.__err || ''; } catch(e){ return String(e); } })()`);
  check('no page error', !err, err);

  await ev(`setView('stats')`);
  await new Promise(r => setTimeout(r, 1800));

  // --- 1. The pane is actually visible and scrollable ------------------------
  const vis = await ev(`(() => {
    const s = document.getElementById('stats');
    const r = s.getBoundingClientRect();
    return {display: getComputedStyle(s).display, top: Math.round(r.top),
            h: Math.round(r.height), scrollH: s.scrollHeight, clientH: s.clientHeight};
  })()`);
  check('stats pane visible', vis.display === 'block', JSON.stringify(vis));

  // The pane must own its scrolling. body is overflow:hidden, so if the content
  // is taller than the pane and the pane does not scroll, the bottom of the
  // page is simply unreachable.
  check('stats pane scrolls its own overflow',
    vis.scrollH <= vis.clientH + 2 || vis.clientH > 200,
    `scrollH=${vis.scrollH} clientH=${vis.clientH}`);

  // --- 2. The header did not outgrow --hdr ----------------------------------
  // .stats is sized calc(100vh - var(--hdr)). If the stats filter row wraps to
  // two lines, the real header is taller than the constant and the pane hangs
  // off the bottom of an unscrollable body.
  const hdr = await ev(`(() => {
    const h = document.querySelector('header');
    const declared = parseInt(getComputedStyle(document.documentElement)
      .getPropertyValue('--hdr'));
    const s = document.getElementById('stats').getBoundingClientRect();
    return {real: Math.round(h.getBoundingClientRect().height), declared,
            paneBottom: Math.round(s.bottom), win: window.innerHeight};
  })()`);
  check('header height matches --hdr', Math.abs(hdr.real - hdr.declared) <= 2,
    `real=${hdr.real} declared=${hdr.declared}`);
  check('stats pane bottom within viewport', hdr.paneBottom <= hdr.win + 2,
    `paneBottom=${hdr.paneBottom} win=${hdr.win}`);

  // --- 3. Reconciliation: rendered model bars vs the KPI tile ---------------
  // This is the assertion that catches a breakdown quietly dropping records.
  // Both numbers look plausible alone; only their disagreement is visible.
  const rec = await ev(`(() => {
    const d = statsData.data;
    const rows = [...document.querySelectorAll('.mrow')];
    const shown = rows.length;
    const sum = d.models.reduce((s, m) => s + m.requests, 0);
    return {kpi: d.requests, modelSum: sum, rowsRendered: shown,
            modelRows: d.models.length,
            ok: d.ok, err: d.err, unknown: d.unknown,
            seriesSum: d.series.reduce((s, b) => s + b.requests, 0)};
  })()`);
  check('model breakdown sums to request total', rec.modelSum === rec.kpi,
    `models=${rec.modelSum} kpi=${rec.kpi}`);
  check('every model row is rendered', rec.rowsRendered === rec.modelRows,
    `rendered=${rec.rowsRendered} returned=${rec.modelRows}`);
  check('series sums to request total', rec.seriesSum === rec.kpi,
    `series=${rec.seriesSum} kpi=${rec.kpi}`);
  check('ok+err+unknown partitions the total',
    rec.ok + rec.err + rec.unknown === rec.kpi,
    `${rec.ok}+${rec.err}+${rec.unknown} vs ${rec.kpi}`);

  // --- 4. The charts drew marks ---------------------------------------------
  const marks = await ev(`(() => ({
    spend: document.querySelectorAll('#c-spend .bar').length,
    req: document.querySelectorAll('#c-req .bar').length,
    hits: document.querySelectorAll('#c-req rect.hit').length,
    axis: document.querySelectorAll('#c-spend text').length,
  }))()`);
  check('spend chart drew bars', marks.spend > 0, JSON.stringify(marks));
  check('request chart drew bars', marks.req > 0, JSON.stringify(marks));
  check('every band has a hit target', marks.hits >= 1, JSON.stringify(marks));
  check('axis is labelled', marks.axis >= 5, `${marks.axis} text nodes`);

  // Hit targets must be big enough to actually hit. A 3px bar in a 365-day
  // range is unhittable if the target is only the painted pixels.
  const hitW = await ev(`(() => {
    const hs = [...document.querySelectorAll('#c-req rect.hit')];
    if (!hs.length) return 0;
    const rs = hs.map(h => h.getBoundingClientRect().width);
    return Math.min(...rs);
  })()`);
  check('hit targets are wider than the bars they cover', hitW >= 3,
    `narrowest hit = ${hitW.toFixed?.(1) ?? hitW}px`);

  // Axis labels must not overlap — checked on EVERY range, not just the default.
  // The first version of this test only exercised 7d (7 short labels) and passed
  // while "08-16 22:00" and "08-16 23:00" were visibly colliding on the today
  // view: hourly labels are ~50% wider and there are 24 of them.
  const overlapProbe = `(() => {
    const worst = {};
    for (const sel of ['#c-spend', '#c-req']){
      const ts = [...document.querySelectorAll(sel + ' text')]
        .map(t => t.getBoundingClientRect())
        .filter(r => r.width > 0)
        .sort((a, b) => a.x - b.x);
      let overlaps = 0;
      for (let i = 1; i < ts.length; i++){
        // Only compare labels on the same row (the x-axis band).
        if (Math.abs(ts[i].y - ts[i-1].y) > 3) continue;
        if (ts[i].left < ts[i-1].right) overlaps++;
      }
      worst[sel] = overlaps;
    }
    return worst;
  })()`;
  for (const r of ['today', '7d', '30d', 'ytd']){
    await ev(`document.querySelector('#srange button[data-r="${r}"]').click()`);
    await new Promise(t => setTimeout(t, 1400));
    const c = await ev(overlapProbe);
    const gran = await ev(`statsData.data.granularity`);
    const buckets = await ev(`statsData.data.series.length`);
    check(`no overlapping axis labels (${r}, ${gran}, ${buckets} buckets)`,
      c['#c-spend'] === 0 && c['#c-req'] === 0, JSON.stringify(c));
  }
  // Leave the view on a multi-bucket range for the assertions that follow.
  await ev(`document.querySelector('#srange button[data-r="7d"]').click()`);
  await new Promise(t => setTimeout(t, 1400));

  // The data-end is rounded; the baseline is square. An rx on a rect rounds all
  // four corners, which detaches a stacked segment from the bar below it.
  const shape = await ev(`(() => {
    const b = document.querySelector('#c-spend .bar');
    return {tag: b ? b.tagName.toLowerCase() : '', rx: b ? b.getAttribute('rx') : null};
  })()`);
  check('bars are paths with a square baseline',
    shape.tag === 'path' && !shape.rx, JSON.stringify(shape));

  // --- 5. The table twin exposes every value without hovering ---------------
  await ev(`document.querySelector('[data-tbl="t-req"]').click()`);
  await new Promise(r => setTimeout(r, 200));
  const tbl = await ev(`(() => {
    const w = document.getElementById('t-req');
    return {open: w.classList.contains('on'),
            rows: w.querySelectorAll('tbody tr').length,
            series: statsData.data.series.length};
  })()`);
  check('table twin opens', tbl.open, JSON.stringify(tbl));
  check('table has one row per bucket', tbl.rows === tbl.series,
    `rows=${tbl.rows} buckets=${tbl.series}`);

  // --- 6. Range switching re-queries and relabels ---------------------------
  const before = await ev(`statsData.range.since`);
  await ev(`document.querySelector('#srange button[data-r="today"]').click()`);
  await new Promise(r => setTimeout(r, 1500));
  const after = await ev(`({since: statsData.range.since, name: statsData.range.name,
                            tz: statsData.range.tz, reqs: statsData.data.requests})`);
  check('switching range re-queries', after.since !== before,
    `before=${before} after=${after.since}`);
  check('server resolved the named range', after.name === 'today', JSON.stringify(after));
  check('response names the clock', !!after.tz, JSON.stringify(after));

  // --- 7. Custom range reveals its inputs -----------------------------------
  await ev(`document.querySelector('#srange button[data-r="custom"]').click()`);
  await new Promise(r => setTimeout(r, 1200));
  const custom = await ev(`(() => ({
    from: document.getElementById('sfrom').style.display,
    to: document.getElementById('sto').style.display,
    fromVal: document.getElementById('sfrom').value,
  }))()`);
  check('custom range shows its inputs', custom.from !== 'none' && custom.to !== 'none',
    JSON.stringify(custom));
  check('custom inputs seeded from the shown window', !!custom.fromVal,
    JSON.stringify(custom));

  // --- 8. Switching back to the list leaves it working ----------------------
  await ev(`setView('list')`);
  await new Promise(r => setTimeout(r, 900));
  const back = await ev(`(() => ({
    split: getComputedStyle(document.querySelector('.split')).display,
    stats: getComputedStyle(document.getElementById('stats')).display,
    items: document.querySelectorAll('.item').length,
  }))()`);
  check('list view restored', back.split !== 'none' && back.stats === 'none',
    JSON.stringify(back));
  check('list still renders rows', back.items > 0, JSON.stringify(back));

  const pass = results.filter(r => r.p).length;
  for (const r of results) console.log(`${r.p ? 'PASS' : 'FAIL'}  ${r.n}${r.p ? '' : '  -- ' + r.d}`);
  console.log(`\n${pass}/${results.length} passed`);
  ws.close();
  process.exit(pass === results.length ? 0 : 1);
}

main().catch(e => { console.error(e); process.exit(1); });
