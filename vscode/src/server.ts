// Installs and starts the VS Code server (REH) inside the pod over SSH.
//
// The server tarball is downloaded on the client and uploaded through the SSH
// session, so pods don't need internet access or curl/wget, only tar.
import { createReadStream, createWriteStream, promises as fs } from 'fs';
import * as path from 'path';
import { Readable } from 'stream';
import { pipeline } from 'stream/promises';
import { Logger } from './log';
import { Session } from './session';

/** The parts of the local editor's product.json the server must match. */
export interface Product {
    commit: string;
    quality: string;
    version: string;
    release?: string;
    serverApplicationName: string;
    serverDataFolderName: string;
    serverDownloadUrlTemplate?: string;
}

export interface ServerInfo {
    port: number;
    connectionToken: string;
}

// Microsoft builds; VSCodium ships its own template in product.json.
const DEFAULT_TEMPLATE = 'https://update.code.visualstudio.com/commit:${commit}/server-${platform}/${quality}';

interface Platform {
    os: 'linux' | 'alpine'; // VSCodium naming
    arch: 'x64' | 'arm64' | 'armhf';
    platform: string; // Microsoft naming, e.g. linux-x64, alpine-arm64
}

export async function ensureServer(
    session: Session,
    product: Product,
    opts: { cacheDir: string; downloadUrlTemplate?: string; log: Logger; report?: (msg: string) => void },
): Promise<ServerInfo> {
    const { log } = opts;
    const report = opts.report ?? (() => undefined);
    const args = [product.serverDataFolderName, product.commit, product.serverApplicationName];

    report('Detecting pod platform');
    const detect = await run(session, DETECT_SCRIPT, args);
    const [uname, machine, libc] = detect.split(/\s+/);
    if (uname !== 'Linux') {
        throw new Error(`unsupported pod OS: ${uname}`);
    }
    const platform = toPlatform(machine, libc);
    log.info(`pod platform: ${machine} ${libc} -> ${platform.platform}`);

    if (!detect.includes('installed')) {
        const url = expand(opts.downloadUrlTemplate || product.serverDownloadUrlTemplate || DEFAULT_TEMPLATE, product, platform);
        report('Downloading VS Code server');
        const tarball = await download(url, path.join(opts.cacheDir, `${product.commit}-${platform.platform}.tar.gz`), log);
        report('Uploading VS Code server to the pod');
        log.info(`uploading ${tarball}`);
        await run(session, UPLOAD_SCRIPT, args, createReadStream(tarball));
    } else {
        log.info(`server ${product.commit} already installed in the pod`);
    }

    report('Starting VS Code server');
    const out = await run(session, START_SCRIPT, args);
    const port = parseInt(/^port=(\d+)$/m.exec(out)?.[1] ?? '', 10);
    const connectionToken = /^token=(\S+)$/m.exec(out)?.[1];
    if (!port || !connectionToken) {
        throw new Error(`could not parse server start output: ${out}`);
    }
    log.info(`server listening on 127.0.0.1:${port} inside the pod`);
    return { port, connectionToken };
}

async function run(session: Session, script: string, args: string[], stdin?: Readable): Promise<string> {
    const cmd = ['sh', '-c', script, 'sh', ...args].map(shQuote).join(' ');
    const res = await session.exec(cmd, stdin);
    if (res.code !== 0) {
        throw new Error(`remote script failed (exit ${res.code}): ${res.stderr.trim() || res.stdout.trim()}`);
    }
    return res.stdout;
}

function toPlatform(machine: string, libc: string): Platform {
    const arch = ({ x86_64: 'x64', amd64: 'x64', aarch64: 'arm64', arm64: 'arm64', armv7l: 'armhf', armv8l: 'armhf' } as const)[machine];
    if (!arch) {
        throw new Error(`unsupported pod architecture: ${machine}`);
    }
    if (libc === 'musl') {
        if (arch === 'armhf') {
            throw new Error('no VS Code server build for musl on armhf');
        }
        return { os: 'alpine', arch, platform: arch === 'x64' ? 'linux-alpine' : 'alpine-arm64' };
    }
    return { os: 'linux', arch, platform: `linux-${arch}` };
}

