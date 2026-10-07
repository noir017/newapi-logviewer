// Behavioural test for respOut - how the detail pane reads a NON-streaming
// call's output - run against the real ui.html source rather than a copy.
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
const src = html.match(/function respOut\(resp\)\{[\s\S]*?\n\}/);
if (!src) throw new Error('respOut() not found in ui.html');
const respOut = new Function(`${src[0]}\nreturn respOut;`)();

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

if (failed) { console.log(`\n${failed} failed`); process.exit(1); }
console.log('\nall passed');
