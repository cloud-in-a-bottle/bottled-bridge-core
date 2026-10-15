# bottled-bridge-core

The shared core of the "bottled" chat clients: OpenHost apps that log in to a chat network as you, keep every
conversation in a sqlite database on your server, and put a web chat UI on top. Each app (bottled-fb-messenger,
bottled-whatsapp, bottled-instagram) is a thin `main` that picks a [mautrix](https://github.com/mautrix) network
connector, configures it, and brands the UI; everything else is here.

## how it works

mautrix's bridges are built on [bridgev2](https://pkg.go.dev/maunium.net/go/mautrix/bridgev2), which splits a bridge
in two: a *network connector* that speaks the remote protocol (mautrix-meta's Messenger and Instagram connectors,
mautrix-whatsapp's, used unmodified as libraries), and a *Matrix connector* that normally talks to a homeserver. This
module replaces the second half:

- `localmx/` implements bridgev2's `MatrixConnector` without Matrix. Where the bridge would create a room, set its
  name or members, send a message, react, redact, or batch-send history to a homeserver, this writes to the message
  store instead. Rooms are conversations, ghost users are people, the bridge's single user is you (and your "double
  puppet", for things you do on other devices), and uploaded media goes into a content-addressed file store. Event
  IDs for messages are derived from the network's message IDs, so a message delivered twice (live and by backfill,
  or again after a restart) is stored once.
- `store/` is the message store (`schema.sql`, with ordered migrations in `migrate.go`): people, conversations,
  messages (with their parts, edits, reactions and read receipts), attachments, full-text search, and a raw copy of
  every event as the bridge delivered it. It also has the helpers an importer of exported history needs (matching
  chats, people known only by name, deduplication).
- `bridge/` sets up the bridge around the app's connector (`bridge.Options`; `DefaultConfig` is deliberately
  conservative about traffic), drives logins, turns UI actions into the Matrix-shaped events the bridge expects from a
  client, and fetches older history a page at a time on request (`LoadOlder`).
- `web/` is the HTTP API and the UI (plain JS modules embedded in the binary, no build step), with live updates over
  SSE (`/api/events`). The app brands it (`web.Branding`), supplies its icons, and can add routes and settings
  sections (eg Messenger's importer).
- `web/messaging*.go` provides the [messaging service](https://github.com/cloud-in-a-bottle/messaging-service-spec)
  to other apps, under `/messaging/`, when the app sets `web.Options.Messaging`: the same store and bridge, in the
  spec's shapes, with what each chat allows taken from the features the connector reports for it.
- `bridgetest/` runs all of the above against a fake network connector; the core's tests use it, and apps use it to
  seed a development database.

## using it from an app

```go
svc, err := bridge.New(bridge.Options{Connector: conn, DBOwner: "bottled-x", MaxFileSize: 512 << 20}, cfg.BridgeDBPath, st, fs, broker, log)
srv, err := web.New(web.Options{Config: cfg, Service: svc, Store: st, Files: fs, Broker: broker, Branding: branding, Static: appStatic, Log: log})
```

To provide the messaging service, also set `Messaging: &web.MessagingOptions{Network: "x"}` and declare it in
`openhost.toml`:

```toml
[[services.v2.provides]]
service = "github.com/cloud-in-a-bottle/messaging-service-spec"
version = "0.1.0"
endpoint = "/messaging/"
```

See bottled-fb-messenger's `cmd/bottled-fb-messenger/main.go` for the whole wiring.

## development

```bash
just setup   # go deps and the pre-commit hooks
just test    # the tests
just check   # gofmt, go vet
```

Go builds need `-tags sqlite_fts5` (FTS5 for search); the justfile sets `GOFLAGS` for that.

## license

AGPL-3.0, as the apps link mautrix's connectors (AGPL-3.0).
