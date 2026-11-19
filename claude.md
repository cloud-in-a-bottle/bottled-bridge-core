- read README.md and style_guide.md at the beginning of every session.
- Go builds and tests need `-tags sqlite_fts5`; `just` recipes set it via `GOFLAGS`, so prefer `just test`, or export
  `GOFLAGS=-tags=sqlite_fts5` yourself.
- this module is imported by the app repos (bottled-fb-messenger, bottled-whatsapp, bottled-instagram). a change here
  isn't done until it's pushed and each app is bumped to it and still passes its tests: develop against an app with a
  `go.work` (`go work init ./ ../../bottled-fb-messenger/main`, not committed), and commit apps against a pushed
  version (`go get github.com/cloud-in-a-bottle/bottled-bridge-core@<commit>`), never a `replace` to a local path.
- the core pins the mautrix-go version (in `go.mod`), and every app must build with exactly that version. to move to a
  newer mautrix (eg for a new mautrix-meta or mautrix-whatsapp release, which mautrix makes against one mautrix commit),
  bump it here first, then bump the apps to the new core. go can't enforce the match (a connector asking for a newer
  mautrix silently wins), so keep the apps' connectors on releases made against the core's mautrix.
- nothing network-specific in the core: that belongs in the app.
