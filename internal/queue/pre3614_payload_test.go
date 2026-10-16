package queue

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// pre3614DeliverJSON / pre3614InboxJSON write p the way releases before #3614
// did: the struct with a []byte Body, marshalled by encoding/json (the
// activity becomes base64). Field order and tags mirror the old types.
func pre3614DeliverJSON(t *testing.T, p DeliverPayload) []byte {
	t.Helper()
	type pre3614Deliver struct {
		Inbox          string `json:"inbox"`
		Body           []byte `json:"body"`
		KeyID          string `json:"keyId"`
		KeyPEM         string `json:"keyPem,omitempty"`
		Ed25519KeyID   string `json:"ed25519KeyId,omitempty"`
		Ed25519PrivPEM string `json:"ed25519PrivPem,omitempty"`
		IsSharedInbox  bool   `json:"isSharedInbox,omitempty"`
		SignerUserID   string `json:"signerUserId,omitempty"`
		NotAfter       int64  `json:"notAfter,omitempty"`
	}
	out, err := json.Marshal(pre3614Deliver{p.Inbox, p.Body, p.KeyID, p.KeyPEM, p.Ed25519KeyID, p.Ed25519PrivPEM, p.IsSharedInbox, p.SignerUserID, p.NotAfter})
	require.NoError(t, err)
	return out
}

func pre3614InboxJSON(t *testing.T, p InboxPayload) []byte {
	t.Helper()
	type pre3614Inbox struct {
		Body    []byte            `json:"body"`
		Host    string            `json:"host,omitempty"`
		Method  string            `json:"method,omitempty"`
		Path    string            `json:"path,omitempty"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	out, err := json.Marshal(pre3614Inbox{p.Body, p.Host, p.Method, p.Path, p.Headers})
	require.NoError(t, err)
	return out
}
