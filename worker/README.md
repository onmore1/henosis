# Hénōsis relay

A tiny Cloudflare Worker + Durable Object that pairs two WebSockets by
session code and pipes bytes between them. Nothing is stored or logged.

## Deploy

One-time:

```bash
npm install -g wrangler
wrangler login
```

From this folder:

```bash
wrangler deploy
```

The command prints the relay URL, something like
`https://henosis-relay.<your-account>.workers.dev`.

To build it into the app, so friends set nothing:

- Releases: the official relay is built in (`config.DefaultRelay`). To
  build with another, add a repository variable `HENOSIS_RELAY` with its
  URL (Settings → Secrets and variables → Actions → Variables).
- Local builds: `go build -ldflags "-H=windowsgui -X henosis/config.DefaultRelay=<url>" ./cmd/henosis-agent`

A relay typed under Advanced on the session card, or the `HENOSIS_RELAY`
environment variable, overrides the built-in one.

The Durable Object uses the SQLite backend (`new_sqlite_classes`), which
is the only kind the Workers Free plan allows.

## Cost

The free plan is enough to start, and it never bills: past the daily
allowance the relay refuses connections until midnight UTC, and the app
tells people so. Per day it allows 100,000 Worker requests, 100,000
Durable Object requests (incoming WebSocket messages count 1/20; pings
are free) and 13,000 GB-s of Durable Object time.

Roughly: a host with the app open all day costs about 300 requests
(socket renewals); each friend connection a few; moving 1 GB up to about
1,000.
So the free plan carries a few hundred sessions open all day, more when
people close the app. Beyond that, the
Workers Paid plan ($5 a month) raises every limit.

## How it works

- The host app opens `wss://<relay>/accept?code=<route>` and waits; it
  keeps two such sockets open and replaces them every ten minutes.
- A friend's app opens `wss://<relay>/dial?code=<route>`.
- `<route>` is a hash of the session secret, never the secret itself.
- The Durable Object for that code pairs them, sends `"OK"` on each side,
  and passes every message from one to the other. Each message is at most
  256 KB.
- When either side closes, the other is closed too.
- One code can have at most 6 waiting host sockets, 8 waiting friend
  sockets and 40 sockets in all; more get `409`.

Everything after `"OK"` is encrypted by the two apps (AES-256-GCM, keys
derived from the session secret and fresh random values from both sides),
so the relay can neither read nor change it. Nothing is stored or logged.

Check a deployed relay end to end with `go run ./cmd/relaycheck <URL>` in
`app/`.
