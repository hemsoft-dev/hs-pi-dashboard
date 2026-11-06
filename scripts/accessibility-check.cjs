#!/usr/bin/env node
// Accessibility check for the dashboard page (issue #90).
//
// Starts the synthetic fixture (scripts/codex-fixture.cjs), loads the page in
// headless Chromium for each state and viewport, expands every collapsed
// section, runs axe-core, then checks with real Tab presses that keyboard
// focus reaches the Pulse toggle and the footer link with a visible indicator.
//
// Exit codes: 0 no violations and keyboard checks passed; 1 axe violations or
// a keyboard failure; 2 the check could not run (missing or mismatched tools,
// fixture or page load errors, axe errors). Incomplete axe results are listed
// for manual review and never count as passes.
//
// Needs playwright and axe-core at the pinned versions, resolvable by require
// (for example through NODE_PATH); see the README for the install command.
'use strict';
const fs = require('node:fs');
const net = require('node:net');
const path = require('node:path');
const {spawn} = require('node:child_process');

const PINNED = {playwright: '1.64.1', 'axe-core': '4.13.0'};
const TAGS = ['wcag2a', 'wcag2aa', 'wcag21a', 'wcag21aa', 'wcag22aa', 'best-practice'];
const STATES = ['mismatch', 'failed-free'];
const VIEWPORTS = {desktop: {width: 1280, height: 900}, phone: {width: 390, height: 844}};
const FOCUS_TARGETS = [
  {name: 'Pulse toggle', selector: '#pulse-toggle'},
  {name: 'footer link', selector: 'footer a[href="/api/fleet"]'},
];
const MAX_TABS = 250;
const EXIT = {OK: 0, FINDINGS: 1, ERROR: 2};
const USAGE = `Usage: node scripts/accessibility-check.cjs [options]
  --states a,b         fixture states (default ${STATES.join(',')})
  --viewports a,b      ${Object.keys(VIEWPORTS).join(',')} (default both)
  --base-url URL       scan a fixture that is already running instead of starting one
  --root DIR           repository root whose fixture to start (default: this checkout)
  --json FILE          write the full report as JSON
  --screenshot-dir DIR save page, Pulse and footer screenshots per state and viewport`;

function parseArgs(argv) {
  const opts = {states: STATES.slice(), viewports: Object.keys(VIEWPORTS), root: path.join(__dirname, '..')};
  const list = (value, allowed, flag) => {
    const items = String(value || '').split(',').map((s) => s.trim()).filter(Boolean);
    const bad = items.filter((s) => !allowed.includes(s));
    if (!items.length || bad.length) throw new Error(`${flag}: expected one or more of ${allowed.join(',')}`);
    return items;
  };
  for (let i = 0; i < argv.length; i++) {
    const flag = argv[i];
    if (flag === '--help' || flag === '-h') { opts.help = true; continue; }
    const takes = ['--states', '--viewports', '--base-url', '--root', '--json', '--screenshot-dir'];
    if (!takes.includes(flag)) throw new Error(`unknown option ${flag}`);
    const value = argv[++i];
    if (value === undefined || value.startsWith('--')) throw new Error(`${flag} needs a value`);
    if (flag === '--states') opts.states = list(value, STATES.concat(['matching', 'unknown']), flag);
    else if (flag === '--viewports') opts.viewports = list(value, Object.keys(VIEWPORTS), flag);
    else if (flag === '--base-url') opts.baseUrl = new URL(value).origin;
    else if (flag === '--root') opts.root = path.resolve(value);
    else if (flag === '--json') opts.json = path.resolve(value);
    else opts.screenshotDir = path.resolve(value);
  }
  return opts;
}

function packageVersion(name, resolve = require.resolve) {
  let dir = path.dirname(resolve(name));
  for (;;) {
    const file = path.join(dir, 'package.json');
    if (fs.existsSync(file)) {
      const pkg = JSON.parse(fs.readFileSync(file, 'utf8'));
      if (pkg.name === name) return pkg.version;
    }
    const up = path.dirname(dir);
    if (up === dir) throw new Error(`no package.json found for ${name}`);
    dir = up;
  }
}

function versionProblems(found, pinned = PINNED) {
  return Object.entries(pinned)
    .filter(([name, want]) => found[name] !== want)
    .map(([name, want]) => `${name} ${found[name] || 'missing'} found, ${want} required`);
}

