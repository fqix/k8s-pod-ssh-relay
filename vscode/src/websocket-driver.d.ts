// The subset of websocket-driver (which ships no types) used by websocket.ts.
declare module 'websocket-driver' {
    import { EventEmitter } from 'events';
    import { Stream } from 'stream';

    /** The wire side: write socket bytes in; framed bytes come out as 'data' events. */
    interface IO extends Stream {
        write(chunk: Buffer): boolean;
        end(): void;
    }

    /**
     * Events: 'open', 'message' ({ data: string | Buffer }), 'error' (Error),
     * 'close' ({ code, reason }).
     */
    interface Driver extends EventEmitter {
        readonly io: IO;
        /** Status code of the handshake response, once it has been parsed. */
        readonly statusCode?: number;
        setHeader(name: string, value: string): boolean;
        start(): boolean;
        text(message: string): boolean;
        binary(message: Buffer): boolean;
        close(reason?: string, code?: number): boolean;
    }

    export function client(url: string, options?: { maxLength?: number; protocols?: string[] }): Driver;
}
