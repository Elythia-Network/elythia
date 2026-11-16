package plugin

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeliveryTargets_Allows(t *testing.T) {
	hosts := []string{"a.example", "b.example:8443"}
	tests := []struct {
		name    string
		targets DeliveryTargets
		host    string
		want    bool
	}{
		{name: "include listed", targets: DeliveryTargets{Mode: DeliveryTargetsInclude, Hosts: hosts}, host: "a.example", want: true},
		{name: "include listed with port", targets: DeliveryTargets{Mode: DeliveryTargetsInclude, Hosts: hosts}, host: "b.example:8443", want: true},
		{name: "include unlisted", targets: DeliveryTargets{Mode: DeliveryTargetsInclude, Hosts: hosts}, host: "c.example", want: false},
		{name: "include does not match subdomains", targets: DeliveryTargets{Mode: DeliveryTargetsInclude, Hosts: hosts}, host: "sub.a.example", want: false},
		{name: "include empty list", targets: DeliveryTargets{Mode: DeliveryTargetsInclude}, host: "a.example", want: false},
		{name: "exclude listed", targets: DeliveryTargets{Mode: DeliveryTargetsExclude, Hosts: hosts}, host: "a.example", want: false},
		{name: "exclude unlisted", targets: DeliveryTargets{Mode: DeliveryTargetsExclude, Hosts: hosts}, host: "c.example", want: true},
		{name: "exclude empty list", targets: DeliveryTargets{Mode: DeliveryTargetsExclude}, host: "c.example", want: true},
		{name: "unknown mode denies", targets: DeliveryTargets{Mode: "all", Hosts: hosts}, host: "a.example", want: false},
		{name: "zero value denies", targets: DeliveryTargets{}, host: "a.example", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.targets.Allows(tt.host))
		})
	}
}

func TestErrBadNoteOption_Wraps(t *testing.T) {
	err := fmt.Errorf("%w: hosts is empty", ErrBadNoteOption)
	assert.True(t, errors.Is(err, ErrBadNoteOption))
}
