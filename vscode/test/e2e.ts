// End-to-end check without VS Code: relay (TS) -> ssh -> install/start the
// VS Code server in the pod -> HTTP GET /version over a direct-tcpip channel.
//
//   node dist/e2e.js <pod> [user]
// env: RELAY_URL, RELAY_CA, RELAY_CONNECT_TO, IDENTITY_FILE, VSCODE_APP_ROOT
import { promises as fs } from 'fs';
import * as os from 'os';
import * as path from 'path';
import { loadSigner } from '../src/keys';
import { consoleLogger as log } from '../src/log';
import { ensureServer, Product } from '../src/server';
import { Session } from '../src/session';

async function main() {
    const [pod, user = 'demo'] = process.argv.slice(2);
    const appRoot = process.env.VSCODE_APP_ROOT ?? '/Applications/Visual Studio Code.app/Contents/Resources/app';
    const product = JSON.parse(await fs.readFile(path.join(appRoot, 'product.json'), 'utf8')) as Product;
    product.version = JSON.parse(await fs.readFile(path.join(appRoot, 'package.json'), 'utf8')).version;

    const t0 = Date.now();
    const signer = await loadSigner(process.env.IDENTITY_FILE ?? path.join(os.homedir(), '.ssh/id_ed25519'));
    const session = await Session.open({
        url: process.env.RELAY_URL!,
        target: pod,
        signer,
        ca: process.env.RELAY_CA ? await fs.readFile(process.env.RELAY_CA) : undefined,
        connectTo: process.env.RELAY_CONNECT_TO,
        user,
        log,
    });
    const info = await ensureServer(session, product, {
        cacheDir: path.join(os.tmpdir(), 'k8s-pod-ssh-relay-cache'),
        log,
        report: (m) => log.info(`progress: ${m}`),
    });
    const ch = await session.openChannel('127.0.0.1', info.port);
    const body = await new Promise<string>((resolve) => {
        let buf = '';
        ch.on('data', (d: Buffer) => (buf += d.toString()));
        ch.on('close', () => resolve(buf));
        ch.end(`GET /version HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n`);
    });
    log.info(`GET /version ->\n${body.split('\r\n').filter((l) => /^HTTP|^\w{40}$/.test(l)).join('\n')}`);
    log.info(`local commit: ${product.commit}; total ${((Date.now() - t0) / 1000).toFixed(1)}s`);
    session.close();
}

main().catch((e) => {
    log.error(e instanceof Error ? e.stack ?? e.message : String(e));
    process.exit(1);
});
