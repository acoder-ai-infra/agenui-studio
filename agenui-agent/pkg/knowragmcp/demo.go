package knowragmcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AGenUI/agenui-studio/agenui-agent/examples"
)

type demoAPI struct {
	ID                string  `json:"id"`
	SearchText        string  `json:"searchText"`
	Path              string  `json:"path"`
	Description       string  `json:"description"`
	DataSourceID      string  `json:"dataSourceId"`
	KnowledgeEntityID string  `json:"knowledgeEntityId"`
	Score             float64 `json:"score"`
	ResponseModel     any     `json:"responseModel"`
	ResponseExample   any     `json:"responseExample"`
	ResponseFields    any     `json:"responseFields"`
	Entity            any     `json:"entity"`
	BindingContract   any     `json:"bindingContract"`
}

// SeedDemo publishes the bundled generic source through the same facts used
// by user-managed APIs. Re-running it only updates this stable demo ID.
func (s *Server) SeedDemo(ctx context.Context) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("knowrag mcp: database is required")
	}
	if err := s.EnsureSchema(ctx); err != nil {
		return err
	}
	demos := []struct {
		definition []byte
		example    []byte
		name       string
	}{
		{definition: examples.ProductAPIJSON, example: examples.ProductsJSON, name: "product"},
		{definition: examples.OfferAPIJSON, example: examples.OffersJSON, name: "offer"},
		{definition: examples.TravelStatusAPIJSON, example: examples.TravelStatusJSON, name: "travel status"},
	}
	for _, source := range demos {
		var api demoAPI
		if err := json.Unmarshal(source.definition, &api); err != nil {
			return fmt.Errorf("knowrag mcp: decode bundled %s API: %w", source.name, err)
		}
		if err := json.Unmarshal(source.example, &api.ResponseExample); err != nil {
			return fmt.Errorf("knowrag mcp: decode bundled %s example: %w", source.name, err)
		}
		if err := s.upsertDemoAPI(ctx, api); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) upsertDemoAPI(ctx context.Context, api demoAPI) error {
	values := make([]string, 5)
	for index, value := range []any{api.ResponseModel, api.ResponseExample, api.ResponseFields, api.Entity, api.BindingContract} {
		encoded, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("seed demo API %s: %w", api.ID, err)
		}
		values[index] = string(encoded)
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO knowrag_api
 (id,search_text,path,method,description,project_name,score,data_source_id,api_version,knowledge_entity_id,knowledge_revision,response_model,response_example,response_fields,entity,binding_contract)
VALUES (?, ?, ?, 'GET', ?, 'agenui-studio-demo', ?, ?, 'v1', ?, 'demo-v1', ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
 search_text=excluded.search_text,path=excluded.path,method=excluded.method,
 description=excluded.description,project_name=excluded.project_name,score=excluded.score,
 data_source_id=excluded.data_source_id,api_version=excluded.api_version,
 knowledge_entity_id=excluded.knowledge_entity_id,knowledge_revision=excluded.knowledge_revision,
 response_model=excluded.response_model,response_example=excluded.response_example,
 response_fields=excluded.response_fields,entity=excluded.entity,binding_contract=excluded.binding_contract`,
		api.ID, api.SearchText, api.Path, api.Description, api.Score, api.DataSourceID,
		api.KnowledgeEntityID, values[0], values[1], values[2], values[3], values[4],
	)
	if err != nil {
		return fmt.Errorf("seed demo API %s: %w", api.ID, err)
	}
	return nil
}
