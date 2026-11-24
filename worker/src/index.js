// Hénōsis relay — a Cloudflare Worker + Durable Object that pairs two
// WebSockets by session code, then passes messages between them. Nothing is
// stored and nothing is logged; a pairing lives only as long as its sockets.
//
// The host app keeps "accept" sockets open per session code. A friend's app
// opens a "dial" socket; the Durable Object pairs it with a free accept
// socket, says "OK" to both, and from then on every message from one side
// goes to the other.
//
// It uses the WebSocket Hibernation API: idle sockets cost nothing, and each
// socket carries its own pairing (as an attachment), so the object can sleep
// between messages without forgetting who talks to whom.
//
// Routes:
//   GET /                     — a line of text with the version
//   GET /accept?code=<hex32>  (WebSocket) — host side
//   GET /dial?code=<hex32>    (WebSocket) — friend side
//
// The code is a hash of the session secret, and everything passed on is
// encrypted by the two apps, so the relay can neither join a session nor
// read a project.

export const VERSION = "3";

// Limits per session code, so one broken or hostile app cannot pile up
// sockets. An app keeps 2 waiting sockets per session; a friend opens a few.
export const MAX_WAITING = { accept: 6, dial: 8 };
export const MAX_SOCKETS = 40;

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    if (url.pathname !== "/accept" && url.pathname !== "/dial") {
      return new Response(`Hénōsis relay ${VERSION}\n`, { status: 200, headers: { "content-type": "text/plain; charset=utf-8" } });
    }
    const code = (url.searchParams.get("code") || "").toLowerCase();
    if (!/^[a-f0-9]{32}$/.test(code)) {
      return new Response("bad code", { status: 400 });
    }
    if (request.headers.get("Upgrade") !== "websocket") {
      return new Response("websocket required", { status: 426 });
    }
    const stub = env.RENDEZVOUS.get(env.RENDEZVOUS.idFromName(code));
    try {
      return await stub.fetch(request);
    } catch (e) {
      // Past the free plan's Durable Object allowance the call throws
      // ("Exceeded allowed ..."). 429 is what the app reads as "the relay is
      // out of today's allowance"; anything else is a plain failure.
      const msg = String((e && e.message) || e);
      const full = /exceeded|limit/i.test(msg);
      return new Response(full ? "relay over its daily allowance" : "relay error", { status: full ? 429 : 503 });
    }
  },
};

// Rendezvous — one Durable Object per session code.
export class Rendezvous {
  constructor(ctx, env) {
    this.ctx = ctx;
  }

  async fetch(request) {
    const side = new URL(request.url).pathname === "/accept" ? "accept" : "dial";
    const other = side === "accept" ? "dial" : "accept";
    const id = crypto.randomUUID();

    const open = this.ctx.getWebSockets();
    const waiting = this.ctx.getWebSockets(side).filter((ws) => {
      const a = ws.deserializeAttachment();
      return a && !a.peer;
    });
    if (open.length >= MAX_SOCKETS || waiting.length >= MAX_WAITING[side]) {
      // 409, not 429: the app reads 429 as "the relay is out of its daily
      // allowance".
      return new Response("too many connections for this session", { status: 409 });
    }

    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    this.ctx.acceptWebSocket(server, [side, "id:" + id]);

    // The first open socket of the other side that is not paired yet.
    const peer = this.ctx.getWebSockets(other).find((ws) => {
      const a = ws.deserializeAttachment();
      return a && !a.peer && ws.readyState === 1;
    });
    if (peer) {
      const peerID = peer.deserializeAttachment().id;
      server.serializeAttachment({ id, peer: peerID });
      peer.serializeAttachment({ id: peerID, peer: id });
      try {
        peer.send("OK");
        server.send("OK");
      } catch (e) {
        this.drop(server);
        this.drop(peer);
      }
    } else {
      server.serializeAttachment({ id, peer: null });
    }
    return new Response(null, { status: 101, webSocket: client });
  }

  peerOf(ws) {
    const a = ws.deserializeAttachment();
    if (!a || !a.peer) return null;
    return this.ctx.getWebSockets("id:" + a.peer)[0] || null;
  }

  async webSocketMessage(ws, message) {
    const a = ws.deserializeAttachment();
    if (!a || !a.peer) return; // not paired yet: nothing to pass on
    const peer = this.peerOf(ws);
    if (!peer) {
      this.drop(ws);
      return;
    }
    try {
      peer.send(message);
    } catch (e) {
      this.drop(ws);
      this.drop(peer);
    }
  }

  async webSocketClose(ws) {
    const peer = this.peerOf(ws);
    this.drop(ws);
    if (peer) this.drop(peer);
  }

  async webSocketError(ws) {
    await this.webSocketClose(ws);
  }

  drop(ws) {
    try { ws.close(1000, "closed"); } catch (e) {}
  }
}
