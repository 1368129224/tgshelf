package config

import (
	"fmt"
	"strconv"
	"strings"
)

// asBool converts any value to a bool.
// It accepts a bool as well as the strings "true"/"1"/"yes" and "false"/"0"/"no"
// (case-insensitive).
// An illegal value returns a clear error naming the field and the original value instead
// of silently falling back to the zero value.
func asBool(field string, v interface{}) (bool, error) {
	switch val := v.(type) {
	case bool:
		return val, nil
	case string:
		lower := strings.ToLower(strings.TrimSpace(val))
		switch lower {
		case "true", "1", "yes":
			return true, nil
		case "false", "0", "no":
			return false, nil
		default:
			return false, fmt.Errorf("field %q: cannot convert %q to bool (accepted: true/false/1/0/yes/no)", field, val)
		}
	case int:
		if val == 1 {
			return true, nil
		}
		if val == 0 {
			return false, nil
		}
		return false, fmt.Errorf("field %q: cannot convert int %d to bool (accepted: 0 or 1)", field, val)
	case int64:
		if val == 1 {
			return true, nil
		}
		if val == 0 {
			return false, nil
		}
		return false, fmt.Errorf("field %q: cannot convert int64 %d to bool (accepted: 0 or 1)", field, val)
	default:
		return false, fmt.Errorf("field %q: cannot convert %T value %v to bool", field, v, v)
	}
}

// asInt converts any value to an int.
// It accepts the various integer types as well as digit-only strings (e.g. "42").
// An illegal value returns a clear error naming the field and the original value instead
// of silently falling back to the zero value.
func asInt(field string, v interface{}) (int, error) {
	switch val := v.(type) {
	case int:
		return val, nil
	case int64:
		return int(val), nil
	case int32:
		return int(val), nil
	case int16:
		return int(val), nil
	case int8:
		return int(val), nil
	case float64:
		return int(val), nil
	case float32:
		return int(val), nil
	case string:
		s := strings.TrimSpace(val)
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("field %q: cannot convert %q to int: %w", field, val, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("field %q: cannot convert %T value %v to int", field, v, v)
	}
}

// asInt64 converts any value to an int64.
func asInt64(field string, v interface{}) (int64, error) {
	switch val := v.(type) {
	case int64:
		return val, nil
	case int:
		return int64(val), nil
	case int32:
		return int64(val), nil
	case int16:
		return int64(val), nil
	case int8:
		return int64(val), nil
	case float64:
		return int64(val), nil
	case float32:
		return int64(val), nil
	case string:
		s := strings.TrimSpace(val)
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("field %q: cannot convert %q to int64: %w", field, val, err)
		}
		return n, nil
	default:
		return 0, fmt.Errorf("field %q: cannot convert %T value %v to int64", field, v, v)
	}
}

// normalizeStringList normalizes a string list:
// it accepts []string and []interface{} (which TOML decoding may produce).
// It returns a string slice with blank entries removed.
func normalizeStringList(field string, v interface{}) ([]string, error) {
	if v == nil {
		return []string{}, nil
	}
	switch val := v.(type) {
	case []string:
		result := make([]string, 0, len(val))
		for _, s := range val {
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				result = append(result, trimmed)
			}
		}
		return result, nil
	case []interface{}:
		result := make([]string, 0, len(val))
		for i, item := range val {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("field %q[%d]: expected string, got %T", field, i, item)
			}
			if trimmed := strings.TrimSpace(s); trimmed != "" {
				result = append(result, trimmed)
			}
		}
		return result, nil
	default:
		return nil, fmt.Errorf("field %q: cannot convert %T to []string", field, v)
	}
}
