package entitycompat

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// distributedDockerfiles are the Dockerfiles that build images mk-go ships or
// that operators build and run in production.
//
// `tests/` の検証用は対象外 (配らないので、base image の付け替えで困るのは
// 検証の再現性だけ)。
var distributedDockerfiles = []string{
	"Dockerfile",
	"Dockerfile.bundled",
	"deploy/uds/Dockerfile.mkgo",
}

// pinnedBaseImageRe accepts `<image>:<tag>@sha256:<64 hex>`.
//
// **tag も要求する。** digest だけだと読む人にも CI の Go 版照合 (vulncheck
// job) にも版が見えない。
var pinnedBaseImageRe = regexp.MustCompile(`^[^@\s:]+(?::\d+)?(?:/[^@\s:]+)*:[^@\s:/]+@sha256:[0-9a-f]{64}$`)

// TestDistributedDockerfileBaseImagesArePinnedByDigest checks that every
// external base image in the shipped Dockerfiles carries a digest.
//
// **tag は付け替えられる。** `golang:1.27.1-alpine` のような patch version の
// tag でも公式 image は alpine の更新などで publish し直すし、distroless は tag を
// 省くと `latest` になる。assets image を digest で固定した (Dockerfile.bundled)
// のと同じ理由で、builder / runtime も固定する。`${...}` を含む FROM は build arg
// で決まるので対象外 (`MISSKEY_ASSETS_IMAGE` は bundled_assets_pin_test が見る)。
func TestDistributedDockerfileBaseImagesArePinnedByDigest(t *testing.T) {
	checked := 0
	for _, path := range distributedDockerfiles {
		for _, ref := range unpinnedBaseImages(readRepoFile(t, path)) {
			assert.Failf(t, "base image が digest で固定されていない",
				"%s:%d: FROM %s\n`<image>:<tag>@sha256:<digest>` の形で書くこと。digest は "+
					"`docker buildx imagetools inspect <image>:<tag>` の Digest 行 (index の digest)。",
				path, ref.line, ref.value)
		}
		checked += len(externalBaseImages(readRepoFile(t, path)))
	}
	require.NotZero(t, checked, "Dockerfile から FROM を 1 つも拾えませんでした。書式が変わったならこのゲートも直すこと")
}

// baseImageRef is one external image referenced by FROM.
type baseImageRef struct {
	line  int
	value string
}

// externalBaseImages returns the FROM images in body that are neither an
// earlier stage, `scratch`, nor a build-arg expansion.
func externalBaseImages(body string) []baseImageRef {
	stages := map[string]bool{}
	var out []baseImageRef
	for i, raw := range strings.Split(body, "\n") {
		fields := strings.Fields(raw)
		if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		rest := fields[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
			rest = rest[1:]
		}
		if len(rest) == 0 {
			continue
		}
		image := rest[0]
		if len(rest) >= 3 && strings.EqualFold(rest[1], "AS") {
			stages[strings.ToLower(rest[2])] = true
		}
		if strings.EqualFold(image, "scratch") || stages[strings.ToLower(image)] && !strings.Contains(image, ":") ||
			strings.Contains(image, "$") {
			continue
		}
		out = append(out, baseImageRef{line: i + 1, value: image})
	}
	return out
}

// unpinnedBaseImages returns the external FROM images in body without a digest.
func unpinnedBaseImages(body string) []baseImageRef {
	var bad []baseImageRef
	for _, ref := range externalBaseImages(body) {
		if !pinnedBaseImageRe.MatchString(ref.value) {
			bad = append(bad, ref)
		}
	}
	return bad
}

// TestUnpinnedBaseImages pins the FROM scanner with synthetic Dockerfiles.
func TestUnpinnedBaseImages(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("ab", 32)
	tests := []struct {
		name     string
		body     string
		external int
		bad      int
	}{
		{"tag and digest", "FROM golang:1.27.1-alpine" + digest + " AS builder", 1, 0},
		{"registry with port", "FROM registry.example:5000/o/img:1.0" + digest, 1, 0},
		{"platform flag", "FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine" + digest + " AS b", 1, 0},
		{"tag only", "FROM golang:1.27.1-alpine AS builder", 1, 1},
		{"implicit latest", "FROM gcr.io/distroless/static-debian13", 1, 1},
		{"digest without tag", "from gcr.io/distroless/static-debian13" + digest, 1, 1},
		{"short digest", "FROM alpine:3.21@sha256:abcd", 1, 1},
		{"earlier stage", "FROM alpine:3.21" + digest + " AS base\nFROM base AS final", 1, 0},
		{"scratch", "FROM scratch AS assets-local", 0, 0},
		{"build arg", "FROM ${MISSKEY_ASSETS_IMAGE} AS assets-external\nFROM assets-${ASSETS_SOURCE} AS assets", 0, 0},
		{"comment", "# FROM golang:1.27-alpine", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Len(t, externalBaseImages(tt.body), tt.external)
			require.Len(t, unpinnedBaseImages(tt.body), tt.bad)
		})
	}
}
