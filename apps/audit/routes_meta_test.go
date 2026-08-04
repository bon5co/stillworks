package audit

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A malformed PUBLIC_ORIGIN is the one configuration mistake that takes the
// whole site down without saying anything: the view layer resolves the document
// head before writing a byte, so every HTML page answers 200 with an empty body
// while /healthz stays 204 and the JSON API keeps serving. The server refuses
// to start instead. A trailing slash is the likeliest thing to be typed.
func TestValidPublicOriginRefusesWhatTheHeadCannotResolve(t *testing.T) {
	valid := []string{
		"",
		"https://stillworks.supercapybara.com",
		"http://127.0.0.1:8115",
		"https://example.test:8443",
	}
	for _, origin := range valid {
		if err := ValidPublicOrigin(origin); err != nil {
			t.Errorf("ValidPublicOrigin(%q) = %v, want nil", origin, err)
		}
	}
	invalid := []string{
		"https://stillworks.supercapybara.com/",
		"stillworks.supercapybara.com",
		"https://example.test/llm/",
		"https://example.test?a=b",
		"ftp://example.test",
		"https://",
	}
	for _, origin := range invalid {
		if err := ValidPublicOrigin(origin); err == nil {
			t.Errorf("ValidPublicOrigin(%q) = nil, want a refusal", origin)
		}
	}
}

// With no origin configured, this site publishes no canonical and no social
// card rather than resolving either against the Host header -- which is the
// client's own text, and is how a request arriving under someone else's name
// writes that name into this page's canonical URL.
func TestNoOriginMeansNoCanonicalAndNoCardRatherThanTheHostHeader(t *testing.T) {
	previous := publicOrigin
	defer SetPublicOrigin(previous)

	SetPublicOrigin("")
	body := renderShelfWithHost(t, "evil.example.com")
	for _, forbidden := range []string{"canonical", "og:image", "og:url", "evil.example.com"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("with no origin configured the head still carries %q:\n%s", forbidden, head(body))
		}
	}
	if !strings.Contains(body, `name="description"`) {
		t.Fatal("the description is origin-free and should still be published")
	}

	SetPublicOrigin("https://stillworks.supercapybara.com")
	body = renderShelfWithHost(t, "evil.example.com")
	if !strings.Contains(body, `href="https://stillworks.supercapybara.com/llm/"`) {
		t.Fatalf("canonical did not use the configured origin:\n%s", head(body))
	}
	if strings.Contains(body, "evil.example.com") {
		t.Fatalf("the Host header reached the head despite a configured origin:\n%s", head(body))
	}
}

// The error page and the 404 declare no description and no canonical, so they
// get no link preview. A shared 404 that previews as the finished product card
// is a small lie of the kind this site exists to complain about.
func TestErrorPagesDoNotPreviewAsTheProduct(t *testing.T) {
	previous := publicOrigin
	defer SetPublicOrigin(previous)
	SetPublicOrigin("https://stillworks.supercapybara.com")

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/llm/nope", nil)
	render(response, request, page{Title: "Not found — stillworks"}, NotFoundPage("no such endpoint"))
	body := response.Body.String()
	for _, forbidden := range []string{"og:image", "og:title", "canonical"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the 404 page advertises %q:\n%s", forbidden, head(body))
		}
	}
	// It still gets a tab icon: that href is relative and needs no origin.
	if !strings.Contains(body, `rel="icon"`) {
		t.Fatal("the 404 page lost its favicon link")
	}
}

func renderShelfWithHost(t *testing.T, host string) string {
	t.Helper()
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/llm/", nil)
	request.Host = host
	render(response, request, page{
		Title:       "Free keyless LLM endpoints — stillworks",
		Description: "Endpoints that answer with no API key.",
		Canonical:   "/llm/",
	}, NotFoundPage("body is irrelevant here"))
	if response.Code != http.StatusOK {
		t.Fatalf("render answered %d, want 200 — the head failed to resolve", response.Code)
	}
	if response.Body.Len() == 0 {
		t.Fatal("render wrote an empty body with a 200, which is the failure this test exists for")
	}
	return response.Body.String()
}

func head(body string) string {
	if index := strings.Index(body, "</head>"); index >= 0 {
		return body[:index]
	}
	return body
}
