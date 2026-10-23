package keys

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Handler serves the key service apps call:
//
//	POST /v1/keys/rotate   Authorization: Bearer <current key>
//	                       -> 200 {"name": "...", "key": "<new key>"}
//	GET  /healthz          -> 200
//
// The current key is the credential: an app can rotate only the key it
// holds, and learns no other. Failed attempts are slowed down so the
// endpoint is useless for guessing keys.
func (s *Service) Handler(log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /v1/keys/rotate", func(w http.ResponseWriter, r *http.Request) {
		presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer key"})
			return
		}
		name, err := s.Owner(strings.TrimSpace(presented))
		if errors.Is(err, ErrUnknownKey) {
			log.Warn("rotation refused: unknown key", "remote", r.RemoteAddr)
			time.Sleep(time.Second)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unknown key"})
			return
		}
		if err != nil {
			log.Error("rotation: reading keys", "err", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot read the key configuration"})
			return
		}
		creds, err := s.Rotate(r.Context(), name)
		switch {
		case errors.Is(err, ErrTooSoon):
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
			return
		case err != nil:
			log.Error("rotation failed", "key", name, "err", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		log.Info("key rotated", "key", name, "remote", r.RemoteAddr)
		writeJSON(w, http.StatusOK, creds)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