function expand(template: string, product: Product, p: Platform): string {
    const vars: Record<string, string> = {
        commit: product.commit, quality: product.quality, version: product.version, release: product.release ?? '',
        os: p.os, arch: p.arch, platform: p.platform,
    };
    return template.replace(/\$\{(\w+)\}/g, (m, k: string) => vars[k] ?? m);
}

async function download(url: string, dest: string, log: Logger): Promise<string> {
    try {
        await fs.access(dest);
        log.info(`using cached ${dest}`);
        return dest;
    } catch {
        // not cached yet
    }
    log.info(`downloading ${url}`);
    const res = await fetch(url);
    if (!res.ok || !res.body) {
        throw new Error(`download ${url}: ${res.status} ${res.statusText}`);
    }
    await fs.mkdir(path.dirname(dest), { recursive: true });
    const tmp = `${dest}.${process.pid}.tmp`;
    await pipeline(Readable.fromWeb(res.body as import('stream/web').ReadableStream), createWriteStream(tmp));
    await fs.rename(tmp, dest);
    return dest;
}

function shQuote(s: string): string {
    return `'${s.replace(/'/g, `'\\''`)}'`;
}

// Positional args for all scripts: $1 = data folder, $2 = commit, $3 = server binary name.

const DETECT_SCRIPT = `
printf '%s %s ' "$(uname -s)" "$(uname -m)"
if [ -f /etc/alpine-release ] || ldd --version 2>&1 | grep -qi musl; then printf 'musl '; else printf 'glibc '; fi
if [ -x "$HOME/$1/bin/$2/bin/$3" ]; then echo installed; else echo missing; fi
`;

// Reads the tarball from stdin. MS tarballs have one top-level directory,
// VSCodium's have none; handle both.
const UPLOAD_SCRIPT = `
set -e
BASE="$HOME/$1/bin"; DIR="$BASE/$2"
mkdir -p "$BASE"
TMP=$(mktemp -d "$BASE/.upload.XXXXXX")
trap 'rm -rf "$TMP"' EXIT
tar -xzf - -C "$TMP"
set -- "$TMP"/*
rm -rf "$DIR"
if [ $# -eq 1 ] && [ -d "$1" ]; then mv "$1" "$DIR"; else mkdir "$DIR" && mv "$TMP"/* "$DIR"/; fi
`;

// Reuses a running server for this commit, otherwise starts one; prints its
// port and connection token.
const START_SCRIPT = `
DIR="$HOME/$1/bin/$2"; STATE="$HOME/$1/relay-$2"
mkdir -p "$STATE"
TOKEN="$STATE/token"; LOG="$STATE/server.log"; PIDF="$STATE/server.pid"
if [ ! -s "$TOKEN" ]; then (umask 077; od -An -N16 -tx1 /dev/urandom | tr -d ' \\n' > "$TOKEN"); fi
running() { [ -f "$PIDF" ] && kill -0 "$(cat "$PIDF")" 2>/dev/null; }
port() { sed -n 's/.*Extension host agent listening on \\([0-9][0-9]*\\).*/\\1/p' "$LOG" 2>/dev/null | tail -n 1; }
if ! running; then
  : > "$LOG"
  nohup "$DIR/bin/$3" --start-server --host=127.0.0.1 --port=0 --connection-token-file "$TOKEN" \\
    --telemetry-level off --enable-remote-auto-shutdown --accept-server-license-terms >> "$LOG" 2>&1 < /dev/null &
  echo $! > "$PIDF"
fi
i=0; P=""
while [ $i -lt 150 ]; do
  P=$(port); [ -n "$P" ] && break
  running || break
  sleep 0.2; i=$((i+1))
done
if [ -z "$P" ]; then echo "VS Code server did not start; last log lines:" >&2; tail -n 30 "$LOG" >&2; exit 1; fi
echo "port=$P"
echo "token=$(cat "$TOKEN")"
`;
