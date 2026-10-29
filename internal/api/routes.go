package api

import (
	"net/http"
	"net/url"
	"path"
	"slices"

	"github.com/felipmateus/harness/internal/api/contract"
)

// handler is a route's handler. q holds the query parameters, already checked against the
// route's allowed list.
type handler func(w http.ResponseWriter, r *http.Request, q url.Values)

// route is one entry of the route table. Every path is registered without a method
// pattern and the method is checked inside the chain, so ServeMux never produces its own
// plain-text 404 or 405.
type route struct {
	pattern string
	method  string
	// exempt routes bypass the admission gate (Safety Invariant 14).
	exempt bool
	// stream routes are long-lived: the gate admits them but they are not counted as
	// in-flight, or a graceful shutdown would wait for them forever.
	stream bool
	// query lists the only query parameters the route accepts.
	query  []string
	handle handler
}

func (s *Server) routes() []route {
	return []route{
		{pattern: "/api/status", method: http.MethodGet, exempt: true, handle: s.handleStatus},
		{pattern: "/api/doctor", method: http.MethodGet, handle: s.handleDoctor},
		{pattern: "/api/daemon/shutdown", method: http.MethodPost, exempt: true, handle: s.handleShutdown},
		{pattern: "/api/streams/{stream}/events", method: http.MethodGet, query: []string{"after", "limit"}, handle: s.handleEvents},
		{pattern: "/api/streams/{stream}/events/stream", method: http.MethodGet, stream: true, query: []string{"after"}, handle: s.handleEventStream},
	}
}

// buildHandler assembles the chain for a route table. From the outside in:
//
//	recovery -> canonical path -> mux -> admission and in-flight tracking -> debug log ->
//	method and query checks -> handler
//
// An unknown path answers 404 in every gate state: a path with no route does not depend on
// what the daemon is doing. A known path is admitted first, then its method and query are
// checked.
func (s *Server) buildHandler(routes []route) http.Handler {
	mux := http.NewServeMux()
	for _, rt := range routes {
		mux.Handle(rt.pattern, s.admit(rt, s.logRequests(s.strict(rt))))
	}
	notFound := s.logRequests(http.HandlerFunc(s.handleNotFound))
	mux.Handle("/", notFound)
	return s.recoverPanics(s.canonical(mux, notFound))
}

// canonical keeps ServeMux's path-clean redirect from ever reaching a client: it answers a
// path that is not already clean with the JSON 404 instead of a redirect with a plain-text
// body. It checks the escaped path, the same string ServeMux cleans. It also covers the
// request targets that are not a rooted path: `*`, which ServeMux answers with an empty
// 400, and the empty path of a CONNECT target, which it answers with a plain 404.
func (s *Server) canonical(mux, notFound http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		if p == "" || p[0] != '/' || path.Clean(p) != p {
			notFound.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeContractError(w, contract.NotFoundError(r.Method, r.URL.Path))
}

// strict enforces the route's method and its allowed query parameters (Business Rule 23).
func (s *Server) strict(rt route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != rt.method {
			w.Header().Set("Allow", rt.method)
			writeContractError(w, contract.MethodNotAllowedError(r.Method, r.URL.Path))
			return
		}
		q, err := checkQuery(r.URL.RawQuery, rt.query)
		if err != nil {
			writeContractError(w, err)
			return
		}
		rt.handle(w, r, q)
	})
}

// checkQuery parses raw and rejects anything the route does not accept: a malformed query
// string, an unknown parameter, or a known parameter given twice. Names are checked in
// sorted order so the error is deterministic.
func checkQuery(raw string, allowed []string) (url.Values, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, contract.InvalidQueryError(err.Error())
	}
	names := make([]string, 0, len(q))
	for name := range q {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if !slices.Contains(allowed, name) {
			return nil, contract.UnknownParameterError(name)
		}
	}
	for _, name := range names {
		if len(q[name]) > 1 {
			return nil, contract.DuplicateParameterError(name)
		}
	}
	return q, nil
}
