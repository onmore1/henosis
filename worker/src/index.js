// Hénōsis relay — a Cloudflare Worker + Durable Object that pairs two
// WebSockets by session code, then pipes bytes between them. Nothing is
// stored, nothing is logged, nothing is kept once both sides disconnect.
//
// The host opens one or more "accept" sockets per session code and waits.
// The friend opens a "dial" socket; the Durable Object pairs it with an
// idle accept, and from that moment the two sockets act as one TCP tunnel.
//
// Routes:
//   GET /accept?code=XXXXXX   (WebSocket) — host side, waits for a dial
//   GET /dial?code=XXXXXX     (WebSocket) — friend side, waits for an accept

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const code = url.searchParams.get("code") || "";
    if (!/^[a-f0-9]{32}$/i.test(code)) {
      return new Response("bad code", { status: 400 });
    }
    if (url.pathname !== "/accept" && url.pathname !== "/dial") {
      return new Response("Hénōsis relay. See https://github.com/onmore1/henosis", { status: 200 });
    }
    if (request.headers.get("Upgrade") !== "websocket") {
      return new Response("websocket required", { status: 426 });
    }
    const id = env.RENDEZVOUS.idFromName(code.toLowerCase());
    const stub = env.RENDEZVOUS.get(id);
    return stub.fetch(request);
  },
};

// Rendezvous — one Durable Object per session code. Holds queues of
// waiting sockets and pairs them up.
export class Rendezvous {
  constructor(state, env) {
    this.state = state;
    // Queues of unpaired sockets. Each entry: { ws, side: "accept" | "dial" }.
    this.accepts = [];
    this.dials = [];
  }

  async fetch(request) {
    const url = new URL(request.url);
    const side = url.pathname === "/accept" ? "accept" : "dial";

    const pair = new WebSocketPair();
    const [client, server] = Object.values(pair);
    server.accept();

    // Pair immediately if the other side is already waiting.
    const other = side === "accept" ? this.dials.shift() : this.accepts.shift();
    if (other) {
      this.pipe(server, other);
    } else {
      this.enqueue(server, side);
    }

    return new Response(null, { status: 101, webSocket: client });
  }

  enqueue(ws, side) {
    const entry = { ws, side };
    const queue = side === "accept" ? this.accepts : this.dials;
    queue.push(entry);
    const cleanup = () => {
      const i = queue.indexOf(entry);
      if (i >= 0) queue.splice(i, 1);
      try { ws.close(1000, "unpaired"); } catch (e) {}
    };
    ws.addEventListener("close", cleanup);
    ws.addEventListener("error", cleanup);
    // Cap the wait so an orphaned socket doesn't sit forever.
    setTimeout(() => { if (queue.indexOf(entry) >= 0) cleanup(); }, 10 * 60 * 1000);
  }

  pipe(a, b) {
    const closeBoth = () => {
      try { a.close(1000, "peer-gone"); } catch (e) {}
      try { b.close(1000, "peer-gone"); } catch (e) {}
    };
    a.addEventListener("message", (e) => { try { b.send(e.data); } catch (_) { closeBoth(); } });
    b.addEventListener("message", (e) => { try { a.send(e.data); } catch (_) { closeBoth(); } });
    a.addEventListener("close", closeBoth);
    b.addEventListener("close", closeBoth);
    a.addEventListener("error", closeBoth);
    b.addEventListener("error", closeBoth);
    // Let each side know the tunnel is open.
    try { a.send("OK"); b.send("OK"); } catch (e) { closeBoth(); }
  }
}
