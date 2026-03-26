package model

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Default is generated from the official botocore SQS service model.
var Default = Registry{}

func (r Registry) Operation(name string) (Operation, bool) {
	op, ok := r.Operations[name]
	return op, ok
}

func (r Registry) Shape(name string) (Shape, bool) {
	shape, ok := r.Shapes[name]
	return shape, ok
}

func (r Registry) MustOperation(name string) Operation {
	op, ok := r.Operation(name)
	if !ok {
		panic(fmt.Sprintf("unknown operation %q", name))
	}
	return op
}

func (r Registry) MustShape(name string) Shape {
	shape, ok := r.Shape(name)
	if !ok {
		panic(fmt.Sprintf("unknown shape %q", name))
	}
	return shape
}

func (r Registry) HasEnum(shapeName, value string) bool {
	shape, ok := r.Shape(shapeName)
	if !ok || len(shape.Enum) == 0 {
		return false
	}
	return slices.Contains(shape.Enum, value)
}

func (r Registry) RequiredMembers(shapeName string) map[string]struct{} {
	shape := r.MustShape(shapeName)
	out := make(map[string]struct{}, len(shape.Required))
	for _, name := range shape.Required {
		out[name] = struct{}{}
	}
	return out
}

func (r Registry) SortedOperationNames() []string {
	names := make([]string, 0, len(r.Operations))
	for name := range r.Operations {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r Registry) Target(operation string) string {
	return r.Metadata.TargetPrefix + "." + operation
}

func (r Registry) ActionFromTarget(target string) (string, bool) {
	prefix := r.Metadata.TargetPrefix + "."
	if !strings.HasPrefix(target, prefix) {
		return "", false
	}
	action := strings.TrimPrefix(target, prefix)
	_, ok := r.Operation(action)
	return action, ok
}
