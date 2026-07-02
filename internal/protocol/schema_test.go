package protocol

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestApprovalEnvelopeSchemaValidJSON ensures the embedded schema parses and has
// the expected top-level shape.
func TestApprovalEnvelopeSchemaValidJSON(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(ApprovalEnvelopeSchema, &doc); err != nil {
		t.Fatalf("embedded schema is not valid JSON: %v", err)
	}
	if doc["title"] != "ApprovalEnvelope" {
		t.Errorf("schema title=%v want ApprovalEnvelope", doc["title"])
	}
	if doc["type"] != "object" {
		t.Errorf("schema type=%v want object", doc["type"])
	}
}

// TestApprovalEnvelopeSchemaMatchesStruct keeps the published schema in lockstep
// with the Go struct: every JSON field name on ApprovalEnvelope (and Choice) must
// appear as a property in the schema. If a field is added to the struct without
// updating docs/approval-envelope.schema.json, this fails the build — which is the
// whole point of publishing the schema as the protocol moat.
func TestApprovalEnvelopeSchemaMatchesStruct(t *testing.T) {
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       struct {
			Choice struct {
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"choice"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(ApprovalEnvelopeSchema, &doc); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}

	assertFieldsCovered(t, "ApprovalEnvelope", reflect.TypeOf(ApprovalEnvelope{}), doc.Properties)
	assertFieldsCovered(t, "Choice", reflect.TypeOf(Choice{}), doc.Defs.Choice.Properties)
}

func assertFieldsCovered(t *testing.T, name string, typ reflect.Type, props map[string]json.RawMessage) {
	t.Helper()
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			continue
		}
		field := tag
		if comma := indexByte(tag, ','); comma >= 0 {
			field = tag[:comma]
		}
		if _, ok := props[field]; !ok {
			t.Errorf("%s.%s (json:%q) is missing from the published schema", name, typ.Field(i).Name, field)
		}
	}
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}
