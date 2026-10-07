// Behavioural test for how the detail pane reads bodies logged in a provider's
// own format - respOut (a non-streaming call's output), reqIn (the request as a
// transcript) and rerankHtml - run against the real ui.html source.
//
//   node respout_test.js
//
// new-api logs the upstream's response as received, so the pane has to read
// three wire formats. It read only OpenAI's, and every Gemini and Claude answer
// rendered as （无文本内容） beside a raw JSON that held it.
const fs = require('fs');
const assert = require('assert');

const html = fs.readFileSync(__dirname + '/ui.html', 'utf8');
// Matched in the whole page rather than in a `<script>` block: a comment above
// the chart include contains that tag literally, which is what a
// `<script>(...)</script>` match lands on.
function extract(name, arg) {
  const m = html.match(new RegExp(`function ${name}\\(${arg}\\)\\{[\\s\\S]*?\\n\\}`));
  if (!m) throw new Error(`${name}() not found in ui.html`);
  return m[0];
}
const esc = s => String(s).replace(/[&<>"']/g, c =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const clamp = inner => inner, statOf = s => String(s).length + ' 字';
const { respOut, reqIn, rerankHtml } = new Function('esc', 'clamp', 'statOf',
  `${extract('respOut', 'resp')}\n${extract('reqIn', 'req')}\n${extract('rerankHtml', 'req, resp')}
   return {respOut, reqIn, rerankHtml};`)(esc, clamp, statOf);

let failed = 0;
function check(what, fn) {
  try { fn(); console.log('ok   ' + what); }
  catch (e) { failed++; console.log('FAIL ' + what + '\n     ' + e.message); }
}

check('OpenAI choices: unchanged', () => {
  const o = respOut({choices: [{message: {content: 'hi', reasoning_content: 'hm',
    tool_calls: [{id: 'c1', function: {name: 'f', arguments: '{"a":1}'}}]}}]});
  assert.strictEqual(o.content, 'hi');
  assert.strictEqual(o.reasoning, 'hm');
  assert.strictEqual(o.tcs.length, 1);
  assert.strictEqual(o.tcs[0].function.arguments, '{"a":1}');
});

check('OpenAI tool-call-only message: empty text, not "null"', () => {
  const o = respOut({choices: [{message: {content: null, tool_calls: []}}]});
  assert.strictEqual(o.content, '');
});

check('Gemini candidates: text, thought parts as reasoning, functionCall as a tool call', () => {
  const o = respOut({candidates: [{content: {role: 'model', parts: [
    {text: 'plan', thought: true},
    {text: '{"facts":[]}', thoughtSignature: 'xyz'},
    {functionCall: {name: 'save', args: {n: 1}}},
  ]}}], usageMetadata: {promptTokenCount: 1}});
  assert.strictEqual(o.content, '{"facts":[]}');
  assert.strictEqual(o.reasoning, 'plan');
  assert.strictEqual(o.tcs.length, 1);
  assert.strictEqual(o.tcs[0].function.name, 'save');
  assert.deepStrictEqual(JSON.parse(o.tcs[0].function.arguments), {n: 1});
});

check('Anthropic content blocks: text, thinking, tool_use', () => {
  const o = respOut({type: 'message', content: [
    {type: 'thinking', thinking: 'why'},
    {type: 'text', text: 'Title'},
    {type: 'tool_use', id: 'toolu_1', name: 'set_title', input: {t: 'x'}},
  ], usage: {input_tokens: 1, output_tokens: 2}});
  assert.strictEqual(o.content, 'Title');
  assert.strictEqual(o.reasoning, 'why');
  assert.strictEqual(o.tcs[0].id, 'toolu_1');
  assert.strictEqual(o.tcs[0].function.name, 'set_title');
  assert.deepStrictEqual(JSON.parse(o.tcs[0].function.arguments), {t: 'x'});
});

check('no response at all: empty, not a throw', () => {
  const o = respOut({});
  assert.deepStrictEqual(o, {content: '', reasoning: '', tcs: []});
});

check('OpenAI request: messages and tools pass through, slim drops them', () => {
  const req = {model: 'm', messages: [{role: 'user', content: 'hi'}],
               tools: [{type: 'function', function: {name: 'f'}}], temperature: 0};
  const {msgs, tools, slim} = reqIn(req);
  assert.strictEqual(msgs, req.messages);
  assert.strictEqual(tools, req.tools);
  assert.deepStrictEqual(slim, {model: 'm', temperature: 0});
});

check('Gemini request: system, turns, a call and its result, tools flattened', () => {
  const {msgs, tools, slim} = reqIn({
    systemInstruction: {parts: [{text: 'be brief'}]},
    contents: [
      {role: 'user', parts: [{text: 'weather?'}, {inlineData: {mimeType: 'image/png', data: 'x'}}]},
      {role: 'model', parts: [{text: 'plan', thought: true}, {functionCall: {name: 'get_weather', args: {city: 'Paris'}}}]},
      {role: 'user', parts: [{functionResponse: {name: 'get_weather', response: {c: 21}}}]},
      {role: 'model', parts: [{text: '21C'}]},
    ],
    tools: [{functionDeclarations: [{name: 'get_weather'}, {name: 'get_time'}]}, {googleSearch: {}}],
    generationConfig: {temperature: 0.1},
  });
  assert.deepStrictEqual(msgs.map(m => m.role), ['system', 'user', 'assistant', 'tool', 'assistant']);
  assert.strictEqual(msgs[0].content, 'be brief');
  assert.strictEqual(msgs[1].content, 'weather?\n[image/png]');
  assert.strictEqual(msgs[2].content, '', 'thought parts are not the answer');
  assert.strictEqual(msgs[2].tool_calls[0].function.name, 'get_weather');
  assert.deepStrictEqual(JSON.parse(msgs[2].tool_calls[0].function.arguments), {city: 'Paris'});
  assert.deepStrictEqual(JSON.parse(msgs[3].content), {c: 21});
  assert.deepStrictEqual(tools.map(t => t.name), ['get_weather', 'get_time']);
  assert.deepStrictEqual(slim, {generationConfig: {temperature: 0.1}});
});

check('rerank: results in ranked order, text taken from the request by index', () => {
  const rr = rerankHtml(
    {query: 'why <fail>', documents: ['doc zero', {text: 'doc one'}]},
    {results: [{index: 1, document: null, relevance_score: 0.9}, {index: 0, document: null, relevance_score: 0.1}]});
  assert.ok(rr.query.includes('why &lt;fail&gt;'), 'query is escaped');
  assert.strictEqual(rr.stat, '2 / 2 篇');
  const one = rr.rows.indexOf('doc one'), zero = rr.rows.indexOf('doc zero');
  assert.ok(one >= 0 && zero > one, 'ranked order, documents resolved by index');
  assert.ok(rr.rows.includes('0.9000'));
});

if (failed) { console.log(`\n${failed} failed`); process.exit(1); }
console.log('\nall passed');
