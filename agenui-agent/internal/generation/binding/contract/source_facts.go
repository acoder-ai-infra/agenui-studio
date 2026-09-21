package contract

import (
	"fmt"
	"strings"
)

// ValidateSourceFacts checks model-selected paths against the exact schema
// facts frozen for each source. It contains no domain vocabulary or aliases.
func ValidateSourceFacts(input Input, result Result) error {
	sources := make(map[string]SourceSnapshot, len(input.Sources))
	for _, source := range input.Sources {
		sources[source.SourceID+"\x00"+source.KnowledgeID] = source
	}
	for _, binding := range result.Bindings {
		source, ok := sources[binding.SourceID+"\x00"+binding.KnowledgeID]
		if !ok {
			return fmt.Errorf("%w: binding %q references an unknown source", ErrInvalidResult, binding.RequirementID)
		}
		path := strings.TrimSpace(binding.FieldPath)
		if path == "" {
			path = strings.TrimSpace(binding.ActionPath)
		}
		if path == "" {
			continue
		}
		allowed := false
		for _, candidate := range source.Paths {
			if path == candidate {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf(
				"%w: source path %q is not present in source %q; choose one of %v",
				ErrInvalidResult, path, source.SourceID, source.Paths,
			)
		}
		if strings.Contains(binding.RefKey, "[*]") && source.ListPath == "" {
			return fmt.Errorf(
				"%w: source %q is single-object but target %q has list-item scope",
				ErrInvalidResult, source.SourceID, binding.RefKey,
			)
		}
	}
	return nil
}
