package context

import (
	gocontext "context"
)

// ConversationSource produces dynamic fragments from conversation history.
type ConversationSource struct {
	MaxTurns int // 0 = no limit
}

func NewConversationSource(maxTurns int) *ConversationSource {
	return &ConversationSource{MaxTurns: maxTurns}
}

func (s *ConversationSource) Kind() SourceKind { return SourceConversation }

func (s *ConversationSource) Collect(_ gocontext.Context, req CollectRequest) ([]ContextFragment, error) {
	msgs := req.Messages
	if s.MaxTurns > 0 {
		msgs = trimToRecentTurns(msgs, s.MaxTurns)
	}

	counter := EstimateCounter{}
	var frags []ContextFragment
	for i, msg := range msgs {
		ordinal := int64(i + 1)
		if msg.Sequence > 0 {
			ordinal = msg.Sequence
		}
		slot := SlotConversation
		if msg.Role == RoleTool {
			slot = SlotToolResult
		}
		frags = append(frags, ContextFragment{
			Slot:       slot,
			Stability:  StabilityDynamic,
			Priority:   50,
			Pinned:     false,
			Generation: req.Generation,
			Source:     string(SourceConversation),
			Role:       msg.Role,
			Messages:   []*Message{msg},
			TokenCost:  counter.CountMessage(msg),
			Ordinal:    ordinal,
			PairingIDs: messagePairingIDs(msg),
		})
	}
	return frags, nil
}

// trimToRecentTurns keeps the last N user-message turns.
// A "turn" starts with a user message and includes subsequent messages until the next user message.
func trimToRecentTurns(msgs []*Message, turns int) []*Message {
	if turns <= 0 || len(msgs) == 0 {
		return msgs
	}
	userCount := 0
	cutIdx := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleUser {
			userCount++
			if userCount >= turns {
				cutIdx = i
				break
			}
		}
	}
	if userCount < turns {
		return msgs
	}
	return msgs[cutIdx:]
}
