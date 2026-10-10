package httpapi

import (
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// isLoopbackHost reports whether h (no port) is one of the accepted loopback names.
func isLoopbackHost(h string) bool {
	switch strings.ToLower(h) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// hostnameOf strips an optional port and IPv6 brackets from a Host header value.
func hostnameOf(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(hostport, "["), "]")
}

// hostGuard rejects requests whose Host is not a loopback name (DNS rebinding defence).
func hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isLoopbackHost(hostnameOf(r.Host)) {
			writeErrorMsg(w, http.StatusForbidden, "Host not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// originGuard rejects requests carrying a non-loopback Origin (cross-site request defence).
func originGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if vals, ok := r.Header["Origin"]; ok {
			u, err := url.Parse(vals[0])
			if len(vals) != 1 || err != nil || !isLoopbackHost(u.Hostname()) {
				writeErrorMsg(w, http.StatusForbidden, "Origin not allowed")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireJSON answers 415 unless the request's media type is application/json.
func requireJSON(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/json" {
			writeErrorMsg(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		next(w, r)
	})
}

// routeErrorWriter replaces the mux's plain-text 404 and 405 bodies with JSON, keeping its headers.
type routeErrorWriter struct {
	http.ResponseWriter
	swallow bool
}

func (w *routeErrorWriter) WriteHeader(code int) {
	h := w.Header()
	if (code == http.StatusNotFound || code == http.StatusMethodNotAllowed) && strings.HasPrefix(h.Get("Content-Type"), "text/plain") {
		w.swallow = true
		h.Del("Content-Length")
		writeErrorMsg(w.ResponseWriter, code, strings.ToLower(http.StatusText(code)))
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *routeErrorWriter) Write(b []byte) (int, error) {
	if w.swallow {
		return len(b), nil
	}
	return w.ResponseWriter.Write(b)
}

func jsonRouteErrors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&routeErrorWriter{ResponseWriter: w}, r)
	})
}
