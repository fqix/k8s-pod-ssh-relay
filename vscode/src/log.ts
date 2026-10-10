// Minimal logger interface so the protocol/SSH/server modules don't depend on
// the vscode API and can be exercised from plain Node.
export interface Logger {
    trace(msg: string): void;
    info(msg: string): void;
    warn(msg: string): void;
    error(msg: string): void;
}

export const consoleLogger: Logger = {
    trace: (m) => console.log(`[trace] ${m}`),
    info: (m) => console.log(`[info] ${m}`),
    warn: (m) => console.warn(`[warn] ${m}`),
    error: (m) => console.error(`[error] ${m}`),
};
