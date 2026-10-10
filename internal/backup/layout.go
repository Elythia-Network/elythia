package backup

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Object names inside a generation directory.
const (
	MetaFile       = "meta.json"
	VerifyFile     = "verify.json"
	DumpFile       = "dump.pgc"
	DumpFileAge    = "dump.pgc.age"
	generationRoot = "generations/"
)

// idLayout is the generation ID format: UTC, sortable as a string.
const idLayout = "20060102T150405Z"

var idPattern = regexp.MustCompile(`^\d{8}T\d{6}Z$`)

// NewID returns the generation ID for t.
func NewID(t time.Time) string { return t.UTC().Format(idLayout) }

// ValidID reports whether id is a generation ID. Callers must check it
// before building keys from user input.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// IDTime parses a generation ID.
func IDTime(id string) (time.Time, error) {
	if !ValidID(id) {
		return time.Time{}, fmt.Errorf("backup: invalid generation id %q", id)
	}
	return time.Parse(idLayout, id)
}

// GenerationPrefix is the key prefix of every generation.
func GenerationPrefix() string { return generationRoot }

// Key returns the key of name inside generation id.
func Key(id, name string) string { return generationRoot + id + "/" + name }

// GenerationIDFromKey extracts the generation ID from a key, or "" when the
// key is not inside a generation directory.
func GenerationIDFromKey(key string) string {
	rest, ok := strings.CutPrefix(key, generationRoot)
	if !ok {
		return ""
	}
	id, _, ok := strings.Cut(rest, "/")
	if !ok || !ValidID(id) {
		return ""
	}
	return id
}
