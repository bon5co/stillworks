package audit

import (
	"net/url"
	"slices"
	"strings"
)

// The shelf's whole state lives in the URL. A sorted, filtered view survives
// being pasted into a chat window, works with JavaScript switched off, and needs
// nothing from the strict default-src 'self' policy this site runs under. The
// cost is a round trip per click, which on tables this size is not a cost.
const (
	sortParameter          = "sort"
	orderParameter         = "order"
	endpointSortParameter  = "endpoint_sort"
	endpointOrderParameter = "endpoint_order"
	endpointParameter      = "endpoint"
	searchParameter        = "q"
	keylessParameter       = "keyless"
	// featureParameter is spelled the same as the API's, and means the same
	// thing, because the page tells visitors to use it. The HTML accepted it
	// silently and ignored it for the whole of the shelf's first day: every
	// row came back for ?feature=tools, so the page read as though all
	// twenty-three models did tool calling.
	featureParameter = "feature"
)

const (
	orderAscending  = "asc"
	orderDescending = "desc"
)

// Filter values for the keyless question. Empty means the question was not
// asked, which is not the same as either answer.
const (
	keylessOnly    = "yes"
	keylessExclude = "no"
)

// filterTextLimit bounds a filter value before it reaches the database or the
// page. Nothing legitimate is this long, and an unbounded value would turn one
// request into a large ILIKE scan and a large echo back into the HTML.
const filterTextLimit = 64

// sortColumn holds the two ORDER BY fragments a sort key can produce. They are
// written out in full rather than assembled from the request, because the only
// safe relationship between a URL and a SQL fragment is a lookup in a fixed
// table: nothing a visitor types is ever concatenated into a query.
type sortColumn struct {
	ascending  string
	descending string
	// defaultOrder is what a bare ?sort=key means. Latency wants fastest first
	// and freshness wants newest first, so a single global default would be
	// wrong half the time.
	defaultOrder string
	// selectExpression is added to the inner select as sort_value when this key
	// is active, for a sort whose value is not already a column of that select.
	// The capability columns need it: a feature verdict lives in another table,
	// and a correlated subquery cannot be reached from the outer ORDER BY.
	//
	// It is a fixed string in this table like every other fragment here, so the
	// same rule holds: nothing a visitor types becomes SQL.
	selectExpression string
}

// workingSorts covers the "Working right now" table. Every fragment names an
// output column of the inner query, which is why that query is wrapped in a
// subselect: latency has to be re-expressed as NULLIF to sort, and an alias
// cannot be used inside an expression in ORDER BY otherwise.
//
// Unmeasured values sort last in both directions on purpose. "No latency
// recorded" is not the fastest endpoint, and putting it first under ?order=asc
// would make the table lie at a glance.
var workingSorts = map[string]sortColumn{
	"model": {
		ascending:    "model_id ASC",
		descending:   "model_id DESC",
		defaultOrder: orderAscending,
	},
	"endpoint": {
		ascending:    "slug ASC, model_id ASC",
		descending:   "slug DESC, model_id ASC",
		defaultOrder: orderAscending,
	},
	"latency": {
		ascending:    "NULLIF(latency_ms, 0) ASC NULLS LAST",
		descending:   "NULLIF(latency_ms, 0) DESC NULLS LAST",
		defaultOrder: orderAscending,
	},
	// Reliability is a ratio, tie-broken by how many attempts produced it. Ten
	// of ten and one of one are both 100%, and they are not the same claim.
	// Freshness is the final tie-break rather than the first sort key. Ten of
	// ten and one of one are both a perfect record and are not the same claim,
	// so attempts decides between them; only after that does the clock.
	"reliability": {
		ascending: "successes::numeric / NULLIF(attempts, 0) ASC NULLS LAST, attempts ASC, last_ok DESC NULLS LAST",
		descending: "successes::numeric / NULLIF(attempts, 0) DESC NULLS LAST, attempts DESC, " +
			"last_ok DESC NULLS LAST",
		defaultOrder: orderDescending,
	},
	"verified": {
		ascending:    "last_ok ASC NULLS LAST",
		descending:   "last_ok DESC NULLS LAST",
		defaultOrder: orderDescending,
	},
	// One entry per capability column, so a heading that shows a verdict can
	// also order by it. supported is a tri-state, and DESC NULLS LAST is
	// exactly the reading order the column wants: proved, then refused, then
	// never established.
	CapabilityTools:      capabilitySort(CapabilityTools),
	CapabilityJSONSchema: capabilitySort(CapabilityJSONSchema),
	CapabilityJSONObject: capabilitySort(CapabilityJSONObject),
	CapabilityVision:     capabilitySort(CapabilityVision),
}

