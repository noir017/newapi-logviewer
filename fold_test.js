// The two things a 351-message record made unusable, asserted as click-depth
// and scroll-distance rather than as markup shape:
//
//   1. reading one message took TWO expands - the card header and an inner
//      clamp both gated the same single block.
//   2. the response you opened the record for sat below every history card, so
//      reaching it meant scrolling past the whole transcript.
//
// Both are layout facts, so this drives a real browser. `node fold_test.js
// <cdp-port> <url>`.
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

  await ev(`(() => { const r = document.querySelector('.item'); r.click(); return 1; })()`);
  await new Promise(r => setTimeout(r, 2500));

  // ---- bug 2: the transcript is folded, response is reachable --------------
  const shape = await ev(`(() => {
    const pane = document.querySelector('#detail');
    const hist = pane.querySelector('.card.hist');
    const secs = [...pane.querySelectorAll('section')];
    const conv = secs.find(s => s.querySelector('h3')?.textContent.includes('会话'));
    const resp = secs.find(s => s.querySelector('h3')?.textContent.includes('本次模型返回'));
    return {
      hist: !!hist,
      histOpen: hist ? hist.classList.contains('open') : null,
      histSummary: hist ? hist.querySelector('.cs').textContent.trim() : null,
      histHasExpandAll: hist ? !!hist.querySelector('.toggle[data-all]') : null,
      // cards left loose in the conversation section (outside the fold)
      looseCards: conv ? [...conv.children].filter(n => n.classList?.contains('card')).length : null,
      hiddenCards: hist ? hist.querySelectorAll('.card').length : 0,
      tailLabel: !!pane.querySelector('.tailhd'),
      convH: conv ? conv.getBoundingClientRect().height : null,
      respTop: resp ? resp.getBoundingClientRect().top - pane.getBoundingClientRect().top + pane.scrollTop : null,
      paneH: pane.scrollHeight,
    };
  })()`);

  check('transcript is folded into one card', shape.hist, String(shape.histSummary));
  check('fold starts collapsed', shape.histOpen === false, `open=${shape.histOpen}`);
  check('fold reports the message count on its header',
        /\d+ 条消息/.test(shape.histSummary || ''), shape.histSummary);
  check('fold carries an 展开全部 like the tools card', shape.histHasExpandAll === true,
        `hasToggle=${shape.histHasExpandAll}`);
  check('history cards live inside the fold, not loose',
        shape.hiddenCards > shape.looseCards * 10,
        `${shape.hiddenCards} inside, ${shape.looseCards} loose`);
  check('only the newest exchange stays outside the fold', shape.looseCards <= 2,
        `${shape.looseCards} loose cards`);
  check('the response is reachable without scrolling past the transcript',
        shape.respTop < 2500, `响应 at ${Math.round(shape.respTop)}px (pane ${shape.paneH}px)`);

  // ---- bug 1: one expand, not two -----------------------------------------
  const depth = await ev(`(() => {
    const hist = document.querySelector('#detail .card.hist');
    if (hist) hist.querySelector('.chd').click();     // open the fold
    // Without a fold the history cards are loose in the section; probe them
    // there so this check reports its own verdict instead of crashing on the
    // missing fold (which the checks above already own).
    const scope = hist || document.querySelector('#detail');
    const cards = [...scope.querySelectorAll('.card')];
    // a standalone message card (user/system), the shape from the screenshot
    const c = cards.find(x => x.classList.contains('usermsg') || x.classList.contains('sysmsg'));
    const before = c.querySelector('.txt').getBoundingClientRect().height;
    c.querySelector('.chd').click();                  // ONE click
    const txt = c.querySelector('.txt');
    // Measure what is VISIBLE, not the text node's own height: inside a clamp
    // the .txt keeps its full height while the parent hides most of it, so
    // measuring .txt would call a double-gated message "revealed".
    const shown = c.querySelector('.cbd').getBoundingClientRect().height;
    return {
      role: c.querySelector('.ct').textContent.trim(),
      open: c.classList.contains('open'),
      innerClamps: c.querySelectorAll('.clamp.on').length,
      innerToggles: [...c.querySelectorAll('.exp')].filter(b => !b.hidden).length,
      before, shown, content: txt.scrollHeight,
      // fully revealed = the card shows essentially all of its content
      visible: shown >= txt.scrollHeight * 0.9,
    };
  })()`);
  check('one click fully reveals a standalone message',
        depth.open && depth.visible,
        `${depth.role}: shows ${Math.round(depth.shown)}px of ${Math.round(depth.content)}px`);
  check('no second expand gating the same block',
        depth.innerClamps === 0 && depth.innerToggles === 0,
        `${depth.innerClamps} clamps, ${depth.innerToggles} toggles inside`);

  // a turn card whose ONLY content is one block behaves the same way
  const soleTurn = await ev(`(() => {
    const cards = [...document.querySelectorAll('#detail .card.turn')];
    for (const c of cards){
      if (!c.dataset.probed){ c.dataset.probed = '1'; }
      c.classList.add('open');
      const blocks = c.querySelectorAll('.cbd > .msg, .cbd > .tc').length;
      if (blocks === 1){
        return {found: true, blocks,
                clamps: c.querySelectorAll('.clamp').length,
                title: c.querySelector('.ct').textContent.trim()};
      }
      c.classList.remove('open');
    }
    return {found: false};
  })()`);
  check('a single-block turn card has no inner clamp either',
        !soleTurn.found || soleTurn.clamps === 0,
        soleTurn.found ? `${soleTurn.title}: ${soleTurn.clamps} clamps` : 'no single-block turn here');

  // multi-block turns KEEP their clamps - that is what stops one long tool
  // result burying the ones below it
  const multi = await ev(`(() => {
    const cards = [...document.querySelectorAll('#detail .card.turn')];
    for (const c of cards){
      c.classList.add('open');
      if (!c.dataset.wired){ c.dataset.wired='1'; }
      const blocks = c.querySelectorAll('.cbd > .msg, .cbd > .tc').length;
      if (blocks > 1) return {found: true, blocks, clamps: c.querySelectorAll('.clamp').length};
      c.classList.remove('open');
    }
    return {found: false};
  })()`);
  check('multi-block turns keep per-block clamps',
        !multi.found || multi.clamps > 1,
        multi.found ? `${multi.blocks} blocks, ${multi.clamps} clamps` : 'none');

  // ---- 展开全部 on the fold must open the fold itself ----------------------
  const expAll = await ev(`(() => {
    const hist = document.querySelector('#detail .card.hist');
    if (!hist) return {absent: true};
    hist.classList.remove('open');                    // close it again
    const btn = hist.querySelector('.toggle[data-all]');
    btn.click();
    const inner = [...hist.querySelectorAll('.card')];
    return {selfOpen: hist.classList.contains('open'),
            label: btn.textContent.trim(),
            innerOpen: inner.filter(c => c.classList.contains('open')).length,
            inner: inner.length,
            bodyH: hist.querySelector('.cbd').getBoundingClientRect().height};
  })()`);
  check('展开全部 opens the fold itself, not just its children',
        !expAll.absent && expAll.selfOpen && expAll.bodyH > 0,
        expAll.absent ? 'no fold card' : `self=${expAll.selfOpen}, body ${Math.round(expAll.bodyH)}px`);
  check('展开全部 expands every card inside the fold',
        !expAll.absent && expAll.innerOpen === expAll.inner,
        expAll.absent ? 'no fold card' : `${expAll.innerOpen}/${expAll.inner} open, label "${expAll.label}"`);

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
