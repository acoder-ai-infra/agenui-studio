package observability

import (
	"crypto/rand"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

type IDGenerator interface {
	NewTraceID() string
	NewSpanID() string
	NewRunID() string
	NewEventID() string
	NewRequestID() string
}

type ULIDGenerator struct {
	prefix string
}

func NewULIDGenerator(prefix string) ULIDGenerator {
	return ULIDGenerator{prefix: strings.Trim(prefix, "_")}
}

func (g ULIDGenerator) NewTraceID() string {
	return g.newID("trace")
}

func (g ULIDGenerator) NewSpanID() string {
	return g.newID("span")
}

func (g ULIDGenerator) NewRunID() string {
	return g.newID("run")
}

func (g ULIDGenerator) NewEventID() string {
	return g.newID("evt")
}

func (g ULIDGenerator) NewRequestID() string {
	return g.newID("req")
}

func (g ULIDGenerator) newID(kind string) string {
	id := ulid.MustNew(ulid.Timestamp(time.Now()), rand.Reader).String()
	if g.prefix == "" {
		return kind + "_" + strings.ToLower(id)
	}
	return g.prefix + "_" + kind + "_" + strings.ToLower(id)
}
