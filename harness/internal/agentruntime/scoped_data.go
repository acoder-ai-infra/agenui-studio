package agentruntime

import "context"

type scopedDataContextKey struct{}

func WithScopedData(ctx context.Context, data ScopedData) context.Context {
	return context.WithValue(ctx, scopedDataContextKey{}, data)
}

func ScopedDataFrom(ctx context.Context) (ScopedData, bool) {
	data, ok := ctx.Value(scopedDataContextKey{}).(ScopedData)
	return data, ok
}

func (d ScopedData) RunValue(key string) (ScopedDataItem, bool) {
	if d.Run == nil {
		return ScopedDataItem{}, false
	}
	item, ok := d.Run[key]
	return item, ok
}

func (d ScopedData) AgentValue(agentID, key string) (ScopedDataItem, bool) {
	if d.Agents == nil {
		return ScopedDataItem{}, false
	}
	agentData := d.Agents[agentID]
	if agentData == nil {
		return ScopedDataItem{}, false
	}
	item, ok := agentData[key]
	return item, ok
}
