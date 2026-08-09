// Drive the real page in a headless browser and assert clamp behaviour.
// Layout-dependent, so a DOM-less sandbox cannot verify this: whether a block
// overflows depends on actual text wrapping.
const CDP_PORT = process.argv[2] || '9222';
const URL = process.argv[3];
// optional: prefer a row containing this text (the fixture's long call)
const PREFER = process.argv[4] || 'demo/long';

async function main(){
  const list = await (await fetch(`http://127.0.0.1:${CDP_PORT}/json/list`)).json();
  const page = list.find(t => t.type === 'page' && t.webSocketDebuggerUrl);
  if (!page) throw new Error('no debuggable page');

  const WebSocket = (await import('ws')).default;
  const ws = new WebSocket(page.webSocketDebuggerUrl);
  await new Promise(r => ws.on('open', r));
  let id = 0;
  const pending = new Map();
  ws.on('message', m => {
    const msg = JSON.parse(m);
    if (msg.id && pending.has(msg.id)){ pending.get(msg.id)(msg); pending.delete(msg.id); }
  });
  const send = (method, params) => new Promise(res => {
    const i = ++id; pending.set(i, res);
    ws.send(JSON.stringify({id: i, method, params}));
  });
  const evalJS = async expr => {
    const r = await send('Runtime.evaluate', {expression: expr, awaitPromise: true, returnByValue: true});
    if (r.result?.exceptionDetails) throw new Error(JSON.stringify(r.result.exceptionDetails));
    return r.result?.result?.value;
  };

  await send('Page.enable');
  await send('Runtime.enable');
  // reload rather than reusing whatever state a previous run left behind
  await send('Page.navigate', {url: 'about:blank'});
  await new Promise(r => setTimeout(r, 300));
  await send('Page.navigate', {url: URL});
  await new Promise(r => setTimeout(r, 4000));   // real deployments are slower than a local fixture

  const results = [];
  const check = (name, pass, detail) => { results.push({name, pass, detail}); };

  // --- click the call with the most content, whichever it is ---
  // Named-record matching would only work against the fixture; against real
  // traffic we want whatever row is actually large.
  const picked = await evalJS(`(() => {
    const rows = [...document.querySelectorAll('.item')];
    if (!rows.length) return null;
    const target = process_pick(rows);
    target.click();
    return target.textContent.trim().slice(0, 60);
    function process_pick(rs){
      const want = rs.find(r => r.textContent.includes(${JSON.stringify(PREFER)}));
      if (want) return want;
      return rs.map(r => [r.textContent.length, r]).sort((a,b) => b[0]-a[0])[0][1];
    }
  })()`);
  check('selected a call to inspect', !!picked, String(picked).replace(/\s+/g,' ').slice(0, 50));
  await new Promise(r => setTimeout(r, 2000));
  // Content now lives inside collapsible cards. Open them all so the clamp
  // assertions below see real layout rather than a display:none subtree.
  await evalJS(`(() => {
    document.querySelectorAll('#detail .card:not(.open) .chd').forEach(h => h.click());
    return true;
  })()`);
  await new Promise(r => setTimeout(r, 900));

  const state1 = await evalJS(`(() => {
    const clamps = [...document.querySelectorAll('#detail .clamp')];
    const btns   = [...document.querySelectorAll('#detail .exp')];
    return {
      clamps: clamps.length,
      clamped: clamps.filter(c => c.classList.contains('on')).length,
      visibleBtns: btns.filter(b => !b.hidden).length,
      hiddenBtns: btns.filter(b => b.hidden).length,
      // every visible toggle must sit on a block that really overflows
      falsePositives: btns.filter(b => !b.hidden).filter(b => {
        const el = document.getElementById(b.dataset.for);
        const lim = parseInt(el.style.getPropertyValue('--clh')) || 84;
        return el.scrollHeight <= lim + 8;
      }).length,
      stats: btns.filter(b => !b.hidden).map(b => b.querySelector('.meta').textContent.trim()),
      sectionToggles: [...document.querySelectorAll('#detail .toggle[data-all]')].map(t => t.textContent.trim()),
      heights: clamps.filter(c => c.classList.contains('on')).map(c => c.getBoundingClientRect().height),
    };
  })()`);

  check('clamps present', state1.clamps > 0, `${state1.clamps} clamp blocks`);
  check('long content is collapsed by default', state1.clamped > 0, `${state1.clamped} collapsed`);
  check('toggles shown only where content overflows',
        state1.falsePositives === 0, `${state1.falsePositives} false positives`);
  check('collapsed height respects the limit',
        state1.heights.every(h => h <= 92), `heights: ${state1.heights.map(h=>Math.round(h)).join(',')}`);
  // Text blocks report characters; tool definitions report parameter counts,
  // which is the more useful number for a schema.
  check('every collapsed block carries a summary',
        state1.stats.length > 0 && state1.stats.every(s => /字符|参数/.test(s)),
        JSON.stringify(state1.stats));
  check('section-level toggles exist',
        state1.sectionToggles.length > 0, JSON.stringify(state1.sectionToggles));

  // --- expand one block ---
  const expanded = await evalJS(`(() => {
    const b = [...document.querySelectorAll('#detail .exp')].find(x => !x.hidden);
    const el = document.getElementById(b.dataset.for);
    const before = el.getBoundingClientRect().height;
    b.click();
    const after = el.getBoundingClientRect().height;
    return {before, after, label: b.querySelector('.lbl').textContent,
            open: b.classList.contains('open'), clamped: el.classList.contains('on')};
  })()`);
  check('expanding grows the block', expanded.after > expanded.before,
        `${Math.round(expanded.before)} -> ${Math.round(expanded.after)}px`);
  check('expanded label switches to 收起', expanded.label === '收起', expanded.label);
  check('expanded block drops the clamp', expanded.clamped === false, `clamped=${expanded.clamped}`);

  // --- collapse it again ---
  const recollapsed = await evalJS(`(() => {
    const b = [...document.querySelectorAll('#detail .exp')].find(x => !x.hidden);
    b.click();
    const el = document.getElementById(b.dataset.for);
    return {h: el.getBoundingClientRect().height, label: b.querySelector('.lbl').textContent,
            clamped: el.classList.contains('on')};
  })()`);
  check('collapsing restores the clamp', recollapsed.clamped && recollapsed.h <= 92,
        `${Math.round(recollapsed.h)}px clamped=${recollapsed.clamped}`);
  check('collapsed label switches back to 展开', recollapsed.label === '展开', recollapsed.label);

  // --- expand all in a section ---
  const all = await evalJS(`(() => {
    // pick a scope that actually contains overflowing blocks
    const t = [...document.querySelectorAll('#detail .toggle[data-all]')].find(x => {
      const sc = x.closest('.card') || x.closest('section');
      return [...sc.querySelectorAll('.exp')].some(b => !b.hidden);
    }) || document.querySelector('#detail .toggle[data-all]');
    const sect = t.closest('.card') || t.closest('section');
    t.click();
    const btns = [...sect.querySelectorAll('.exp')].filter(b => !b.hidden);
    return {label: t.textContent.trim(), total: btns.length,
            open: btns.filter(b => b.classList.contains('open')).length};
  })()`);
  check('expand-all opens every block in the section',
        all.total > 0 && all.open === all.total, `${all.open}/${all.total}, label now "${all.label}"`);
  check('expand-all label flips to 收起全部', all.label === '收起全部', all.label);

  const collapseAll = await evalJS(`(() => {
    const t = [...document.querySelectorAll('#detail .toggle[data-all]')].find(x =>
      x.textContent.trim() === '收起全部') || document.querySelector('#detail .toggle[data-all]');
    const sect = t.closest('.card') || t.closest('section');
    t.click();
    const btns = [...sect.querySelectorAll('.exp')].filter(b => !b.hidden);
    return {label: t.textContent.trim(), open: btns.filter(b => b.classList.contains('open')).length};
  })()`);
  check('collapse-all closes them again',
        collapseAll.open === 0 && collapseAll.label === '展开全部',
        `${collapseAll.open} open, label "${collapseAll.label}"`);

  // --- short content must NOT get a toggle ---
  // Pick the smallest row that actually HAS a conversation: a record whose
  // request body was truncated renders no message cards at all, which would
  // make this assertion vacuous rather than meaningful.
  await evalJS(`(() => {
    const rows = [...document.querySelectorAll('.item')];
    const scored = rows.map(r => [r.textContent.length, r]).sort((a,b) => a[0]-b[0]);
    for (const [, r] of scored){
      r.click();
      if (document.querySelectorAll('#detail .card').length) return true;
    }
    return false;
  })()`);
  await new Promise(r => setTimeout(r, 1500));
  await evalJS(`(() => {
    document.querySelectorAll('#detail .card:not(.open) .chd').forEach(h => h.click());
    return true;
  })()`);
  await new Promise(r => setTimeout(r, 600));
  const short = await evalJS(`(() => {
    // Assert on the shortest MESSAGE: a row can be short overall while still
    // containing one long message.
    const msgs = [...document.querySelectorAll('#detail .msg')];
    let best = null;
    for (const m of msgs){
      const c = m.querySelector('.clamp');
      if (!c || !c.scrollHeight) continue;
      if (!best || c.scrollHeight < best.h) best = {h: c.scrollHeight, m, c};
    }
    if (!best) return null;
    const btn = best.m.querySelector('.exp');
    return {h: best.h, clamped: best.c.classList.contains('on'),
            hasToggle: !!(btn && !btn.hidden),
            text: (best.m.querySelector('.txt')||{}).textContent};
  })()`);
  check('the shortest message is not clamped',
        short && !short.clamped && !short.hasToggle,
        short ? `${short.h}px clamped=${short.clamped} toggle=${short.hasToggle}` : 'no message found');
  check('short message still renders text', !!(short && (short.text || '').length > 0),
        String(short && short.text).slice(0, 40));

  // --- no console errors anywhere ---
  const errs = await evalJS(`window.__errs ? window.__errs.length : 0`);
  check('no JS errors captured', errs === 0, `${errs} errors`);

  console.log('');
  let fail = 0;
  for (const r of results){
    console.log(`  ${r.pass ? 'PASS' : 'FAIL'}  ${r.name}  (${r.detail})`);
    if (!r.pass) fail++;
  }
  console.log(`\n${results.length - fail}/${results.length} passed`);
  ws.close();
  process.exit(fail ? 1 : 0);
}
main().catch(e => { console.error('ERROR', e.message); process.exit(1); });
