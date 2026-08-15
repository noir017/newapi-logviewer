// Verify the card hierarchy on an agent transcript: tool catalogue collapsed at
// top, conversation grouped into turn cards, and clamps inside a collapsed card
// measured correctly when it is first opened (measuring a display:none subtree
// returns 0, which would silently delete every toggle).
const CDP_PORT = process.argv[2] || '9223';
const URL = process.argv[3];
const PICK = process.argv[4] || 'agent-long';

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
    const r = await send('Runtime.evaluate', {expression: expr, returnByValue: true, awaitPromise: true});
    if (r.result?.exceptionDetails)
      throw new Error(r.result.exceptionDetails.exception?.description || 'eval failed');
    return r.result?.result?.value;
  };

  await send('Page.enable'); await send('Runtime.enable');
  await send('Page.navigate', {url: 'about:blank'});
  await new Promise(r => setTimeout(r, 300));
  await send('Page.navigate', {url: URL});
  await new Promise(r => setTimeout(r, 4000));

  const results = [];
  const check = (n, p, d) => results.push({n, p, d});

  // list row: counts, not a wall of tool names
  const row = await ev(`(() => {
    const rows = [...document.querySelectorAll('.item')];
    const t = rows.find(r => r.textContent.includes(${JSON.stringify(PICK)})) || rows[0];
    t.click();
    return {text: t.textContent.replace(/\\s+/g,' ').trim(),
            toolChips: t.querySelectorAll('[data-tools]').length,
            namedChips: t.querySelectorAll('[data-tool]').length,
            chipText: [...t.querySelectorAll('.i3 .tag')].map(x => x.textContent.trim())};
  })()`);
  check('row shows a tool COUNT chip, not names',
        row.toolChips === 1 && row.namedChips === 0,
        JSON.stringify(row.chipText));
  check('row shows the call count', /↻\d+ 次调用/.test(row.text),
        (row.text.match(/↻[^ ]* 次调用/) || ['none'])[0]);
  await new Promise(r => setTimeout(r, 2000));

  const s = await ev(`(() => {
    const cards = [...document.querySelectorAll('#detail .card')];
    const tools = cards.find(c => c.querySelector('.ct')?.textContent.includes('Tools'));
    const turns = cards.filter(c => c.classList.contains('turn'));
    const secs = [...document.querySelectorAll('#detail section h3')].map(h => h.textContent);
    return {
      total: cards.length,
      turns: turns.length,
      sections: secs,
      toolsCardExists: !!tools,
      toolsCardOpen: tools ? tools.classList.contains('open') : null,
      toolsSummary: tools ? tools.querySelector('.cs').textContent.trim() : null,
      toolsIsFirstSection: secs[1] === '可用工具' || secs[0] === '可用工具',
      firstTurnSummary: turns[0] ? turns[0].querySelector('.chd').textContent.replace(/\\s+/g,' ').trim() : null,
      openTurns: turns.filter(c => c.classList.contains('open')).length,
      // a collapsed card must contribute no visible height beyond its header
      collapsedBodyVisible: turns.filter(c => !c.classList.contains('open'))
        .filter(c => c.querySelector('.cbd').getBoundingClientRect().height > 0).length,
      paneHeight: document.querySelector('#detail').scrollHeight,
    };
  })()`);

  check('conversation is grouped into turn cards', s.turns >= 40, `${s.turns} turn cards`);
  check('tools card exists', s.toolsCardExists, String(s.toolsSummary));
  check('tools card sits above the conversation', s.toolsIsFirstSection, JSON.stringify(s.sections));
  check('tools card starts collapsed', s.toolsCardOpen === false, `open=${s.toolsCardOpen}`);
  check('turn cards start collapsed', s.openTurns === 0, `${s.openTurns} open`);
  check('collapsed cards take no vertical space', s.collapsedBodyVisible === 0,
        `${s.collapsedBodyVisible} leaking`);
  check('turn header summarises the tools used',
        /⚒/.test(s.firstTurnSummary || ''), s.firstTurnSummary);

  // THE regression risk: clamps inside a card that was hidden at render time.
  // Turn cards now live inside the collapsed 会话历史 fold, so open that first —
  // clicking a card in a display:none subtree measures nothing, which is the
  // very failure mode the lazy wiring exists to avoid.
  const lazy = await ev(`(() => {
    const hist = document.querySelector('#detail .card.hist');
    if (hist && !hist.classList.contains('open')) hist.querySelector('.chd').click();
    const c = [...document.querySelectorAll('#detail .card.turn')][1];
    const before = {clamps: c.querySelectorAll('.clamp').length,
                    visibleToggles: [...c.querySelectorAll('.exp')].filter(b => !b.hidden).length};
    c.querySelector('.chd').click();
    const after = {open: c.classList.contains('open'),
                   visibleToggles: [...c.querySelectorAll('.exp')].filter(b => !b.hidden).length,
                   clampsOn: c.querySelectorAll('.clamp.on').length,
                   removedToggles: c.querySelectorAll('.exp').length,
                   bodyH: c.querySelector('.cbd').getBoundingClientRect().height};
    return {before, after};
  })()`);
  check('opening a collapsed card reveals its content',
        lazy.after.open && lazy.after.bodyH > 0, `body ${Math.round(lazy.after.bodyH)}px`);
  check('clamps inside a previously-hidden card are measured on open',
        lazy.after.visibleToggles > 0 || lazy.after.removedToggles < lazy.before.clamps,
        `toggles ${lazy.before.visibleToggles} -> ${lazy.after.visibleToggles}, clamps ${lazy.before.clamps}`);

  // and the toggle must actually work after that lazy wiring
  const lazyExpand = await ev(`(() => {
    const c = [...document.querySelectorAll('#detail .card.turn')][1];
    const b = [...c.querySelectorAll('.exp')].find(x => !x.hidden);
    if (!b) return {skipped: true};
    const el = document.getElementById(b.dataset.for);
    const before = el.getBoundingClientRect().height;
    b.click();
    return {before, after: el.getBoundingClientRect().height, label: b.querySelector('.lbl').textContent};
  })()`);
  check('a lazily-wired toggle still expands',
        lazyExpand.skipped || lazyExpand.after > lazyExpand.before,
        lazyExpand.skipped ? 'no overflowing block in this card'
          : `${Math.round(lazyExpand.before)} -> ${Math.round(lazyExpand.after)}px`);

  // re-collapsing must not destroy the wiring
  const recollapse = await ev(`(() => {
    const c = [...document.querySelectorAll('#detail .card.turn')][1];
    c.querySelector('.chd').click();
    const closed = !c.classList.contains('open');
    c.querySelector('.chd').click();
    return {closed, reopened: c.classList.contains('open'),
            toggles: [...c.querySelectorAll('.exp')].filter(b => !b.hidden).length};
  })()`);
  check('close/reopen keeps the card usable',
        recollapse.closed && recollapse.reopened,
        `toggles still ${recollapse.toggles}`);

  // expand-all on the tools card must open it before measuring
  const toolsAll = await ev(`(() => {
    const t = [...document.querySelectorAll('#detail .card')]
      .find(c => c.querySelector('.ct')?.textContent.includes('Tools'));
    t.querySelector('.chd').click();               // open the card
    const defs = t.querySelectorAll('.tool-def').length;
    const btn = t.querySelector('.toggle[data-all]');
    btn.click();
    return {defs, open: t.classList.contains('open'),
            label: btn.textContent.trim(),
            expanded: [...t.querySelectorAll('.exp')].filter(b => b.classList.contains('open')).length};
  })()`);
  check('tools card lists every definition', toolsAll.defs === 20, `${toolsAll.defs} tool defs`);
  check('expand-all works inside the tools card',
        toolsAll.label === '收起全部', `label "${toolsAll.label}", ${toolsAll.expanded} expanded`);

  const errs = await ev(`window.__errs ? window.__errs.length : 0`);
  check('no JS errors', errs === 0, `${errs}`);

  console.log('');
  let fail = 0;
  for (const r of results){ console.log(`  ${r.p ? 'PASS' : 'FAIL'}  ${r.n}  (${r.d})`); if (!r.p) fail++; }
  console.log(`\n${results.length - fail}/${results.length} passed`);
  ws.close();
  process.exit(fail ? 1 : 0);
}
main().catch(e => { console.error('ERROR', e.message); process.exit(1); });
