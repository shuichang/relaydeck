package server

import (
	"context"
	"net/http"
	"time"

	"github.com/shuichang/relaydeck/internal/config"
	"github.com/shuichang/relaydeck/internal/forward"
)

func (s *Server) probeRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rule config.Rule `json:"rule"`
	}
	if !decode(w, r, &body) {
		return
	}
	if err := forward.ValidateRules([]config.Rule{body.Rule}); err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	select {
	case s.probes <- struct{}{}:
		defer func() { <-s.probes }()
	default:
		w.Header().Set("Retry-After", "3")
		fail(w, http.StatusTooManyRequests, "连通性检测繁忙，请稍后重试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()
	result, err := s.forward.Probe(ctx, body.Rule)
	if err != nil {
		fail(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}