function loadTools() {
  const playwright = require('playwright');
  const axe = require('axe-core');
  const found = {playwright: packageVersion('playwright'), 'axe-core': packageVersion('axe-core')};
  const problems = versionProblems(found);
  if (problems.length) throw new Error(`tool version mismatch: ${problems.join('; ')}`);
  return {chromium: playwright.chromium, axeSource: axe.source, versions: found};
}

// Compact axe results; incomplete nodes keep axe's own reason for review.
function summarizeAxe(results) {
  const nodeMessages = (node) => [...(node.any || []), ...(node.all || []), ...(node.none || [])]
    .map((check) => check.message).filter(Boolean);
  const pick = (rule, withReason) => ({
    id: rule.id, impact: rule.impact || null, help: rule.help,
    nodes: rule.nodes.map((node) => ({
      target: node.target.join(' '),
      html: String(node.html || '').slice(0, 200),
      ...(withReason ? {reason: [...new Set(nodeMessages(node))].join(' | ')} : {summary: node.failureSummary || ''}),
    })),
  });
  return {
    violations: results.violations.map((rule) => pick(rule, false)),
    incomplete: results.incomplete.map((rule) => pick(rule, true)),
    passes: results.passes.length,
  };
}

// A target passes when Tab reaches it, :focus-visible applies, an outline is
// drawn and the element is on screen.
function keyboardFailures(records) {
  return FOCUS_TARGETS.flatMap(({name}) => {
    const hit = records[name];
    if (!hit) return [`${name}: not reached within ${MAX_TABS} Tab presses`];
    const problems = [];
    if (!hit.focusVisible) problems.push(':focus-visible does not apply');
    if (hit.outlineStyle === 'none' || !(parseFloat(hit.outlineWidth) > 0)) problems.push('no visible outline');
    if (!hit.onScreen) problems.push('not scrolled into view');
    return problems.map((p) => `${name}: ${p}`);
  });
}

function exitCode(report) {
  if (report.errors.length || report.cases.some((c) => c.errors.length)) return EXIT.ERROR;
  const findings = report.cases.some((c) => c.violations.length || c.keyboard.failures.length);
  return findings ? EXIT.FINDINGS : EXIT.OK;
}

function formatReport(report) {
  const lines = [];
  for (const c of report.cases) {
    const status = c.errors.length ? 'ERROR' : c.violations.length || c.keyboard.failures.length ? 'FAIL' : 'ok';
    lines.push(`${status}  ${c.state} @ ${c.viewport} ${c.width}x${c.height}: ${c.violations.length} violations, `
      + `${c.incomplete.length} incomplete, ${c.passes} passed rules, ${c.expanded} sections expanded`);
    for (const e of c.errors) lines.push(`  error: ${e}`);
    for (const v of c.violations) {
      lines.push(`  violation ${v.id} (${v.impact}): ${v.help}`);
      for (const n of v.nodes) lines.push(`    ${n.target}`);
    }
    for (const r of c.incomplete) {
      lines.push(`  needs review ${r.id} (${r.impact}): ${r.nodes.length} node(s)`);
      for (const n of r.nodes) lines.push(`    ${n.target}: ${n.reason}`);
    }
    for (const f of c.keyboard.failures) lines.push(`  keyboard: ${f}`);
    for (const [name, hit] of Object.entries(c.keyboard.reached)) {
      lines.push(`  keyboard: ${name} after ${hit.tabs} Tab(s), outline ${hit.outlineStyle} ${hit.outlineWidth} ${hit.outlineColor}`);
    }
  }
  for (const e of report.errors) lines.push(`error: ${e}`);
  return lines.join('\n');
}

function freePort() {
  return new Promise((resolve, reject) => {
    const server = net.createServer();
    server.once('error', reject);
    server.listen(0, '127.0.0.1', () => {
      const {port} = server.address();
      server.close(() => resolve(port));
    });
  });
}

async function startFixture(root) {
  const script = path.join(root, 'scripts', 'codex-fixture.cjs');
  if (!fs.existsSync(script)) throw new Error(`fixture not found: ${script}`);
  const port = await freePort();
  const child = spawn(process.execPath, [script, String(port)], {stdio: ['ignore', 'pipe', 'pipe']});
  let stderr = '';
  child.stderr.on('data', (d) => { stderr += d; });
  const baseUrl = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error('fixture did not start within 10 s')), 10000);
    child.stdout.on('data', (d) => {
      const match = String(d).match(/http:\/\/127\.0\.0\.1:\d+/);
      if (match) { clearTimeout(timer); resolve(match[0]); }
    });
    child.once('exit', (code) => { clearTimeout(timer); reject(new Error(`fixture exited ${code}: ${stderr.trim()}`)); });
  });
  return {baseUrl, stop: () => child.kill()};
}