// capabilitySort builds the sort for one feature column. The capability name is
// interpolated here and nowhere else, from the fixed Capabilities list, so the
// only strings that reach the SQL text are ones this package wrote.
func capabilitySort(capability string) sortColumn {
	return sortColumn{
		ascending:    "sort_value ASC NULLS LAST",
		descending:   "sort_value DESC NULLS LAST",
		defaultOrder: orderDescending,
		selectExpression: `(SELECT c.supported FROM llm_model_capabilities c
		                     WHERE c.model_id = m.id AND c.capability = '` + capability + `')`,
	}
}

// endpointSorts covers the "Every endpoint we track" table.
//
// The "keyless" key is a sort key, not a claim. Both shelves sort by the count
// of models that answered them, and on the keyed shelf that column is headed
// "Answered our key". The parameter keeps its original spelling so links to the
// keyless shelf that people have already shared keep working; renaming it would
// break those to make a URL read slightly better on a page that states the
// difference in three other places.
var endpointSorts = map[string]sortColumn{
	"endpoint": {
		ascending:    "slug ASC",
		descending:   "slug DESC",
		defaultOrder: orderAscending,
	},
	"latency": {
		ascending:    "NULLIF(latency_ms, 0) ASC NULLS LAST",
		descending:   "NULLIF(latency_ms, 0) DESC NULLS LAST",
		defaultOrder: orderAscending,
	},
	"checked": {
		ascending:    "checked_at ASC NULLS LAST",
		descending:   "checked_at DESC NULLS LAST",
		defaultOrder: orderDescending,
	},
	"listed": {
		ascending:    "models_listed ASC",
		descending:   "models_listed DESC",
		defaultOrder: orderDescending,
	},
	"keyless": {
		ascending:    "working_models ASC",
		descending:   "working_models DESC",
		defaultOrder: orderDescending,
	},
}

// The working table defaults to reliability, not freshness.
//
// It shipped sorted by last verification, which churned: the top row changed
// twice inside ninety seconds during a review, once to a model that had
// answered three of its last five checks. That row was also the one the page's
// only copyable snippet was built from, so the single thing a visitor was
// invited to paste was whichever model happened to have been probed most
// recently. Reliability moves much more slowly and is the property somebody
// choosing an endpoint actually wants, so it decides both the order and the
// snippet.
//
// The endpoint table keeps its original default.
const (
	defaultWorkingSort  = "reliability"
	defaultEndpointSort = "keyless"
)

// ShelfQuery is a shelf URL that has already been checked against the
// allow-lists. Nothing outside this file constructs one from raw input, so a
// value of this type can only ever name a sort key that exists.
type ShelfQuery struct {
	WorkingSort   string
	WorkingOrder  string
	EndpointSort  string
	EndpointOrder string
	// Endpoint filters both tables to one slug. Search matches model ids; on the
	// endpoint table it keeps the endpoints that offer a matching model, which
	// is the only reading of "models called gpt" that makes sense for a row
	// describing a whole endpoint.
	Endpoint string
	Search   string
	Keyless  string
	// Features narrows the working table to models where every named capability
	// was proved by a real call. It is ANDed, like the API's, because an agent
	// that needs tools and a schema needs one model with both, not two models.
	//
	// Only names in Capabilities survive parsing, so this can never carry a
	// value the query layer has no column for.
	Features []string
	// UnknownFeatures are the names a visitor asked for that we publish no
	// verdict for, kept rather than discarded so the page can say so. Dropping
	// them silently is what the shelf did with the whole parameter, and a
	// filter that quietly does nothing is worse than one that refuses.
	UnknownFeatures []string
	// Base is the path this state belongs to, so the same sort and filter
	// machinery serves both shelves without either one linking into the other.
	// It is set by the route from a constant and never read from the query
	// string: a URL that could redirect its own controls to the other shelf is
	// the blurring these two pages exist to prevent.
	Base string
}

