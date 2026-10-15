package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
)

// Branding is what makes the UI one network's client. Texts are plain text unless noted.
type Branding struct {
	// The page title and the setup screen's heading, eg "Bottled Messenger".
	Title string `json:"title"`
	// The network's name as the UI uses it in sentences ("Connecting to Messenger…"), eg "Messenger".
	Network string `json:"network"`
	// A CSS color for buttons, links and your own messages.
	AccentColor string `json:"-"`
	// The setup screen's explanation, above the login options.
	LoginIntro string `json:"login_intro"`
	// The order and wording of the connector's login flows. Flows not listed come after, with the connector's own
	// name and description.
	LoginFlows []LoginFlowLabel `json:"login_flows"`
	// Shown when the network asks for a login step this UI can't do, eg "Try logging in with cookies instead."
	LoginFallbackHint string `json:"login_fallback_hint"`
	// Settings › History: how much history this app gets from the network, and how.
	HistoryHelp string `json:"history_help"`
	// Set when chats can be started with something the user types, eg "Phone number": ResolveIdentifier gets it.
	IdentifierLabel       string `json:"identifier_label,omitempty"`
	IdentifierPlaceholder string `json:"identifier_placeholder,omitempty"`
	// Show a button to fetch a chat's older messages from the network, a page at a time (bridge.Service.LoadOlder).
	OnDemandHistory bool `json:"on_demand_history"`
	// JS modules in the app's static files that add sections to settings.
	Extensions []Extension `json:"extensions"`
}

type LoginFlowLabel struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Hint  string `json:"hint"`
}

// Extension is a JS module (a path within Options.Static) exporting settingsSection(ctx), which returns an element
// to add to settings. ctx has api (the fetch helper), conversations() (the current list) and on(kind, fn), which
// gets the data of server-sent events of the listed kinds.
type Extension struct {
	Module string   `json:"module"`
	Events []string `json:"events"`
}

func (b Branding) validate() error {
	if b.Title == "" || b.Network == "" || b.AccentColor == "" || b.LoginIntro == "" || b.HistoryHelp == "" {
		return errors.New("branding needs a title, network name, accent color, login intro and history help")
	}
	return nil
}

type indexData struct {
	Title  string
	Accent template.CSS
	Config template.JS
}

func renderIndex(b Branding) ([]byte, error) {
	src, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		return nil, err
	}
	tpl, err := template.New("index").Parse(string(src))
	if err != nil {
		return nil, err
	}
	if b.LoginFlows == nil {
		b.LoginFlows = []LoginFlowLabel{}
	}
	if b.Extensions == nil {
		b.Extensions = []Extension{}
	}
	config, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	// The accent color is the app's own constant, not user input.
	err = tpl.Execute(&out, indexData{Title: b.Title, Accent: template.CSS(b.AccentColor), Config: template.JS(config)})
	return out.Bytes(), err
}