async function expandAll(page) {
  let total = 0;
  for (let round = 0; round < 5; round++) {
    const opened = await page.evaluate(() => {
      let n = 0;
      for (const el of document.querySelectorAll('button[aria-expanded="false"]')) {
        if (el.getClientRects().length && !el.disabled) { el.click(); n++; }
      }
      for (const el of document.querySelectorAll('details:not([open])')) {
        if (el.getClientRects().length) { el.open = true; n++; }
      }
      return n;
    });
    if (!opened) break;
    total += opened;
    await page.waitForTimeout(250);
  }
  return total;
}

async function checkKeyboard(page) {
  await page.evaluate(() => { document.activeElement?.blur(); window.scrollTo(0, 0); });
  const reached = {};
  let first = null;
  for (let tabs = 1; tabs <= MAX_TABS && Object.keys(reached).length < FOCUS_TARGETS.length; tabs++) {
    await page.keyboard.press('Tab');
    const state = await page.evaluate((targets) => {
      const el = document.activeElement;
      if (!el || el === document.body) return null;
      const css = getComputedStyle(el);
      const box = el.getBoundingClientRect();
      const target = targets.find((t) => el.matches(t.selector));
      return {
        key: el.outerHTML.slice(0, 120), name: target ? target.name : null,
        focusVisible: el.matches(':focus-visible'),
        outlineStyle: css.outlineStyle, outlineWidth: css.outlineWidth, outlineColor: css.outlineColor,
        onScreen: box.bottom > 0 && box.top < innerHeight && box.right > 0 && box.left < innerWidth,
      };
    }, FOCUS_TARGETS);
    if (!state) continue;
    if (first === null) first = state.key;
    else if (state.key === first) break;
    if (state.name && !reached[state.name]) reached[state.name] = {tabs, ...state};
  }
  const toggle = page.locator('#pulse-toggle');
  const before = await toggle.getAttribute('aria-expanded');
  await toggle.focus();
  await page.keyboard.press('Enter');
  const after = await toggle.getAttribute('aria-expanded');
  const keptFocus = await toggle.evaluate((el) => el === document.activeElement);
  await page.keyboard.press('Enter');
  const failures = keyboardFailures(reached);
  if (before === after) failures.push('Pulse toggle: Enter did not change aria-expanded');
  if (!keptFocus) failures.push('Pulse toggle: focus moved after Enter');
  return {reached, failures, pulseEnter: {before, after, keptFocus}};
}

// Include room for the link underline and focus outline outside the footer box.
async function screenshotFooter(page, file) {
  const clip = await page.locator('footer').evaluate((el) => {
    const r = el.getBoundingClientRect();
    const x = Math.max(0, r.left + scrollX - 8);
    const y = Math.max(0, r.top + scrollY - 8);
    const right = Math.min(document.documentElement.scrollWidth, r.right + scrollX + 8);
    const bottom = Math.min(document.documentElement.scrollHeight, r.bottom + scrollY + 8);
    return {x, y, width: right - x, height: bottom - y};
  });
  await page.screenshot({path: file, clip});
}

