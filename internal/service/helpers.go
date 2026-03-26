package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func stringValue(input map[string]any, key string) string {
	raw, ok := input[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return typed
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	case float64:
		return strconv.FormatInt(int64(typed), 10)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(raw)
	}
}

func int64Value(input map[string]any, key string, fallback int64) (int64, error) {
	raw, ok := input[key]
	if !ok || raw == nil {
		return fallback, nil
	}
	switch typed := raw.(type) {
	case int:
		return int64(typed), nil
	case int64:
		return typed, nil
	case float64:
		return int64(typed), nil
	case string:
		value, err := strconv.ParseInt(typed, 10, 64)
		if err != nil {
			return 0, err
		}
		return value, nil
	default:
		return 0, fmt.Errorf("field %s is not an integer", key)
	}
}

func stringMapValue(input map[string]any, key string) map[string]string {
	raw, ok := input[key]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case map[string]string:
		return typed
	case map[string]any:
		out := make(map[string]string, len(typed))
		for innerKey, value := range typed {
			out[innerKey] = stringValue(map[string]any{"value": value}, "value")
		}
		return out
	default:
		return nil
	}
}

func stringSliceValue(input map[string]any, key string) []string {
	raw, ok := input[key]
	if !ok || raw == nil {
		return nil
	}
	switch typed := raw.(type) {
	case []string:
		return append([]string(nil), typed...)
	case []any:
		out := make([]string, 0, len(typed))
		for _, entry := range typed {
			out = append(out, stringValue(map[string]any{"value": entry}, "value"))
		}
		return out
	default:
		return nil
	}
}

func encodeNextToken(offset int) string {
	if offset <= 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

func decodeNextToken(token string) (int, error) {
	if token == "" {
		return 0, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(string(raw))
}

func normalizePolicyJSON(raw string) string {
	if raw == "" {
		return raw
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return raw
	}
	buf, err := json.Marshal(value)
	if err != nil {
		return raw
	}
	return string(buf)
}

func parseJSONDocument(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil, err
	}
	return value, nil
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func containsFold(values []string, needle string) bool {
	for _, value := range values {
		if strings.EqualFold(value, needle) {
			return true
		}
	}
	return false
}
