package modelgateway

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
)

const promptCacheAffinityDomain = "harness.prompt-cache-affinity.v1"

// DerivePromptCacheAffinityKey returns an opaque, stable routing key for a
// tenant-scoped Session. It deliberately excludes Run/Request/Trace identity so
// all rounds and retries in the same Session reach the same provider cache
// shard. Missing tenant or Session identity disables affinity instead of
// falling back to a process-global value.
//
// The input must be the Harness-owned TraceContext. HTTP headers and other
// client-controlled metadata are not inputs to this derivation.
func DerivePromptCacheAffinityKey(trace observability.TraceContext) string {
	if trace.TenantID == "" || trace.SessionID == "" {
		return ""
	}
	hash := sha256.New()
	writeAffinityPart(hash, promptCacheAffinityDomain)
	writeAffinityPart(hash, trace.TenantID)
	writeAffinityPart(hash, trace.SessionID)
	return "pc1_" + base64.RawURLEncoding.EncodeToString(hash.Sum(nil))
}

type affinityHashWriter interface {
	Write([]byte) (int, error)
}

func writeAffinityPart(writer affinityHashWriter, value string) {
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	_, _ = writer.Write(size[:])
	_, _ = writer.Write([]byte(value))
}
