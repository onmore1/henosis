// Runs the real Worker code (src/index.js) with fake WebSockets.
// node --test test/
import { test } from "node:test";
import assert from "node:assert/strict";

class FakeSocket extends EventTarget {
  constructor() { super(); this.readyState = 1; this.sent = []; this.peer = null; }
  accept() {}
  send(data) { if (this.readyState !== 1) throw new Error("closed"); this.sent.push(data); }
  close() { if (this.readyState === 3) return; this.readyState = 3; this.dispatchEvent(new Event("close")); }
  // A message arriving from the far end of this socket (the app).
  receive(data) { const e = new Event("message"); e.data = data; this.dispatchEvent(e); }
}
globalThis.WebSocketPair = function () { const client = new FakeSocket(), server = new FakeSocket(); return { 0: client, 1: server }; };
globalThis.Response = class { constructor(body, init = {}) { this.status = init.status ?? 200; this.webSocket = init.webSocket; this.body = body; } };

const { default: worker, Rendezvous } = await import("../src/index.js");
const CODE = "00112233445566778899aabbccddeeff";
const req = (path, code = CODE, ws = true) =>
  new Request(`https://relay.test${path}?code=${code}`, { headers: ws ? { Upgrade: "websocket" } : {} });

// Rendezvous.fetch returns the client end; the server end is what the DO
// holds. Grab the server end from the pair the DO created.
let lastServer;
const realPair = globalThis.WebSocketPair;
globalThis.WebSocketPair = function () { const p = realPair(); lastServer = p[1]; return p; };
async function open(room, path) { await room.fetch(req(path)); return lastServer; }

test("accept then dial: both get OK and bytes flow both ways", async () => {
  const room = new Rendezvous({}, {});
  const host = await open(room, "/accept");
  assert.deepEqual(host.sent, []);
  const friend = await open(room, "/dial");
  assert.deepEqual(host.sent, ["OK"]);
  assert.deepEqual(friend.sent, ["OK"]);
  friend.receive(new Uint8Array([1, 2, 3]));
  assert.deepEqual(host.sent.at(-1), new Uint8Array([1, 2, 3]));
  host.receive("reply");
  assert.equal(friend.sent.at(-1), "reply");
  host.close();
  assert.equal(friend.readyState, 3, "the friend is closed when the host goes");
});

test("dial first, then accept, pairs too", async () => {
  const room = new Rendezvous({}, {});
  const friend = await open(room, "/dial");
  const host = await open(room, "/accept");
  assert.deepEqual(friend.sent, ["OK"]);
  assert.deepEqual(host.sent, ["OK"]);
});

test("a waiting socket that went away is skipped, not paired", async () => {
  const room = new Rendezvous({}, {});
  const gone = await open(room, "/accept");
  const alive = await open(room, "/accept");
  gone.close();
  const friend = await open(room, "/dial");
  assert.deepEqual(alive.sent, ["OK"]);
  assert.deepEqual(friend.sent, ["OK"]);
});

test("each dial takes its own waiting accept", async () => {
  const room = new Rendezvous({}, {});
  const h1 = await open(room, "/accept"), h2 = await open(room, "/accept");
  const f1 = await open(room, "/dial"), f2 = await open(room, "/dial");
  f1.receive("one"); f2.receive("two");
  assert.equal(h1.sent.at(-1), "one");
  assert.equal(h2.sent.at(-1), "two");
});

test("the Worker refuses bad codes and plain requests", async () => {
  const env = { RENDEZVOUS: { idFromName: (n) => n, get: () => ({ fetch: () => new Response("room") }) } };
  assert.equal((await worker.fetch(req("/dial", "nothex"), env)).status, 400);
  assert.equal((await worker.fetch(req("/dial", CODE, false), env)).status, 426);
  assert.equal((await worker.fetch(req("/dial"), env)).body, "room");
});
