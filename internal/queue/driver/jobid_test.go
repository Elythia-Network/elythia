package driver

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestJobID(t *testing.T) {
	assert.Equal(t, "", JobID(context.Background()))
	assert.Equal(t, "42", JobID(WithJobID(context.Background(), "42")))
}
