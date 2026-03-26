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

func collectionWireName(memberName string, shapeName string) string {
	switch shapeName {
	case "QueueAttributeMap":
		return "Attribute"
	case "MessageBodyAttributeMap":
		return "MessageAttribute"
	case "MessageSystemAttributeMap":
		return "MessageSystemAttribute"
	case "TagMap":
		return "Tag"
	}
	if strings.HasSuffix(shapeName, "List") {
		return strings.TrimSuffix(shapeName, "List")
	}
	if strings.HasSuffix(shapeName, "Map") {
		return strings.TrimSuffix(shapeName, "Map")
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
