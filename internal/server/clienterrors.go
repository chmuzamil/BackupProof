package server

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/chmuzamil/backupproof/internal/errreport"
)

// clientErrorBudget caps how many dashboard errors are forwarded per minute.
// The endpoint has no sign-in (errors on the sign-in page count too), so the
// cap keeps anyone from using it to flood the error tracker.
const clientErrorBudget = 10

var clientErrors struct {
	sync.Mutex
	window time.Time
	n      int
}

// handleClientError forwards an uncaught dashboard error to the error
// tracker when error reporting is on (BP_SENTRY_DSN). The browser never
// talks to the tracker itself, so the page's CSP stays 'self' and the same
// scrubbing applies to browser and server reports.
func (s *Server) handleClientError(w http.ResponseWriter, r *http.Request) {
	if !errreport.Enabled() {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var b errreport.BrowserError
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&b); err != nil || b.Message == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	clientErrors.Lock()
	if now := time.Now(); now.Sub(clientErrors.window) > time.Minute {
		clientErrors.window, clientErrors.n = now, 0
	}
	clientErrors.n++
	over := clientErrors.n > clientErrorBudget
	clientErrors.Unlock()
	if !over {
		errreport.Browser(b)
	}
	w.WriteHeader(http.StatusNoContent)
}
