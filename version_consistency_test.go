// version_consistency_test.go is the single-source-of-truth version-consistency
// guard. VERSION (at the repo root, no leading "v") is the authoritative
// release version. Every other release-traceability surface must agree with it:
//
//   - cmd/agentq/main.go declares `var version = "<VERSION>"` (the default the
//     bare `go build` bakes in when goreleaser's -ldflags override is absent).
//   - web/site.json's content_version is "v<VERSION>" (the version the live
//     GitHub-Pages site advertises).
//   - web/site.json's meta.implementation_version, when the schema carries it
//     (the schema-3 site does), is "<VERSION>".
//   - CHANGELOG.md has a "## [<VERSION>]" entry (the release is documented).
//
// Past iterations shipped a git tag while VERSION, the CLI --version, the site
// content_version, and the CHANGELOG lagged behind by several releases; this
// test fails the build the moment any surface drifts, so a bump that touches
// only one surface cannot ship.
package agentq

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func TestVersionSurfacesAgree(t *testing.T) {
	// The test file lives at the module root, next to VERSION, web/, cmd/, and
	// CHANGELOG.md. Resolve the repo root from this file so the test works
	// regardless of the cwd `go test` was launched from.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine test file path")
	}
	repoRoot := filepath.Dir(thisFile)

	versionBytes, err := os.ReadFile(filepath.Join(repoRoot, "VERSION"))
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	version := strings.TrimSpace(string(versionBytes))
	if version == "" {
		t.Fatal("VERSION is empty")
	}

	// cmd/agentq/main.go must declare `var version = "<version>"`.
	mainGo, err := os.ReadFile(filepath.Join(repoRoot, "cmd", "agentq", "main.go"))
	if err != nil {
		t.Fatalf("read cmd/agentq/main.go: %v", err)
	}
	varVersionRe := regexp.MustCompile(`var version = "([^"]*)"`)
	m := varVersionRe.FindStringSubmatch(string(mainGo))
	if m == nil {
		t.Fatal(`cmd/agentq/main.go: no "var version = \"...\"" declaration found`)
	}
	if m[1] != version {
		t.Errorf("version drift: VERSION=%q but cmd/agentq/main.go var version=%q", version, m[1])
	}

	// web/site.json content_version must be "v<version>".
	siteJSON, err := os.ReadFile(filepath.Join(repoRoot, "web", "site.json"))
	if err != nil {
		t.Fatalf("read web/site.json: %v", err)
	}
	contentVersionRe := regexp.MustCompile(`"content_version":\s*"([^"]*)"`)
	m = contentVersionRe.FindStringSubmatch(string(siteJSON))
	if m == nil {
		t.Fatal("web/site.json: no content_version field found")
	}
	if want := "v" + version; m[1] != want {
		t.Errorf("version drift: VERSION=%q but web/site.json content_version=%q (want %q)", version, m[1], want)
	}

	// web/site.json meta.implementation_version, when the schema carries it,
	// must equal VERSION. (The schema-3 site has it; schema 1 did not, so a
	// missing field is not an error — only a stale value is.)
	implRe := regexp.MustCompile(`"implementation_version":\s*"([^"]*)"`)
	if mm := implRe.FindStringSubmatch(string(siteJSON)); mm != nil {
		if mm[1] != version {
			t.Errorf("version drift: VERSION=%q but web/site.json meta.implementation_version=%q", version, mm[1])
		}
	}

	// CHANGELOG.md must carry a "## [<version>]" entry for this release.
	changelog, err := os.ReadFile(filepath.Join(repoRoot, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	entry := "## [" + version + "]"
	if !strings.Contains(string(changelog), entry) {
		t.Errorf("version drift: CHANGELOG.md has no %q entry for VERSION=%q", entry, version)
	}
}
