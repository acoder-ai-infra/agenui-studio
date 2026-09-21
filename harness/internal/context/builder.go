package context

import (
	gocontext "context"
	"fmt"
	"sort"
)

// Builder compiles multi-source fragments into a final prompt.
type Builder interface {
	Build(ctx gocontext.Context, req BuildRequest) (*BuildResult, error)
}

// BuildRequest is the input for Builder.Build.
type BuildRequest struct {
	SessionID   string
	Generation  int
	Messages    []*Message
	Fragments   []ContextFragment
	TokenBudget ContextBudget
}

// BuildResult is the output of Builder.Build.
type BuildResult struct {
	Messages   []*Message        `json:"-"`
	TokenCount int               `json:"token_count"`
	Fragments  []ContextFragment `json:"-"`
	Trimmed    []TrimRecord      `json:"trimmed,omitempty"`
	CacheBreak int               `json:"cache_break"`
}

// BuilderConfig configures the DefaultBuilder.
type BuilderConfig struct {
	MinPreserve MinPreserveConfig
}

// MinPreserveConfig defines what must never be trimmed.
type MinPreserveConfig struct {
	StablePrefix bool
	CurrentInput bool
	RecentTurns  int
}

// DefaultBuilder implements Builder with linear assembly, stability-first sorting,
// and multi-level item trimming.
type DefaultBuilder struct {
	registry *SourceRegistry
	counter  TokenCounter
	config   BuilderConfig
}

func NewDefaultBuilder(registry *SourceRegistry, counter TokenCounter, config BuilderConfig) *DefaultBuilder {
	if counter == nil {
		counter = EstimateCounter{}
	}
	if config.MinPreserve.RecentTurns <= 0 {
		config.MinPreserve.RecentTurns = 1
	}
	return &DefaultBuilder{
		registry: registry,
		counter:  counter,
		config:   config,
	}
}

func (b *DefaultBuilder) Build(ctx gocontext.Context, req BuildRequest) (*BuildResult, error) {
	// Step 1: Collect
	collectReq := CollectRequest{
		SessionID:   req.SessionID,
		Generation:  req.Generation,
		TokenBudget: req.TokenBudget.Limit,
		Messages:    req.Messages,
	}
	fragments, err := b.registry.CollectAll(ctx, collectReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBuilderBuild, err)
	}
	fragments = append(fragments, cloneFragments(req.Fragments)...)
	if b.config.MinPreserve.CurrentInput {
		pinCurrentInputFragments(fragments)
	}
	if err := validateToolPairing(fragments); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBuilderBuild, err)
	}

	// Step 2: Sort (stability-first)
	sortFragments(fragments)

	// Step 3: Budget check
	totalTokens := sumTokenCost(fragments)

	// Step 4: Trim (only if over budget)
	var trimmed []TrimRecord
	if totalTokens > req.TokenBudget.Limit {
		fragments, trimmed = b.trim(fragments, req.TokenBudget.Limit)
		totalTokens = sumTokenCost(fragments)
		if totalTokens > req.TokenBudget.Limit {
			return nil, fmt.Errorf("%w: required context uses %d tokens, limit is %d", ErrBudgetExceeded, totalTokens, req.TokenBudget.Limit)
		}
	}

	// Step 5: Assemble
	messages, cacheBreak := assemble(fragments)

	return &BuildResult{
		Messages:   messages,
		TokenCount: totalTokens,
		Fragments:  fragments,
		Trimmed:    trimmed,
		CacheBreak: cacheBreak,
	}, nil
}

func cloneFragments(fragments []ContextFragment) []ContextFragment {
	if fragments == nil {
		return nil
	}
	out := make([]ContextFragment, len(fragments))
	copy(out, fragments)
	for i := range out {
		out[i].PairingIDs = append([]string(nil), fragments[i].PairingIDs...)
		if fragments[i].Messages != nil {
			out[i].Messages = make([]*Message, len(fragments[i].Messages))
			for j, message := range fragments[i].Messages {
				out[i].Messages[j] = cloneMessagePtr(message)
			}
		}
	}
	return out
}

