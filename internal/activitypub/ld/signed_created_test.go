package ld_test

import (
	"testing"

	"github.com/shiroha-a/mk/internal/activitypub/ld"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SignedCreated は JSON のキー名ではなく、署名が覆う正規形の dc:created を返す。
// 別名で書いても同じ値が取れ、署名されない位置 (`type` / `id` /
// `signatureValue`、signature 側の `@context`) は効かない。
func TestSignedCreated(t *testing.T) {
	const created = "2026-09-26T00:00:00.000Z"
	tests := []struct {
		name string
		sig  map[string]any
		want []string
	}{
		{
			name: "plain created key",
			sig:  map[string]any{"type": "RsaSignature2017", "creator": "https://a.example/u#k", "created": created},
			want: []string{created},
		},
		{
			name: "compact IRI alias",
			sig: map[string]any{
				"creator":    "https://a.example/u#k",
				"dc:created": map[string]any{"@value": created, "@type": "xsd:dateTime"},
			},
			want: []string{created},
		},
		{
			name: "absolute IRI alias",
			sig: map[string]any{
				"creator": "https://a.example/u#k",
				"http://purl.org/dc/terms/created": map[string]any{
					"@value": created, "@type": "http://www.w3.org/2001/XMLSchema#dateTime",
				},
			},
			want: []string{created},
		},
		{
			name: "missing",
			sig:  map[string]any{"creator": "https://a.example/u#k"},
			want: nil,
		},
		{
			name: "signature-side context cannot rename a term",
			sig: map[string]any{
				"@context": map[string]any{"when": "dc:created"},
				"creator":  "https://a.example/u#k",
				"when":     created,
			},
			want: nil,
		},
		{
			name: "node reference is reported as unreadable",
			sig:  map[string]any{"creator": "https://a.example/u#k", "created": map[string]any{"@id": "https://a.example/t"}},
			want: []string{""},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ld.NewProcessor().SignedCreated(tc.sig)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestSignedCreated_NormalizeError(t *testing.T) {
	_, err := ld.NewProcessor().SignedCreated(map[string]any{
		"created": map[string]any{"@value": map[string]any{"nested": true}},
	})
	require.Error(t, err)
}
