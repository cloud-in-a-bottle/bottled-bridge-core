package bridge

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
)

var (
	curlCookieHeader = regexp.MustCompile(`(?i)(?:-H|--header)\s+(?:'cookie:\s*([^']*)'|"cookie:\s*([^"]*)"|\$'cookie:\s*([^']*)')`)
	curlCookieFlag   = regexp.MustCompile(`(?:-b|--cookie)\s+(?:'([^']*)'|"([^"]*)"|\$'([^']*)')`)
)

// ParseCookies reads cookies pasted in whatever form was handy: a JSON object of name to value, a JSON list of
// {name, value} objects (as cookie-export extensions produce), a "Copy as cURL" command, a Cookie header value, or
// rows copied from the browser's cookie table (name, tab, value, ...).
func ParseCookies(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	out := make(map[string]string)
	switch {
	case strings.HasPrefix(raw, "{"):
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return nil, errors.New("couldn't read the cookies as a JSON object of names to values")
		}
	case strings.HasPrefix(raw, "["):
		var list []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.Unmarshal([]byte(raw), &list); err != nil {
			return nil, errors.New("couldn't read the cookies as a JSON list of {name, value} objects")
		}
		for _, c := range list {
			out[c.Name] = c.Value
		}
	case strings.HasPrefix(raw, "curl "):
		header := ""
		for _, re := range []*regexp.Regexp{curlCookieHeader, curlCookieFlag} {
			if m := re.FindStringSubmatch(raw); m != nil {
				header = m[1] + m[2] + m[3]
				break
			}
		}
		if header == "" {
			return nil, errors.New("the cURL command has no cookies; copy it from a request to the site you log in on")
		}
		parseCookieHeader(header, out)
	case strings.Contains(raw, "\t"):
		for line := range strings.SplitSeq(raw, "\n") {
			fields := strings.Split(strings.TrimSpace(line), "\t")
			if len(fields) >= 2 && fields[0] != "" {
				out[fields[0]] = fields[1]
			}
		}
	default:
		parseCookieHeader(strings.TrimPrefix(raw, "Cookie:"), out)
	}
	if len(out) == 0 {
		return nil, errors.New("no cookies found in the pasted text")
	}
	return out, nil
}

func parseCookieHeader(header string, out map[string]string) {
	for pair := range strings.SplitSeq(header, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if ok && name != "" {
			out[name] = value
		}
	}
}
