// VS Code remote authority resolver for k8s-pod-ssh-relay.
//
// Like the Remote - Tunnels extension, it returns a ManagedResolvedAuthority:
// VS Code gets its connections from makeConnection() instead of a local TCP
// port. Each connection is an SSH direct-tcpip channel to the VS Code server
// inside the pod, over an SSH session that runs on the relay protocol
// implemented in relay.ts (no relay-client binary).
import { promises as fs } from 'fs';
import * as os from 'os';
import * as path from 'path';
import * as vscode from 'vscode';
import { ClientChannel } from 'ssh2';
import { loadSigner } from './keys';
import { Logger } from './log';
import { ensureServer, Product } from './server';
import { Session, SessionOptions } from './session';

const AUTHORITY = 'k8s-pod-ssh-relay';

// A remote authority can't contain '/', so namespace/pod is encoded as
// <pod>.<namespace> (namespaces can't contain dots; pod names can).
function toAuthority(namespace: string, pod: string): string {
    return `${AUTHORITY}+${pod}.${namespace}`;
}

function parseAuthority(authority: string): { namespace: string; pod: string } {
    const host = authority.slice(AUTHORITY.length + 1);
    const i = host.lastIndexOf('.');
    if (i <= 0 || i === host.length - 1) {
        throw vscode.RemoteAuthorityResolverError.NotAvailable(`invalid authority ${authority}: want ${AUTHORITY}+<pod>.<namespace>`);
    }
    return { pod: host.slice(0, i), namespace: host.slice(i + 1) };
}

export function activate(context: vscode.ExtensionContext) {
    const output = vscode.window.createOutputChannel('Remote - PodRelay', { log: true });
    const log: Logger = {
        trace: (m) => output.trace(m),
        info: (m) => output.info(m),
        warn: (m) => output.warn(m),
        error: (m) => output.error(m),
    };
    const resolver = new PodResolver(context, log);

    context.subscriptions.push(
        output,
        resolver,
        vscode.workspace.registerRemoteAuthorityResolver(AUTHORITY, resolver),
        vscode.commands.registerCommand('k8sPodSshRelay.connect', () => connect(false)),
        vscode.commands.registerCommand('k8sPodSshRelay.connectCurrentWindow', () => connect(true)),
        vscode.commands.registerCommand('k8sPodSshRelay.showLog', () => output.show()),
    );
}

export function deactivate() {}

const TARGET_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?\/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;

async function connect(reuseWindow: boolean) {
    const target = await vscode.window.showInputBox({
        title: 'Connect to Pod',
        prompt: 'namespace/pod of a pod reachable through the relay',
        placeHolder: 'namespace/pod',
        validateInput: (v) => (TARGET_RE.test(v.trim()) ? undefined : 'Expected namespace/pod, e.g. default/my-pod-7d9c'),
    });
    if (target) {
        const [namespace, pod] = target.trim().split('/');
        await vscode.commands.executeCommand('vscode.newWindow', { remoteAuthority: toAuthority(namespace, pod), reuseWindow });
    }
}

class PodResolver implements vscode.RemoteAuthorityResolver, vscode.Disposable {
    private session: Promise<Session> | undefined;
    private sessionOptions: SessionOptions | undefined;
    private labelFormatter: vscode.Disposable | undefined;

    constructor(private readonly context: vscode.ExtensionContext, private readonly log: Logger) {}

