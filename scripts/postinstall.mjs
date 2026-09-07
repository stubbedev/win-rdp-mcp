// Best-effort download of the prebuilt binary at install time. Failures are not
// fatal — the CLI launcher retries the download on first run — so installs in
// offline/CI environments still succeed.
import { ensureBinary } from './download.mjs';

if (process.env.WIN_RDP_MCP_SKIP_DOWNLOAD === '1') {
  process.exit(0);
}

try {
  await ensureBinary();
} catch (err) {
  console.error(`[win-rdp-mcp] postinstall: ${err.message}`);
  console.error('[win-rdp-mcp] The binary will be fetched on first run instead.');
}
