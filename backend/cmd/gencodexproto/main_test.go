package main

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestFlattenedUnionDiscriminatorNamesRemainDistinct(t *testing.T) {
	s := &schema{}
	for _, value := range []string{"openai/form", "openaiForm"} {
		s.OneOf = append(s.OneOf, &schema{Properties: map[string]*schema{"mode": {Enum: []any{value}}}})
	}
	var out strings.Builder
	var inline []inlineType
	(&generator{}).renderFlattenedUnion(&out, "Request", s, "mode", &inline, map[string]bool{})
	generated := "package fixture\n" + out.String()
	if _, err := parser.ParseFile(token.NewFileSet(), "fixture.go", generated, parser.DeclarationErrors); err != nil {
		t.Fatalf("generated colliding enum names: %v\n%s", err, generated)
	}
	for _, declaration := range []string{"RequestTypeOpenaiForm RequestType = \"openai/form\"", "RequestTypeOpenaiForm2 RequestType = \"openaiForm\""} {
		if !strings.Contains(generated, declaration) {
			t.Errorf("missing %s in %s", declaration, generated)
		}
	}
}

func TestFlattenedUnionDiscriminatorSuffixCollision(t *testing.T) {
	s := &schema{}
	for _, value := range []string{"openai/form", "openai/form2", "openaiForm"} {
		s.OneOf = append(s.OneOf, &schema{Properties: map[string]*schema{"mode": {Enum: []any{value}}}})
	}
	var out strings.Builder
	var inline []inlineType
	(&generator{}).renderFlattenedUnion(&out, "Request", s, "mode", &inline, map[string]bool{})
	if _, err := parser.ParseFile(token.NewFileSet(), "fixture.go", "package fixture\n"+out.String(), parser.DeclarationErrors); err != nil {
		t.Fatalf("generated suffix collides with another enum: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "RequestTypeOpenaiForm3 RequestType = \"openaiForm\"") {
		t.Fatalf("suffix must skip the existing OpenaiForm2 identifier: %s", out.String())
	}
}
