package proxy

import (
	"encoding/json"
	"net/http"
	"time"
)

// serveModelList answers GET /v1/models locally with the models this proxy
// offers, so clients can discover them without an upstream round trip.
func (p *Proxy) serveModelList(w http.ResponseWriter, r *http.Request) *requestInfo {
	if !p.cfg.KnownKey(clientKey(r)) {
		return fail(w, nil, http.StatusUnauthorized, "invalid_api_key", "the API key is not recognised by this proxy")
	}
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	created := time.Now().Unix()
	data := make([]model, 0, len(p.cfg.Models))
	for _, name := range p.cfg.ModelNames() {
		data = append(data, model{ID: name, Object: "model", Created: created, OwnedBy: appName})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
	return &requestInfo{}
}
