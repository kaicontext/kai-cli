package graph

import (
	"encoding/json"
	"testing"
)

func TestExportEdgeMarshalJSON(t *testing.T) {
	e := ExportEdge{}
	b, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("Marshal ExportEdge: %v", err)
	}
	t.Logf("ExportEdge JSON: %s", string(b))
}

func TestExportNodeMarshalJSON(t *testing.T) {
	n := ExportNode{}
	b, err := json.Marshal(n)
	if err != nil {
		t.Fatalf("Marshal ExportNode: %v", err)
	}
	t.Logf("ExportNode JSON: %s", string(b))
}