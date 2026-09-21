package edit

import (
	"errors"
	"strings"

	"github.com/AGenUI/agenui-studio/agenui-agent/internal/generation/workspace"
)

// AuthorizedPatch converts one frozen edit contract into the exact Workspace
// mutations it authorizes. It is shared by contract admission and execution so
// a contract accepted by the Host cannot later fail through a second mapping.
func AuthorizedPatch(contract Contract, baseRevision string) (workspace.PatchRequest, error) {
	if contract.SchemaVersion != SchemaVersion ||
		(contract.Operation != "update" && contract.Operation != "upsert") ||
		len(contract.TargetSet) == 0 {
		return workspace.PatchRequest{}, errors.New("agenui workspace: invalid edit contract")
	}
	request := workspace.PatchRequest{BaseRevision: baseRevision}
	for _, target := range contract.TargetSet {
		switch target.Kind {
		case "design_component_insert":
			if target.ComponentID == "" || target.ParentID == "" || target.Component == nil {
				return workspace.PatchRequest{}, errors.New("agenui workspace: edit contract insertion is incomplete")
			}
			request.Operations = append(request.Operations, workspace.Operation{
				Op: "insert_component", ComponentID: target.ComponentID,
				Component: target.Component, ParentID: target.ParentID, AfterID: target.AfterID,
			})
		case "design_slot":
			if target.ComponentID == "" || strings.TrimSpace(target.Path) == "" {
				return workspace.PatchRequest{}, errors.New("agenui workspace: edit contract target is incomplete")
			}
			path := "/" + strings.ReplaceAll(target.Path, ".", "/")
			request.Operations = append(request.Operations, workspace.Operation{
				Op: "set_component_property", ComponentID: target.ComponentID,
				Path: path, Value: target.Value,
			})
			request.EditablePaths = append(request.EditablePaths,
				"/components/"+escapeComponentID(target.ComponentID)+path)
		default:
			return workspace.PatchRequest{}, errors.New("agenui workspace: edit contract target kind is unsupported")
		}
	}
	return request, nil
}

func escapeComponentID(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
