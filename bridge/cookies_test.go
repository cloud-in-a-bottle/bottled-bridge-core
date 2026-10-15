package bridge

import (
	"maps"
	"testing"
)

func TestParseCookies(t *testing.T) {
	want := map[string]string{"c_user": "100001", "xs": "12%3Aabc", "datr": "d-1"}
	cases := map[string]string{
		"json object":   `{"c_user": "100001", "xs": "12%3Aabc", "datr": "d-1"}`,
		"json list":     `[{"name": "c_user", "value": "100001"}, {"name": "xs", "value": "12%3Aabc"}, {"name": "datr", "value": "d-1"}]`,
		"cookie header": `c_user=100001; xs=12%3Aabc; datr=d-1`,
		"with prefix":   `Cookie: c_user=100001; xs=12%3Aabc; datr=d-1`,
		"curl -H":       `curl 'https://www.messenger.com/' -H 'accept: text/html' -H 'cookie: c_user=100001; xs=12%3Aabc; datr=d-1' -H 'user-agent: x'`,
		"curl -b":       `curl 'https://www.messenger.com/' \` + "\n" + `  -b 'c_user=100001; xs=12%3Aabc; datr=d-1' \` + "\n" + `  -H 'accept: text/html'`,
		"curl windows":  `curl "https://www.messenger.com/" -H "cookie: c_user=100001; xs=12%3Aabc; datr=d-1"`,
		"cookie table":  "c_user\t100001\t.messenger.com\t/\nxs\t12%3Aabc\t.messenger.com\t/\ndatr\td-1\t.messenger.com\t/",
	}
	for name, raw := range cases {
		got, err := ParseCookies(raw)
		if err != nil {
			t.Errorf("%s: %v", name, err)
		} else if !maps.Equal(got, want) {
			t.Errorf("%s: got %v", name, got)
		}
	}
	for _, bad := range []string{"", "curl 'https://www.messenger.com/' -H 'accept: text/html'", "{not json"} {
		if _, err := ParseCookies(bad); err == nil {
			t.Errorf("expected an error for %q", bad)
		}
	}
}
