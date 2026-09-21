package httptransport

import (
	"net/http"
	"strings"
)

// corsAllowedHeaders lists the request headers used by Harness Chat/Resume and
// the development principal headers resolved by developmentPrincipalResolver.
const corsAllowedHeaders = "Content-Type, Idempotency-Key, " +
	"X-AGenUI-Tenant-ID, X-AGenUI-User-ID, Authorization"

const corsAllowedMethods = "GET, POST, OPTIONS"

// corsMaxAgeSeconds caches a successful preflight for ten minutes so the
// browser does not re-issue OPTIONS before every Chat/Resume call.
const corsMaxAgeSeconds = "600"

// CORS wraps next with an origin allow-list. An empty allowedOrigins leaves
// behavior unchanged (no CORS headers, same as an unwrapped handler). A single
// "*" entry reflects any origin. Otherwise only exact scheme+host origins in
// the list are allowed. The matched origin is reflected (never "*") together
// with Access-Control-Allow-Credentials so cookie/credentialed fetches from the
// admin console work. Preflight OPTIONS requests are answered here with 204 and
// never reach the route mux, whose patterns only match GET/POST.
func CORS(allowedOrigins []string, next http.Handler) http.Handler {
	allowAll := false
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			allowAll = true
			continue
		}
		allowed[origin] = struct{}{}
	}
	if !allowAll && len(allowed) == 0 {
		return next
	}
	return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		origin := strings.TrimSpace(request.Header.Get("Origin"))
		if origin != "" && originAllowed(origin, allowed, allowAll) {
			header := response.Header()
			header.Set("Access-Control-Allow-Origin", origin)
			header.Add("Vary", "Origin")
			header.Set("Access-Control-Allow-Credentials", "true")
			if isPreflight(request) {
				header.Set("Access-Control-Allow-Methods", corsAllowedMethods)
				requested := strings.TrimSpace(
					request.Header.Get("Access-Control-Request-Headers"),
				)
				if requested == "" {
					requested = corsAllowedHeaders
				}
				header.Set("Access-Control-Allow-Headers", requested)
				header.Set("Access-Control-Max-Age", corsMaxAgeSeconds)
				response.WriteHeader(http.StatusNoContent)
				return
			}
		} else if isPreflight(request) {
			// Preflight from a disallowed origin: answer without CORS headers so
			// the browser blocks it, but do not fall through to the mux (whose
			// patterns reject OPTIONS with 405).
			response.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(response, request)
	})
}

func originAllowed(origin string, allowed map[string]struct{}, allowAll bool) bool {
	if allowAll {
		return true
	}
	_, ok := allowed[origin]
	return ok
}

func isPreflight(request *http.Request) bool {
	return request.Method == http.MethodOptions &&
		request.Header.Get("Access-Control-Request-Method") != ""
}
