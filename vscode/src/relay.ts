// TypeScript implementation of the k8s-pod-ssh-relay client protocol (see
// internal/auth in the Go code). After the WebSocket upgrade:
//
//   relay  -> client  {"nonce": base64}
//   client -> relay   {"public_key": base64(wire), "signature": {"Format", "Blob"}}
//                     signature over Message(nonce, target)
//   relay  -> client  {"ok": true} | {"error": "..."}
//
// then binary messages carry the pod's sshd byte stream in both directions.
import { Duplex } from 'stream';
import { Signer, sshString } from './keys';
import { Logger } from './log';
import { HandshakeError, openWebSocket, WebSocketConn } from './websocket';

const MAGIC = 'k8s-pod-ssh-relay-auth-v1';

export interface RelayOptions {
    /** Relay base URL, e.g. wss://relay.example.com */
    url: string;
    /** Target pod name. */
    target: string;
    signer: Signer;
    /** Extra CA certificates to trust (PEM). */
    ca?: Buffer;
    /** Dial this host:port instead of the URL host (Host header and TLS name unchanged). */
    connectTo?: string;
    timeoutMs?: number;
    log: Logger;
}

/** The data the client signs; must match ssh.Marshal of the Go struct. */
export function authMessage(nonce: Buffer, target: string): Buffer {
    return Buffer.concat([sshString(Buffer.from(MAGIC)), sshString(nonce), sshString(Buffer.from(target))]);
}

/**
 * Connects to the relay, authenticates, and resolves with a Duplex carrying
 * the pod's sshd stream.
 */
export async function connectRelay(opts: RelayOptions): Promise<Duplex> {
    const url = new URL('connect', opts.url.replace(/\/*$/, '/'));
    url.searchParams.set('target', opts.target);
    const timeoutMs = opts.timeoutMs ?? 15000;

    let ws: WebSocketConn;
    try {
        ws = await openWebSocket(url, { ca: opts.ca, connectTo: opts.connectTo, timeoutMs });
    } catch (e) {
        throw e instanceof HandshakeError ? new Error(`relay: ${e.message}`) : e;
    }

    return new Promise((resolve, reject) => {
        let stage: 'challenge' | 'result' | 'stream' = 'challenge';
        let stream: WsStream | undefined;
        const fail = (err: Error) => {
            if (stage !== 'stream') {
                clearTimeout(timer);
                reject(err);
            }
            ws.terminate();
        };
        const timer = setTimeout(() => fail(new Error('relay authentication timed out')), timeoutMs);

        ws.on('error', (err: Error) => (stream ? stream.destroy(err) : fail(err)));
        ws.on('close', () => (stream ? stream.push(null) : fail(new Error('relay closed the connection during authentication'))));

        // One listener for the whole lifetime: the first SSH bytes may arrive
        // in the same tick as the auth result, so the switch to streaming must
        // happen synchronously here.
        ws.on('message', (data: Buffer, isBinary: boolean) => {
            if (stage === 'stream') {
                if (!stream!.push(data)) {
                    ws.pause();
                }
                return;
            }
            let msg: Record<string, unknown>;
            try {
                if (isBinary) {
                    throw new Error('unexpected binary message during authentication');
                }
                msg = JSON.parse(data.toString());
            } catch (e) {
                return fail(e as Error);
            }
            if (stage === 'challenge') {
                stage = 'result';
                const nonce = Buffer.from(String(msg.nonce ?? ''), 'base64');
                opts.signer.sign(authMessage(nonce, opts.target)).then((sig) => {
                    ws.send(JSON.stringify({
                        public_key: opts.signer.publicKey.toString('base64'),
                        signature: { Format: sig.format, Blob: sig.blob.toString('base64') },
                    }), false);
                }, fail);
                return;
            }
            // stage === 'result'
            if (msg.ok !== true) {
                return fail(new Error(`relay: ${String(msg.error ?? 'authentication failed')}`));
            }
            clearTimeout(timer);
            stage = 'stream';
            stream = new WsStream(ws);
            opts.log.info(`relay: authenticated, connected to pod ${opts.target}`);
            resolve(stream);
        });
    });
}

/** Duplex over the binary messages of an established WebSocket. */
class WsStream extends Duplex {
    constructor(private readonly ws: WebSocketConn) {
        super();
    }
    _read() {
        this.ws.resume();
    }
    _write(chunk: Buffer, _enc: BufferEncoding, cb: (err?: Error | null) => void) {
        this.ws.send(chunk, true, cb);
    }
    _final(cb: (err?: Error | null) => void) {
        this.ws.close();
        cb();
    }
    _destroy(err: Error | null, cb: (err?: Error | null) => void) {
        this.ws.terminate();
        cb(err);
    }
}
