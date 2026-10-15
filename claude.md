- read README.md and style_guide.md at the beginning of every session.
- Go builds and tests need `-tags sqlite_fts5`; `just` recipes set it via `GOFLAGS`, so prefer `just test`, or export
  `GOFLAGS=-tags=sqlite_fts5` yourself.
- this module is imported by the app repos (bottled-fb-messenger, bottled-whatsapp, bottled-instagram). a change here
  isn't done until it's pushed and each app is bumped to it and still passes its tests: develop against an app with a
  `go.work` (`go work init ./ ../../bottled-fb-messenger/main`, not committed), and commit apps against a pushed
  version (`go get github.com/cloud-in-a-bottle/bottled-bridge-core@<commit>`), never a `replace` to a local path.
- each app pins its own mautrix-go (through its connector). the core must build against all of their versions, so use
  only bridgev2's stable interfaces, and nothing network-specific: that belongs in the app.
