// A WebSocket client: websocket-driver speaks RFC 6455 over a net/tls socket
// that we dial ourselves.
//
// We don't use http(s).request (or the `ws` package, which builds on it):
// VS Code's extension host patches http/https for proxy support and replaces
// the request agent, which would bypass connectTo and send the relay
// connection through the system proxy.
import { EventEmitter } from 'events';
import * as net from 'net';
import * as tls from 'tls';
import * as websocket from 'websocket-driver';

/** Matches the relay's read limit (wsconn.ReadLimit in the Go code). */
const MAX_MESSAGE = 1 << 20;

export interface WebSocketOptions {
    /** Dial this host:port instead of the URL host; Host header and TLS name are unchanged. */
    connectTo?: string;
    timeoutMs?: number;
}

export class HandshakeError extends Error {}

/**
 * Events: 'message' (data: Buffer, isBinary: boolean), 'close', 'error' (Error).
 *
 * Events are held until start() is called, so none is emitted before the
 * caller has attached its listeners.
 */
export class WebSocketConn extends EventEmitter {
    // Events raised before start(), e.g. the relay's challenge arriving in the
    // same TCP segment as the 101 response.
    private pending: [string, unknown[]][] | null = [];
    // Collects the frames written during send() so they go out as one write.
    private out: Buffer[] | null = null;

    constructor(private readonly sock: net.Socket, private readonly driver: websocket.Driver) {
        super();
        sock.on('data', (d: Buffer) => driver.io.write(d));
        sock.on('close', () => this.deliver('close'));
        sock.on('error', (e) => this.deliver('error', e));
        driver.io.on('data', (chunk: Buffer) => (this.out ? this.out.push(chunk) : sock.write(chunk)));
        driver.on('message', (e: { data: string | Buffer }) =>
            typeof e.data === 'string' ? this.deliver('message', Buffer.from(e.data), false) : this.deliver('message', e.data, true));
        driver.on('error', (e: Error) => this.deliver('error', e));
        // The handshake failed, or the close handshake finished (the driver
        // has already answered the relay's close frame).
        driver.on('close', () => sock.end());
    }

    /** Starts delivering events; call after attaching listeners. */
    start(): void {
        const pending = this.pending;
        if (!pending) {
            return;
        }
        this.pending = null;
        for (const [event, args] of pending) {
            this.emit(event, ...args);
        }
        this.sock.resume();
    }

    send(data: Buffer | string, binary: boolean, cb?: (err?: Error | null) => void): void {
        // Without extensions the driver frames synchronously.
        const out: Buffer[] = (this.out = []);
        try {
            if (binary) {
                this.driver.binary(typeof data === 'string' ? Buffer.from(data) : data);
            } else {
                this.driver.text(data.toString());
            }
        } finally {
            this.out = null;
        }
        if (!out.length) {
            process.nextTick(() => cb?.(new Error('WebSocket is not open')));
            return;
        }
        this.sock.write(out.length === 1 ? out[0] : Buffer.concat(out), cb);
    }

    close(code = 1000): void {
        this.driver.close(undefined, code);
        this.sock.end();
    }

    terminate(): void {
        this.sock.destroy();
    }

    pause(): void {
        this.sock.pause();
    }

    resume(): void {
        this.sock.resume();
    }

    private deliver(event: string, ...args: unknown[]): void {
        if (this.pending) {
            this.pending.push([event, args]);
        } else {
            this.emit(event, ...args);
        }
    }
}

/** Opens a WebSocket connection; rejects with HandshakeError on a non-101 reply. */
export function openWebSocket(url: URL, opts: WebSocketOptions, headers: Record<string, string> = {}): Promise<WebSocketConn> {
    const secure = url.protocol === 'wss:';
    let host = url.hostname;
    let port = url.port ? parseInt(url.port, 10) : secure ? 443 : 80;
    if (opts.connectTo) {
        const i = opts.connectTo.lastIndexOf(':');
        if (i <= 0) {
            throw new Error(`connectTo must be host:port, got ${opts.connectTo}`);
        }
        host = opts.connectTo.slice(0, i).replace(/^\[|\]$/g, '');
        port = parseInt(opts.connectTo.slice(i + 1), 10);
    }

    return new Promise((resolve, reject) => {
        const sock: net.Socket = secure
            ? tls.connect({
                host,
                port,
                servername: net.isIP(url.hostname) ? undefined : url.hostname,
                ALPNProtocols: ['http/1.1'],
            })
            : net.connect({ host, port });
        sock.setNoDelay(true);
        const driver = websocket.client(url.toString(), { maxLength: MAX_MESSAGE });
        for (const [k, v] of Object.entries(headers)) {
            driver.setHeader(k, v);
        }

        // The raw handshake response, for the error message on a non-101 reply.
        // Registered before the connection's listener, so a chunk is recorded
        // before the driver parses it.
        let resp = Buffer.alloc(0);
        const onData = (d: Buffer) => (resp = Buffer.concat([resp, d]));
        sock.on('data', onData);
        const conn = new WebSocketConn(sock, driver);

        const timer = setTimeout(() => {
            sock.destroy();
            reject(new Error(`timed out connecting to ${url.host}`));
        }, opts.timeoutMs ?? 15000);
        const done = (err?: Error) => {
            clearTimeout(timer);
            sock.removeListener('data', onData);
            sock.removeListener('error', onError);
            sock.removeListener('end', onEnd);
            driver.removeListener('open', onOpen);
            driver.removeListener('error', onFail);
            if (err) {
                sock.destroy();
                reject(err);
            } else {
                resolve(conn);
            }
        };
        const onError = (e: Error) => done(e);
        const onEnd = () => done(handshakeError(resp, 'connection closed during handshake'));
        const onFail = (e: Error) => done(handshakeError(resp, e.message.replace(/^Error during WebSocket handshake: /, '')));
        // Hold further data until the caller calls start(); anything already
        // parsed is queued in conn.
        const onOpen = () => {
            sock.pause();
            done();
        };
        sock.on('error', onError);
        sock.on('end', onEnd);
        driver.on('open', onOpen);
        driver.on('error', onFail);
        sock.once(secure ? 'secureConnect' : 'connect', () => driver.start());
    });
}

/**
 * Describes a failed handshake by the response's status line and body, which
 * carry the server's reason, falling back to the driver's message.
 */
function handshakeError(resp: Buffer, fallback: string): HandshakeError {
    const s = resp.toString();
    const status = s.split('\r\n', 1)[0].replace(/^HTTP\/1\.\d\s*/, '');
    if (!status || status.startsWith('101')) {
        return new HandshakeError(fallback);
    }
    const end = s.indexOf('\r\n\r\n');
    const body = end >= 0 ? s.slice(end + 4).trim() : '';
    return new HandshakeError(`${status}${body ? `: ${body}` : ''}`);
}
