// An SSH session to a pod's sshd, running over the relay stream.
import { Readable } from 'stream';
import { Client, ClientChannel } from 'ssh2';
import { Logger } from './log';
import { connectRelay, RelayOptions } from './relay';

export interface SessionOptions extends RelayOptions {
    /** Login user inside the pod. */
    user: string;
}

export interface ExecResult {
    code: number | null;
    stdout: string;
    stderr: string;
}

export class Session {
    private closed = false;

    private constructor(private readonly client: Client, private readonly log: Logger) {
        const markClosed = () => {
            if (!this.closed) {
                this.closed = true;
                log.info('ssh: connection closed');
            }
        };
        client.on('end', markClosed);
        client.on('close', markClosed);
        client.on('error', (err) => log.error(`ssh: ${err.message}`));
    }

    static async open(opts: SessionOptions): Promise<Session> {
        const sock = await connectRelay(opts);
        const client = new Client();
        await new Promise<void>((resolve, reject) => {
            client.once('ready', resolve);
            client.once('error', reject);
            client.connect({
                sock,
                username: opts.user,
                ...opts.signer.sshAuth,
                // Pods generate new host keys on every start; the relay
                // already authenticated the target by name over TLS.
                hostVerifier: () => true,
                readyTimeout: opts.timeoutMs ?? 30000,
                keepaliveInterval: 15000,
            });
        });
        opts.log.info(`ssh: logged in as ${opts.user}`);
        return new Session(client, opts.log);
    }

    get isOpen(): boolean {
        return !this.closed;
    }

    /** Runs a command, optionally feeding stdin, and collects its output. */
    exec(command: string, stdin?: Readable | string): Promise<ExecResult> {
        return new Promise((resolve, reject) => {
            this.client.exec(command, (err, ch) => {
                if (err) {
                    return reject(err);
                }
                let stdout = '';
                let stderr = '';
                ch.on('data', (d: Buffer) => (stdout += d.toString()));
                ch.stderr.on('data', (d: Buffer) => (stderr += d.toString()));
                ch.on('close', (code: number | null) => resolve({ code, stdout, stderr }));
                if (stdin === undefined) {
                    ch.end();
                } else if (typeof stdin === 'string') {
                    ch.end(stdin);
                } else {
                    stdin.on('error', () => ch.destroy());
                    stdin.pipe(ch);
                }
            });
        });
    }

    /** Opens a direct-tcpip channel to host:port as seen from inside the pod. */
    openChannel(host: string, port: number): Promise<ClientChannel> {
        return new Promise((resolve, reject) =>
            this.client.forwardOut('127.0.0.1', 0, host, port, (err, ch) => (err ? reject(err) : resolve(ch))));
    }

    close(): void {
        this.client.end();
    }
}
