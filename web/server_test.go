package web_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridgetest"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/web"
)

func newServer(t *testing.T) (*httptest.Server, *bridgetest.Harness) {
	h := bridgetest.New(t)
	srv, err := web.New(web.Options{
		Service: h.Svc, Store: h.Store, Files: h.Files, Broker: h.Broker,
		Branding: web.Branding{
			Title: "Bottled Fake", Network: "Fake</script>", AccentColor: "#123456", LoginIntro: "intro",
			HistoryHelp: "help", OnDemandHistory: true,
		},
		Static: fstest.MapFS{"favicon.svg": {Data: []byte("<svg/>")}, "apple-touch-icon.png": {Data: []byte("png")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, h
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

func TestIndexCarriesBranding(t *testing.T) {
	ts, _ := newServer(t)
	status, body := get(t, ts.URL+"/")
	if status != http.StatusOK || !strings.Contains(body, "<title>Bottled Fake</title>") || !strings.Contains(body, "--accent: #123456") {
		t.Fatalf("unexpected index (%d): %s", status, body)
	}
	start := strings.Index(body, `type="application/json">`) + len(`type="application/json">`)
	end := strings.Index(body[start:], "</script>")
	var config web.Branding
	if err := json.Unmarshal([]byte(body[start:start+end]), &config); err != nil {
		t.Fatalf("config isn't JSON: %v: %s", err, body[start:start+end])
	}
	if config.Network != "Fake</script>" || !config.OnDemandHistory {
		t.Fatalf("unexpected config: %+v", config)
	}
	if status, _ = get(t, ts.URL+"/static/app/favicon.svg"); status != http.StatusOK {
		t.Fatalf("app static file: %d", status)
	}
}

func TestMissingAppStaticFilesFail(t *testing.T) {
	h := bridgetest.New(t)
	_, err := web.New(web.Options{
		Service: h.Svc, Store: h.Store, Files: h.Files, Broker: h.Broker,
		Branding: web.Branding{Title: "T", Network: "N", AccentColor: "red", LoginIntro: "i", HistoryHelp: "h"},
		Static:   fstest.MapFS{},
	})
	if err == nil {
		t.Fatal("expected an error for missing icons")
	}
}

func TestNoQRWithoutLogin(t *testing.T) {
	ts, _ := newServer(t)
	if status, _ := get(t, ts.URL+"/api/login/qr"); status != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", status)
	}
}

func TestHistoryEndpoint(t *testing.T) {
	ts, h := newServer(t)
	h.Queue(bridgetest.TextMessage(bridgetest.DMKey(), "alice", "m1", "hi", time.UnixMilli(1_700_000_000_000)))
	status, body := get(t, ts.URL+"/api/conversations/"+strconv.FormatInt(h.ConversationID(bridgetest.DMKey()), 10)+"/history")
	if status != http.StatusOK || !strings.Contains(body, `"has_more":true`) {
		t.Fatalf("unexpected history (%d): %s", status, body)
	}
}

// Paging back with before and before_id neither skips nor repeats messages sent at the same millisecond.
func TestMessagesPageThroughTies(t *testing.T) {
	ts, h := newServer(t)
	at := time.UnixMilli(1_700_000_000_000)
	for _, id := range []string{"a", "b", "c"} {
		h.Queue(bridgetest.TextMessage(bridgetest.DMKey(), "alice", networkid.MessageID("m-"+id), id, at))
	}
	base := ts.URL + "/api/conversations/" + strconv.FormatInt(h.ConversationID(bridgetest.DMKey()), 10) + "/messages"
	var page struct {
		Messages []struct {
			ID     int64  `json:"id"`
			Body   string `json:"body"`
			SentAt int64  `json:"sent_at"`
		} `json:"messages"`
		HasMore bool `json:"has_more"`
	}
	got := ""
	url := base + "?limit=1"
	for {
		status, body := get(t, url)
		if status != http.StatusOK {
			t.Fatalf("unexpected page (%d): %s", status, body)
		}
		if err := json.Unmarshal([]byte(body), &page); err != nil {
			t.Fatal(err)
		}
		got = page.Messages[0].Body + got
		if !page.HasMore {
			break
		}
		first := page.Messages[0]
		url = base + "?limit=1&before=" + strconv.FormatInt(first.SentAt, 10) + "&before_id=" + strconv.FormatInt(first.ID, 10)
	}
	if got != "abc" {
		t.Fatalf("paged out %q", got)
	}
}