async function scanCase(browser, axeSource, baseUrl, state, viewport, screenshotDir) {
  const size = VIEWPORTS[viewport];
  const result = {state, viewport, ...size, errors: [], violations: [], incomplete: [], passes: 0, expanded: 0,
    keyboard: {reached: {}, failures: []}, screenshots: []};
  const context = await browser.newContext({viewport: size, deviceScaleFactor: 1});
  const page = await context.newPage();
  page.on('pageerror', (err) => result.errors.push(`page error: ${err.message}`));
  page.on('requestfailed', (req) => result.errors.push(`request failed: ${req.url()} ${req.failure()?.errorText || ''}`));
  page.on('response', (res) => { if (res.status() >= 400) result.errors.push(`HTTP ${res.status()}: ${res.url()}`); });
  try {
    await page.goto(`${baseUrl}/?state=${encodeURIComponent(state)}`, {waitUntil: 'load', timeout: 15000});
    await page.waitForFunction(() => document.querySelector('#generated b')
      && !/loading/.test(document.getElementById('usage-body').textContent), null, {timeout: 15000});
    const unreachable = await page.locator('#app').textContent();
    if (/fleet unreachable/.test(unreachable)) throw new Error(unreachable.trim());
    result.expanded = await expandAll(page);
    await page.addScriptTag({content: axeSource});
    const raw = await page.evaluate((tags) => window.axe.run(document, {runOnly: {type: 'tag', values: tags}}), TAGS);
    Object.assign(result, summarizeAxe(raw));
    if (screenshotDir) {
      const base = path.join(screenshotDir, `${state}-${viewport}`);
      await page.screenshot({path: `${base}-page.png`, fullPage: true});
      await page.locator('.pulse-section').screenshot({path: `${base}-pulse.png`});
      await screenshotFooter(page, `${base}-footer.png`);
      result.screenshots.push(`${base}-page.png`, `${base}-pulse.png`, `${base}-footer.png`);
    }
    result.keyboard = await checkKeyboard(page);
    if (screenshotDir) {
      const shot = path.join(screenshotDir, `${state}-${viewport}-footer-focus.png`);
      await page.locator('footer a').focus();
      await page.keyboard.press('Shift+Tab');
      await page.keyboard.press('Tab');
      await screenshotFooter(page, shot);
      result.screenshots.push(shot);
    }
  } catch (err) {
    result.errors.push(err.message.split('\n')[0]);
  } finally {
    await context.close();
  }
  return result;
}

async function run(opts, tools = loadTools) {
  const report = {tags: TAGS, versions: null, baseUrl: null, cases: [], errors: []};
  let fixture = null;
  let browser = null;
  try {
    const {chromium, axeSource, versions} = tools();
    report.versions = versions;
    if (opts.screenshotDir) fs.mkdirSync(opts.screenshotDir, {recursive: true});
    if (opts.baseUrl) report.baseUrl = opts.baseUrl;
    else { fixture = await startFixture(opts.root); report.baseUrl = fixture.baseUrl; }
    browser = await chromium.launch({headless: true});
    report.versions.browser = browser.version();
    for (const state of opts.states) {
      for (const viewport of opts.viewports) {
        report.cases.push(await scanCase(browser, axeSource, report.baseUrl, state, viewport, opts.screenshotDir));
      }
    }
  } catch (err) {
    report.errors.push(err.message.split('\n')[0]);
  } finally {
    if (browser) await browser.close().catch(() => {});
    if (fixture) fixture.stop();
  }
  return report;
}

async function main(argv = process.argv.slice(2), tools = loadTools, out = console) {
  let opts;
  try { opts = parseArgs(argv); } catch (err) { out.error(`${err.message}\n${USAGE}`); return EXIT.ERROR; }
  if (opts.help) { out.log(USAGE); return EXIT.OK; }
  const report = await run(opts, tools);
  if (opts.json) {
    try {
      fs.mkdirSync(path.dirname(opts.json), {recursive: true});
      fs.writeFileSync(opts.json, JSON.stringify(report, null, 2) + '\n');
    } catch (err) { report.errors.push(`could not write ${opts.json}: ${err.message}`); }
  }
  const versions = report.versions ? Object.entries(report.versions).map(([k, v]) => `${k} ${v}`).join(', ') : 'tools not loaded';
  out.log(`accessibility check: ${versions}; tags ${TAGS.join(', ')}`);
  out.log(formatReport(report));
  if (report.errors.some((e) => /Cannot find module '(playwright|axe-core)'|version mismatch/.test(e))) {
    out.log(`install the pinned tools: npm install --prefix <dir> playwright@${PINNED.playwright} axe-core@${PINNED['axe-core']}`
      + ', then run with NODE_PATH=<dir>/node_modules (see README)');
  }
  const code = exitCode(report);
  out.log(code === EXIT.OK ? 'result: no violations' : code === EXIT.FINDINGS ? 'result: FAILED (violations or keyboard)' : 'result: ERROR (check did not complete)');
  return code;
}

if (require.main === module) {
  main().then((code) => { process.exitCode = code; }, (err) => {
    console.error(`error: ${err.stack || err}`);
    process.exitCode = EXIT.ERROR;
  });
}

module.exports = {PINNED, TAGS, STATES, VIEWPORTS, EXIT, parseArgs, packageVersion, versionProblems, summarizeAxe,
  keyboardFailures, exitCode, formatReport, main};
