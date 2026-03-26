package query

import (
	"strings"

	"emulator-aws-sqs/internal/model"
)

func memberWireName(reg model.Registry, memberName string, member model.Member) string {
	if member.LocationName != "" {
		return member.LocationName
	}
	shape := reg.MustShape(member.Shape)
	switch shape.Type {
	case "list", "map":
		return collectionWireName(memberName, member.Shape)
	default:
		return memberName
	}
}

func collectionWireName(memberName, shapeName string) string {
	switch shapeName {
	case "QueueAttributeMap":
		return "Attribute"
	case "MessageBodyAttributeMap":
		return "MessageAttribute"
	case "MessageSystemAttributeMap":
		return "Attribute"
	case "MessageSystemAttributeList":
		return "MessageSystemAttributeName"
	case "TagMap":
		return "Tag"
	}
	if before, ok := strings.CutSuffix(shapeName, "List"); ok {
		return before
	}
	if before, ok := strings.CutSuffix(shapeName, "Map"); ok {
		return before
	}
	return singularize(memberName)
}

func singularize(name string) string {
	switch {
	case strings.HasSuffix(name, "ies"):
		return strings.TrimSuffix(name, "ies") + "y"
	case strings.HasSuffix(name, "sses"):
		return strings.TrimSuffix(name, "es")
	case strings.HasSuffix(name, "s"):
		return strings.TrimSuffix(name, "s")
	default:
		return name
	}
}
