package runtime

import (
	"fmt"
	"strconv"
	"strings"
)

type pathTokenKind uint8

const (
	pathTokenField pathTokenKind = iota
	pathTokenIndex
	pathTokenWildcard
)

type pathToken struct {
	kind  pathTokenKind
	field string
	index int
}

type pathMatch struct {
	coordinates []int
	value       any
	found       bool
	empty       bool
	code        string
}

func Extract(data any, path string) (any, bool) {
	tokens, err := parseFieldPath(path)
	if err != nil {
		return nil, false
	}
	value, found, _ := extractProjected(data, tokens)
	return value, found
}

func parseFieldPath(path string) ([]pathToken, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("field path is empty")
	}
	if path == "$" {
		return nil, nil
	}
	if strings.HasPrefix(path, "$") {
		path = path[1:]
		if strings.HasPrefix(path, ".") {
			path = path[1:]
		} else if !strings.HasPrefix(path, "[") {
			return nil, fmt.Errorf("field path must use $.field or $[index]: %q", path)
		}
	}
	return parsePathBody(path, '.')
}

func parseRefKey(path string) ([]pathToken, error) {
	path = strings.TrimSpace(path)
	if path == "" || !strings.HasPrefix(path, "/") {
		return nil, fmt.Errorf("refKey must start with '/': %q", path)
	}
	if path == "/" {
		return nil, fmt.Errorf("refKey must address a field")
	}
	parts := strings.Split(path[1:], "/")
	result := make([]pathToken, 0, len(parts)*2)
	for _, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("refKey contains an empty segment: %q", path)
		}
		part = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		tokens, err := parsePathBody(part, 0)
		if err != nil {
			return nil, fmt.Errorf("refKey %q: %w", path, err)
		}
		result = append(result, tokens...)
	}
	return result, nil
}

func parsePathBody(path string, separator byte) ([]pathToken, error) {
	if path == "" {
		return nil, fmt.Errorf("path is empty")
	}
	result := make([]pathToken, 0, strings.Count(path, "[")+1)
	for len(path) > 0 {
		if separator != 0 && path[0] == separator {
			return nil, fmt.Errorf("path contains an empty segment")
		}
		if path[0] == '[' {
			token, rest, err := parseBracketToken(path)
			if err != nil {
				return nil, err
			}
			result = append(result, token)
			path = rest
		} else {
			end := len(path)
			if separator != 0 {
				if index := strings.IndexByte(path, separator); index >= 0 && index < end {
					end = index
				}
			}
			if index := strings.IndexByte(path, '['); index >= 0 && index < end {
				end = index
			}
			result = append(result, pathToken{kind: pathTokenField, field: path[:end]})
			path = path[end:]
		}
		if separator != 0 && strings.HasPrefix(path, string(separator)) {
			path = path[1:]
			if path == "" {
				return nil, fmt.Errorf("path has a trailing separator")
			}
		}
	}
	return result, nil
}

func parseBracketToken(path string) (pathToken, string, error) {
	end := strings.IndexByte(path, ']')
	if end < 2 {
		return pathToken{}, "", fmt.Errorf("invalid bracket expression")
	}
	content := path[1:end]
	rest := path[end+1:]
	if content == "*" {
		return pathToken{kind: pathTokenWildcard}, rest, nil
	}
	if len(content) >= 2 && ((content[0] == '\'' && content[len(content)-1] == '\'') ||
		(content[0] == '"' && content[len(content)-1] == '"')) {
		key, err := unquoteMapKey(content)
		if err != nil {
			return pathToken{}, "", err
		}
		return pathToken{kind: pathTokenField, field: key}, rest, nil
	}
	index, err := strconv.Atoi(content)
	if err != nil || index < 0 {
		return pathToken{}, "", fmt.Errorf("invalid array index %q", content)
	}
	return pathToken{kind: pathTokenIndex, index: index}, rest, nil
}

