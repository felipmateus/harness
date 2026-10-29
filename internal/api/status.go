package api

import (
	"net/http"
	"net/url"

	"github.com/felipmateus/harness/internal/api/contract"
)

// handleStatus answers GET /api/status. It is exempt from the gate: it reports the state
// instead of being refused by it.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, _ url.Values) {
	st, err := s.status.Status(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	st.State = string(s.gate.State())
	_ = writeJSON(w, http.StatusOK, contract.StatusResponse{Status: st})
}
