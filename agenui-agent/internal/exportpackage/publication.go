package exportpackage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const PublicationSchemaVersion = "agenui.package-publication.v1"

type PublicationTarget struct {
	Kind     string `json:"kind"`
	Endpoint string `json:"endpoint"`
	Enabled  bool   `json:"enabled"`
}

type PublicationTargetStore interface {
	LoadPublicationTarget(context.Context) (PublicationTarget, bool, error)
}

type Publication struct {
	SchemaVersion string          `json:"schema_version"`
	DeliveryID    string          `json:"delivery_id"`
	SessionID     string          `json:"session_id"`
	Package       json.RawMessage `json:"package"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Delivery is the package publication boundary. CallbackDelivery is bundled;
// applications may register an MQ adapter without changing package creation or
// the Studio HTTP contract.
type Delivery interface {
	Kind() string
	Publish(context.Context, PublicationTarget, Publication) error
}

type Publisher struct {
	targets    PublicationTargetStore
	deliveries map[string]Delivery
	now        func() time.Time
}

func NewPublisher(targets PublicationTargetStore, deliveries ...Delivery) (*Publisher, error) {
	if targets == nil {
		return nil, errors.New("package publication: target store is required")
	}
	registered := make(map[string]Delivery, len(deliveries))
	for _, delivery := range deliveries {
		if delivery == nil || strings.TrimSpace(delivery.Kind()) == "" {
			return nil, errors.New("package publication: delivery kind is required")
		}
		kind := strings.TrimSpace(delivery.Kind())
		if _, duplicate := registered[kind]; duplicate {
			return nil, fmt.Errorf("package publication: duplicate delivery kind %q", kind)
		}
		registered[kind] = delivery
	}
	return &Publisher{targets: targets, deliveries: registered, now: time.Now}, nil
}

func (p *Publisher) Publish(ctx context.Context, sessionID string, packageJSON []byte) (Publication, error) {
	if p == nil || p.targets == nil || strings.TrimSpace(sessionID) == "" || !json.Valid(packageJSON) {
		return Publication{}, errors.New("package publication: valid session and package are required")
	}
	target, configured, err := p.targets.LoadPublicationTarget(ctx)
	if err != nil {
		return Publication{}, fmt.Errorf("package publication: load target: %w", err)
	}
	if !configured || !target.Enabled {
		return Publication{}, errors.New("package publication: no enabled target is configured")
	}
	delivery := p.deliveries[strings.TrimSpace(target.Kind)]
	if delivery == nil {
		return Publication{}, fmt.Errorf("package publication: delivery kind %q is not installed", target.Kind)
	}
	sum := sha256.Sum256(append([]byte(sessionID+"\x00"), packageJSON...))
	event := Publication{
		SchemaVersion: PublicationSchemaVersion,
		DeliveryID:    "delivery_" + hex.EncodeToString(sum[:12]),
		SessionID:     sessionID,
		Package:       append(json.RawMessage(nil), packageJSON...),
		CreatedAt:     p.now().UTC(),
	}
	if err := delivery.Publish(ctx, target, event); err != nil {
		return Publication{}, err
	}
	return event, nil
}

type CallbackDelivery struct {
	client *http.Client
}

func NewCallbackDelivery(client *http.Client) *CallbackDelivery {
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	return &CallbackDelivery{client: client}
}

func (*CallbackDelivery) Kind() string { return "callback" }

func (d *CallbackDelivery) Publish(ctx context.Context, target PublicationTarget, event Publication) error {
	endpoint, err := url.Parse(strings.TrimSpace(target.Endpoint))
	if err != nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" {
		return errors.New("package publication: callback endpoint must be an absolute HTTP URL")
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("package publication: encode callback: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("package publication: create callback request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", event.DeliveryID)
	response, err := d.client.Do(request)
	if err != nil {
		return fmt.Errorf("package publication: callback request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("package publication: callback status %s", response.Status)
	}
	return nil
}
