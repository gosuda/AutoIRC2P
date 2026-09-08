# AutoIRC2P

Read and chat on IRC2P with optional automatic translation. Each account has its own persistent I2P destination pool. The I2P router is built in—no separate daemon is needed.

## One-command Docker launch

From the cloned repository, with `OPENAI_API_KEY` exported in your shell, run this single command to build and start Portalite mode—no `.env`, Compose, domain, or inbound ports needed:

```sh
: "${OPENAI_API_KEY:?Export OPENAI_API_KEY first}" && docker build -t autoirc2p . && docker run -d \
  --name autoirc2p --restart unless-stopped \
  --read-only --cap-drop=ALL --security-opt=no-new-privileges \
  --tmpfs /tmp:rw,nosuid,noexec,size=64m,mode=1777 \
  --mount type=volume,src=autoirc2p-data,dst=/data \
  --env PORTALITE=1 --env OPENAI_API_KEY \
  --env OPENAI_BASE_URL=https://generativelanguage.googleapis.com/v1beta/openai \
  autoirc2p
```

Run `docker logs -f autoirc2p` and open a ready HTTPS URL. The command uses Google's API; change `OPENAI_BASE_URL` and pass `OPENAI_MODEL_0`/`OPENAI_MODEL_1` for another provider. The key is forwarded from your environment rather than included in the command arguments.

The image builds and includes the frontend at `/web/build`. If you override `WEB_DIR`, use `/web/build` or `web/build` (the container working directory is `/`). Mount persistent data at `/data`, not over the bundled frontend.

This is an alternative to Compose, with its own `autoirc2p-data` volume. To rebuild, first run `docker stop autoirc2p && docker rm autoirc2p`, then repeat the command; the volume preserves your data and Portalite identity. Do not run it alongside an existing deployment when migrating the same accounts.

## Quick start with Docker Compose

You need Docker Engine with Compose v2 and an OpenAI-compatible API key. The example uses Google's API and Gemma models.

### 1. Get the app

```sh
git clone https://github.com/gosuda/AutoIRC2P.git
cd AutoIRC2P
cp .env.example .env
chmod 600 .env
```

Already installed? Keep your existing `.env` and data instead of copying over them.

### 2. Choose how to connect

Set `OPENAI_API_KEY` in `.env`, then choose **one** mode.

**Your own domain (default)**

Point your domain's DNS at the deployment server and open TCP ports 80 and 443. Caddy handles HTTPS automatically.

```dotenv
COMPOSE_FILE=compose.yaml
PORTALITE=0
PUBLIC_HOST=chat.example.org
OPENAI_API_KEY=your-api-key
```

**Portalite: no domain or inbound ports**

