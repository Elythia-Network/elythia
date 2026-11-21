package vary

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAdd_KeepsWhatWasThere(t *testing.T) {
	h := http.Header{}
	h.Add("Vary", "Origin")
	h.Add("Vary", "Accept-Encoding")
	Add(h, "Accept")
	assert.Equal(t, []string{"Origin", "Accept-Encoding", "Accept"}, h.Values("Vary"))
}

func TestAdd_DoesNotRepeat(t *testing.T) {
	h := http.Header{}
	h.Set("Vary", "Origin, accept")
	Add(h, "Accept")
	assert.Equal(t, []string{"Origin, accept"}, h.Values("Vary"))

	h = http.Header{}
	h.Set("Vary", "*")
	Add(h, "Accept")
	assert.Equal(t, []string{"*"}, h.Values("Vary"))
}

func TestHas(t *testing.T) {
	h := http.Header{}
	assert.False(t, Has(h, "Accept"))
	h.Set("Vary", "Origin , Accept-Encoding")
	assert.True(t, Has(h, "accept-encoding"))
	assert.False(t, Has(h, "Accept"))
}
