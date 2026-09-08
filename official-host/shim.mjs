// shim.mjs — Electron utilityProcess emulation so the OFFICIAL ZCode web-remote
// host (out/host/index.js) runs under plain Node, driven by the Go bridge.
//
// The desktop main process forks host/index.js via utilityProcess.fork and
// talks to it over process.parentPort (MessagePort-like: message events carry
// {data, ports}). Service attachments (AttachServicePort) pass MessagePorts.
// We emulate both and bridge them over newline-delimited JSON on stdio:
//
//   shim -> Go: {"t":"pp","msg":{...}}                      parentPort.postMessage
//   shim -> Go: {"t":"port","id":I,"b64":"..."}             service port data
//   Go  -> shim: {"t":"pp","msg":{...},"ports":["I"...]}    deliver to parentPort
//                (ports referenced are locally created duplex pairs)
//   Go  -> shim: {"t":"port","id":I,"b64":"..."}            data into local port
//   Go  -> shim: {"t":"port-open","id":I}                   mark port deliverable
//
// The Go side thus speaks the parentPort protocol (InitLocal, AttachServicePort,
// CronRun...) and pipes the phone's wire bytes straight into the official host.

import { EventEmitter } from 'node:events';
import { createInterface } from 'node:readline';
import { appendFileSync } from 'node:fs';
import process from 'node:process';

const rid = (() => { let n = 0; return () => `p${++n}`; })();

// ---- crash forensics ---------------------------------------------------------
// The host's own uncaughtException handler truncates the stack into one log
// line before disposing the process; keep the full trace on disk first.
process.on('uncaughtException', (err) => {
  try {
    appendFileSync(
      process.env.ZQF_CRASH_LOG ?? '/tmp/zqwf-host-crash.log',
      `\n==== ${new Date().toISOString()} pid=${process.pid} ====\n${err?.stack ?? String(err)}\n`,
    );
  } catch { /* nothing to salvage */ }
});

// ---- MessagePort emulation -------------------------------------------------
class FakePort extends EventEmitter {
  constructor(id) {
    super();
    this._id = id;
    this._started = false;
    this._queue = [];
    this.on('message', () => {}); // listener installed by host later
  }
  postMessage(data, transfer) {
    if (data instanceof Uint8Array) {
      if (process.env.ZQF_SVC_DEBUG) {
        try {
          appendFileSync(process.env.ZQF_SVC_DEBUG + '.out', `${this._id} ${Buffer.byteLength(data)} ${Buffer.from(data).toString('base64')}\n`);
        } catch { /* debug only */ }
      }
      write({ t: 'raw', id: this._id, b64: Buffer.from(data).toString('base64') });
      return;
    }
    write({ t: 'port', id: this._id, b64: enc(data) });
  }
  start() { this._started = true; for (const d of this._queue) this.emit('message', { data: d }); this._queue = []; }
  close() { this.emit('close'); }
  deliver(data) {
    const ev = { data, ports: [] };
    if (this._started) this.emit('message', ev);
    else this._queue.push(ev);
  }
}

const ports = new Map();

function makePort() {
  const id = rid();
  const p = new FakePort(id);
  ports.set(id, p);
  return p;
}

// Electron MessagePort data is structured-cloned; we pass JSON-safe values or
// {__b64:...} wrappers produced by enc/dec.
function enc(data) {
  return Buffer.from(JSON.stringify(data), 'utf8').toString('base64');
}

// ---- parentPort emulation ---------------------------------------------------
const parentPort = new EventEmitter();
parentPort.postMessage = (msg) => {
  write({ t: 'pp', msg: jsonSafe(msg) });
};

// parentPort message events look like {data, ports: MessagePortMain[]}
parentPort.deliver = (msg, portList) => {
  parentPort.emit('message', { data: msg, ports: portList ?? [] });
};

process.parentPort = parentPort;

function jsonSafe(v) {
  // strip functions; pass everything else through (JSON-encoded on the wire)
  return v;
}

// ---- stdio transport ---------------------------------------------------------
function write(obj) {
  try {
    process.stdout.write(JSON.stringify(obj) + '\n');
  } catch {
    // pipe closed — let the process die naturally
  }
}

// The phone frontend serializes workspace-scoped EventListens with EMPTY args
// whenever its workspace scope hasn't loaded, and the host's
// resolveWorkspaceKey then throws on the missing workspace and the uncaught
// exception kills the whole host (phone stuck at "Paired. Loading
// workspace…"). Repair those frames: rewrite the empty-args terminator of a
// zcode-agent onDynamic*Frame listen into a JSON args record carrying the
// init-local workspace path. Wire shape (verified by capture):
//   04 04 06 <kind> 06 <id> 01 <len> <channel> 01 <len> <event> <args>
//   args := 00 | 05 <len> <json>
let localWorkspacePath = null;

