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
    // .mrow alone now also matches the token rows, which share the geometry.
    // Scope to the model rows or this counts 8 models + 3 tokens and reports a
    // breakdown mismatch that does not exist.
    const rows = [...document.querySelectorAll('.mrow[data-model]')];
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

  // --- 4b. Per-token spend --------------------------------------------------
  // The card must reconcile like the model breakdown does, and it must be
  // ranked by SPEND. Those are different assertions: a breakdown can add up
  // perfectly and still answer the wrong question.
  const tk = await ev(`(() => {
    const d = statsData.data, rows = [...document.querySelectorAll('.mrow.tkrow')];
    const t = d.tokens_by_token || [];
    return {
      rows: rows.length, returned: t.length,
      reqSum: t.reduce((s, x) => s + x.requests, 0),
      quotaSum: t.reduce((s, x) => s + x.quota, 0),
      kpiReq: d.requests, kpiQuota: d.quota,
      quotas: t.map(x => x.quota),
      nameCount: d.token_name_count,
      // Rendered bar widths, to check the bar encodes the same thing the
      // number does. A row sorted by spend with a bar scaled to volume would
      // read as a chart contradicting its own labels.
      bars: rows.map(r => parseFloat(r.querySelector('.fill').style.width)),
      unnamedFilterable: rows.filter(r =>
        r.classList.contains('na') && r.dataset.token !== undefined).length,
    };
  })()`);
  check('every token row is rendered', tk.rows === tk.returned,
    `rendered=${tk.rows} returned=${tk.returned}`);
  check('token breakdown sums to the request total', tk.reqSum === tk.kpiReq,
    `tokens=${tk.reqSum} kpi=${tk.kpiReq}`);
  check('token breakdown sums to total spend', tk.quotaSum === tk.kpiQuota,
    `tokens=${tk.quotaSum} kpi=${tk.kpiQuota}`);
  // The ordering assertion. Ranked by requests instead, this fails on any
  // archive where the biggest spender is not also the busiest — which is the
  // normal case, not the corner one.
  check('token rows are ranked by spend',
    tk.quotas.every((q, i) => i === 0 || tk.quotas[i - 1] >= q),
    JSON.stringify(tk.quotas));
  check('bars are scaled to spend, matching the order',
    tk.bars.every((b, i) => i === 0 || tk.bars[i - 1] >= b - 0.01),
    JSON.stringify(tk.bars));
  // An unnamed row is a gap in the measurement, so it must not offer a
  // drill-down: the name is exactly what is missing to filter by.
  check('the unnamed row is not filterable', tk.unnamedFilterable === 0,
    `${tk.unnamedFilterable} unnamed rows carry data-token`);
  // A named token row must read as ordinary body text. Naming the row class
  // `.tk` put it under the JSON highlighter's token-KEY rule and every token
  // name rendered green — a stylesheet collision no layout or reconciliation
  // assertion can see, because the numbers were all correct.
  const hue = await ev(`(() => {
    const rows = [...document.querySelectorAll('.mrow.tkrow[data-token] .nm')];
    const body = getComputedStyle(document.body).color;
    return {colors: [...new Set(rows.map(n => getComputedStyle(n).color))], body};
  })()`);
  check('token names use the default text colour',
    hue.colors.length === 0 || hue.colors.every(c => c === hue.body),
    `names=${JSON.stringify(hue.colors)} body=${hue.body}`);
  // Coverage must be stated, not assumed. A zero here on a non-empty range is
  // a stale index, and the page has to say so rather than attribute every
  // call to nobody.
  const warned = await ev(`(() => {
    const d = statsData.data;
    const stale = d.requests > 0 && d.token_name_count === 0;
    const box = [...document.querySelectorAll('.warnbox')]
      .some(b => b.textContent.includes('reindex') && b.textContent.includes('令牌'));
    return {stale, box, count: d.token_name_count};
  })()`);
  check('a stale token index is reported, not rendered as fact',
    warned.stale === warned.box,
    `stale=${warned.stale} warned=${warned.box} count=${warned.count}`);

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

  // --- 8. Token drill-down lands on the calls the row counted ---------------
  // The row claims N calls; the list must show exactly N. This is the assertion
  // that catches a drill-down whose filter silently does nothing — the list
  // would render a perfectly plausible page of the wrong token's calls.
  await ev(`setView('stats')`);
  await new Promise(r => setTimeout(r, 1600));
  const drill = await ev(`(() => {
    const row = document.querySelector('.mrow.tkrow[data-token]');
    if (!row) return {skip: true};
    const t = statsData.data.tokens_by_token
      .find(x => x.token === row.dataset.token);
    row.click();
    return {skip: false, token: row.dataset.token, want: t.requests};
  })()`);
  if (drill.skip){
    check('token drill-down (no named token in range)', true, 'skipped');
  } else {
    await new Promise(r => setTimeout(r, 1600));
    const landed = await ev(`(() => ({
      total: state.total,
      sel: document.getElementById('tokensel').value,
      view: document.body.classList.contains('view-stats'),
      // Every row on screen must actually belong to that token, not merely add
      // up to the right count.
      rows: state.items.length,
      foreign: state.items.filter(i => i.token_name !== ${JSON.stringify(drill.token)}).length,
    }))()`);
    check('drill-down switches to the list view', !landed.view, JSON.stringify(landed));
    check('drill-down applies the token filter',
      landed.sel === drill.token, `select=${landed.sel} want=${drill.token}`);
    check('list total matches the row that was clicked',
      landed.total === drill.want,
      `list=${landed.total} row=${drill.want} token=${drill.token}`);
    check('every listed call belongs to that token', landed.foreign === 0,
      `${landed.foreign} of ${landed.rows} rows carry another token`);
  }

  // --- 9. Switching back to the list leaves it working ----------------------
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
