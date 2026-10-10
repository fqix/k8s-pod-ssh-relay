// SSH key loading and signing, mirroring cmd/podrelay: an unencrypted key
// is used directly; a passphrase-protected one is used through ssh-agent.
import { promises as fs } from 'fs';
import { utils, OpenSSHAgent, ParsedKey } from 'ssh2';

export interface Signature {
    format: string;
    blob: Buffer;
}

export interface Signer {
    /** Public key in SSH wire format. */
    readonly publicKey: Buffer;
    readonly type: string;
    sign(data: Buffer): Promise<Signature>;
    /** Credentials for ssh2's Client.connect(). */
    readonly sshAuth: { privateKey: Buffer } | { agent: string };
}

export async function loadSigner(identityFile: string, agentSock = process.env.SSH_AUTH_SOCK): Promise<Signer> {
    const data = await fs.readFile(identityFile);
    const parsed = utils.parseKey(data);
    if (!(parsed instanceof Error)) {
        const key = Array.isArray(parsed) ? parsed[0] : parsed;
        if (!key.isPrivateKey()) {
            throw new Error(`${identityFile} is not a private key`);
        }
        return localSigner(key, data);
    }
    if (!/passphrase/i.test(parsed.message)) {
        throw new Error(`${identityFile}: ${parsed.message}`);
    }
    // Passphrase-protected: find the matching key in ssh-agent.
    if (!agentSock) {
        throw new Error(`${identityFile} is passphrase-protected and no ssh-agent is running`);
    }
    const pub = utils.parseKey(await fs.readFile(`${identityFile}.pub`));
    if (pub instanceof Error) {
        throw new Error(`${identityFile}.pub: ${pub.message}`);
    }
    const want = (Array.isArray(pub) ? pub[0] : pub).getPublicSSH();
    const agent = new OpenSSHAgent(agentSock);
    const keys = await new Promise<ParsedKey[]>((resolve, reject) =>
        agent.getIdentities((err, ks) => (err ? reject(err) : resolve((ks ?? []) as ParsedKey[]))));
    const key = keys.find((k) => k.getPublicSSH().equals(want));
    if (!key) {
        throw new Error(`${identityFile} is passphrase-protected and not loaded in ssh-agent (run: ssh-add ${identityFile})`);
    }
    return agentSigner(agent, agentSock, key);
}

function localSigner(key: ParsedKey, privateKey: Buffer): Signer {
    return {
        publicKey: key.getPublicSSH(),
        type: key.type,
        sshAuth: { privateKey },
        async sign(data) {
            // Plain "ssh-rsa" signatures use SHA-1; use rsa-sha2-256 like the Go client.
            if (key.type === 'ssh-rsa') {
                return { format: 'rsa-sha2-256', blob: unwrap(key.sign(data, 'sha256')) };
            }
            const sig = unwrap(key.sign(data));
            if (key.type.startsWith('ecdsa-sha2-')) {
                return { format: key.type, blob: derToSSHEcdsa(sig) };
            }
            return { format: key.type, blob: sig };
        },
    };
}

function agentSigner(agent: OpenSSHAgent, sock: string, key: ParsedKey): Signer {
    return {
        publicKey: key.getPublicSSH(),
        type: key.type,
        sshAuth: { agent: sock },
        sign(data) {
            const rsa = key.type === 'ssh-rsa';
            return new Promise((resolve, reject) => {
                // ssh2 strips the format from the agent's reply and returns the blob.
                const cb = (err: Error | null | undefined, blob?: Buffer) =>
                    err || !blob ? reject(err ?? new Error('empty agent signature'))
                        : resolve({ format: rsa ? 'rsa-sha2-256' : key.type, blob });
                if (rsa) {
                    agent.sign(key, data, { hash: 'sha256' }, cb);
                } else {
                    agent.sign(key, data, cb);
                }
            });
        },
    };
}

function unwrap(sig: Buffer | Error): Buffer {
    if (sig instanceof Error) {
        throw sig;
    }
    return sig;
}

/** Node returns ECDSA signatures as DER; SSH wants string(mpint r, mpint s). */
function derToSSHEcdsa(der: Buffer): Buffer {
    let p = 2; // SEQUENCE tag + length (signatures are < 128 bytes for P-256/384; handle long form too)
    if (der[1] & 0x80) {
        p = 2 + (der[1] & 0x7f);
    }
    const ints: Buffer[] = [];
    for (let i = 0; i < 2; i++) {
        if (der[p] !== 0x02) {
            throw new Error('bad ECDSA DER signature');
        }
        const len = der[p + 1];
        ints.push(der.subarray(p + 2, p + 2 + len)); // DER INTEGER content == mpint content
        p += 2 + len;
    }
    return Buffer.concat(ints.map(sshString));
}

/** SSH wire "string": uint32 length + bytes. */
export function sshString(b: Buffer): Buffer {
    const len = Buffer.alloc(4);
    len.writeUInt32BE(b.length);
    return Buffer.concat([len, b]);
}
