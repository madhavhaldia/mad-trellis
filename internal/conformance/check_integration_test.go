package conformance

import (
	"reflect"
	"testing"
)

func TestIntegrationMeshMethodsIncludeEventAuthoringDecoys(t *testing.T) {
	for _, want := range []string{"integration.publish", "integration.emit", "integration.notify"} {
		if !containsString(meshMethods, want) {
			t.Fatalf("meshMethods missing event-authoring decoy %q", want)
		}
	}
}

func TestIntegrationEventMetadataFieldsPinned(t *testing.T) {
	want := []string{"id", "kind", "branch", "created_at_ms"}
	if !reflect.DeepEqual(eventMetadataFields, want) {
		t.Fatalf("eventMetadataFields = %v, want %v", eventMetadataFields, want)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
