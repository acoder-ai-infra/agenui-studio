package workspace

import (
	"errors"
)

// ParseDesignArtifact restores a Workspace Document from its typed artifact.
func ParseDesignArtifact(raw string) (Document, error) {
	artifact, err := DecodeDesignArtifact(raw)
	if err != nil {
		return Document{}, err
	}
	document := Document{SchemaVersion: SchemaVersion}
	for _, message := range artifact.Messages {
		if create, ok := message["createSurface"].(map[string]any); ok {
			document.SurfaceID, _ = create["surfaceId"].(string)
			document.CatalogID, _ = create["catalogId"].(string)
		}
		if update, ok := message["updateComponents"].(map[string]any); ok {
			if document.SurfaceID == "" {
				document.SurfaceID, _ = update["surfaceId"].(string)
			}
			if values, ok := update["components"].([]any); ok {
				for _, value := range values {
					component, ok := value.(map[string]any)
					if !ok {
						return Document{}, errors.New("agenui workspace: invalid component")
					}
					document.Components = append(document.Components, cloneMap(component))
				}
			}
		}
		if update, ok := message["updateDataModel"].(map[string]any); ok {
			value, _ := update["value"].(map[string]any)
			if value == nil {
				// Read-only compatibility for artifacts created before v0.9 was
				// normalized. Messages() always emits the canonical value field.
				value, _ = update["data"].(map[string]any)
			}
			document.DataModel = cloneMap(value)
		}
	}
	document.FieldSlots = cloneMaps(artifact.FieldSlots)
	document.ActionSlots = cloneMaps(artifact.ActionSlots)
	document.DesignKnowledgeReceipt = cloneMap(artifact.DesignKnowledgeReceipt)
	document.RootID = inferRootID(document.Components)
	if err := document.ValidateStructural(); err != nil {
		return Document{}, err
	}
	return document, nil
}

func inferRootID(components []map[string]any) string {
	ids := make(map[string]struct{}, len(components))
	referenced := make(map[string]struct{})
	for _, component := range components {
		if id, _ := component["id"].(string); id != "" {
			ids[id] = struct{}{}
		}
		collectRefs(component["child"], referenced)
		collectRefs(component["children"], referenced)
	}
	if _, exists := ids["root"]; exists {
		return "root"
	}
	for id := range ids {
		if _, exists := referenced[id]; !exists {
			return id
		}
	}
	return ""
}

func collectRefs(value any, output map[string]struct{}) {
	switch current := value.(type) {
	case string:
		if current != "" {
			output[current] = struct{}{}
		}
	case []any:
		for _, item := range current {
			collectRefs(item, output)
		}
	}
}