func pinCurrentInputFragments(fragments []ContextFragment) {
	for i := range fragments {
		if len(fragments[i].Messages) > 0 && fragments[i].Messages[0] != nil && fragments[i].Messages[0].CurrentInput {
			fragments[i].Pinned = true
		}
	}
}

func sortFragments(frags []ContextFragment) {
	sort.SliceStable(frags, func(i, j int) bool {
		fi, fj := frags[i], frags[j]
		ri, rj := fragmentSectionRank(fi), fragmentSectionRank(fj)
		if ri != rj {
			return ri < rj
		}
		if ri == 2 && fi.Ordinal != fj.Ordinal {
			return fi.Ordinal < fj.Ordinal
		}
		si, sj := stabilityRank[fi.Stability], stabilityRank[fj.Stability]
		if si != sj {
			return si < sj
		}
		if fi.Priority != fj.Priority {
			return fi.Priority > fj.Priority
		}
		return sourceOrderRank(fi.Source) < sourceOrderRank(fj.Source)
	})
}

func fragmentSectionRank(fragment ContextFragment) int {
	switch fragment.Slot {
	case SlotSystemPrompt, SlotAgentIdentity, SlotPolicies:
		return 0
	case SlotUserProfile, SlotMemory, SlotRAG, SlotSummary, SlotWorkspace:
		return 1
	case SlotConversation, SlotToolResult:
		return 2
	default:
		return 3
	}
}

func sourceOrderRank(source string) int {
	order := map[string]int{
		string(SourceSystemPrompt): 0,
		string(SourceRetrieval):    1,
		string(SourceWorkspace):    2,
		string(SourceConversation): 3,
		string(SourceToolResult):   4,
	}
	if r, ok := order[source]; ok {
		return r
	}
	return 99
}

type trimFunc func(frags []ContextFragment) (keep, dropped []ContextFragment)

func (b *DefaultBuilder) trim(frags []ContextFragment, budget int) ([]ContextFragment, []TrimRecord) {
	var trimmed []TrimRecord

	levels := []struct {
		name   string
		filter trimFunc
	}{
		{"ephemeral_drop", dropEphemeral},
		{"old_tool_drop", dropOldTools(3)},
		{"old_turn_drop", dropOldTurns(b.config.MinPreserve.RecentTurns)},
		{"semi_item_drop", dropSemiByPriority},
	}

	current := frags
	for _, level := range levels {
		if sumTokenCost(current) <= budget {
			break
		}
		var dropped []ContextFragment
		var pairedDropped []ContextFragment
		current, dropped = level.filter(current)
		current, pairedDropped = enforceAtomicPairing(frags, current)
		dropped = append(dropped, pairedDropped...)
		for _, d := range dropped {
			trimmed = append(trimmed, TrimRecord{Fragment: d, Reason: level.name})
		}
	}

	// Emergency trim: keep only stable + current input.
	if sumTokenCost(current) > budget {
		before := current
		current = emergencyTrim(current, budget)
		current, _ = enforceAtomicPairing(frags, current)
		for _, dropped := range removedFragments(before, current) {
			trimmed = append(trimmed, TrimRecord{Fragment: dropped, Reason: "emergency_trim"})
		}
	}
	return current, trimmed
}

func messagePairingIDs(msg *Message) []string {
	if msg == nil {
		return nil
	}
	ids := make([]string, 0, len(msg.ToolCalls)+1)
	for _, call := range msg.ToolCalls {
		if call.ID != "" {
			ids = append(ids, call.ID)
		}
	}
	if msg.ToolResult != nil && msg.ToolResult.CallID != "" {
		ids = append(ids, msg.ToolResult.CallID)
	}
	return ids
}

