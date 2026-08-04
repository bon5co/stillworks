package audit

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestClientIPPrefersForwardedHeader(t *testing.T) {
	// Behind Traefik every RemoteAddr is the proxy. Getting this wrong does not
	// error; it silently reports one visitor forever.
	request := httptest.NewRequest(http.MethodGet, "/llm/", nil)
	request.RemoteAddr = "10.0.0.7:54321"
	request.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := clientIP(request); got != "203.0.113.9" {
		t.Fatalf("clientIP = %q, want the leftmost forwarded address", got)
	}

	bare := httptest.NewRequest(http.MethodGet, "/llm/", nil)
	bare.RemoteAddr = "198.51.100.4:9999"
	if got := clientIP(bare); got != "198.51.100.4" {
		t.Fatalf("clientIP = %q, want the host without its port", got)
	}
}

func TestVisitorHashRotatesDailyAndHidesTheAddress(t *testing.T) {
	recorder := &Recorder{salt: "test-salt"}
	day := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	recorder.now = func() time.Time { return day }

	first := recorder.visitorHash("203.0.113.9", "curl/8.5")
	same := recorder.visitorHash("203.0.113.9", "curl/8.5")
	if first != same {
		t.Fatal("the same visitor hashed differently within one day")
	}
	if first == recorder.visitorHash("203.0.113.10", "curl/8.5") {
		t.Fatal("two addresses collided")
	}
	if first == recorder.visitorHash("203.0.113.9", "Mozilla/5.0") {
		t.Fatal("two user agents collided")
	}

	recorder.now = func() time.Time { return day.AddDate(0, 0, 1) }
	if first == recorder.visitorHash("203.0.113.9", "curl/8.5") {
		t.Fatal("the hash did not rotate across a UTC day boundary")
	}

	unsalted := &Recorder{salt: "other-salt", now: func() time.Time { return day }}
	if first == unsalted.visitorHash("203.0.113.9", "curl/8.5") {
		t.Fatal("the salt is not part of the hash input")
	}
}

func TestReferrerHostDropsInternalNavigationAndQueryStrings(t *testing.T) {
	cases := []struct {
		referer string
		own     string
		want    string
	}{
		{"https://news.ycombinator.com/item?id=123", "stillworks.supercapybara.com", "news.ycombinator.com"},
		{"https://stillworks.supercapybara.com/llm/", "stillworks.supercapybara.com", ""},
		{"https://Stillworks.Supercapybara.com/llm/", "stillworks.supercapybara.com:443", ""},
		{"", "stillworks.supercapybara.com", ""},
		{"not a url", "stillworks.supercapybara.com", ""},
	}
	for _, testCase := range cases {
		if got := referrerHost(testCase.referer, testCase.own); got != testCase.want {
			t.Errorf("referrerHost(%q) = %q, want %q", testCase.referer, got, testCase.want)
		}
	}
}

func TestIsCrawlerKeepsAgentClients(t *testing.T) {
	// The audience for this site is scripted. Filtering automation as robots
	// would delete the exact metric the project is judged on.
	keep := []string{
		"curl/8.5.0",
		"python-httpx/0.27.0",
		"OpenAI/Python 1.40.0",
		"node-fetch/1.0",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 Chrome/127 Safari/537.36",
		"",
	}
	for _, agent := range keep {
		if isCrawler(agent) {
			t.Errorf("isCrawler(%q) = true, want false", agent)
		}
	}
	drop := []string{
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
		"Mozilla/5.0 (compatible; bingbot/2.0)",
		"facebookexternalhit/1.1",
		"Scrapy/2.11 (+https://scrapy.org)",
		"Better Uptime Bot",
	}
	for _, agent := range drop {
		if !isCrawler(agent) {
			t.Errorf("isCrawler(%q) = false, want true", agent)
		}
	}
}

