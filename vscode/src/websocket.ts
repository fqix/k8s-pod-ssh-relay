// A small RFC 6455 WebSocket client over a raw net/tls socket.
//
// We don't use http(s).request (or the `ws` package, which builds on it):
// VS Code's extension host patches http/https for proxy support and replaces
// the request agent, which would bypass connectTo and send the relay
// connection through the system proxy.
import { createHash, randomBytes } from 'crypto';
import { EventEmitter } from 'events';
import * as net from 'net';
import * as tls from 'tls';

const GUID = '258EAFA5-E914-47DA-95CA-C5AB0DC85B11';

export interface WebSocketOptions {
    /** Dial this host:port instead of the URL host; Host header and TLS name are unchanged. */
    connectTo?: string;
    timeoutMs?: number;
}

export class HandshakeError extends Error {}

/**
 * Events: 'message' (data: Buffer, isBinary: boolean), 'close', 'error' (Error).
 *
 * The connection is paused until start() is called, so no message is emitted
 * before the caller has attached its listeners.
 */
export class WebSocketConn extends EventEmitter {
    private buf: Buffer = Buffer.alloc(0);
    private fragments: Buffer[] = [];
    private fragmentBinary = false;
    private closing = false;

    private started = false;

    constructor(private readonly sock: net.Socket, private head: Buffer) {
        super();
    }

    /** Starts delivering events; call after attaching listeners. */
    start(): void {
        if (this.started) {
            return;
        }
        this.started = true;
        this.sock.on('data', (d: Buffer) => this.onData(d));
        this.sock.on('close', () => this.emit('close'));
        this.sock.on('error', (e) => this.emit('error', e));
        // Bytes that arrived together with the handshake response, e.g. the
        // relay's challenge in the same TCP segment as the 101.
        const head = this.head;
        this.head = Buffer.alloc(0);
        if (head.length) {
            this.onData(head);
        }
        this.sock.resume();
    }

    send(data: Buffer | string, binary: boolean, cb?: (err?: Error | null) => void): void {
        const payload = typeof data === 'string' ? Buffer.from(data) : data;
        this.sock.write(frame(binary ? 0x2 : 0x1, payload), cb);
    }

