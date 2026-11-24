// Runs the real Worker code (src/index.js) against fakes of the Workers
// runtime: WebSocketPair, Response with status 101, and the Durable Object
// hibernation API (acceptWebSocket, getWebSockets, attachments).
//
//   node --test --test-force-exit test/
import { test } from "node:test";
import assert from "node:assert/strict";

class FakeSocket {
  constructor() { this.readyState = 1; this.sent = []; this.tags = []; this.attachment = null; this.closedWith = null; }
  send(data) { if (this.readyState !== 1) throw new Error("socket closed"); this.sent.push(data); }
  close(code, reason) { if (this.readyState === 3) return; this.readyState = 3; this.closedWith = code; }
  serializeAttachment(v) { this.attachment = structuredClone(v); }
  deserializeAttachment() { return structuredClone(this.attachment); }
}
globalThis.WebSocketPair = function () { return { 0: new FakeSocket(), 1: new FakeSocket() }; };
globalThis.Response = class {
  constructor(body, init = {}) { this.status = init.status ?? 200; this.webSocket = init.webSocket; this.body = body; }
  async text() { return String(this.body); }
};

class FakeCtx {
  constructor() { this.sockets = []; this.last = null; }
  acceptWebSocket(ws, tags = []) { ws.tags = tags; this.sockets.push(ws); this.last = ws; }
  getWebSockets(tag) { return this.sockets.filter((w) => w.readyState !== 3 && (!tag || w.tags.includes(tag))); }
}

const { default: worker, Rendezvous, VERSION, MAX_WAITING, MAX_SOCKETS } = await import("../src/index.js");
const CODE = "00112233445566778899aabbccddeeff";
const req = (path, code = CODE, ws = true) =>
  new Request(`https://relay.test${path}?code=${code}`, { headers: ws ? { Upgrade: "websocket" } : {} });

// open makes one app socket through the room and returns the server end the
// Durable Object holds.
async function open(room, path) {
  const resp = await room.fetch(req(path));
  assert.equal(resp.status, 101);
  return room.ctx.last;
}
const room = () => new Rendezvous(new FakeCtx(), {});

test("accept then dial: both get OK and messages pass both ways", async () => {
  const r = room();
  const host = await open(r, "/accept");
  assert.deepEqual(host.sent, []);
  const friend = await open(r, "/dial");
  assert.deepEqual(host.sent, ["OK"]);
  assert.deepEqual(friend.sent, ["OK"]);
  await r.webSocketMessage(friend, new Uint8Array([1, 2, 3]));
  assert.deepEqual(host.sent.at(-1), new Uint8Array([1, 2, 3]));
  await r.webSocketMessage(host, "reply");
  assert.equal(friend.sent.at(-1), "reply");
});

test("closing one side closes the other", async () => {
  const r = room();
  const host = await open(r, "/accept");
  const friend = await open(r, "/dial");
  host.readyState = 3; // the app went away
  await r.webSocketClose(host, 1006, "", false);
  assert.equal(friend.readyState, 3);
});

test("dial first, then accept, pairs too", async () => {
  const r = room();
  const friend = await open(r, "/dial");
  const host = await open(r, "/accept");
  assert.deepEqual(friend.sent, ["OK"]);
  assert.deepEqual(host.sent, ["OK"]);
});

test("a waiting socket that went away is not paired", async () => {
  const r = room();
  const gone = await open(r, "/accept");
  const alive = await open(r, "/accept");
  gone.readyState = 3;
  await r.webSocketClose(gone, 1006, "", false);
  const friend = await open(r, "/dial");
  assert.deepEqual(alive.sent, ["OK"]);
  assert.deepEqual(friend.sent, ["OK"]);
});

test("each dial takes its own free accept; paired sockets are not reused", async () => {
  const r = room();
  const h1 = await open(r, "/accept"), h2 = await open(r, "/accept");
  const f1 = await open(r, "/dial"), f2 = await open(r, "/dial");
  await r.webSocketMessage(f1, "one");
  await r.webSocketMessage(f2, "two");
  assert.deepEqual([h1.sent.at(-1), h2.sent.at(-1)].sort(), ["one", "two"]);
  const f3 = await open(r, "/dial");
  assert.deepEqual(f3.sent, [], "a third dial waits instead of joining a busy pair");
});

test("pairing survives the object waking from hibernation", async () => {
  const r = room();
  const host = await open(r, "/accept");
  const friend = await open(r, "/dial");
  const woken = new Rendezvous(r.ctx, {}); // a fresh object over the same sockets
  await woken.webSocketMessage(friend, "after sleep");
  assert.equal(host.sent.at(-1), "after sleep");
});

test("unpaired messages go nowhere", async () => {
  const r = room();
  const host = await open(r, "/accept");
  await r.webSocketMessage(host, "hello?");
  assert.deepEqual(host.sent, []);
});

test("the Worker answers the root, refuses bad codes and plain requests", async () => {
  const env = { RENDEZVOUS: { idFromName: (n) => n, get: () => ({ fetch: () => new Response("room") }) } };
  assert.equal(await (await worker.fetch(new Request("https://relay.test/"), env)).text(), `Hénōsis relay ${VERSION}\n`);
  assert.equal((await worker.fetch(new Request("https://relay.test/robots.txt"), env)).status, 200);
  assert.equal((await worker.fetch(req("/dial", "nothex"), env)).status, 400);
  assert.equal((await worker.fetch(req("/dial", CODE, false), env)).status, 426);
  assert.equal((await worker.fetch(req("/dial"), env)).body, "room");
});

test("one session cannot pile up waiting sockets", async () => {
  const r = room();
  for (let i = 0; i < MAX_WAITING.accept; i++) await open(r, "/accept");
  const resp = await r.fetch(req("/accept"));
  assert.equal(resp.status, 409);
  // A friend can still get through, and pairing frees a place.
  const friend = await open(r, "/dial");
  assert.deepEqual(friend.sent, ["OK"]);
  assert.equal((await r.fetch(req("/accept"))).status, 101);
});

test("a session has a ceiling on all its sockets", async () => {
  const r = room();
  let made = 0;
  while (made < MAX_SOCKETS) {
    await open(r, "/accept");
    await open(r, "/dial");
    made += 2;
  }
  assert.equal((await r.fetch(req("/dial"))).status, 409);
  assert.equal((await r.fetch(req("/accept"))).status, 409);
});

test("a Durable Object over its allowance becomes 429, other failures 503", async () => {
  const throwing = (msg) => ({ RENDEZVOUS: { idFromName: (n) => n, get: () => ({ fetch: async () => { throw new Error(msg); } }) } });
  const r1 = await worker.fetch(req("/dial"), throwing("Exceeded allowed volume of requests in Durable Objects free tier."));
  assert.equal(r1.status, 429);
  const r2 = await worker.fetch(req("/dial"), throwing("Network connection lost."));
  assert.equal(r2.status, 503);
});
