package api

import (
	"encoding/json"
	"testing"
)

// A PATCH tells "deadline": null (clear it) from no deadline (leave it).
func TestUpdateSandboxDeadline(t *testing.T) {
	var absent, null, set UpdateSandbox
	if err := json.Unmarshal([]byte(`{"protected":true}`), &absent); err != nil || absent.Deadline != nil {
		t.Fatalf("absent: %v %v", err, absent.Deadline)
	}
	if err := json.Unmarshal([]byte(`{"deadline":null}`), &null); err != nil || null.Deadline == nil || *null.Deadline != nil {
		t.Fatalf("null: %v %v", err, null.Deadline)
	}
	if err := json.Unmarshal([]byte(`{"deadline":{"at":"2030-01-01T00:00:00Z","action":"stop"}}`), &set); err != nil ||
		set.Deadline == nil || *set.Deadline == nil || (*set.Deadline).Action != "stop" {
		t.Fatalf("set: %v %v", err, set.Deadline)
	}
	if err := json.Unmarshal([]byte(`{"bogus":1}`), &set); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	// And it still marshals a clear as null.
	var none *Deadline
	b, _ := json.Marshal(UpdateSandbox{Deadline: &none})
	if string(b) != `{"deadline":null}` {
		t.Fatalf("marshal: %s", b)
	}
}