    close(code = 1000): void {
        if (!this.closing) {
            this.closing = true;
            const body = Buffer.alloc(2);
            body.writeUInt16BE(code);
            this.sock.write(frame(0x8, body));
        }
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

    private onData(d: Buffer): void {
        this.buf = this.buf.length ? Buffer.concat([this.buf, d]) : d;
        for (;;) {
            if (this.buf.length < 2) {
                return;
            }
            const b0 = this.buf[0];
            const b1 = this.buf[1];
            let len = b1 & 0x7f;
            let off = 2;
            if (len === 126) {
                if (this.buf.length < 4) {
                    return;
                }
                len = this.buf.readUInt16BE(2);
                off = 4;
            } else if (len === 127) {
                if (this.buf.length < 10) {
                    return;
                }
                len = Number(this.buf.readBigUInt64BE(2));
                off = 10;
            }
            const masked = (b1 & 0x80) !== 0;
            const maskOff = off;
            if (masked) {
                off += 4;
            }
            if (this.buf.length < off + len) {
                return;
            }
            let payload: Buffer = this.buf.subarray(off, off + len);
            if (masked) {
                payload = applyMask(Buffer.from(payload), this.buf.subarray(maskOff, maskOff + 4));
            }
            this.buf = this.buf.subarray(off + len);
            this.onFrame((b0 & 0x80) !== 0, b0 & 0x0f, payload);
        }
    }

    private onFrame(fin: boolean, opcode: number, payload: Buffer): void {
        switch (opcode) {
            case 0x0: // continuation
            case 0x1: // text
            case 0x2: // binary
                if (opcode !== 0x0) {
                    this.fragments = [];
                    this.fragmentBinary = opcode === 0x2;
                }
                this.fragments.push(payload);
                if (fin) {
                    const msg = this.fragments.length === 1 ? this.fragments[0] : Buffer.concat(this.fragments);
                    this.fragments = [];
                    this.emit('message', msg, this.fragmentBinary);
                }
                return;
            case 0x8: // close
                this.close();
                return;
            case 0x9: // ping
                this.sock.write(frame(0xa, payload));
                return;
            default: // pong and anything else
                return;
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
        const timer = setTimeout(() => {
            sock.destroy();
            reject(new Error(`timed out connecting to ${url.host}`));
        }, opts.timeoutMs ?? 15000);
        const done = (err?: Error, conn?: WebSocketConn) => {
            clearTimeout(timer);
            sock.removeListener('error', onError);
            sock.removeListener('data', onData);
            sock.removeListener('end', onEnd);
            if (err) {
                sock.destroy();
                reject(err);
            } else {
                resolve(conn!);
            }
        };
        const onError = (e: Error) => done(e);
        sock.on('error', onError);

        const key = randomBytes(16).toString('base64');
        sock.once(secure ? 'secureConnect' : 'connect', () => {
            const lines = [
                `GET ${url.pathname}${url.search} HTTP/1.1`,
                `Host: ${url.host}`,
                'Upgrade: websocket',
                'Connection: Upgrade',
                `Sec-WebSocket-Key: ${key}`,
                'Sec-WebSocket-Version: 13',
                ...Object.entries(headers).map(([k, v]) => `${k}: ${v}`),
            ];
            sock.write(lines.join('\r\n') + '\r\n\r\n');
        });

        let resp = Buffer.alloc(0);
        const onEnd = () => {
            const s = resp.toString();
            const status = s.split('\r\n', 1)[0] || 'connection closed during handshake';
            const body = s.includes('\r\n\r\n') ? s.slice(s.indexOf('\r\n\r\n') + 4).trim() : '';
            done(new HandshakeError(`${status.replace(/^HTTP\/1\.\d\s*/, '')}${body ? `: ${body}` : ''}`));
        };
        const onData = (d: Buffer) => {
            resp = Buffer.concat([resp, d]);
            const end = resp.indexOf('\r\n\r\n');
            if (end < 0) {
                return;
            }
            const head = resp.subarray(0, end).toString().split('\r\n');
            const code = parseInt(head[0].split(' ')[1], 10);
            if (code !== 101) {
                // Read the (short) error body until the server closes or Content-Length is reached.
                const cl = /^content-length:\s*(\d+)/im.exec(head.join('\n'));
                if (!cl || resp.length - end - 4 >= parseInt(cl[1], 10)) {
                    onEnd();
                }
                return;
            }
            const accept = head.find((l) => /^sec-websocket-accept:/i.test(l))?.split(':')[1]?.trim();
            const expected = createHash('sha1').update(key + GUID).digest('base64');
            if (accept !== expected) {
                return done(new HandshakeError('invalid Sec-WebSocket-Accept from server'));
            }
            // Hold further data until the caller calls start().
            sock.pause();
            done(undefined, new WebSocketConn(sock, resp.subarray(end + 4)));
        };
        sock.on('data', onData);
        sock.on('end', onEnd);
    });
}

/** Client frames are always masked (RFC 6455 §5.3). */
function frame(opcode: number, payload: Buffer): Buffer {
    const len = payload.length;
    const ext = len < 126 ? 0 : len < 65536 ? 2 : 8;
    const header = Buffer.alloc(2 + ext + 4);
    header[0] = 0x80 | opcode;
    if (ext === 0) {
        header[1] = 0x80 | len;
    } else if (ext === 2) {
        header[1] = 0x80 | 126;
        header.writeUInt16BE(len, 2);
    } else {
        header[1] = 0x80 | 127;
        header.writeBigUInt64BE(BigInt(len), 2);
    }
    const mask = randomBytes(4);
    mask.copy(header, 2 + ext);
    return Buffer.concat([header, applyMask(Buffer.from(payload), mask)]);
}

function applyMask(data: Buffer, mask: Buffer): Buffer {
    for (let i = 0; i < data.length; i++) {
        data[i] ^= mask[i & 3];
    }
    return data;
}
