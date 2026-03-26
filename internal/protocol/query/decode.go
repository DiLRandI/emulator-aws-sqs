package query

import (
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	apierrors "emulator-aws-sqs/internal/errors"
	"emulator-aws-sqs/internal/model"
)

func DecodeRequest(reg model.Registry, r *http.Request, body []byte) (string, map[string]any, error) {
	values, err := mergeValues(r.URL.RawQuery, body)
	if err != nil {
		return "", nil, apierrors.New("MalformedQueryString", http.StatusBadRequest, "The query string is malformed.").WithCause(err)
	}

	action := values.Get("Action")
	if action == "" {
		return "", nil, apierrors.ErrUnsupportedOperation.WithCause(fmt.Errorf("missing Action"))
	}
	op, ok := reg.Operation(action)
	if !ok {
		return "", nil, apierrors.ErrUnsupportedOperation.WithCause(fmt.Errorf("unknown action %q", action))
	}

	input := map[string]any{}
	if op.InputShape == "" {
		return action, input, nil
	}

	shape := reg.MustShape(op.InputShape)
	for _, memberName := range shape.MemberOrder {
		member := shape.Members[memberName]
		prefix := memberWireName(reg, memberName, member)
		value, found, err := decodeShape(reg, values, member.Shape, prefix, memberName)
		if err != nil {
			return "", nil, err
		}
		if found {
			input[memberName] = value
		}
	}
	return action, input, nil
}

func mergeValues(rawQuery string, body []byte) (url.Values, error) {
	values := url.Values{}
	if rawQuery != "" {
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			return nil, err
		}
		for key, entries := range q {
			for _, entry := range entries {
				values.Add(key, entry)
			}
		}
	}
	if len(body) != 0 {
		q, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, err
		}
		for key, entries := range q {
			for _, entry := range entries {
				values.Add(key, entry)
			}
		}
	}
	return values, nil
}

func decodeShape(reg model.Registry, values url.Values, shapeName, prefix, parentMember string) (any, bool, error) {
	shape := reg.MustShape(shapeName)
	switch shape.Type {
	case "structure":
		out := map[string]any{}
		found := false
		for _, memberName := range shape.MemberOrder {
			member := shape.Members[memberName]
			childPrefix := memberWireName(reg, memberName, member)
			if prefix != "" {
				childPrefix = prefix + "." + childPrefix
			}
			value, childFound, err := decodeShape(reg, values, member.Shape, childPrefix, memberName)
			if err != nil {
				return nil, false, err
			}
			if childFound {
				found = true
				out[memberName] = value
			}
		}
		return out, found, nil
	case "list":
		indices := collectIndices(values, prefix)
		if len(indices) == 0 {
			return nil, false, nil
		}
		items := make([]any, 0, len(indices))
		for _, idx := range indices {
			itemPrefix := fmt.Sprintf("%s.%d", prefix, idx)
			value, found, err := decodeShape(reg, values, shape.Member.Shape, itemPrefix, parentMember)
			if err != nil {
				return nil, false, err
			}
			if !found {
				raw := values.Get(itemPrefix)
				if raw == "" {
					continue
				}
				primitive, err := decodePrimitive(reg.MustShape(shape.Member.Shape), raw)
				if err != nil {
					return nil, false, err
				}
				items = append(items, primitive)
				continue
			}
			items = append(items, value)
		}
		return items, len(items) != 0, nil
	case "map":
		indices := collectIndices(values, prefix)
		if len(indices) == 0 {
			return nil, false, nil
		}
		out := map[string]any{}
		for _, idx := range indices {
			key := values.Get(fmt.Sprintf("%s.%d.Name", prefix, idx))
			if key == "" {
				continue
			}
			value, found, err := decodeShape(reg, values, shape.Value.Shape, fmt.Sprintf("%s.%d.Value", prefix, idx), parentMember)
			if err != nil {
				return nil, false, err
			}
			if !found {
				raw := values.Get(fmt.Sprintf("%s.%d.Value", prefix, idx))
				if raw == "" {
					continue
				}
				value, err = decodePrimitive(reg.MustShape(shape.Value.Shape), raw)
				if err != nil {
					return nil, false, err
				}
			}
			out[key] = value
		}
		return out, len(out) != 0, nil
	default:
		raw, ok := values[prefix]
		if !ok || len(raw) == 0 {
			return nil, false, nil
		}
		value, err := decodePrimitive(shape, raw[0])
		if err != nil {
			return nil, false, err
		}
		return value, true, nil
	}
}

func decodePrimitive(shape model.Shape, raw string) (any, error) {
	switch shape.Type {
	case "integer", "long":
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "A parameter value is invalid.").WithCause(err)
		}
		return value, nil
	case "boolean":
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, apierrors.New("InvalidParameterValue", http.StatusBadRequest, "A parameter value is invalid.").WithCause(err)
		}
		return value, nil
	default:
		return raw, nil
	}
}

func collectIndices(values url.Values, prefix string) []int {
	seen := map[int]struct{}{}
	token := prefix + "."
	for key := range values {
		if !strings.HasPrefix(key, token) {
			continue
		}
		remainder := strings.TrimPrefix(key, token)
		parts := strings.SplitN(remainder, ".", 2)
		idx, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		seen[idx] = struct{}{}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]int, 0, len(seen))
	for idx := range seen {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}
