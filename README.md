# AutoIRC2P

A translated IRC2P client with per-user I2P identities. Go serves the API, WebSocket feed, and static SvelteKit/TypeScript frontend.

## Run

Requires Go 1.27 and Bun. Generated sqlc code is included.

```sh
cd web
bun install --frozen-lockfile
bun run build
cd ..
go run ./cmd/autoirc2p
```

Open `http://localhost:8080`. The server loads `.env`; existing environment variables take precedence. See `.env.example`. Do not overwrite an existing `.env` or commit API keys.

IVNP runs inside the server process. No separate I2P daemon or SAM proxy is required. Reseeding and tunnel construction can take several minutes. **Live feed** shows the browser connection; **I2P network** shows the IRC connection.

`IRC_SERVER` accepts an addressbook hostname or `.b32.i2p` address with a port. The manager resolves hostnames through IVNP's addressbook before dialing the user's destination endpoint. Connection failures identify the readiness, dial, or IRC-session stage in server logs.

For frontend development, start Go with `APP_ORIGIN=http://127.0.0.1:5173`, then run `bun run dev` inside `web`. Vite proxies `/api`, including WebSockets, to `127.0.0.1:8080`. Production needs no Node server. Restart Go after replacing the frontend build so its CSP script hashes match.

## Production deployment

Terminate HTTPS at a reverse proxy and keep the app port private. `deploy/Caddyfile` supports automatic TLS for `PUBLIC_HOST`; `APP_UPSTREAM` defaults to `127.0.0.1:8080`. It overwrites forwarded client addresses and does not retry requests.

For a proxy on the same host, set:

```dotenv
APP_ORIGIN=https://chat.example.org
PUBLIC_HOST=chat.example.org
LISTEN_ADDR=127.0.0.1:8080
TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128
```

Pass `PUBLIC_HOST` to the Caddy process environment and load `deploy/Caddyfile`; Caddy does not load the app's `.env` automatically.

Only trusted socket peers may supply `X-Forwarded-For`. Chains are checked right-to-left; malformed chains fall back to the socket peer. Other forwarding headers are ignored. For container networks, use the proxy's exact internal address, not a trust-all range. Account for bridge/NAT addresses if a host proxy connects through a published container port.

Limits are configurable in `.env.example`:

| Control | Default |
| --- | --- |
| HTTP body / response deadline | 15s / 150s |
| WebSockets: total / per IP / per account | 512 / 8 / 4 |
| Handshake attempts per IP | 30/min, burst 10 |
| Cursor updates per socket | 120/min, burst 30 |
| Send attempts per account / per IP | 20/min / 60/min, bursts 5 / 10 |
| Pending sends / active user destinations | 32 / 16 |
| Unused account connection grace | 2m |

Request limits reject rather than queue more work. The shared observer does not expire. User connections retain their stored identity after idle teardown; active browser tabs and sends hold leases. The browser coalesces read updates to avoid turning normal scrolling into a request burst.

### Container

The Dockerfile builds static SvelteKit assets and a CGO-free Go executable, then uses a non-root distroless runtime. It contains no shell, source tree, `.env`, or application keys. Run with a persistent `/data` volume, a writable `/tmp`, and an explicit `APP_ORIGIN`; a non-loopback listener without an explicit origin is rejected.

```sh
docker run --rm --read-only --cap-drop=ALL \
  --security-opt=no-new-privileges \
  --tmpfs /tmp:rw,nosuid,noexec,size=64m \
  --env-file .env --env APP_ORIGIN=https://chat.example.org \
  --mount type=volume,src=autoirc2p-data,dst=/data \
  --publish 127.0.0.1:8080:8080 autoirc2p:release
```

Provide UID/GID `65532:65532` ownership for bind-mounted data. The image uses `/` as its working directory so IVNP's default `./data` resolves inside the persistent volume. Supply an IVNP configuration if fixed peer transport ports are needed. Size container memory and file-descriptor limits for the configured destination count. Docker image construction and runtime execution were not exercised locally.

