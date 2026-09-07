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
import process from 'node:process';

const rid = (() => { let n = 0; return () => `p${++n}`; })();

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

const rl = createInterface({ input: process.stdin, terminal: false });
rl.on('line', (line) => {
  let m;
  try { m = JSON.parse(line); } catch { return; }
  if (m.t === 'pp') {
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
  process.stderr.write(`[shim] host booted from ${entry}\n`);
} catch (err) {
  process.stderr.write(`[shim] host boot failed: ${err?.stack ?? err}\n`);
  process.exit(1);
}
