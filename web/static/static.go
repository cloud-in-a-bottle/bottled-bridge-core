// Package static holds the chat UI's files, which the web package serves under /static/. ui.js (the UI's building
// blocks) and app.css are for other UIs with the same look too, eg the inbox app's: importing this package doesn't
// bring in the rest of the core.
package static

import "embed"

//go:embed *.html *.js *.css
var FS embed.FS