// On returns the same state anchored to a shelf path.
func (query ShelfQuery) On(base string) ShelfQuery {
	query.Base = base
	return query
}

// basePath falls back to the keyless shelf, which is where this machinery lived
// before there were two of them.
func (query ShelfQuery) basePath() string {
	if query.Base == "" {
		return keylessShelfPath
	}
	return query.Base
}

// ParseShelfQuery reads the query string and discards anything it does not
// recognise. An unknown sort key falls back to the default rather than
// erroring: a stale or mistyped link should still show the shelf.
func ParseShelfQuery(values url.Values) ShelfQuery {
	known, unknown := parseFeatures(values[featureParameter])
	query := ShelfQuery{
		Endpoint:        clipFilter(values.Get(endpointParameter)),
		Search:          clipFilter(values.Get(searchParameter)),
		Keyless:         parseKeyless(values.Get(keylessParameter)),
		Features:        known,
		UnknownFeatures: unknown,
	}
	query.WorkingSort, query.WorkingOrder = parseSort(
		workingSorts,
		defaultWorkingSort,
		values.Get(sortParameter),
		values.Get(orderParameter),
	)
	query.EndpointSort, query.EndpointOrder = parseSort(
		endpointSorts,
		defaultEndpointSort,
		values.Get(endpointSortParameter),
		values.Get(endpointOrderParameter),
	)
	return query
}

func parseSort(columns map[string]sortColumn, fallback, key, order string) (string, string) {
	key = strings.ToLower(strings.TrimSpace(key))
	column, known := columns[key]
	if !known {
		key = fallback
		column = columns[fallback]
	}
	switch strings.ToLower(strings.TrimSpace(order)) {
	case orderAscending:
		return key, orderAscending
	case orderDescending:
		return key, orderDescending
	default:
		return key, column.defaultOrder
	}
}

// parseFeatures reads ?feature=tools&feature=vision and ?feature=tools,vision,
// both spellings, the same way the API does -- a visitor copying the curl line
// off the page into the address bar has to land on the same answer.
//
// image_out is refused here rather than carried: it belongs to models that draw,
// the working table holds models that chat, and a filter that can only ever
// empty the table is a broken control rather than a narrow one. The drawing
// table below has no filter of its own to confuse it with.
func parseFeatures(values []string) (known []string, unknown []string) {
	for _, value := range values {
		for _, name := range strings.Split(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			switch {
			case name == "":
				continue
			case name == CapabilityImageOut, !KnownCapability(name):
				if len(unknown) < len(Capabilities) && !slices.Contains(unknown, name) {
					unknown = append(unknown, clipFilter(name))
				}
			case !slices.Contains(known, name):
				known = append(known, name)
			}
		}
	}
	return known, unknown
}

func parseKeyless(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case keylessOnly:
		return keylessOnly
	case keylessExclude:
		return keylessExclude
	default:
		return ""
	}
}

func clipFilter(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > filterTextLimit {
		return value[:filterTextLimit]
	}
	return value
}

// workingOrderBy and endpointOrderBy return the SQL fragment for the parsed
// state. They take no argument from the caller for the same reason the tables
// are fixed: the fragment must come from the map or from nowhere.
func (query ShelfQuery) workingOrderBy() string {
	return orderByFrom(workingSorts, query.WorkingSort, query.WorkingOrder, defaultWorkingSort)
}

func (query ShelfQuery) endpointOrderBy() string {
	return orderByFrom(endpointSorts, query.EndpointSort, query.EndpointOrder, defaultEndpointSort)
}

func orderByFrom(columns map[string]sortColumn, key, order, fallback string) string {
	column, known := columns[key]
	if !known {
		column = columns[fallback]
	}
	if order == orderAscending {
		return column.ascending
	}
	return column.descending
}

