package httputil

import "net/http"

// Chain composes middleware so that mws[0] is the outermost wrapper (it sees
// the request first and the response last). Nil entries are skipped. With an
// empty slice it returns next unchanged.
//
// The wiring layer passes these in a deliberate order: Recover outermost (so a
// panic anywhere below still becomes a 500 envelope), then Observe (so every
// request is timed and counted even if a later middleware short-circuits),
// then SecurityHeaders and CORS, then the body cap closest to the handlers.
func Chain(next http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] != nil {
			next = mws[i](next)
		}
	}
	return next
}
