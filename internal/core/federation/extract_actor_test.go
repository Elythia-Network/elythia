package federation_test

import (
	"testing"

	corefed "github.com/elythia-network/elythia/internal/core/federation"
	"github.com/stretchr/testify/assert"
)

// ExtractActorIRI must derive the SAME actor that Process dispatches on, after
// applying the singleton-array unwrap + JSON-LD normalization. This is what
// makes the InboxProcessor actor-authorization gate robust against
// as:actor / {"@id":...} / array-wrapped spoof shapes (#parity review AUTH-1).
func TestExtractActorIRI(t *testing.T) {
	const victim = "https://victim.example/users/alice"
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain string", `{"type":"Delete","actor":"` + victim + `"}`, victim},
		{"@id object", `{"type":"Delete","actor":{"@id":"` + victim + `"}}`, victim},
		{"as:actor prefix", `{"type":"Delete","as:actor":"` + victim + `"}`, victim},
		{"singleton array", `[{"type":"Delete","actor":"` + victim + `"}]`, victim},
		{"missing actor", `{"type":"Delete"}`, ""},
		{"invalid json", `{not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, corefed.ExtractActorIRI([]byte(tc.body)))
		})
	}
}
