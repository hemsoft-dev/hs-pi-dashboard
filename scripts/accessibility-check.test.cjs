// Unit tests for scripts/accessibility-check.cjs. No browser or axe needed:
// these cover argument handling, version pins, result shaping and exit codes.
const assert = require('node:assert/strict');
const {test} = require('node:test');
const path = require('node:path');
const check = require('./accessibility-check.cjs');

const quiet = () => {
  const lines = [];
  return {lines, log: (s) => lines.push(String(s)), error: (s) => lines.push(String(s))};
};
const reached = (overrides = {}) => ({
  tabs: 3, focusVisible: true, outlineStyle: 'solid', outlineWidth: '2px', outlineColor: 'rgb(255, 215, 0)', onScreen: true,
  ...overrides,
});
const cleanCase = (extra = {}) => ({
  state: 'mismatch', viewport: 'desktop', width: 1280, height: 900, errors: [], violations: [], incomplete: [],
  passes: 46, expanded: 3, keyboard: {reached: {'Pulse toggle': reached(), 'footer link': reached()}, failures: []},
  ...extra,
});

test('defaults cover both issue states at desktop and phone sizes', () => {
  const opts = check.parseArgs([]);
  assert.deepEqual(opts.states, ['mismatch', 'failed-free']);
  assert.deepEqual(opts.viewports, ['desktop', 'phone']);
  assert.deepEqual(check.VIEWPORTS.desktop, {width: 1280, height: 900});
  assert.deepEqual(check.VIEWPORTS.phone, {width: 390, height: 844});
  assert.equal(opts.root, path.join(__dirname, '..'));
  assert.deepEqual(check.TAGS, ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice']);
});

test('options are parsed and bad input is refused', () => {
  const opts = check.parseArgs(['--states', 'failed-free', '--viewports', 'phone', '--base-url', 'http://127.0.0.1:5/x?y=1']);
  assert.deepEqual(opts.states, ['failed-free']);
  assert.deepEqual(opts.viewports, ['phone']);
  assert.equal(opts.baseUrl, 'http://127.0.0.1:5');
  assert.throws(() => check.parseArgs(['--states', 'bogus']), /--states/);
  assert.throws(() => check.parseArgs(['--viewports', 'tablet']), /--viewports/);
  assert.throws(() => check.parseArgs(['--json']), /needs a value/);
  assert.throws(() => check.parseArgs(['--fast']), /unknown option/);
});

test('tool versions must match the pins', () => {
  assert.deepEqual(check.versionProblems({playwright: '1.64.1', 'axe-core': '4.13.0'}), []);
  assert.deepEqual(check.versionProblems({playwright: '1.65.0'}),
    ['playwright 1.65.0 found, 1.64.1 required', 'axe-core missing found, 4.13.0 required']);
});

test('packageVersion walks up from the resolved entry point', () => {
  const fake = path.join(__dirname, '..', 'scripts', 'accessibility-check.cjs');
  assert.throws(() => check.packageVersion('no-such-package-name', () => fake), /no package.json found/);
});

test('axe results keep violation summaries and incomplete reasons', () => {
  const summary = check.summarizeAxe({
    passes: [{}, {}],
    violations: [{id: 'region', impact: 'moderate', help: 'All page content should be contained by landmarks',
      nodes: [{target: ['#pulse-body'], html: '<div id="pulse-body">', failureSummary: 'Fix any of the following'}]}],
    incomplete: [{id: 'color-contrast', impact: 'serious', help: 'Contrast',
      nodes: [{target: ['.pulse-count'], html: '<span>', any: [{message: 'Background could not be determined'}],
        all: [], none: [{message: 'Background could not be determined'}]}]}],
  });
  assert.equal(summary.passes, 2);
  assert.deepEqual(summary.violations[0].nodes[0], {target: '#pulse-body', html: '<div id="pulse-body">', summary: 'Fix any of the following'});
  assert.equal(summary.incomplete[0].nodes[0].reason, 'Background could not be determined');
});

test('keyboard check needs focus-visible, an outline and an on-screen target', () => {
  assert.deepEqual(check.keyboardFailures({'Pulse toggle': reached(), 'footer link': reached()}), []);
  assert.deepEqual(check.keyboardFailures({'Pulse toggle': reached()}), ['footer link: not reached within 250 Tab presses']);
  assert.deepEqual(check.keyboardFailures({
    'Pulse toggle': reached({outlineStyle: 'none', outlineWidth: '0px'}),
    'footer link': reached({focusVisible: false, onScreen: false}),
  }), ['Pulse toggle: no visible outline', 'footer link: :focus-visible does not apply', 'footer link: not scrolled into view']);
});

test('exit code: 0 clean, 1 findings, 2 when the check could not complete', () => {
  const report = (cases, errors = []) => ({cases, errors});
  assert.equal(check.exitCode(report([cleanCase()])), check.EXIT.OK);
  assert.equal(check.exitCode(report([cleanCase({incomplete: [{id: 'color-contrast', nodes: []}]})])), check.EXIT.OK);
  assert.equal(check.exitCode(report([cleanCase({violations: [{id: 'region', nodes: []}]})])), check.EXIT.FINDINGS);
  assert.equal(check.exitCode(report([cleanCase({keyboard: {reached: {}, failures: ['x']}})])), check.EXIT.FINDINGS);
  assert.equal(check.exitCode(report([cleanCase({errors: ['HTTP 404: /x'], violations: [{id: 'region', nodes: []}]})])), check.EXIT.ERROR);
  assert.equal(check.exitCode(report([], ['tool version mismatch'])), check.EXIT.ERROR);
});

test('the report lists incomplete nodes as needing review, never as passes', () => {
  const text = check.formatReport({errors: [], cases: [cleanCase({incomplete: [{id: 'color-contrast', impact: 'serious',
    nodes: [{target: '.pulse-count', reason: 'overlapped'}]}]})]});
  assert.match(text, /^ok {2}mismatch @ desktop 1280x900: 0 violations, 1 incomplete, 46 passed rules/);
  assert.match(text, /needs review color-contrast \(serious\): 1 node/);
  assert.match(text, /\.pulse-count: overlapped/);
});

test('main reports missing tools as an error, not a pass', async () => {
  const out = quiet();
  const code = await check.main([], () => { throw new Error("Cannot find module 'playwright'"); }, out);
  assert.equal(code, check.EXIT.ERROR);
  assert.match(out.lines.join('\n'), /error: Cannot find module 'playwright'/);
  assert.match(out.lines.join('\n'), /result: ERROR/);
});

test('main refuses bad arguments before loading tools', async () => {
  const out = quiet();
  let loaded = false;
  const code = await check.main(['--states', 'nope'], () => { loaded = true; }, out);
  assert.equal(code, check.EXIT.ERROR);
  assert.equal(loaded, false);
});
