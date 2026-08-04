package audit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bon5co/godjango/database"
	"github.com/uptrace/bun"
)

// Traffic event kinds. The runtime API and the pages are separate metrics
// because they answer separate questions: whether an agent found this useful,
// and whether a human did.
const (
	KindAPI  = "api"
	KindPage = "page"
)

const (
	// trafficBuffer is how many events may be waiting to be written before new
	// ones are dropped. Dropping is the correct failure: a request must never
	// wait on the measurement of a previous one.
	trafficBuffer = 2048
	// trafficBatch and trafficFlush bound how long an event sits in memory. A
	// crash loses at most one flush interval of counts, which is a better
	// trade than one INSERT per request.
	trafficBatch = 100
	trafficFlush = 2 * time.Second
	// userAgentLimit keeps a hostile or absurd header from bloating the table.
	userAgentLimit = 300
)

// TrafficEvent is one request worth counting. Append-only, like the probe
// table, and for the same reason: the record is the point.
type TrafficEvent struct {
	bun.BaseModel `bun:"table:traffic_events,alias:t"`

	ID           int64     `bun:"id,pk,autoincrement"`
	OccurredAt   time.Time `bun:"occurred_at,nullzero,notnull,default:now()"`
	Kind         string    `bun:"kind,notnull"`
	Path         string    `bun:"path,notnull"`
	Status       int       `bun:"status,notnull"`
	VisitorHash  string    `bun:"visitor_hash,notnull"`
	ReferrerHost string    `bun:"referrer_host,notnull"`
	UserAgent    string    `bun:"user_agent,notnull"`
	IsCrawler    bool      `bun:"is_crawler,notnull"`
	IsInternal   bool      `bun:"is_internal,notnull"`
}

// InternalNetworks is the set of addresses whose requests are our own work --
// the machine this is developed on, the host it is deployed to, anything else
// named in INTERNAL_NETWORKS. It exists because the first day's numbers read
// fifteen visitors and every one of them was a deploy check.
type InternalNetworks struct {
	prefixes []netip.Prefix
}

// ParseInternalNetworks reads a comma-separated list of addresses or CIDR
// blocks. An unparseable entry is skipped rather than fatal: a typo in one
// deployment variable must not stop the site from serving, and the consequence
// is only that some of our own traffic gets counted as somebody else's -- which
// is the situation this whole change is correcting, not a new failure.
func ParseInternalNetworks(raw string, logger *slog.Logger) InternalNetworks {
	var networks InternalNetworks
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			networks.prefixes = append(networks.prefixes, prefix)
			continue
		}
		address, err := netip.ParseAddr(entry)
		if err != nil {
			if logger != nil {
				logger.Warn("stillworks ignoring unparseable internal network", "entry", entry, "error", err)
			}
			continue
		}
		networks.prefixes = append(networks.prefixes, netip.PrefixFrom(address, address.BitLen()))
	}
	return networks
}

// Contains reports whether an address belongs to us.
func (n InternalNetworks) Contains(address string) bool {
	parsed, err := netip.ParseAddr(address)
	if err != nil {
		return false
	}
	for _, prefix := range n.prefixes {
		if prefix.Contains(parsed) {
			return true
		}
	}
	return false
}

type trafficSalt struct {
	bun.BaseModel `bun:"table:traffic_salt,alias:ts"`

	ID   int16  `bun:"id,pk"`
	Salt string `bun:"salt,notnull"`
}

// Recorder writes traffic events without ever making a request wait for the
// database. Events go onto a buffered channel and a single goroutine batches
// them out; when the buffer is full events are counted and discarded rather
// than queued, because a measurement that slows the thing being measured is
// worse than a gap in the numbers.
type Recorder struct {
	events  chan TrafficEvent
	salt    string
	logger  *slog.Logger
	dropped atomic.Int64
	// now is injected so the daily rotation of the visitor hash is testable
	// without waiting a day.
	now func() time.Time
	// closed is closed once the writer has flushed and returned, so shutdown
	// can wait for the last batch instead of racing the process exit.
	closed chan struct{}
	// internal is the set of addresses whose requests are our own work.
	internal InternalNetworks
}

