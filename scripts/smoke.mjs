// Dev smoke test: drives the stdio MCP server through the real lifecycle
// (initialize -> initialized -> tools/list) and asserts the tool list is
// well-formed. Run via `npm run smoke` or `just smoke`. Not published.
//
// Builds the binary if it is missing, so this works as a one-liner in CI on
// both Linux and Windows runners.
import { spawnSync, spawn } from 'node:child_process';
import { existsSync } from 'node:fs';

const exe = process.platform === 'win32' ? './win-rdp-mcp.exe' : './win-rdp-mcp';

if (!existsSync(exe)) {
  const build = spawnSync('go', ['build', '-o', exe, '.'], { stdio: 'inherit' });
  if (build.status !== 0) {
    console.error('smoke FAIL: go build failed');
    process.exit(1);
  }
}

// -enable-all so the smoke covers every registered schema, not just tiers 1-2.
const proc = spawn(exe, ['-transport', 'stdio', '-enable-all'], { stdio: ['pipe', 'pipe', 'inherit'] });

const send = (msg) => proc.stdin.write(JSON.stringify(msg) + '\n');
const fail = (m) => {
  console.error('smoke FAIL:', m);
  proc.kill();
  process.exit(1);
};

const timer = setTimeout(() => fail('timed out waiting for tools/list response'), 15000);

send({ jsonrpc: '2.0', id: 1, method: 'initialize', params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'smoke', version: '0' } } });
send({ jsonrpc: '2.0', method: 'notifications/initialized' });
send({ jsonrpc: '2.0', id: 2, method: 'tools/list', params: {} });
// Wait is the one tool that does something real on every platform, so it also
// proves the dispatch path and the task-id stamping end to end.
send({ jsonrpc: '2.0', id: 3, method: 'tools/call', params: { name: 'Wait', arguments: { seconds: 0.05 } } });

const seen = new Map();
let buf = '';
proc.stdout.on('data', (chunk) => {
  buf += chunk;
  let nl;
  while ((nl = buf.indexOf('\n')) >= 0) {
    const line = buf.slice(0, nl).trim();
    buf = buf.slice(nl + 1);
    if (!line) continue;
    let r;
    try { r = JSON.parse(line); } catch { continue; }
    if (r.id === undefined) continue;
    seen.set(r.id, r);

    if (seen.has(2) && seen.has(3)) {
      clearTimeout(timer);

      const list = seen.get(2);
      if (list.error) fail('tools/list error: ' + list.error.message);
      const tools = list.result?.tools;
      if (!Array.isArray(tools) || !tools.length) fail('no tools in response');
      const bad = tools.filter((t) => !t.name || !t.inputSchema);
      if (bad.length) fail('malformed tools: ' + bad.map((t) => t.name ?? '(unnamed)').join(', '));

      const call = seen.get(3);
      if (call.error) fail('tools/call error: ' + call.error.message);
      const text = call.result?.content?.[0]?.text ?? '';
      if (call.result?.isError) fail('Wait returned an error result: ' + text);
      if (!text.startsWith('[task:')) fail('tool result is missing its task id: ' + text);

      console.log(`smoke OK — ${tools.length} tools, Wait returned ${JSON.stringify(text)}`);
      proc.kill();
      process.exit(0);
    }
  }
});

proc.on('exit', (code) => fail('server exited early (code ' + code + ')'));
