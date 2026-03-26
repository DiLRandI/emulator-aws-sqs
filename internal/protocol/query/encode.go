package query

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"sort"
	"strconv"

	apierrors "emulator-aws-sqs/internal/errors"
	"emulator-aws-sqs/internal/model"
	"emulator-aws-sqs/internal/protocol"
)

func EncodeResponse(reg model.Registry, operation string, requestID string, resp protocol.Response) (int, http.Header, []byte, error) {
	headers := http.Header{}
	headers.Set("Content-Type", "text/xml")
	var body bytes.Buffer
	body.WriteString(xml.Header)
	body.WriteString("<")
	body.WriteString(operation)
	body.WriteString("Response xmlns=\"")
	body.WriteString(reg.Metadata.QueryNamespace)
	body.WriteString("\">")
	op := reg.MustOperation(operation)
	if op.OutputShape != "" && len(resp.Output) != 0 {
		body.WriteString("<")
		body.WriteString(operation)
		body.WriteString("Result>")
		if err := encodeShape(&body, reg, op.OutputShape, resp.Output, "", ""); err != nil {
			return 0, nil, nil, err
		}
		body.WriteString("</")
		body.WriteString(operation)
		body.WriteString("Result>")
	}
	body.WriteString("<ResponseMetadata><RequestId>")
	xml.EscapeText(&body, []byte(requestID))
	body.WriteString("</RequestId></ResponseMetadata>")
	body.WriteString("</")
	body.WriteString(operation)
	body.WriteString("Response>")
	return http.StatusOK, headers, body.Bytes(), nil
}

func EncodeError(requestID string, err error) (int, http.Header, []byte) {
	apiErr, ok := apierrors.As(err)
	if !ok {
		apiErr = apierrors.Internal("internal server error", err)
	}
	headers := http.Header{}
	headers.Set("Content-Type", "text/xml")
	var body bytes.Buffer
	body.WriteString(xml.Header)
	body.WriteString("<ErrorResponse><Error><Type>")
	body.WriteString(string(apiErr.Fault))
	body.WriteString("</Type><Code>")
	xml.EscapeText(&body, []byte(apiErr.Code))
	body.WriteString("</Code><Message>")
	xml.EscapeText(&body, []byte(apiErr.Message))
	body.WriteString("</Message>")
	if apiErr.Detail != "" {
		body.WriteString("<Detail>")
		xml.EscapeText(&body, []byte(apiErr.Detail))
		body.WriteString("</Detail>")
	}
	body.WriteString("</Error><RequestId>")
	xml.EscapeText(&body, []byte(requestID))
	body.WriteString("</RequestId></ErrorResponse>")
	return apiErr.HTTPStatusCode, headers, body.Bytes()
}

func encodeShape(buf *bytes.Buffer, reg model.Registry, shapeName string, value any, parentMember string, explicitName string) error {
	shape := reg.MustShape(shapeName)
	switch shape.Type {
	case "structure":
		obj, _ := value.(map[string]any)
		for _, memberName := range shape.MemberOrder {
			memberValue, ok := obj[memberName]
			if !ok {
				continue
			}
			member := shape.Members[memberName]
			elemName := memberWireName(reg, memberName, member)
			memberShape := reg.MustShape(member.Shape)
			if memberShape.Type == "list" || memberShape.Type == "map" {
				if err := encodeShape(buf, reg, member.Shape, memberValue, memberName, elemName); err != nil {
					return err
				}
				continue
			}
			buf.WriteString("<")
			buf.WriteString(elemName)
			buf.WriteString(">")
			if err := encodeShape(buf, reg, member.Shape, memberValue, memberName, elemName); err != nil {
				return err
			}
			buf.WriteString("</")
			buf.WriteString(elemName)
			buf.WriteString(">")
		}
	case "list":
		items, _ := value.([]any)
		itemName := explicitName
		if itemName == "" {
			itemName = collectionWireName(parentMember, shapeName)
		}
		for _, item := range items {
			buf.WriteString("<")
			buf.WriteString(itemName)
			buf.WriteString(">")
			if err := encodeShape(buf, reg, shape.Member.Shape, item, parentMember, ""); err != nil {
				return err
			}
			buf.WriteString("</")
			buf.WriteString(itemName)
			buf.WriteString(">")
		}
	case "map":
		obj, _ := value.(map[string]any)
		itemName := explicitName
		if itemName == "" {
			itemName = collectionWireName(parentMember, shapeName)
		}
		keys := make([]string, 0, len(obj))
		for key := range obj {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			buf.WriteString("<")
			buf.WriteString(itemName)
			buf.WriteString("><Name>")
			xml.EscapeText(buf, []byte(key))
			buf.WriteString("</Name><Value>")
			if err := encodeShape(buf, reg, shape.Value.Shape, obj[key], key, ""); err != nil {
				return err
			}
			buf.WriteString("</Value></")
			buf.WriteString(itemName)
			buf.WriteString(">")
		}
	default:
		switch typed := value.(type) {
		case nil:
			return nil
		case string:
			xml.EscapeText(buf, []byte(typed))
		case int:
			buf.WriteString(strconv.Itoa(typed))
		case int64:
			buf.WriteString(strconv.FormatInt(typed, 10))
		case bool:
			if typed {
				buf.WriteString("true")
			} else {
				buf.WriteString("false")
			}
		default:
			xml.EscapeText(buf, []byte(fmt.Sprint(typed)))
		}
	}
	return nil
}
