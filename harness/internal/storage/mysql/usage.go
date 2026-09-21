package mysql

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/AGenUI/agenui-studio/harness/internal/storage"
)

type usageStore struct {
	db     *sql.DB
	schema physicalSchema
}

func (s *usageStore) Record(ctx context.Context, r *storage.ModelUsageRecord) error {
	if r == nil || r.ID == "" {
		return storage.NewError(storage.ErrInvalidArgument, "usage id required")
	}
	scope := storage.ScopeFromLenient(ctx)
	if r.TenantID == "" {
		r.TenantID = scope.TenantID
	}
	if err := storage.ValidateUsageIdentifiers(r); err != nil {
		return err
	}
	if err := scope.EnforceTenant(r.TenantID); err != nil {
		return err
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_usage_records
		(usage_id, request_id, trace_id, tenant_id, session_id, run_id, agent_id, provider, model,
		 attempt, fallback_applied, prompt_tokens, completion_tokens, reasoning_tokens,
		 cache_read_tokens, cache_write_tokens, usage_source, currency, estimated_cost,
		 total_latency_ms, first_token_observed, first_token_ms, generation_duration_ms,
		 output_tokens_per_second, cache_hit, output_ref, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE
			prompt_tokens=VALUES(prompt_tokens),
			completion_tokens=VALUES(completion_tokens),
			reasoning_tokens=VALUES(reasoning_tokens),
			cache_read_tokens=VALUES(cache_read_tokens),
			cache_write_tokens=VALUES(cache_write_tokens),
			estimated_cost=VALUES(estimated_cost),
			total_latency_ms=VALUES(total_latency_ms),
			first_token_observed=VALUES(first_token_observed),
			first_token_ms=VALUES(first_token_ms),
			generation_duration_ms=VALUES(generation_duration_ms),
			output_tokens_per_second=VALUES(output_tokens_per_second),
			cache_hit=VALUES(cache_hit),
			output_ref=VALUES(output_ref)`,
		s.schema.id("model_usage_records", "usage_id", r.ID), s.schema.nullID("model_usage_records", "request_id", r.RequestID), nullStr(r.TraceID), r.TenantID, nullStr(r.SessionID),
		nullStr(r.RunID), nullStr(r.AgentID), nullStr(r.Provider), nullStr(r.Model),
		r.Attempt, r.FallbackApplied, r.PromptTokens, r.CompletionTokens, r.ReasoningTokens,
		r.CacheReadTokens, r.CacheWriteTokens, nullStr(r.UsageSource), nullStr(r.Currency),
		r.EstimatedCost, r.TotalLatencyMS, r.FirstTokenObserved, r.FirstTokenMS,
		r.GenerationDurationMS, r.OutputTokensPerSecond, r.CacheHit, nullStr(r.OutputRef), r.CreatedAt,
	)
	return err
}

func (s *usageStore) List(ctx context.Context, q storage.ModelUsageQuery) ([]*storage.ModelUsageRecord, error) {
	query, args := s.usageSelectQuery(ctx, q, false)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storage.ModelUsageRecord
	for rows.Next() {
		r, err := scanUsage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *usageStore) Summary(ctx context.Context, q storage.ModelUsageQuery) (storage.ModelUsageSummary, error) {
	query, args := s.usageSelectQuery(ctx, q, true)
	var out storage.ModelUsageSummary
	var currency sql.NullString
	err := s.db.QueryRowContext(ctx, query, args...).Scan(
		&out.Records, &out.PromptTokens, &out.CompletionTokens, &out.ReasoningTokens,
		&out.CacheReadTokens, &out.CacheWriteTokens, &out.EstimatedCost, &currency,
	)
	if err != nil {
		return storage.ModelUsageSummary{}, err
	}
	out.Currency = currency.String
	out.TenantID = q.TenantID
	if out.TenantID == "" {
		out.TenantID = storage.ScopeFromLenient(ctx).TenantID
	}
	return out, nil
}

func (s *usageStore) usageSelectQuery(ctx context.Context, q storage.ModelUsageQuery, summary bool) (string, []any) {
	scope := storage.ScopeFromLenient(ctx)
	var sb strings.Builder
	if summary {
		sb.WriteString(`SELECT COUNT(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0),
			COALESCE(SUM(reasoning_tokens),0), COALESCE(SUM(cache_read_tokens),0),
			COALESCE(SUM(cache_write_tokens),0), COALESCE(SUM(estimated_cost),0), MAX(currency)
			FROM model_usage_records WHERE 1=1`)
	} else {
		sb.WriteString(`SELECT usage_id, request_id, trace_id, tenant_id, session_id, run_id, agent_id,
			provider, model, attempt, fallback_applied, prompt_tokens, completion_tokens,
			reasoning_tokens, cache_read_tokens, cache_write_tokens, usage_source, currency,
			estimated_cost, total_latency_ms, first_token_observed, first_token_ms,
			generation_duration_ms, output_tokens_per_second, cache_hit, output_ref, created_at
			FROM model_usage_records WHERE 1=1`)
	}
	var args []any
	tenant := q.TenantID
	if tenant == "" {
		tenant = scope.TenantID
	}
	if tenant != "" {
		sb.WriteString(" AND tenant_id=?")
		args = append(args, tenant)
	}
	if q.SessionID != "" {
		sb.WriteString(" AND session_id=?")
		args = append(args, q.SessionID)
	}
	if q.RunID != "" {
		sb.WriteString(" AND run_id=?")
		args = append(args, q.RunID)
	}
	if q.AgentID != "" {
		sb.WriteString(" AND agent_id=?")
		args = append(args, q.AgentID)
	}
	if q.Provider != "" {
		sb.WriteString(" AND provider=?")
		args = append(args, q.Provider)
	}
	if q.Model != "" {
		sb.WriteString(" AND model=?")
		args = append(args, q.Model)
	}
	if !q.From.IsZero() {
		sb.WriteString(" AND created_at>=?")
		args = append(args, q.From)
	}
	if !q.To.IsZero() {
		sb.WriteString(" AND created_at<?")
		args = append(args, q.To)
	}
	if !summary {
		sb.WriteString(" ORDER BY created_at ASC, usage_id ASC")
		if q.Limit > 0 {
			sb.WriteString(" LIMIT ?")
			args = append(args, q.Limit)
		}
	}
	return sb.String(), args
}

func scanUsage(s scanner) (*storage.ModelUsageRecord, error) {
	var (
		r                                                              storage.ModelUsageRecord
		requestID, traceID, sessionID, runID, agentID, provider, model sql.NullString
		usageSource, currency, outputRef                               sql.NullString
	)
	if err := s.Scan(
		&r.ID, &requestID, &traceID, &r.TenantID, &sessionID, &runID, &agentID,
		&provider, &model, &r.Attempt, &r.FallbackApplied, &r.PromptTokens,
		&r.CompletionTokens, &r.ReasoningTokens, &r.CacheReadTokens,
		&r.CacheWriteTokens, &usageSource, &currency, &r.EstimatedCost,
		&r.TotalLatencyMS, &r.FirstTokenObserved, &r.FirstTokenMS,
		&r.GenerationDurationMS, &r.OutputTokensPerSecond, &r.CacheHit, &outputRef, &r.CreatedAt,
	); err != nil {
		return nil, err
	}
	r.RequestID, r.TraceID = requestID.String, traceID.String
	r.SessionID, r.RunID, r.AgentID = sessionID.String, runID.String, agentID.String
	r.Provider, r.Model = provider.String, model.String
	r.UsageSource, r.Currency = usageSource.String, currency.String
	r.OutputRef = outputRef.String
	return &r, nil
}

var _ storage.ModelUsageStore = (*usageStore)(nil)
