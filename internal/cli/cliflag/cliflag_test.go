package cliflag

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantCode int
		wantOK   bool
		wantOut  []string
	}{
		{name: "no args", args: nil, wantCode: 0, wantOK: true},
		{name: "known flag", args: []string{"-config", "x.yml"}, wantCode: 0, wantOK: true},
		{name: "help", args: []string{"-h"}, wantCode: 0, wantOK: false, wantOut: []string{"Usage: elythia demo [flags]", "-config"}},
		{name: "unknown flag", args: []string{"-nope"}, wantCode: 2, wantOK: false, wantOut: []string{"flag provided but not defined: -nope", "Usage: elythia demo"}},
		{name: "stray positional argument", args: []string{"dry-run"}, wantCode: 2, wantOK: false, wantOut: []string{`elythia demo: unexpected argument "dry-run"`, "Usage: elythia demo"}},
		{name: "positional after flags", args: []string{"-config", "x.yml", "extra"}, wantCode: 2, wantOK: false, wantOut: []string{`unexpected argument "extra"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			fs := New("demo", &out)
			cfg := fs.String("config", "default.yml", "path to configuration file")
			code, ok := Parse(fs, tt.args)
			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, tt.wantOK, ok)
			for _, w := range tt.wantOut {
				assert.Contains(t, out.String(), w)
			}
			if len(tt.wantOut) == 0 {
				assert.Empty(t, out.String())
			}
			if tt.wantOK && len(tt.args) == 2 {
				assert.Equal(t, "x.yml", *cfg)
			}
		})
	}
}