func unquoteMapKey(value string) (string, error) {
	if value[0] == '\'' {
		body := strings.ReplaceAll(value[1:len(value)-1], `\'`, `'`)
		body = strings.ReplaceAll(body, `\\`, `\`)
		if body == "" {
			return "", fmt.Errorf("map key is empty")
		}
		return body, nil
	}
	key, err := strconv.Unquote(value)
	if err != nil || key == "" {
		return "", fmt.Errorf("invalid map key %q", value)
	}
	return key, nil
}

func wildcardCount(tokens []pathToken) int {
	count := 0
	for _, token := range tokens {
		if token.kind == pathTokenWildcard {
			count++
		}
	}
	return count
}

func extractProjected(node any, tokens []pathToken) (any, bool, string) {
	if len(tokens) == 0 {
		return node, true, ""
	}
	token := tokens[0]
	switch token.kind {
	case pathTokenField:
		if object, ok := node.(map[string]any); ok {
			value, exists := object[token.field]
			if !exists || value == nil {
				return nil, false, CodeMapKeyNotFound
			}
			return extractProjected(value, tokens[1:])
		}
		if array, ok := node.([]any); ok {
			index, err := strconv.Atoi(token.field)
			if err != nil {
				return nil, false, CodeBindingSourcePathInvalid
			}
			if index < 0 || index >= len(array) {
				return nil, false, CodeFixedIndexOutOfRange
			}
			return extractProjected(array[index], tokens[1:])
		}
		return nil, false, CodeBindingSourcePathInvalid
	case pathTokenIndex:
		array, ok := node.([]any)
		if !ok {
			return nil, false, CodeBindingSourcePathInvalid
		}
		if token.index >= len(array) {
			return nil, false, CodeFixedIndexOutOfRange
		}
		return extractProjected(array[token.index], tokens[1:])
	default:
		array, ok := node.([]any)
		if !ok {
			return nil, false, CodeBindingSourcePathInvalid
		}
		values := make([]any, 0, len(array))
		for _, item := range array {
			value, found, code := extractProjected(item, tokens[1:])
			if !found {
				return nil, false, code
			}
			values = append(values, value)
		}
		return values, true, ""
	}
}

func selectPath(node any, tokens []pathToken) []pathMatch {
	result := make([]pathMatch, 0)
	selectPathInto(node, tokens, nil, &result)
	return result
}

func selectPathInto(node any, tokens []pathToken, coordinates []int, result *[]pathMatch) {
	if len(tokens) == 0 {
		*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), value: node, found: true})
		return
	}
	token := tokens[0]
	switch token.kind {
	case pathTokenField:
		if object, ok := node.(map[string]any); ok {
			value, exists := object[token.field]
			if !exists || value == nil {
				*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), code: CodeMapKeyNotFound})
				return
			}
			selectPathInto(value, tokens[1:], coordinates, result)
			return
		}
		if array, ok := node.([]any); ok {
			index, err := strconv.Atoi(token.field)
			if err != nil {
				*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), code: CodeBindingSourcePathInvalid})
				return
			}
			if index < 0 || index >= len(array) {
				*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), code: CodeFixedIndexOutOfRange})
				return
			}
			selectPathInto(array[index], tokens[1:], coordinates, result)
			return
		}
		*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), code: CodeBindingSourcePathInvalid})
	case pathTokenIndex:
		array, ok := node.([]any)
		if !ok {
			*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), code: CodeBindingSourcePathInvalid})
			return
		}
		if token.index >= len(array) {
			*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), code: CodeFixedIndexOutOfRange})
			return
		}
		selectPathInto(array[token.index], tokens[1:], coordinates, result)
	default:
		array, ok := node.([]any)
		if !ok {
			*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), code: CodeBindingSourcePathInvalid})
			return
		}
		if len(array) == 0 {
			*result = append(*result, pathMatch{coordinates: append([]int(nil), coordinates...), found: true, empty: true})
			return
		}
		for index, item := range array {
			selectPathInto(item, tokens[1:], append(coordinates, index), result)
		}
	}
}

func assignEmptyRef(root map[string]any, tokens []pathToken, coordinates []int) error {
	wildcards := 0
	for index, token := range tokens {
		if token.kind != pathTokenWildcard {
			continue
		}
		if wildcards == len(coordinates) {
			if index == 0 {
				return fmt.Errorf("target wildcard has no parent")
			}
			return assignRef(root, tokens[:index], coordinates, []any{})
		}
		wildcards++
	}
	return fmt.Errorf("target has no unmatched wildcard")
}

func assignRef(root map[string]any, tokens []pathToken, coordinates []int, value any) error {
	if root == nil {
		return fmt.Errorf("target root is nil")
	}
	if len(tokens) == 0 {
		return fmt.Errorf("target path is empty")
	}
	_, used, err := assignPathNode(root, tokens, coordinates, 0, value)
	if err != nil {
		return err
	}
	if used != len(coordinates) {
		return fmt.Errorf("target path consumed %d of %d coordinates", used, len(coordinates))
	}
	return nil
}

func assignPathNode(node any, tokens []pathToken, coordinates []int, used int, value any) (any, int, error) {
	if len(tokens) == 0 {
		return value, used, nil
	}
	token := tokens[0]
	switch token.kind {
	case pathTokenField:
		object, ok := node.(map[string]any)
		if node == nil {
			object = make(map[string]any)
			ok = true
		}
		if !ok {
			return node, used, fmt.Errorf("field %q collides with %T", token.field, node)
		}
		child, nextUsed, err := assignPathNode(object[token.field], tokens[1:], coordinates, used, value)
		if err != nil {
			return node, used, err
		}
		object[token.field] = child
		return object, nextUsed, nil
	case pathTokenIndex, pathTokenWildcard:
		index := token.index
		if token.kind == pathTokenWildcard {
			if used >= len(coordinates) {
				return node, used, fmt.Errorf("target wildcard has no source coordinate")
			}
			index = coordinates[used]
			used++
		}
		array, ok := node.([]any)
		if node == nil {
			array = make([]any, index+1)
			ok = true
		}
		if !ok {
			return node, used, fmt.Errorf("array index %d collides with %T", index, node)
		}
		if len(array) <= index {
			grown := make([]any, index+1)
			copy(grown, array)
			array = grown
		}
		child, nextUsed, err := assignPathNode(array[index], tokens[1:], coordinates, used, value)
		if err != nil {
			return node, used, err
		}
		array[index] = child
		return array, nextUsed, nil
	default:
		return node, used, fmt.Errorf("unknown path token")
	}
}

func Assign(root map[string]any, path string, value any) bool {
	var tokens []pathToken
	var err error
	if strings.HasPrefix(strings.TrimSpace(path), "/") {
		tokens, err = parseRefKey(path)
	} else {
		tokens, err = parseFieldPath(path)
	}
	if err != nil || wildcardCount(tokens) != 0 {
		return false
	}
	return assignRef(root, tokens, nil, value) == nil
}

func renderParam(template string, params map[string]any) string {
	out := template
	for name, value := range params {
		out = strings.ReplaceAll(out, "{{"+name+"}}", fmt.Sprintf("%v", value))
	}
	return out
}
