package api

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/felipmateus/harness/internal/api/contract"
)

// handleDoctor answers GET /api/doctor with the daemon-mode report.
func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request, _ url.Values) {
	if s.doctor == nil {
		s.internalError(w, r, errors.New("doctor runner is not configured"))
		return
	}
	d, err := s.doctor.Run(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	if d.Items == nil {
		d.Items = []contract.DoctorItem{} // a report always has the array
	}
	_ = writeJSON(w, http.StatusOK, contract.DoctorResponse{Doctor: d})
}
