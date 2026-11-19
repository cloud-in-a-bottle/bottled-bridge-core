package localmx

import (
	"context"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
)

// plainTextParser renders a message's HTML as the plain text the messaging service carries: formatting is written out
// in the conventions people type (*bold*, _italic_, ~strike~, `code`, @name, ||spoiler||) rather than dropped, and a
// link keeps its target. Matrix's own fallback body is Markdown ([label](url), **bold**), which consumers would show
// as literal punctuation.
var plainTextParser = &format.HTMLParser{
	TabsToSpaces:   4,
	Newline:        "\n",
	HorizontalLine: "\n---\n",
	PillConverter: func(displayname, mxid, eventID string, ctx format.Context) string {
		if strings.HasPrefix(mxid, "@") && eventID == "" {
			if strings.HasPrefix(displayname, "@") {
				return displayname
			}
			return "@" + displayname
		}
		return format.DefaultPillConverter(displayname, mxid, eventID, ctx)
	},
	LinkConverter:          plainLink,
	BoldConverter:          func(s string, _ format.Context) string { return "*" + s + "*" },
	ItalicConverter:        func(s string, _ format.Context) string { return "_" + s + "_" },
	StrikethroughConverter: func(s string, _ format.Context) string { return "~" + s + "~" },
	MonospaceConverter:     func(s string, _ format.Context) string { return "`" + s + "`" },
	SpoilerConverter: func(s, reason string, _ format.Context) string {
		if reason != "" {
			return fmt.Sprintf("||%s: %s||", reason, s)
		}
		return "||" + s + "||"
	},
}

// plainLink writes a link as its target when its label is just a URL (often a shortened form of the target, as in
// Instagram's shares), and as "label (target)" otherwise.
func plainLink(label, href string, _ format.Context) string {
	bare := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(href, "mailto:"), "https://"), "http://")
	switch {
	case label == "", label == href, label == bare:
		return href
	case strings.Contains(label, "://"), strings.HasPrefix(label, "www."):
		return href
	}
	return fmt.Sprintf("%s (%s)", label, href)
}

// plainBody is the plain text of a message's body or caption.
func plainBody(body string, msgFormat event.Format, formatted string) string {
	if msgFormat != event.FormatHTML || formatted == "" {
		return body
	}
	return plainTextParser.Parse(formatted, format.NewContext(context.TODO()))
}
