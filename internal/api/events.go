package api

import (
	"net/http"
	"net/url"

	"github.com/felipmateus/harness/internal/api/contract"
	"github.com/felipmateus/harness/internal/ledger"
)

// handleEvents answers GET /api/streams/{stream}/events: one page of history.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, q url.Values) {
	streamID := r.PathValue("stream")
	if err := contract.ValidateStream(streamID); err != nil {
		writeContractError(w, err)
		return
	}
	after, err := contract.ParseAfter(q.Get("after"))
	if err != nil {
		writeContractError(w, err)
		return
	}
	limit, err := contract.ParseLimit(q.Get("limit"))
	if err != nil {
		writeContractError(w, err)
		return
	}

	// The page first, then the head: an event that commits in between can only make the
	// head larger than the page's last sequence, never smaller.
	page, err := s.events.Read(r.Context(), streamID, after, limit)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	head, err := s.events.Head(r.Context(), streamID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	next := after
	if n := len(page.Events); n > 0 {
		next = page.Events[n-1].Seq
	}
	_ = writeJSON(w, http.StatusOK, contract.EventsPage{
		Events: toContractEvents(page.Events),
		Page: contract.Page{
			After:     after,
			NextAfter: next,
			HasMore:   page.HasMore,
			Limit:     limit,
			Head:      head,
		},
	})
}

// toContractEvent converts a ledger event to its wire form. contract.Event mirrors
// ledger.Event field for field (the contract package is a leaf, so it cannot reuse the
// type); the JSON of both is identical.
func toContractEvent(ev ledger.Event) contract.Event {
	return contract.Event{
		Stream: ev.Stream,
		Seq:    ev.Seq,
		ID:     ev.ID,
		Type:   ev.Type,
		Time:   ev.Time.String(),
		Data:   ev.Data,
	}
}

// toContractEvents converts a page; the result is never nil, so an empty page marshals as [].
func toContractEvents(evs []ledger.Event) []contract.Event {
	out := make([]contract.Event, len(evs))
	for i, ev := range evs {
		out[i] = toContractEvent(ev)
	}
	return out
}
