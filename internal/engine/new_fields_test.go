package engine

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestTxnRecordNewFieldsRoundTrip pins that the source-record and agent-list
// fields added for add/adopt/rm survive the transaction journal's JSON
// encoding intact.
func TestTxnRecordNewFieldsRoundTrip(t *testing.T) {
	in := TxnRecord{
		Op:       "add",
		TxnID:    "0123456789abcdef0123456789abcdef",
		Sequence: 0,
		Name:     "pdf-tools",
		SourceFields: map[string]string{
			"type": "git", "url": "https://x/y.git", "commit": "a1b2c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c",
		},
		Agents:  []string{"claude", "codex"},
		Stage:   "prepared",
		Digest:  "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Message: "add: pdf-tools",
	}
	// Round trip through the same wire encoding the journal uses (fields
	// that are never persisted must not be required on the way back).
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out TxnRecord
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out.SourceFields, in.SourceFields) {
		t.Fatalf("SourceFields round trip = %v, want %v", out.SourceFields, in.SourceFields)
	}
	if !reflect.DeepEqual(out.Agents, in.Agents) {
		t.Fatalf("Agents round trip = %v, want %v", out.Agents, in.Agents)
	}
	if out.Op != in.Op || out.Name != in.Name || out.Message != in.Message {
		t.Fatalf("scalar fields lost in round trip: %+v", out)
	}
}
