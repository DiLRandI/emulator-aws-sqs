package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type rawServiceModel struct {
	Metadata   rawMetadata             `json:"metadata"`
	Operations map[string]rawOperation `json:"operations"`
	Shapes     map[string]rawShape     `json:"shapes"`
}

type rawMetadata struct {
	APIVersion     string   `json:"apiVersion"`
	EndpointPrefix string   `json:"endpointPrefix"`
	JSONVersion    string   `json:"jsonVersion"`
	Protocol       string   `json:"protocol"`
	Protocols      []string `json:"protocols"`
	TargetPrefix   string   `json:"targetPrefix"`
	ServiceID      string   `json:"serviceId"`
}

type rawOperation struct {
	Input  rawShapeRef   `json:"input"`
	Output rawShapeRef   `json:"output"`
	Errors []rawShapeRef `json:"errors"`
}

type rawShape struct {
	Type      string               `json:"type"`
	Enum      []string             `json:"enum"`
	Required  []string             `json:"required"`
	Members   map[string]rawMember `json:"members"`
	Key       *rawShapeRef         `json:"key"`
	Value     *rawShapeRef         `json:"value"`
	Member    *rawShapeRef         `json:"member"`
	Flattened bool                 `json:"flattened"`
	Exception bool                 `json:"exception"`
}

type rawShapeRef struct {
	Shape        string `json:"shape"`
	LocationName string `json:"locationName"`
	Flattened    bool   `json:"flattened"`
}

type rawMember struct {
	Shape        string `json:"shape"`
	LocationName string `json:"locationName"`
	Flattened    bool   `json:"flattened"`
}

type rawPaginators struct {
	Pagination map[string]rawPaginator `json:"pagination"`
}

type rawPaginator struct {
	InputToken  string `json:"input_token"`
	OutputToken string `json:"output_token"`
	LimitKey    string `json:"limit_key"`
	ResultKey   string `json:"result_key"`
}

type outputRegistry struct {
	Metadata   outputMetadata             `json:"metadata"`
	Operations map[string]outputOperation `json:"operations"`
	Shapes     map[string]outputShape     `json:"shapes"`
	Pagination map[string]outputPaginator `json:"pagination"`
}

type outputMetadata struct {
	APIVersion     string `json:"apiVersion"`
	EndpointPrefix string `json:"endpointPrefix"`
	JSONVersion    string `json:"jsonVersion"`
	Protocol       string `json:"protocol"`
	TargetPrefix   string `json:"targetPrefix"`
	QueryNamespace string `json:"queryNamespace"`
	ServiceID      string `json:"serviceId"`
}

type outputOperation struct {
	Name        string   `json:"name"`
	InputShape  string   `json:"inputShape,omitempty"`
	OutputShape string   `json:"outputShape,omitempty"`
	Errors      []string `json:"errors,omitempty"`
}

type outputPaginator struct {
	InputToken  string `json:"inputToken,omitempty"`
	OutputToken string `json:"outputToken,omitempty"`
	LimitKey    string `json:"limitKey,omitempty"`
	ResultKey   string `json:"resultKey,omitempty"`
}

type outputShape struct {
	Name        string                  `json:"name"`
	Type        string                  `json:"type"`
	Enum        []string                `json:"enum,omitempty"`
	Required    []string                `json:"required,omitempty"`
	MemberOrder []string                `json:"memberOrder,omitempty"`
	Members     map[string]outputMember `json:"members,omitempty"`
	Key         *outputShapeRef         `json:"key,omitempty"`
	Value       *outputShapeRef         `json:"value,omitempty"`
	Member      *outputShapeRef         `json:"member,omitempty"`
	Flattened   bool                    `json:"flattened,omitempty"`
	Exception   bool                    `json:"exception,omitempty"`
}

type outputShapeRef struct {
	Shape        string `json:"shape"`
	LocationName string `json:"locationName,omitempty"`
	Flattened    bool   `json:"flattened,omitempty"`
}

type outputMember struct {
	Shape        string `json:"shape"`
	LocationName string `json:"locationName,omitempty"`
	Flattened    bool   `json:"flattened,omitempty"`
}

