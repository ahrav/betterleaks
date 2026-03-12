package validate

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/google/cel-go/common/types/ref"
)

var mapAnyType = reflect.TypeFor[map[string]any]()

// validStatuses is the set of recognised validation statuses.
var validStatuses = map[string]bool{
	"valid":   true,
	"invalid": true,
	"revoked": true,
	"unknown": true,
	"error":   true,
}

// Result holds the outcome of a CEL validation evaluation.
type Result struct {
	Status   string         // "valid", "invalid", "revoked", "unknown", "error"
	Reason   string         // human-readable explanation
	Metadata map[string]any // extra fields from the CEL result map
}

// ParseResult interprets the CEL output value into a Result.
func ParseResult(val ref.Val) *Result {
	switch v := val.Value().(type) {
	case map[string]any:
		return parseResultMap(v)

	default:
		nativeVal, err := val.ConvertToNative(mapAnyType)
		if err == nil {
			if m, ok := nativeVal.(map[string]any); ok {
				return parseResultMap(m)
			}
		}
		return &Result{
			Status:   "error",
			Reason:   fmt.Sprintf("expression returned unexpected type: %T", val.Value()),
			Metadata: map[string]any{},
		}
	}
}

// reservedKeys are map keys consumed by parseResultMap and excluded from metadata.
var reservedKeys = map[string]bool{
	"result": true, "reason": true,
}

// parseResultMap interprets a map result from a CEL expression.
//
// The expected form is {"result": "<status>", ...} where <status> is one of
// the validStatuses.
func parseResultMap(m map[string]any) *Result {
	result := &Result{
		Status:   "unknown",
		Metadata: make(map[string]any),
	}

	// Primary: explicit "result" key with a string status.
	if v, ok := m["result"]; ok {
		if s, ok := v.(string); ok {
			s = strings.ToLower(s)
			if validStatuses[s] {
				result.Status = s
			}
		}
	}

	// Extract reason.
	if r, ok := m["reason"]; ok {
		if s, ok := r.(string); ok {
			result.Reason = s
		}
	}

	// Everything else is metadata.
	for k, v := range m {
		if !reservedKeys[k] {
			result.Metadata[k] = v
		}
	}

	return result
}