func enforceAtomicPairing(all, kept []ContextFragment) ([]ContextFragment, []ContextFragment) {
	keptIDs := make(map[*Message]bool)
	for _, fragment := range kept {
		if len(fragment.Messages) > 0 {
			keptIDs[fragment.Messages[0]] = true
		}
	}
	droppedPair := make(map[string]bool)
	for _, fragment := range all {
		if len(fragment.Messages) == 0 || keptIDs[fragment.Messages[0]] {
			continue
		}
		for _, pairingID := range fragment.PairingIDs {
			droppedPair[pairingID] = true
		}
	}
	var dropped []ContextFragment
	current := append([]ContextFragment(nil), kept...)
	for {
		result := make([]ContextFragment, 0, len(current))
		changed := false
		for _, fragment := range current {
			remove := false
			for _, pairingID := range fragment.PairingIDs {
				if droppedPair[pairingID] {
					remove = true
					break
				}
			}
			if !remove {
				result = append(result, fragment)
				continue
			}
			dropped = append(dropped, fragment)
			for _, pairingID := range fragment.PairingIDs {
				droppedPair[pairingID] = true
			}
			changed = true
		}
		current = result
		if !changed {
			return current, dropped
		}
	}
}

func validateToolPairing(fragments []ContextFragment) error {
	calls := make(map[string]int)
	results := make(map[string]int)
	for _, fragment := range fragments {
		if len(fragment.Messages) == 0 || fragment.Messages[0] == nil {
			continue
		}
		message := fragment.Messages[0]
		for _, call := range message.ToolCalls {
			if call.ID == "" {
				return fmt.Errorf("%w: empty tool call id", ErrToolPairingInvalid)
			}
			calls[call.ID]++
		}
		if message.ToolResult != nil {
			if message.ToolResult.CallID == "" {
				return fmt.Errorf("%w: empty tool result call id", ErrToolPairingInvalid)
			}
			results[message.ToolResult.CallID]++
		}
	}
	for id, count := range calls {
		if count != 1 || results[id] != 1 {
			return fmt.Errorf("%w: call_id=%s calls=%d results=%d", ErrToolPairingInvalid, id, count, results[id])
		}
	}
	for id, count := range results {
		if count != 1 || calls[id] != 1 {
			return fmt.Errorf("%w: call_id=%s calls=%d results=%d", ErrToolPairingInvalid, id, calls[id], count)
		}
	}
	return nil
}

func removedFragments(before, after []ContextFragment) []ContextFragment {
	kept := make(map[string]int)
	for _, fragment := range after {
		kept[fragmentRemovalKey(fragment)]++
	}
	var removed []ContextFragment
	for _, fragment := range before {
		key := fragmentRemovalKey(fragment)
		if kept[key] > 0 {
			kept[key]--
			continue
		}
		removed = append(removed, fragment)
	}
	return removed
}

func fragmentRemovalKey(fragment ContextFragment) string {
	var message *Message
	if len(fragment.Messages) > 0 {
		message = fragment.Messages[0]
	}
	return fmt.Sprintf("%p\x00%s\x00%s\x00%d\x00%d\x00%s", message, fragment.Slot, fragment.Source, fragment.Ordinal, fragment.Priority, fragment.Content)
}

func dropEphemeral(frags []ContextFragment) (keep, dropped []ContextFragment) {
	for _, f := range frags {
		if f.Stability == StabilityEphemeral && !f.Pinned {
			dropped = append(dropped, f)
		} else {
			keep = append(keep, f)
		}
	}
	return
}

