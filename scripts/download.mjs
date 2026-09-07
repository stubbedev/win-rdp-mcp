// Resolves (and, if needed, downloads) the prebuilt Go binary that matches the
// current platform. Shared by the postinstall script and the CLI launcher so a
// failed install (e.g. offline) self-heals on first run.
import { createWriteStream, readFileSync } from 'node:fs';
import { chmod, mkdir, rename, rm, stat } from 'node:fs/promises';
import { Readable } from 'node:stream';
import { pipeline } from 'node:stream/promises';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const here = dirname(fileURLToPath(import.meta.url));
const root = join(here, '..');
const binDir = join(root, 'bin');

const pkg = JSON.parse(readFileSync(join(root, 'package.json'), 'utf-8'));
const REPO = 'stubbedev/win-rdp-mcp';

// Map Node's platform/arch onto the Go release asset naming. Anything not
// listed falls back to the "go install" hint in target().
const PLATFORMS = {
  'win32:x64': { os: 'windows', arch: 'amd64', ext: '.exe' },
  'win32:arm64': { os: 'windows', arch: 'arm64', ext: '.exe' },
  'win32:ia32': { os: 'windows', arch: '386', ext: '.exe' },
  'linux:x64': { os: 'linux', arch: 'amd64', ext: '' },
  'linux:arm64': { os: 'linux', arch: 'arm64', ext: '' },
  'linux:arm': { os: 'linux', arch: 'arm', ext: '' },
  'darwin:x64': { os: 'darwin', arch: 'amd64', ext: '' },
  'darwin:arm64': { os: 'darwin', arch: 'arm64', ext: '' },
};

function target() {
  const key = `${process.platform}:${process.arch}`;
  const t = PLATFORMS[key];
  if (!t) {
    throw new Error(
      `Unsupported platform ${key}. Build from source with: go install github.com/${REPO}@latest`,
    );
  }
  return t;
}

export function binaryPath() {
  const { ext } = target();
  return join(binDir, `win-rdp-mcp-native${ext}`);
}

function assetUrl() {
  const { os, arch, ext } = target();
  return `https://github.com/${REPO}/releases/download/v${pkg.version}/win-rdp-mcp_${os}_${arch}${ext}`;
}

async function exists(path) {
  try {
    const s = await stat(path);
    return s.isFile() && s.size > 0;
  } catch {
    return false;
  }
}

// ensureBinary returns the path to the platform binary, downloading it from the
// matching GitHub release if it is not already present.
export async function ensureBinary() {
  const dest = binaryPath();
  if (await exists(dest)) return dest;

  await mkdir(binDir, { recursive: true });
  const url = assetUrl();
  const res = await fetch(url, { redirect: 'follow' });
  if (!res.ok || !res.body) {
    throw new Error(`Failed to download ${url}: HTTP ${res.status}`);
  }
  const tmp = `${dest}.download`;
  await pipeline(Readable.fromWeb(res.body), createWriteStream(tmp));
  await chmod(tmp, 0o755).catch(() => {});
  await rm(dest, { force: true });
  await rename(tmp, dest);
  return dest;
}