// StartRecorder loads (or creates) the hashing salt and starts the writer for
// the lifetime of ctx.
func StartRecorder(
	ctx context.Context,
	db *database.DB,
	internal InternalNetworks,
	logger *slog.Logger,
) (*Recorder, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stdout, nil))
	}
	salt, err := loadOrCreateSalt(ctx, db)
	if err != nil {
		return nil, err
	}
	recorder := &Recorder{
		events:   make(chan TrafficEvent, trafficBuffer),
		salt:     salt,
		logger:   logger,
		now:      time.Now,
		closed:   make(chan struct{}),
		internal: internal,
	}
	go recorder.write(ctx, db)
	return recorder, nil
}

// loadOrCreateSalt is an insert-then-read rather than a read-then-insert so two
// instances starting at once cannot end up hashing with different salts, which
// would silently double-count every visitor.
func loadOrCreateSalt(ctx context.Context, db *database.DB) (string, error) {
	generated := make([]byte, 32)
	if _, err := rand.Read(generated); err != nil {
		return "", err
	}
	candidate := trafficSalt{ID: 1, Salt: hex.EncodeToString(generated)}
	if _, err := db.Bun().NewInsert().
		Model(&candidate).
		On("CONFLICT (id) DO NOTHING").
		Exec(ctx); err != nil {
		return "", err
	}
	var stored trafficSalt
	if err := db.Bun().NewSelect().
		Model(&stored).
		Where("id = 1").
		Scan(ctx); err != nil {
		return "", err
	}
	return stored.Salt, nil
}

func (r *Recorder) write(ctx context.Context, db *database.DB) {
	defer close(r.closed)

	batch := make([]TrafficEvent, 0, trafficBatch)
	ticker := time.NewTicker(trafficFlush)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		// Deliberately not ctx: a shutdown must still be able to write the
		// events it already accepted.
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if _, err := db.Bun().NewInsert().Model(&batch).Exec(writeCtx); err != nil {
			r.logger.Error("stillworks traffic insert failed", "events", len(batch), "error", err)
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Take whatever is still queued before leaving. Events already
			// accepted from a served request are owed a write; only what
			// arrives after this point is allowed to be lost.
			for {
				select {
				case event := <-r.events:
					batch = append(batch, event)
					if len(batch) >= trafficBatch {
						flush()
					}
					continue
				default:
				}
				break
			}
			flush()
			return
		case event := <-r.events:
			batch = append(batch, event)
			if len(batch) >= trafficBatch {
				flush()
			}
		case <-ticker.C:
			flush()
			if dropped := r.dropped.Swap(0); dropped > 0 {
				r.logger.Warn("stillworks traffic events dropped", "count", dropped)
			}
		}
	}
}

// Track wraps a handler so every served request is counted. Wrapping each
// handler rather than installing router middleware is on purpose: /stats and
// /api/stats are excluded at the call site, so looking at the numbers can never
// inflate them.
func (r *Recorder) Track(kind string, next http.HandlerFunc) http.HandlerFunc {
	if r == nil {
		return next
	}
	return func(response http.ResponseWriter, request *http.Request) {
		tracked := &statusWriter{ResponseWriter: response, status: http.StatusOK}
		next(tracked, request)
		r.Record(request, kind, tracked.status)
	}
}

