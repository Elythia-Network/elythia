package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHasSecretFieldName(t *testing.T) {
	assert.Equal(t, "hasSmtpPass", hasSecretFieldName("smtpPass"))
	assert.Equal(t, "has", hasSecretFieldName(""))
}