// Filtered reports whether the visitor narrowed the shelf, so the page can say
// what it is showing instead of quietly showing less than it claims.
func (query ShelfQuery) Filtered() bool {
	return query.Endpoint != "" || query.Search != "" || query.Keyless != "" ||
		len(query.Features) > 0 || len(query.UnknownFeatures) > 0
}

// HasFeature reports whether one capability is in the active filter, so a
// checkbox can render itself already ticked after a round trip.
func (query ShelfQuery) HasFeature(name string) bool {
	return slices.Contains(query.Features, name)
}

// workingSortValue is the extra inner-select column this sort needs, or the
// empty string when the sort reads a column that is already there.
func (query ShelfQuery) workingSortValue() string {
	column, known := workingSorts[query.WorkingSort]
	if !known {
		return ""
	}
	return column.selectExpression
}

// values rebuilds the query string, omitting anything left at its default. A
// link that carries only what was actually chosen stays readable, and a shared
// URL keeps meaning the same thing when a default changes.
func (query ShelfQuery) values() url.Values {
	values := url.Values{}
	if query.Endpoint != "" {
		values.Set(endpointParameter, query.Endpoint)
	}
	if query.Search != "" {
		values.Set(searchParameter, query.Search)
	}
	if query.Keyless != "" {
		values.Set(keylessParameter, query.Keyless)
	}
	// One parameter per feature rather than one comma-separated value, so a
	// checkbox group round-trips through the form unchanged and the address bar
	// reads the way the API's own examples do.
	for _, feature := range query.Features {
		values.Add(featureParameter, feature)
	}
	for _, feature := range query.UnknownFeatures {
		values.Add(featureParameter, feature)
	}
	if query.WorkingSort != defaultWorkingSort || query.WorkingOrder != workingSorts[defaultWorkingSort].defaultOrder {
		values.Set(sortParameter, query.WorkingSort)
		values.Set(orderParameter, query.WorkingOrder)
	}
	if query.EndpointSort != defaultEndpointSort || query.EndpointOrder != endpointSorts[defaultEndpointSort].defaultOrder {
		values.Set(endpointSortParameter, query.EndpointSort)
		values.Set(endpointOrderParameter, query.EndpointOrder)
	}
	return values
}

// URL renders the shelf address for this state.
func (query ShelfQuery) URL() string {
	encoded := query.values().Encode()
	if encoded == "" {
		return query.basePath()
	}
	return query.basePath() + "?" + encoded
}

// WorkingSortLink is the address a column header points at: sort by this key,
// flipping the direction if the table is already sorted by it.
func (query ShelfQuery) WorkingSortLink(key string) string {
	next := query
	next.WorkingSort, next.WorkingOrder = nextSort(workingSorts, key, query.WorkingSort, query.WorkingOrder)
	return next.URL()
}

func (query ShelfQuery) EndpointSortLink(key string) string {
	next := query
	next.EndpointSort, next.EndpointOrder = nextSort(endpointSorts, key, query.EndpointSort, query.EndpointOrder)
	return next.URL()
}

func nextSort(columns map[string]sortColumn, key, currentKey, currentOrder string) (string, string) {
	column, known := columns[key]
	if !known {
		return currentKey, currentOrder
	}
	if key != currentKey {
		return key, column.defaultOrder
	}
	if currentOrder == orderAscending {
		return key, orderDescending
	}
	return key, orderAscending
}

// WorkingSortMark and EndpointSortMark are the arrow next to the active column.
// The direction is stated rather than implied, because a table that is sorted
// and does not say so is a table that can be read backwards.
func (query ShelfQuery) WorkingSortMark(key string) string {
	return sortMark(key, query.WorkingSort, query.WorkingOrder)
}

func (query ShelfQuery) EndpointSortMark(key string) string {
	return sortMark(key, query.EndpointSort, query.EndpointOrder)
}

func sortMark(key, currentKey, currentOrder string) string {
	if key != currentKey {
		return ""
	}
	if currentOrder == orderAscending {
		return " ↑"
	}
	return " ↓"
}

// escapeLikePattern keeps a visitor's text a literal. Without it a search for
// "%" matches every model and reads like a bug, and "_" silently matches one of
// anything.
func escapeLikePattern(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(value) + "%"
}