// Record queues one event. Safe to call from any goroutine and never blocks.
func (r *Recorder) Record(request *http.Request, kind string, status int) {
	userAgent := clip(request.UserAgent(), userAgentLimit)
	event := TrafficEvent{
		OccurredAt:   r.now().UTC(),
		Kind:         kind,
		Path:         request.URL.Path,
		Status:       status,
		VisitorHash:  r.visitorHash(clientIP(request), userAgent),
		ReferrerHost: referrerHost(request.Referer(), request.Host),
		UserAgent:    userAgent,
		IsCrawler:    isCrawler(userAgent),
		IsInternal:   r.internal.Contains(clientIP(request)),
	}
	select {
	case r.events <- event:
	default:
		r.dropped.Add(1)
	}
}

// Close waits for the writer to persist everything it has already accepted.
// The caller cancels the recorder's context first; without this wait the
// process can exit while the last batch is still in memory, which would lose
// exactly the requests served during a deploy.
func (r *Recorder) Close(timeout time.Duration) {
	if r == nil {
		return
	}
	select {
	case <-r.closed:
	case <-time.After(timeout):
		r.logger.Warn("stillworks traffic writer did not finish before shutdown timeout")
	}
}

// Dropped reports events discarded because the buffer was full. Published on
// the stats page: a number that silently under-reports itself is the failure
// mode this whole project exists to point at in other people's directories.
func (r *Recorder) Dropped() int64 {
	if r == nil {
		return 0
	}
	return r.dropped.Load()
}

// visitorHash identifies a visitor for one UTC day and no longer. The date is
// part of the input, so yesterday's identifier cannot be matched to today's
// even with the salt in hand.
func (r *Recorder) visitorHash(ip, userAgent string) string {
	day := r.now().UTC().Format("2006-01-02")
	sum := sha256.Sum256([]byte(r.salt + "\x00" + day + "\x00" + ip + "\x00" + userAgent))
	return hex.EncodeToString(sum[:16])
}

// clientIP reads the proxy headers because this runs behind Traefik on
// Dokploy, where RemoteAddr is always the proxy and would collapse every
// visitor into one.
func clientIP(request *http.Request) string {
	if forwarded := request.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, found := strings.Cut(forwarded, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}
	if real := request.Header.Get("X-Real-IP"); real != "" {
		return strings.TrimSpace(real)
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

// referrerHost keeps the sending host and discards the rest of the URL: the
// question is which channel worked, and a full referring URL can carry
// somebody's session token in a query string.
func referrerHost(referer, ownHost string) string {
	if referer == "" {
		return ""
	}
	parsed, err := url.Parse(referer)
	if err != nil || parsed.Host == "" {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host == strings.ToLower(hostOnly(ownHost)) {
		// Internal navigation is not a channel.
		return ""
	}
	return host
}

func hostOnly(host string) string {
	if trimmed, _, err := net.SplitHostPort(host); err == nil {
		return trimmed
	}
	return host
}

// crawlerMarkers are matched against a lowercased user agent. The list is
// deliberately conservative: an agent framework calling the API is the intended
// audience and must never be filtered out as a robot just for being automated.
var crawlerMarkers = []string{
	"bot", "crawler", "spider", "slurp", "archiver", "monitor", "uptime",
	"facebookexternalhit", "embedly", "quora link preview", "pingdom",
	"headlesschrome", "phantomjs", "scrapy", "wget",
}

func isCrawler(userAgent string) bool {
	lowered := strings.ToLower(userAgent)
	if lowered == "" {
		// A missing user agent is far more often a scripted client than a
		// browser, and this project's own audience is scripted clients.
		return false
	}
	for _, marker := range crawlerMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// clip bounds a header before it is stored. Unlike the view layer's truncate it
// adds no ellipsis: this value is data, not display text.
func clip(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// statusWriter remembers the status code for the event. It forwards Flush so a
// streamed response is not buffered by the fact that it is being counted.
type statusWriter struct {
	http.ResponseWriter
	status  int
	written bool
}

func (w *statusWriter) WriteHeader(status int) {
	if !w.written {
		w.status = status
		w.written = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(payload []byte) (int, error) {
	w.written = true
	return w.ResponseWriter.Write(payload)
}

func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
