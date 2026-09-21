package redisstore

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/AGenUI/agenui-studio/harness/internal/observability"
	"github.com/AGenUI/agenui-studio/harness/internal/protocol"
)

// Broker is a distributed protocol.EventBroker backed by Redis Pub/Sub. Unlike
// the in-process MemoryBroker, it fans post-persist events out across every
// gateway process, so a client connected to instance B receives live events for
// a run driven on instance A. Delivery stays best-effort: the EventStore is the
// source of truth and clients recover missed frames via after_sequence replay.
type Broker struct {
	client Client
	prefix string
}

// NewBroker builds a Redis Pub/Sub EventBroker.
func NewBroker(client Client, cfg Config) *Broker {
	return &Broker{client: client, prefix: prefix(cfg) + "events:"}
}

func (b *Broker) channel(runID string) string         { return b.prefix + runID }
func (b *Broker) sessionChannel(sessID string) string { return b.prefix + "session:" + sessID }

func (b *Broker) Publish(ctx context.Context, ev observability.AgentEvent) error {
	raw, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if err := b.client.Publish(ctx, b.channel(ev.RunID), raw).Err(); err != nil {
		return err
	}
	// Also fan out on the session channel so run-tree fan-in subscribers
	// (SubscribeSession) receive descendant child-run events across processes.
	if ev.SessionID != "" {
		if err := b.client.Publish(ctx, b.sessionChannel(ev.SessionID), raw).Err(); err != nil {
			return err
		}
	}
	return nil
}

// SubscribeSession streams every event in sessionID (across its runs) via a
// dedicated Redis Pub/Sub session channel that Publish mirrors to.
func (b *Broker) SubscribeSession(ctx context.Context, sessionID string) (<-chan observability.AgentEvent, func(), error) {
	return b.subscribeChannel(ctx, b.sessionChannel(sessionID))
}

func (b *Broker) Subscribe(ctx context.Context, runID string) (<-chan observability.AgentEvent, func(), error) {
	return b.subscribeChannel(ctx, b.channel(runID))
}

func (b *Broker) subscribeChannel(ctx context.Context, channel string) (<-chan observability.AgentEvent, func(), error) {
	pubsub := b.client.Subscribe(ctx, channel)
	// Wait for the subscription to be established so events published right after
	// Subscribe returns are not missed by this consumer.
	if _, err := pubsub.Receive(ctx); err != nil {
		_ = pubsub.Close()
		return nil, nil, err
	}

	out := make(chan observability.AgentEvent, 256)
	done := make(chan struct{})
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			close(done)
			_ = pubsub.Close()
		})
	}

	go func() {
		defer close(out)
		ch := pubsub.Channel()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case msg, ok := <-ch:
				if !ok {
					return
				}
				var ev observability.AgentEvent
				if err := json.Unmarshal([]byte(msg.Payload), &ev); err != nil {
					continue
				}
				select {
				case out <- ev:
				case <-done:
					return
				case <-ctx.Done():
					return
				}
			}
		}
	}()

	return out, cancel, nil
}

var (
	_ protocol.EventBroker       = (*Broker)(nil)
	_ protocol.SessionSubscriber = (*Broker)(nil)
)
