package audit

import (
	"net/url"
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
	"reliability": {
		ascending:    "successes::numeric / NULLIF(attempts, 0) ASC NULLS LAST, attempts ASC",
		descending:   "successes::numeric / NULLIF(attempts, 0) DESC NULLS LAST, attempts DESC",
		defaultOrder: orderDescending,
	},
	"verified": {
		ascending:    "last_ok ASC NULLS LAST",
		descending:   "last_ok DESC NULLS LAST",
		defaultOrder: orderDescending,
	},
}

// endpointSorts covers the "Every endpoint we track" table.
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

// The defaults are the order the shelf shipped with: freshest verification
// first, and the endpoints with the most keyless models at the top.
const (
	defaultWorkingSort  = "verified"
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
}

// ParseShelfQuery reads the query string and discards anything it does not
// recognise. An unknown sort key falls back to the default rather than
// erroring: a stale or mistyped link should still show the shelf.
func ParseShelfQuery(values url.Values) ShelfQuery {
	query := ShelfQuery{
		Endpoint: clipFilter(values.Get(endpointParameter)),
		Search:   clipFilter(values.Get(searchParameter)),
		Keyless:  parseKeyless(values.Get(keylessParameter)),
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
	return query.Endpoint != "" || query.Search != "" || query.Keyless != ""
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
		return "/llm/"
	}
	return "/llm/?" + encoded
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
