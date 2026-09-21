package exportpackage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type publicationTargetStoreStub struct {
	target PublicationTarget
}

func (s publicationTargetStoreStub) LoadPublicationTarget(context.Context) (PublicationTarget, bool, error) {
	return s.target, true, nil
}

func TestPublisherDeliversSelfContainedPackageToCallback(t *testing.T) {
	var got Publication
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" {
			t.Fatal("missing idempotency key")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	publisher, err := NewPublisher(
		publicationTargetStoreStub{target: PublicationTarget{Kind: "callback", Endpoint: server.URL, Enabled: true}},
		NewCallbackDelivery(server.Client()),
	)
	if err != nil {
		t.Fatal(err)
	}
	event, err := publisher.Publish(t.Context(), "session-1", []byte(`{"version":"1.0","cardId":"run-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryID != event.DeliveryID || got.SessionID != "session-1" || !json.Valid(got.Package) {
		t.Fatalf("callback event = %#v", got)
	}
}

func TestPublisherLeavesMQToRegisteredAdapter(t *testing.T) {
	publisher, err := NewPublisher(publicationTargetStoreStub{target: PublicationTarget{
		Kind: "mq", Endpoint: "mq://cards", Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := publisher.Publish(t.Context(), "session-1", []byte(`{"version":"1.0"}`)); err == nil {
		t.Fatal("publish unexpectedly succeeded without an MQ adapter")
	}
}
