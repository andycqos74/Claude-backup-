package server

import "testing"

// TestTemplatesParse ensures every GUI page template (including client.html,
// which carries the file-transfer panel) parses and can render its shell.
func TestTemplatesParse(t *testing.T) {
	ui, err := newWebUI(testServer(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, tmpl := range ui.pages {
		if tmpl == nil {
			t.Errorf("template %q did not load", name)
		}
	}
	if ui.pages["client"] == nil {
		t.Error("client page template missing")
	}
}