## Isolated router state

Set `IVNP_CONFIG` to a private configuration file to test a fresh router without replacing the app database or user destinations. Leave `DATA_DIR` and `application.key` unchanged.

```ini
[paths]
data_dir = /absolute/path/to/new-router-state
[tunnel]
hops = 3
maintenance_interval = 5s
[ntcp2]
enabled = true
max_sessions = 128
[ssu2]
enabled = true
max_sessions = 128
[reseed]
enabled = true
[addressbook]
enabled = true
```

Use mode `0600` for the file. Omitted reseed and addressbook URLs retain IVNP's public defaults. The embedded client disables SAM and management listeners; it uses destination endpoints directly. Judge IRC connectivity by numeric `001` and channel JOIN replies, not aggregate router readiness. SAM readiness failures occur before IRC and are unrelated to IRC PING/PONG deadlines.

## Chat

- Guests can read configured rooms without an account. Sending requires authentication.
- Default channels are listed in `.env.example`; `IRC_ROOMS` overrides them. JOINs are spaced two seconds apart per connection without blocking PING handling. Disconnecting cancels pending joins.
- Search covers every configured channel. Favorites and read cursors are stored per account in this browser. Unread counts exclude service notices and your own messages; a focused conversation advances its cursor only at the latest messages.
- English browsers display English; all other browser languages default to Korean. Readers can override the display language.
- Each message shows its translation, original, and source/target languages. Pending or failed translations remain explicit.
- Sending stays disabled until both the shared receiver and your account have joined the selected room. Drafts remain editable while connecting.
- A single check means the shared receiver observed the actual IRC echo, not recipient delivery or reading. Unconfirmed outgoing items disappear after two minutes; internal request records remain to prevent duplicate sends. A late echo still appears as a real message.
- Outgoing messages default to English. Russian or German takes over only with at least 20 recognized messages and a 60% share within the latest 100 messages. Statistics use the language sent over IRC, not the local draft language.
- Click **Send** or press Enter to translate. Hold for 650 ms and release inside the button, or choose **Send original**, to bypass outgoing translation. Pointer cancellation or release outside the button sends nothing.
- IRC messages carry no translation prefix or browser metadata. Each account has its own nickname and destination. USER and CTCP VERSION use HexChat-style metadata; wording can still reveal machine translation.
- Service messages, NOTICE, and CTCP are not translated. Echoed IRC passwords are redacted. Private service replies reach only their account.
- Only authenticated `POST /api/messages` sends channel messages. Durable request IDs prevent duplicate sends. Reconnects never replay messages. A failed socket write may have delivered the message; the server does not retry it.
- IRC framing rejects control characters and lines exceeding 512 bytes. Oversized translations fail rather than being split or truncated.
- `IRC_IDLE_TIMEOUT` defaults to `20m` for registration and subsequent idle reads. `IRC_PONG_TIMEOUT` defaults to `2m` for PONG writes. Replies are immediate; these limits tolerate transport delays, not intentional pauses. The remote server's own timeout remains outside this client's control.

Database version 3 migrates existing records, preserves identities and idempotency keys, and keeps message IDs monotonic after retention empties a room.

## Translation

`net/http` calls OpenAI-compatible `/chat/completions` directly; no translation SDK is used. Both configured Gemma models participate in rotation and bounded failover. Complete leading `<thought>` blocks are separated from the final translation. Random boundary tokens validate output; protected tokens preserve code and URLs.

SQLite caches by source text, target language, and prompt version. Concurrent requests share one translation regardless of room or user. Defaults: five seconds between provider calls, a 60-second model cooldown after three consecutive failures or HTTP 429. `Retry-After` also delays the shared pacer. Recent failures are briefly cached to prevent request storms.

