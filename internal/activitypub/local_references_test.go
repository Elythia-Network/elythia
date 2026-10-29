package activitypub

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExtractLocalAPReferences(t *testing.T) {
	body := `{
		"actor":"https://example.com/users/a",
		"object":{
			"id":"https://example.com/notes/n1#fragment",
			"attributedTo":{"id":"https://example.com/users/b"},
			"inReplyTo":"https://example.com/notes/n2",
			"quoteUrl":"https://example.com/notes/n3",
			"tag":[{"href":"https://example.com/users/c"}],
			"content":"<a href=\"https://example.com/notes/body\">body</a> https://example.com/users/body-user",
			"summary":"https://example.com/users/summary-user",
			"attachment":[{"url":"https://example.com/notes/arbitrary-property"}]
		},
		"alsoKnownAs":["https://example.com/users/d"],
		"movedTo":"https://example.com/users/e",
		"target":"https://example.com/users/f",
		"instrument":{"id":"https://example.com/users/g"},
		"result":{"id":"https://example.com/notes/n4"},
		"interactionTarget":"https://example.com/notes/n5",
		"orderedItems":[{"id":"https://example.com/notes/n6"}],
		"profile":{"fields":[{"name":"link","value":"https://example.com/users/profile-value"}]},
		"source":{"id":"https://example.com/users/source"},
		"external":"https://example.com.evil.test/users/x",
		"subpath":"https://example.com/users/y/followers",
		"query":"https://example.com/users/z?x=1"
	}`
	users, notes := ExtractLocalAPReferences("https://example.com", body)
	assert.Equal(t, []string{"a", "b", "c", "d", "e", "f", "g"}, users)
	assert.Equal(t, []string{"n1", "n2", "n3", "n4", "n5", "n6"}, notes)
}

func TestExtractLocalAPReferencesCoversStructuralReferenceFields(t *testing.T) {
	body := `{"actor":"https://example.com/users/u1","object":"https://example.com/notes/n1","attributedTo":"https://example.com/users/u2","to":["https://example.com/users/u3"],"cc":"https://example.com/users/u4","bto":"https://example.com/users/u5","bcc":"https://example.com/users/u6","audience":"https://example.com/users/u7","inReplyTo":"https://example.com/notes/n2","quoteUrl":"https://example.com/notes/n3","_misskey_quote":"https://example.com/notes/n4","quote":"https://example.com/notes/n5","quoteUri":"https://example.com/notes/n6","quoteAuthorization":{"id":"https://example.com/notes/n7"},"tag":[{"href":"https://example.com/users/u8"}],"alsoKnownAs":["https://example.com/users/u9"],"movedTo":"https://example.com/users/u10","target":"https://example.com/users/u11","instrument":{"id":"https://example.com/users/u12"},"result":{"id":"https://example.com/notes/n8"},"interactingObject":"https://example.com/notes/n9","interactionTarget":"https://example.com/notes/n10","items":[{"id":"https://example.com/notes/n11"}],"orderedItems":["https://example.com/users/u13"]}`
	users, notes := ExtractLocalAPReferences("https://example.com", body)
	assert.Equal(t, []string{"u1", "u10", "u11", "u12", "u13", "u2", "u3", "u4", "u5", "u6", "u7", "u8", "u9"}, users)
	assert.Equal(t, []string{"n1", "n10", "n11", "n2", "n3", "n4", "n5", "n6", "n7", "n8", "n9"}, notes)
}
func TestExtractLocalAPReferencesAcceptsCanonicalNoteRouteURIs(t *testing.T) {
	users, notes := ExtractLocalAPReferences("https://example.com", `{"items":["https://example.com/notes/n/activity","https://example.com/notes/n/quote-authorizations/approval-1"]}`)
	assert.Empty(t, users)
	assert.Equal(t, []string{"n"}, notes)
}
func TestExtractLocalAPReferencesIgnoresMalformedAndNonCanonicalURLs(t *testing.T) {
	users, notes := ExtractLocalAPReferences("https://example.com", `<a href="https://example.com/users/raw">`)
	assert.Empty(t, users)
	assert.Empty(t, notes)

	users, notes = ExtractLocalAPReferences("https://example.com", `{"actor":["http://example.com/users/a","https://example.com.evil/users/b","https://example.com/users/c/extra","https://example.com/users/d?x=1"],"object":["https://example.com/notes/n/activity/extra","https://example.com/notes/n/quote-authorizations/approval-1/extra"]}`)
	assert.Empty(t, users)
	assert.Empty(t, notes)
}
