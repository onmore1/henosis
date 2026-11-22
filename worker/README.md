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

- Releases: add a repository variable `HENOSIS_RELAY` with that URL
  (Settings → Secrets and variables → Actions → Variables). The release
  workflow passes it to the build.
- Local builds: `go build -ldflags "-H=windowsgui -X henosis/config.DefaultRelay=<url>" ./cmd/henosis-agent`

A relay typed under Advanced on the session card, or the `HENOSIS_RELAY`
environment variable, overrides the built-in one.

The Durable Object uses the SQLite backend (`new_sqlite_classes`), which
is the only kind the Workers Free plan allows.

## Cost

Cloudflare's free tier covers it: 100,000 Worker requests a day,
1,000,000 Durable Object requests a month, and 400,000 GB-seconds of
Durable Object compute a month. Each Hénōsis sync pair uses a handful
of requests per minute while active, and the sockets hibernate when
idle.

## How it works

- Host opens `wss://<relay>/accept?code=<secret>` and waits.
- Friend opens `wss://<relay>/dial?code=<secret>`.
- The Durable Object for that code pairs them, sends `"OK"` on each
  side, and pipes every message from one to the other.
- When either side closes, the other is closed too.
- An unpaired socket is cut after ten minutes.

Bytes between the two sides are the host's HTTP traffic. The relay
cannot read them because they are already encrypted end-to-end
between the two Hénōsis instances (TLS is added in the next step).
