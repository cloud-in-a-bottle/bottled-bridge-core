package localmx

import (
	"testing"

	"maunium.net/go/mautrix/event"
)

func TestPlainBody(t *testing.T) {
	reel := "https://www.instagram.com/reel/Dfz18H7IpCQ/?id=4031803319568732304_3570725367&is_sponsored=false"
	cases := []struct {
		name, body, html, want string
	}{
		{"unformatted", "plain *as typed*", "", "plain *as typed*"},
		{"styles", "", "<strong>bold</strong> <em>it</em> <del>gone</del> <code>x := 1</code>", "*bold* _it_ ~gone~ `x := 1`"},
		{"mention", "", `hi <a href="https://matrix.to/#/@fake_alice:fb.local">Alice</a>`, "hi @Alice"},
		{"spoiler", "", `<span data-mx-spoiler>the end</span>`, "||the end||"},
		// Instagram shares label the full link with a shortened copy of it.
		{"url label", "", `<a href="` + reel + `">https://www.instagram.com/reel/Dfz18H7IpCQ/</a>`, reel},
		{"bare url", "", `<a href="https://example.com/">example.com/</a>`, "https://example.com/"},
		{"text label", "", `see <a href="https://example.com/a">the docs</a>`, "see the docs (https://example.com/a)"},
		{"lines", "", "one<br>two", "one\ntwo"},
		{"code block", "", "<pre><code>a\n  b\n</code></pre>", "```\na\n  b\n```"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			format := event.Format("")
			if c.html != "" {
				format = event.FormatHTML
			}
			if got := plainBody(c.body, format, c.html); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
