package harness

import (
	"fmt"
	"net/http"
)

// HTTPHandler returns the HTTP handler already assembled by the source Harness
// Composition Root. It does not create another runtime, translate events, or
// wrap the native harness.sse.v1 protocol; embedded hosts can mount it beside
// their own product routes.
func HTTPHandler(engine Engine) (http.Handler, error) {
	impl, ok := engine.(*engineImpl)
	if !ok || impl == nil || impl.kernel == nil || impl.kernel.Handler == nil {
		return nil, fmt.Errorf("%w: source HTTP handler is unavailable", ErrNotReady)
	}
	return impl.kernel.Handler, nil
}