[Portalite](https://github.com/gosuda/portalite/blob/master/docs/usage.md) provides public HTTPS URLs through outbound relay connections. No Caddy or `PUBLIC_HOST` is needed.

```dotenv
COMPOSE_FILE=compose.portalite.yaml
PORTALITE=1
OPENAI_API_KEY=your-api-key
```

Leave `PORTALITE_RELAYS` empty for the default relays, or set a comma-separated list such as `https://rly.best,https://gosunuts.xyz`. The generated private identity is saved in the data volume; keep it to retain the same identity across restarts. Relay availability is not guaranteed.

Leave `PORTALITE_NAME` empty to generate a unique name, or choose an available name before the first start. Uppercase input is accepted; the SDK normalizes the DNS name to lowercase. Changing it later does not replace the saved identity.

### 3. Start and open

```sh
docker compose up -d --build --remove-orphans
docker compose logs -f app
```

Open `https://chat.example.org` in domain mode, or a ready HTTPS URL printed in the logs in Portalite mode.

The first I2P connection can take several minutes. **Live feed** means your browser is connected; **I2P network** shows whether IRC is connected. Guests can read; create an account to send messages.

Rooms appear in a vertically scrollable list from startup, ordered by the latest message. New activity reorders the list without switching your open conversation.

**Auto-translate** defaults to **Off**. Off displays and sends original text. On translates incoming messages into your selected reading language and outgoing messages into the fixed target below. **Send original** bypasses translation for one message. The setting is saved separately for guests and each account.

Outgoing translation targets depend only on the room name, ignoring case:

| Room | Translation target |
| --- | --- |
| `#ru` or a name ending in `-ru` | Russian |
| `#de` or a name ending in `-de` | German |
| `#ko` | Korean |
| All other rooms | English |

There is no automatic message-language detection or room-language inference. Open a message's details to see its original text, translation target, and timestamp. NickServ and ChanServ messages remain hidden from chat without disabling service authentication.

The shared router and IRC reader stay active without browser users and retry startup failures and disconnects until the app shuts down. Account sessions also retry nickname conflicts while leased, allowing stale IRC registrations to expire without changing the IRC nickname. Every retry waits **1 second**; tunnel preparation and network timeouts can take longer. Registered IRC sessions send **PING every 30 seconds** (sooner if half the configured idle timeout is shorter). Incoming traffic, including PONG, keeps the session alive; an unresponsive connection still expires under `IRC_IDLE_TIMEOUT`. Recovery never replays outgoing messages.

Without `IRC_OBSERVER_NICK` and `IRC_OBSERVER_PASSWORD`, the shared reader uses a random `Irc2PGuest00000`–`Irc2PGuest99999` nickname on every IRC connection, excluding its previous nickname. Nickname conflicts trigger the same one-second reconnect with a new nickname. Explicit observer NickServ credentials and individual account nicknames remain fixed.

Each account and the shared reader maintain **three I2P destinations**, isolated from other accounts. Failed attempts and disconnected sessions select the next destination in round-robin order; only one IRC connection is active per account. The original identity remains the first entry, and two additional identities are encrypted in the database and reused across restarts. Healthy destinations keep their tunnels and route caches; initial readiness is not repeated during LeaseSet renewal. Browser reconnects reuse leased account sessions within `IRC_ACCOUNT_IDLE_GRACE` (default two minutes).

Missing or expired local tunnel routes recreate only the affected pool entry, using the same saved keys. A channel-level `437` response marks only that room unavailable. A failed browser session refresh leaves the existing feed connected; WebSocket retry backoff resets only after identity verification and the room snapshot. Account network status is not overwritten by shared-reader status.

NickServ verification timeout does not discard a partially received IRC frame;
the next read resumes it under the same 512-byte wire limit. An IRC `KILL` or
`ERROR` containing `spambot kill` is a server-side refusal, not a tunnel timeout.
The pool does not remove nickname-level limits or operator bans. Obtain the IRC
operator's permission for bots and relaying; use `IRC_OFFLINE=1` while resolving
an explicit persistent refusal.

## Customize

Edit `.env`, then run the start command again.

| Setting | Use |
| --- | --- |
| `IRC_ROOMS='#i2p,#i2p-chat,#ko'` | Channels to join; keep the whole value quoted so `#` is preserved. |
| `OPENAI_BASE_URL`, `OPENAI_MODEL_0`, `OPENAI_MODEL_1` | Choose your translation provider and models. |
| `TRANSLATION_INTERVAL=5s` | Increase this if you hit your provider's quota. |
| `IRC_OBSERVER_NICK`, `IRC_OBSERVER_PASSWORD` | Optional NickServ credentials for the shared reader; set both or neither. |
| `RATE_LIMIT_ENABLED=0` | Disable login, WebSocket handshake, message-send, and read-cursor request quotas. Default: `1`. |
| `TRUSTED_PROXY_CIDRS=0.0.0.0/0,::/0` | Explicitly trust forwarded client addresses from any peer, including in Portalite mode. |

Disabling request quotas leaves connection/queue capacity limits and translation-provider pacing active. Trust-all permits forged `X-Forwarded-For` values; use it only when you accept that risk. Both Compose modes honor an explicit trusted-proxy value; their existing defaults remain unchanged. For standalone `docker run`, pass these settings with `--env RATE_LIMIT_ENABLED=0` and `--env TRUSTED_PROXY_CIDRS=0.0.0.0/0,::/0`.

`ivnp.conf` is created automatically if missing: `/data/ivnp.conf` in Docker, `data/ivnp.conf` for a native run. The default tunnel length is **1 hop**. To change it, edit the `[tunnel]` section and restart:

```ini
[tunnel]
hops = 3
```

Existing explicit hop settings are preserved. One hop offers less anonymity margin than multiple hops.

Configured tunnel pool capacities are preserved. IVNP removes replaced tunnels from selection but retains their execution state until the original advertised expiration, so renewal does not interrupt peers still using cached leases.

The default `IRC_MAX_ACCOUNTS=16` requires capacity for 52 destinations, including the reader pool and IVNP's default destination. If `[state] max_destinations` is omitted, embedded capacity grows with the account limit, up to 256 destinations (84 accounts). Explicit destination limits are preserved. For example, `IRC_MAX_ACCOUNTS=62` requires 190 destinations; pools are created only for active accounts.

## Updates and data

```sh
git pull
docker compose up -d --build --remove-orphans
```

`docker compose down` stops the deployment without deleting data. **Do not add `-v` unless you intend to erase the volumes.** Both deployment modes share the same application volume; switching modes removes the old Caddy container with `--remove-orphans`.

Back up the application data, including `chat.sqlite`, `application.key`, and `portalite-identity.json` if present. Do not copy a live SQLite database without a consistent backup. Native installations can use `autoirc2p backup --data-dir data --out /path/to/new-backup`; the parent directory must exist. Host `data/` is not automatically imported into Docker.

History is retained for 30 days by default. More settings are listed in [.env.example](.env.example).

**Privacy:** the app server sees messages and account mappings; translated text is sent to your translation provider. I2P protects the IRC transport, not the browser connection. This is not end-to-end encrypted messaging. Keep `.env`, backups, and private keys secret.

## Run without Docker

Requires Go 1.27 and Bun. Create `.env` from the example and set your API key, then:

```sh
cd web
bun install --frozen-lockfile
bun run build
cd ..
go run ./cmd/autoirc2p
```

Open `http://localhost:8080`, or set `PORTALITE=1` and use the HTTPS URL in the logs. Native runs ignore `COMPOSE_FILE`; existing process environment variables override `.env`.

If port 8080 is occupied, set both `LISTEN_ADDR=127.0.0.1:18080` and `APP_ORIGIN=http://localhost:18080` in `.env`, then open `http://localhost:18080`. Build the frontend **before** starting Go; its inline-script CSP hash is loaded at server startup, so rebuilding assets requires an application restart.

For development checks: `gojgp check`, `go test -race ./...`, and `bun run check` in `web/`. `IRC_OFFLINE=1` skips live I2P startup for local inspection; it does not disable Portalite.

Translation prompts derive from [website](https://github.com/gosuda/website) and [deeplingua](https://github.com/gosuda/deeplingua); attribution is in [third_party/translation/LICENSE](third_party/translation/LICENSE).