func TestTrackRecordsTheServedStatus(t *testing.T) {
	recorder := &Recorder{
		events: make(chan TrafficEvent, 4),
		salt:   "test-salt",
		now:    time.Now,
	}
	handler := recorder.Track(KindAPI, func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusNotFound)
		_, _ = response.Write([]byte(`{"error":"unknown endpoint"}`))
	})

	request := httptest.NewRequest(http.MethodGet, "/api/llm/nope", nil)
	request.Header.Set("User-Agent", "curl/8.5.0")
	request.Header.Set("Referer", "https://news.ycombinator.com/item?id=1")
	response := httptest.NewRecorder()
	handler(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status passed through as %d", response.Code)
	}
	select {
	case event := <-recorder.events:
		if event.Kind != KindAPI || event.Path != "/api/llm/nope" || event.Status != http.StatusNotFound {
			t.Fatalf("recorded %+v", event)
		}
		if event.ReferrerHost != "news.ycombinator.com" {
			t.Fatalf("referrer recorded as %q", event.ReferrerHost)
		}
		if event.IsCrawler {
			t.Fatal("curl was recorded as a crawler")
		}
	default:
		t.Fatal("no event was queued")
	}
}

func TestTrackNeverBlocksWhenTheBufferIsFull(t *testing.T) {
	// A full buffer must cost the request nothing. Dropping is the designed
	// failure, and it has to be visible.
	recorder := &Recorder{
		events: make(chan TrafficEvent, 1),
		salt:   "test-salt",
		now:    time.Now,
	}
	handler := recorder.Track(KindPage, func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	})
	for range 5 {
		handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}
	if recorder.Dropped() != 4 {
		t.Fatalf("dropped = %d, want 4", recorder.Dropped())
	}
}

func TestNilRecorderStillServes(t *testing.T) {
	// A management process wires the app without a recorder. It must not panic
	// and must not silently refuse to serve.
	var recorder *Recorder
	served := false
	handler := recorder.Track(KindPage, func(http.ResponseWriter, *http.Request) { served = true })
	handler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !served {
		t.Fatal("a nil recorder swallowed the request")
	}
	if recorder.Dropped() != 0 {
		t.Fatal("a nil recorder reported drops")
	}
}

func TestCloseWaitsForTheWriterAndTimesOut(t *testing.T) {
	// The shutdown path is the one that decides whether a deploy loses the
	// requests it just served, so it is asserted rather than assumed.
	finished := &Recorder{closed: make(chan struct{}), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	close(finished.closed)
	done := make(chan struct{})
	go func() { finished.Close(time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close blocked on a writer that had already finished")
	}

	stuck := &Recorder{closed: make(chan struct{}), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	start := time.Now()
	stuck.Close(20 * time.Millisecond)
	if time.Since(start) > time.Second {
		t.Fatal("Close did not give up on a stuck writer")
	}

	var absent *Recorder
	absent.Close(time.Second) // a management process has no recorder
}

func TestInternalNetworksMatchAddressesAndBlocks(t *testing.T) {
	// The addresses this recognises decide which numbers the kill rule reads,
	// so both directions are asserted: ours must match, everyone else must not.
	networks := ParseInternalNetworks(" 106.73.62.0 , 10.0.0.0/8 , nonsense , ", slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, address := range []string{"106.73.62.0", "10.4.5.6", "10.0.0.1"} {
		if !networks.Contains(address) {
			t.Errorf("Contains(%q) = false, want true", address)
		}
	}
	for _, address := range []string{"106.73.62.1", "203.0.113.9", "", "not-an-address"} {
		if networks.Contains(address) {
			t.Errorf("Contains(%q) = true, want false", address)
		}
	}

	// An empty setting must not accidentally match everything: that would zero
	// the numbers rather than clean them.
	empty := ParseInternalNetworks("", nil)
	if empty.Contains("203.0.113.9") {
		t.Fatal("an empty INTERNAL_NETWORKS matched a visitor")
	}
}

func TestRecordMarksOurOwnRequests(t *testing.T) {
	recorder := &Recorder{
		events:   make(chan TrafficEvent, 4),
		salt:     "test-salt",
		now:      time.Now,
		internal: ParseInternalNetworks("106.73.62.0", nil),
	}
	ours := httptest.NewRequest(http.MethodGet, "/llm/", nil)
	ours.Header.Set("X-Forwarded-For", "106.73.62.0")
	recorder.Record(ours, KindPage, http.StatusOK)

	theirs := httptest.NewRequest(http.MethodGet, "/llm/", nil)
	theirs.Header.Set("X-Forwarded-For", "203.0.113.9")
	recorder.Record(theirs, KindPage, http.StatusOK)

	first := <-recorder.events
	second := <-recorder.events
	if !first.IsInternal {
		t.Error("our own request was recorded as a visitor")
	}
	if second.IsInternal {
		t.Error("a visitor was recorded as internal")
	}
}
