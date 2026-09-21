package operator

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// validateBindableValue accepts ordinary JSON values and rejects only values
// that cannot be transported deterministically through the tool/runtime ABI.
func validateBindableValue(value any) error {
	switch current := value.(type) {
	case map[string]any:
		for _, child := range current {
			if err := validateBindableValue(child); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range current {
			if err := validateBindableValue(child); err != nil {
				return err
			}
		}
	case bool, string, nil:
		return nil
	case json.Number:
		value, err := strconv.ParseFloat(current.String(), 64)
		if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
			return fmt.Errorf("operator: non-finite number")
		}
	default:
		return fmt.Errorf("operator: unsupported JSON value")
	}
	return nil
}