    resolve(authority: string, context: vscode.RemoteAuthorityResolverContext): Thenable<vscode.ResolverResult> {
        const { namespace, pod } = parseAuthority(authority);
        const target = `${namespace}/${pod}`;
        this.log.info(`resolving ${target} (attempt ${context.resolveAttempt})`);
        // Shown in the remote indicator and window title, like "SSH: host".
        this.labelFormatter?.dispose();
        this.labelFormatter = vscode.workspace.registerResourceLabelFormatter({
            scheme: 'vscode-remote',
            authority: `${AUTHORITY}+*`,
            formatting: { label: '${path}', separator: '/', tildify: true, workspaceSuffix: `Pod: ${target}` },
        });
        return vscode.window.withProgress(
            { location: vscode.ProgressLocation.Notification, title: `Pod ${target}` },
            async (progress) => {
                try {
                    this.sessionOptions = await this.loadOptions(target);
                    const product = await readProduct();
                    const setup = async () => ensureServer(await this.getSession(), product, {
                        cacheDir: this.context.globalStorageUri.fsPath,
                        downloadUrlTemplate: config().get<string>('serverDownloadUrlTemplate') || undefined,
                        log: this.log,
                        report: (message) => progress.report({ message }),
                    });
                    progress.report({ message: 'Connecting through relay' });
                    let server;
                    try {
                        server = await setup();
                    } catch (e) {
                        // A reused session may have died without us noticing yet
                        // (e.g. the relay restarted); retry once on a fresh one.
                        if (!this.session) {
                            throw e;
                        }
                        this.log.warn(`retrying on a new session: ${e instanceof Error ? e.message : e}`);
                        this.dropSession();
                        server = await setup();
                    }
                    return new vscode.ManagedResolvedAuthority(() => this.makeConnection(server.port), server.connectionToken);
                } catch (e) {
                    const msg = e instanceof Error ? e.message : String(e);
                    this.log.error(`resolve failed: ${msg}`);
                    this.session = undefined;
                    // Authorization and configuration problems won't fix themselves on retry.
                    if (/access denied|unauthorized|authentication|not configured|ENOENT|passphrase/i.test(msg)) {
                        throw vscode.RemoteAuthorityResolverError.NotAvailable(msg);
                    }
                    throw vscode.RemoteAuthorityResolverError.TemporarilyNotAvailable(msg);
                }
            },
        );
    }

    /** Called by VS Code for every connection it needs (management, extension host, reconnects). */
    private async makeConnection(port: number): Promise<vscode.ManagedMessagePassing> {
        const session = await this.getSession();
        const ch = await session.openChannel('127.0.0.1', port);
        this.log.trace(`opened channel to server port ${port}`);
        return toMessagePassing(ch);
    }

    /** Shares one SSH session; reconnects through the relay if it dropped. */
    private async getSession(): Promise<Session> {
        const current = this.session && (await this.session.catch(() => undefined));
        if (current?.isOpen) {
            return current;
        }
        if (!this.sessionOptions) {
            throw new Error('not resolved yet');
        }
        this.session = Session.open(this.sessionOptions);
        this.session.catch(() => (this.session = undefined));
        return this.session;
    }

    private async loadOptions(target: string): Promise<SessionOptions> {
        const c = config();
        const url = c.get<string>('relayUrl');
        if (!url) {
            throw new Error('k8sPodSshRelay.relayUrl is not configured');
        }
        return {
            url,
            target,
            user: c.get<string>('user') || 'root',
            connectTo: c.get<string>('connectTo') || undefined,
            signer: await loadSigner(untildify(c.get<string>('identityFile') || '~/.ssh/id_ed25519')),
            log: this.log,
        };
    }

    private dropSession() {
        this.session?.then((s) => s.close(), () => undefined);
        this.session = undefined;
    }

    dispose() {
        this.dropSession();
        this.labelFormatter?.dispose();
    }
}

function toMessagePassing(ch: ClientChannel): vscode.ManagedMessagePassing {
    const onMessage = new vscode.EventEmitter<Uint8Array>();
    const onClose = new vscode.EventEmitter<Error | undefined>();
    const onEnd = new vscode.EventEmitter<void>();
    let closed = false;
    const close = (err?: Error) => {
        if (!closed) {
            closed = true;
            onClose.fire(err);
        }
    };
    ch.on('data', (d: Buffer) => onMessage.fire(d));
    ch.on('end', () => onEnd.fire());
    ch.on('error', (err: Error) => close(err));
    ch.on('close', () => close());
    return {
        onDidReceiveMessage: onMessage.event,
        onDidClose: onClose.event,
        onDidEnd: onEnd.event,
        send: (data) => {
            ch.write(Buffer.from(data.buffer, data.byteOffset, data.byteLength));
        },
        end: () => ch.end(),
        drain: () => new Promise<void>((resolve) => (ch.writableNeedDrain ? ch.once('drain', () => resolve()) : resolve())),
    };
}

async function readProduct(): Promise<Product> {
    const product = JSON.parse(await fs.readFile(path.join(vscode.env.appRoot, 'product.json'), 'utf8'));
    return { ...product, version: vscode.version.replace(/-insider$/, '') };
}

function config() {
    return vscode.workspace.getConfiguration('k8sPodSshRelay');
}

function untildify(p: string): string {
    return p.startsWith('~/') ? path.join(os.homedir(), p.slice(2)) : p;
}