function leb(n) {
  const out = [];
  do { let x = n & 0x7f; n >>>= 7; if (n) x |= 0x80; out.push(x); } while (n);
  return out;
}

function repairListenFrame(b) {
  if (!(b instanceof Buffer) || b.length < 10 || b[0] !== 4 || b[1] !== 4) return b;
  if (b[2] !== 6) return b;
  if (b[3] !== 0x66 && b[3] !== 0x64) return b; // 102=EventListen, 100=PromiseCall
  if (b[4] !== 6 || (b[5] & 0x80) !== 0) return b; // single-byte id only
  let off = 6;
  if (b[off] !== 1) return b;
  const clen = b[off + 1];
  if (clen & 0x80) return b;
  const channel = b.slice(off + 2, off + 2 + clen).toString('utf8');
  off += 2 + clen;
  if (b[off] !== 1) return b;
  const elen = b[off + 1];
  if (elen & 0x80) return b;
  const event = b.slice(off + 2, off + 2 + elen).toString('utf8');
  off += 2 + elen;
  const isListen = b[3] === 0x66;
  if (!isListen) {
    // Calls: the 0.7.0 web frontend sends the request value directly
    // (Object tag 05, but also strings/numbers), while this host's call
    // adapters spread the wire arg as a positional list — every scalar-arg
    // call died with "CreateListFromArrayLike" or a missing-argument
    // TypeError. Wrap any non-array, non-undefined arg in a one-element
    // Array (tag 04, count 1). Arrays pass through untouched.
    const argTag = off < b.length ? b[off] : undefined;
    if (argTag === 0x01 || argTag === 0x02 || argTag === 0x03 || argTag === 0x05 || argTag === 0x06) {
      return Buffer.concat([b.slice(0, off), Buffer.from([0x04, 0x01]), b.slice(off)]);
    }
    return b;
  }
  if (!WORKSPACE_CHANNELS.has(channel) || !event.startsWith('onDynamic') || !localWorkspacePath) return b;
  if (off >= b.length || b[off] !== 0x00) return b; // args present — fine
  const payload = Buffer.from(JSON.stringify({ workspacePath: localWorkspacePath }), 'utf8');
  return Buffer.concat([b.slice(0, off), Buffer.from([0x05, ...leb(payload.length)]), payload]);
}

// Channels whose onDynamic* listeners resolve a workspace off the listen args
// and crash the host when the frontend omits them. window-controller is
// deliberately excluded — its listeners take no args.
const WORKSPACE_CHANNELS = new Set(['zcode-agent', 'zcode-task']);

const rl = createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  let m;
  try { m = JSON.parse(line); } catch { return; }
  if (m.t === 'pp') {
    if (m.msg?.type === 'init-local' && typeof m.msg.workspacePath === 'string') {
      localWorkspacePath = m.msg.workspacePath;
    }
    const list = (m.ports ?? []).map((id) => {
      let p = ports.get(id);
      if (!p) { p = new FakePort(id); ports.set(id, p); }
      return p;
    });
    parentPort.deliver(m.msg, list);
  } else if (m.t === 'port') {
    const p = ports.get(m.id);
    if (!p) return;
    let data = null;
    try { data = JSON.parse(Buffer.from(m.b64 ?? '', 'base64').toString('utf8')); } catch { data = null; }
    p.deliver(data);
  } else if (m.t === 'raw') {
    const p = ports.get(m.id);
    if (!p) return;
    const rawBuf = Buffer.from(m.b64 ?? '', 'base64');
    if (process.env.ZQF_SVC_DEBUG) {
      try {
        appendFileSync(process.env.ZQF_SVC_DEBUG, `${m.id} ${m.b64?.length ?? 0} ${m.b64 ?? ''}\n`);
      } catch { /* debug only */ }
    }
    p.deliver(new Uint8Array(repairListenFrame(rawBuf)));
  } else if (m.t === 'port-open') {
    const p = ports.get(m.id);
    if (p) p.start();
  }
});
rl.on('close', () => {
  // Go side went away — exit so the bridge can respawn us cleanly
  process.exit(0);
});

process.stderr.write('[shim] parentPort emulation ready\n');

// ---- boot the official host ---------------------------------------------------
const entry = process.env.ZCODE_HOST_ENTRY ?? './host/index.js';
process.env.ZCODE_PROCESS_LABEL ??= 'web-remote-host';
try {
  await import(entry);
  write({ t: 'ready' });
  process.stderr.write(`[shim] host booted from ${entry}\n`);
} catch (err) {
  process.stderr.write(`[shim] host boot failed: ${err?.stack ?? err}\n`);
  process.exit(1);
}
