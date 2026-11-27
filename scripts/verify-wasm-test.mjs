// Checks the browser verifier (cmd/verify-wasm) against the command-line one
// on every compatibility fixture: each proof bundle any release made, with
// its trusted keys, without them, and tampered with. The two must give the
// same report, and every untouched fixture must verify.
//
//   node scripts/verify-wasm-test.mjs VERIFY.wasm WASM_EXEC.js BACKUPPROOF_BINARY
import { readFile, readdir } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { join, basename, dirname } from 'node:path';
import { execFileSync } from 'node:child_process';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { pathToFileURL } from 'node:url';

const [wasmPath, execJs, cli] = process.argv.slice(2);
if (!cli) {
  console.error('usage: node scripts/verify-wasm-test.mjs VERIFY.wasm WASM_EXEC.js BACKUPPROOF_BINARY');
  process.exit(2);
}
const repo = join(dirname(new URL(import.meta.url).pathname.replace(/^\/([A-Za-z]:)/, '$1')), '..');

// Start the verifier the way the page does.
await import(pathToFileURL(execJs).href);
const ready = new Promise((resolve) => { globalThis.backupproofReady = resolve; });
const go = new globalThis.Go();
const { instance } = await WebAssembly.instantiate(await readFile(wasmPath), go.importObject);
go.run(instance);
await ready;

const tmp = mkdtempSync(join(tmpdir(), 'bp-wasm-'));
function viaCLI(bundle, keys) {
  const file = join(tmp, 'b.json');
  writeFileSync(file, bundle);
  const args = ['proof', 'verify', '--json', ...keys.flatMap((k) => ['--key', k]), file];
  try {
    return { report: JSON.parse(execFileSync(cli, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] })) };
  } catch (e) {
    // An invalid proof exits 1 after printing its report; a bundle it can't
    // read prints no report at all.
    if (e.stdout && e.stdout.trim().startsWith('{')) return { report: JSON.parse(e.stdout) };
    return { error: (e.stderr || e.message).trim() };
  }
}
function viaWasm(bundle, keys) {
  return JSON.parse(globalThis.backupproofVerify(bundle, keys.join('\n'), false));
}

// Fixtures: proofs from the command-line tool (key.txt) and from a dashboard
// (keys.json), one folder per release.
const cases = [];
for (const base of ['internal/proof/testdata/compat', 'internal/server/testdata/compat']) {
  for (const tag of (await readdir(join(repo, base))).sort()) {
    const dir = join(repo, base, tag);
    let keys = [];
    if (existsSync(join(dir, 'key.txt'))) keys = [(await readFile(join(dir, 'key.txt'), 'utf8')).trim()];
    if (existsSync(join(dir, 'keys.json'))) {
      keys = JSON.parse(await readFile(join(dir, 'keys.json'), 'utf8')).bpkeys.split('\n').map((s) => s.trim()).filter(Boolean);
    }
    for (const f of (await readdir(dir)).filter((f) => f.endsWith('.bundle.json')).sort()) {
      cases.push({ name: `${base.split('/')[1]} ${tag} ${f}`, file: join(dir, f), keys });
    }
  }
}

// Tampering: change one character inside the signed payload.
function tamper(raw) {
  const b = JSON.parse(raw);
  const env = b.envelope || b.Envelope;
  const p = env.payload;
  const i = Math.floor(p.length / 2);
  env.payload = p.slice(0, i) + (p[i] === 'A' ? 'B' : 'A') + p.slice(i + 1);
  return JSON.stringify(b);
}

const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
let failures = 0, checked = 0;
for (const c of cases) {
  const raw = await readFile(c.file, 'utf8');
  for (const [label, bundle, keys, mustBeValid] of [
    ['trusted keys', raw, c.keys, true],
    ['embedded keys', raw, [], null],
    ['tampered', tamper(raw), c.keys, false],
  ]) {
    const w = viaWasm(bundle, keys);
    const k = viaCLI(bundle, keys);
    checked++;
    const problems = [];
    if (w.report && k.report) {
      if (!same(w.report, k.report)) problems.push(`reports differ:\n  wasm ${JSON.stringify(w.report)}\n  cli  ${JSON.stringify(k.report)}`);
    } else if (!!w.report !== !!k.report) {
      problems.push(`one verifier read the bundle and the other didn't: wasm ${JSON.stringify(w)} cli ${JSON.stringify(k)}`);
    }
    const valid = w.report ? w.report.valid : false;
    if (mustBeValid === true && !valid) problems.push(`doesn't verify in the browser: ${JSON.stringify(w)}`);
    if (mustBeValid === false && valid) problems.push('a tampered proof verified');
    if (problems.length) {
      failures++;
      console.log(`FAIL ${c.name} (${label})\n  ${problems.join('\n  ')}`);
    }
  }
}
rmSync(tmp, { recursive: true, force: true });
console.log(`${checked} checks on ${cases.length} proofs from ${new Set(cases.map((c) => c.name.split(' ').slice(0, 2).join(' '))).size} release fixtures: ${failures ? failures + ' failed' : 'browser and command line agree'}`);
process.exit(failures ? 1 : 0);