func dropOldTools(keepN int) trimFunc {
	return func(frags []ContextFragment) (keep, dropped []ContextFragment) {
		var toolIndexes []int
		for _, f := range frags {
			if f.Slot == SlotToolResult {
				toolIndexes = append(toolIndexes, len(toolIndexes))
			}
		}
		dropCount := len(toolIndexes) - keepN
		if dropCount <= 0 {
			return frags, nil
		}
		seenTools := 0
		for _, fragment := range frags {
			if fragment.Slot == SlotToolResult && seenTools < dropCount && !fragment.Pinned {
				dropped = append(dropped, fragment)
				seenTools++
				continue
			}
			if fragment.Slot == SlotToolResult {
				seenTools++
			}
			keep = append(keep, fragment)
		}
		return
	}
}

func dropOldTurns(minTurns int) trimFunc {
	return func(frags []ContextFragment) (keep, dropped []ContextFragment) {
		userCount := 0
		cutIdx := -1
		for i := len(frags) - 1; i >= 0; i-- {
			if frags[i].Slot == SlotConversation && frags[i].Role == RoleUser {
				userCount++
				if userCount >= minTurns {
					cutIdx = i
					break
				}
			}
		}
		if cutIdx <= 0 {
			return frags, nil
		}
		for i, f := range frags {
			if f.Slot == SlotConversation && i < cutIdx && !f.Pinned {
				dropped = append(dropped, f)
			} else {
				keep = append(keep, f)
			}
		}
		return
	}
}

func dropSemiByPriority(frags []ContextFragment) (keep, dropped []ContextFragment) {
	dropIndex := -1
	for i, f := range frags {
		if f.Stability == StabilitySemi && !f.Pinned {
			if dropIndex < 0 || f.Priority < frags[dropIndex].Priority {
				dropIndex = i
			}
		}
	}
	for i, fragment := range frags {
		if i == dropIndex {
			dropped = append(dropped, fragment)
			continue
		}
		keep = append(keep, fragment)
	}
	return
}

func emergencyTrim(frags []ContextFragment, budget int) []ContextFragment {
	var result []ContextFragment
	// Keep stable + pinned + last user message.
	var lastUserIdx = -1
	for i := len(frags) - 1; i >= 0; i-- {
		if frags[i].Slot == SlotConversation && frags[i].Role == RoleUser {
			lastUserIdx = i
			break
		}
	}
	for i, f := range frags {
		if f.Stability == StabilityStable || f.Pinned {
			result = append(result, f)
		} else if i == lastUserIdx {
			result = append(result, f)
		}
	}
	// 未标记为必保留的最后用户消息仍可降级；Pinned（含 current input）绝不能静默丢弃。
	if sumTokenCost(result) > budget {
		for _, f := range result {
			if f.Pinned && f.Stability != StabilityStable {
				return result
			}
		}
		result = result[:0]
		for _, f := range frags {
			if f.Stability == StabilityStable {
				result = append(result, f)
			}
		}
	}
	return result
}

func assemble(frags []ContextFragment) (msgs []*Message, cacheBreak int) {
	cacheBreak = -1
	for i, f := range frags {
		var msg *Message
		if len(f.Messages) > 0 {
			msg = cloneMessagePtr(f.Messages[0])
			if f.Content != "" {
				msg.Content = f.Content
			}
		} else {
			msg = &Message{
				Role:    f.Role,
				Content: f.Content,
			}
		}
		// Write fragment metadata.
		if msg.Extra == nil {
			msg.Extra = make(map[string]any)
		}
		msg.Extra["cm_fragment"] = map[string]any{
			"slot":      string(f.Slot),
			"stability": string(f.Stability),
			"priority":  f.Priority,
			"source":    f.Source,
		}
		if f.Stability == StabilityStable {
			cacheBreak = i
		}
		msgs = append(msgs, msg)
	}
	return
}

func cloneMessagePtr(msg *Message) *Message {
	if msg == nil {
		return &Message{}
	}
	cloned := cloneMessageValue(*msg)
	return &cloned
}

func sumTokenCost(frags []ContextFragment) int {
	total := 0
	for _, f := range frags {
		total += f.TokenCost
	}
	return total
}