func main() {
	serviceModelPath := flag.String("service-model", "", "path to service-2.json(.gz)")
	paginatorsPath := flag.String("paginators", "", "path to paginators-1.json")
	outPath := flag.String("out", "", "generated go output")
	flag.Parse()

	if *serviceModelPath == "" || *paginatorsPath == "" || *outPath == "" {
		fmt.Fprintln(os.Stderr, "missing required flags")
		os.Exit(2)
	}

	serviceModel := mustReadServiceModel(*serviceModelPath)
	paginators := mustReadPaginators(*paginatorsPath)

	registry := outputRegistry{
		Metadata: outputMetadata{
			APIVersion:     serviceModel.Metadata.APIVersion,
			EndpointPrefix: serviceModel.Metadata.EndpointPrefix,
			JSONVersion:    serviceModel.Metadata.JSONVersion,
			Protocol:       serviceModel.Metadata.Protocol,
			TargetPrefix:   serviceModel.Metadata.TargetPrefix,
			QueryNamespace: fmt.Sprintf("http://queue.amazonaws.com/doc/%s/", serviceModel.Metadata.APIVersion),
			ServiceID:      serviceModel.Metadata.ServiceID,
		},
		Operations: make(map[string]outputOperation, len(serviceModel.Operations)),
		Shapes:     make(map[string]outputShape, len(serviceModel.Shapes)),
		Pagination: make(map[string]outputPaginator, len(paginators.Pagination)),
	}

	for name, op := range serviceModel.Operations {
		item := outputOperation{Name: name}
		item.InputShape = op.Input.Shape
		item.OutputShape = op.Output.Shape
		item.Errors = make([]string, 0, len(op.Errors))
		for _, errRef := range op.Errors {
			item.Errors = append(item.Errors, errRef.Shape)
		}
		registry.Operations[name] = item
	}

	for name, shape := range serviceModel.Shapes {
		item := outputShape{
			Name:      name,
			Type:      shape.Type,
			Enum:      append([]string(nil), shape.Enum...),
			Required:  append([]string(nil), shape.Required...),
			Flattened: shape.Flattened,
			Exception: shape.Exception,
		}
		if len(shape.Members) != 0 {
			item.Members = make(map[string]outputMember, len(shape.Members))
			item.MemberOrder = make([]string, 0, len(shape.Members))
			for memberName, member := range shape.Members {
				item.MemberOrder = append(item.MemberOrder, memberName)
				item.Members[memberName] = outputMember{
					Shape:        member.Shape,
					LocationName: member.LocationName,
					Flattened:    member.Flattened,
				}
			}
			sort.Strings(item.MemberOrder)
		}
		if shape.Key != nil {
			item.Key = &outputShapeRef{
				Shape:        shape.Key.Shape,
				LocationName: shape.Key.LocationName,
				Flattened:    shape.Key.Flattened,
			}
		}
		if shape.Value != nil {
			item.Value = &outputShapeRef{
				Shape:        shape.Value.Shape,
				LocationName: shape.Value.LocationName,
				Flattened:    shape.Value.Flattened,
			}
		}
		if shape.Member != nil {
			item.Member = &outputShapeRef{
				Shape:        shape.Member.Shape,
				LocationName: shape.Member.LocationName,
				Flattened:    shape.Member.Flattened,
			}
		}
		registry.Shapes[name] = item
	}

	for name, paginator := range paginators.Pagination {
		registry.Pagination[name] = outputPaginator{
			InputToken:  paginator.InputToken,
			OutputToken: paginator.OutputToken,
			LimitKey:    paginator.LimitKey,
			ResultKey:   paginator.ResultKey,
		}
	}

	jsonPayload, err := json.MarshalIndent(registry, "", "\t")
	must(err)

	var source bytes.Buffer
	source.WriteString("package model\n\n")
	source.WriteString("// Code generated by tools/gensqs. DO NOT EDIT.\n\n")
	source.WriteString("func init() {\n")
	source.WriteString("\tDefault = Registry{}\n")
	source.WriteString("\tconst raw = `")
	source.Write(jsonPayload)
	source.WriteString("`\n")
	source.WriteString("\tif err := decodeRegistry([]byte(raw), &Default); err != nil {\n")
	source.WriteString("\t\tpanic(err)\n")
	source.WriteString("\t}\n")
	source.WriteString("}\n")

	formatted, err := format.Source(source.Bytes())
	if err != nil {
		fmt.Fprintln(os.Stderr, string(source.Bytes()))
		must(err)
	}

	must(os.MkdirAll(filepath.Dir(*outPath), 0o755))
	must(os.WriteFile(*outPath, formatted, 0o644))
}

func mustReadServiceModel(path string) rawServiceModel {
	var model rawServiceModel
	raw := mustReadMaybeCompressed(path)
	must(json.Unmarshal(raw, &model))
	return model
}

func mustReadPaginators(path string) rawPaginators {
	var model rawPaginators
	raw := mustReadMaybeCompressed(path)
	must(json.Unmarshal(raw, &model))
	return model
}

func mustReadMaybeCompressed(path string) []byte {
	data, err := os.ReadFile(path)
	must(err)

	if strings.HasSuffix(path, ".gz") {
		reader, err := gzip.NewReader(bytes.NewReader(data))
		must(err)
		defer reader.Close()
		out, err := io.ReadAll(reader)
		must(err)
		return out
	}
	return data
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
