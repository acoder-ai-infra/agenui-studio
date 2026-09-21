package controlticket

import (
	"errors"
	"testing"
	"time"
)

func TestCodecRoundTripAndRejectsTampering(t *testing.T) {
	codec, err := New([]byte("shared-production-secret-with-at-least-32-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	want := Claims{TenantID: "t1", UserID: "u1", SessionID: "s1", RunID: "r1", RequestID: "c1", ResumeToken: "secret"}
	expires := time.Now().Add(time.Hour).Truncate(time.Nanosecond)
	ticket, err := codec.Seal(want, expires)
	if err != nil {
		t.Fatal(err)
	}
	got, gotExpiry, err := codec.Open(ticket)
	if err != nil || got != want || !gotExpiry.Equal(expires) {
		t.Fatalf("Open() = %#v %v %v", got, gotExpiry, err)
	}
	tampered := tamperTicket(ticket)
	if _, _, err := codec.Open(tampered); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("tampered ticket error = %v", err)
	}
}

func tamperTicket(ticket string) string {
	index := len(ticket) / 2
	replacement := byte('A')
	if ticket[index] == replacement {
		replacement = 'B'
	}
	return ticket[:index] + string(replacement) + ticket[index+1:]
}

func TestCodecRejectsIncompleteClaims(t *testing.T) {
	codec, err := New([]byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.Seal(Claims{RunID: "r1"}, time.Now().Add(time.Hour)); !errors.Is(err, ErrInvalidTicket) {
		t.Fatalf("Seal() error = %v", err)
	}
}