Anonymous reading has no login gate, but Google's free quota is finite. Quota exhaustion, queue saturation, and provider failure leave the original visible with translation status. Set `TRANSLATION_INTERVAL` for the project's actual quota.

Prompts and boundary handling derive from [website](https://github.com/gosuda/website) and [deeplingua](https://github.com/gosuda/deeplingua). Language detection uses website's 14-language `lingua-go` configuration, the approved dependency exception. Attribution is in `third_party/translation/LICENSE`.

## Authentication and privacy

Set both `IRC_OBSERVER_NICK` and `IRC_OBSERVER_PASSWORD` in `.env` to authenticate the shared read-only observer with NickServ. Existing nicknames are identified; unregistered nicknames use the normal registration flow. Leave both blank to retain the generated guest nickname without NickServ authentication. Partial configuration is rejected.

These credentials do not create a web account or grant guests permission to send. The observer remains read-only after authentication. Its I2P keys stay in the database; changing the configured nickname does not regenerate them. The shared password stays in the environment rather than the database. Restart the server after changing it, and keep `.env` private (`0600`).

The browser derives 256 bits with WebCrypto PBKDF2-SHA256, 600,000 iterations. The server hashes that proof with a separate random salt and SHA-256, then compares in constant time. Public salts are HMAC-derived from normalized email identifiers for both existing and nonexistent accounts.

The browser-derived proof is a reusable credential. Public deployment requires HTTPS. Sessions use HttpOnly, SameSite=Strict cookies and Secure on HTTPS. Authentication limits use the validated client address; arbitrary forwarded headers cannot change that identity.

Registration generates an ElGamal/Ed25519 destination with an LS2 X25519 key, compatible with IVNP Streaming. IRC passwords are random and independent of app passwords. Private destination keys and IRC passwords are AES-GCM encrypted in SQLite. Back up the database **and** `data/application.key`; losing the key breaks existing login salts and identity recovery. Restrict both to the server's OS account.

NickServ authentication requires WHOIS 311/312/313/318 verification and matching NOTICE origins. Credentials are routed to the verified service server. Registration uses a destination-derived `@irc.invalid` address, not the app email. Networks lacking service attestation or `NickServ@server` routing fail closed. Mandatory email verification requires manual recovery.

I2P protects the IRC transport. **The web server sees account mappings and messages; translated text is sent to Google.** Local history remains in SQLite. This is not end-to-end encrypted messaging. Review Google's API data-use policy before deployment.

## Backup and retention

```sh
autoirc2p backup --data-dir data --out /srv/backups/chat-2026-09-06
autoirc2p restore --from /srv/backups/chat-2026-09-06 --data-dir /srv/restored-chat
```

The destination's parent must exist; the destination itself must not. Restore never overwrites live data. Stop the old instance and verify the restored directory before switching `DATA_DIR`. Snapshot creation reads committed WAL data without migrating or recovering the live database.

Bundles contain `chat.sqlite`, `application.key`, and a checksum manifest. Files use mode `0600`, directories `0700`. Restore checks integrity, supported schema, foreign keys, and encrypted credential/key consistency. Checksums detect corruption, not malicious replacement: keep backups in encrypted, access-controlled storage. Re-provision API keys and shared observer credentials separately. Atomic bundle publication currently supports Linux and macOS.

Chat history, translations, and completed outgoing payloads default to 30-day retention (`720h`). Set a category to `0` to retain it indefinitely. Cleanup runs at startup and hourly in bounded batches. Send payload retention must be at least one hour; purged request IDs remain tombstones and cannot trigger a resend. Retention is logical deletion: older backups and WAL/free pages may retain bytes.

## Verify

```sh
sqlc generate
go test -race ./...
gojgp check
cd web
bun run check
bun run build
```

Tests use local HTTP/WebSocket boundaries and offline IVNP objects. They do not post to public IRC rooms. Test ordinary sends only with an isolated transport.
