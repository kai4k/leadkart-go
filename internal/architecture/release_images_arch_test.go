// release_images_arch_test.go — a deploy must never be unpullable: every
// LeadKart service image docker/compose.prod.yml pulls has to be built and
// published by .github/workflows/release.yml, from a target that exists in
// docker/Dockerfile. The final review found the worker service in the compose
// file while the release matrix still built only api + migrate.
//
// Both files are parsed (YAML), not grepped; the comparison is a pure function
// with a self-test, so the gate itself cannot rot into always-green.
package architecture_test

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeImageRepoPrefix is the repository-name prefix every LeadKart image
// carries (ghcr.io/<org>/leadkart-go-<target>).
const composeImageRepoPrefix = "leadkart-go-"

// composeImageRef matches the default of ${VAR:-ghcr.io/org/leadkart-go-api:latest}
// or a literal reference, capturing the target name.
var composeImageRef = regexp.MustCompile(`leadkart-go-([a-z0-9]+)(?::[A-Za-z0-9_.-]{1,128})?\}?$`)

// composeLeadkartTargets returns the Dockerfile targets of every LeadKart
// image referenced by the compose document, keyed by service name.
func composeLeadkartTargets(t *testing.T, composeYAML []byte) map[string]string {
	t.Helper()
	var doc struct {
		Services map[string]struct {
			Image string `yaml:"image"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(composeYAML, &doc); err != nil {
		t.Fatalf("parse compose: %v", err)
	}
	out := map[string]string{}
	for name, svc := range doc.Services {
		if !strings.Contains(svc.Image, composeImageRepoPrefix) {
			continue // postgres, redis: third-party images
		}
		m := composeImageRef.FindStringSubmatch(strings.TrimSpace(svc.Image))
		if m == nil {
			t.Fatalf("compose service %q image %q: cannot derive the %s<target> name", name, svc.Image, composeImageRepoPrefix)
		}
		out[name] = m[1]
	}
	return out
}

// releaseMatrixTargets returns the release workflow's build matrix targets.
func releaseMatrixTargets(t *testing.T, releaseYAML []byte) []string {
	t.Helper()
	var doc struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Target []string `yaml:"target"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(releaseYAML, &doc); err != nil {
		t.Fatalf("parse release workflow: %v", err)
	}
	var out []string
	for _, job := range doc.Jobs {
		out = append(out, job.Strategy.Matrix.Target...)
	}
	slices.Sort(out)
	return out
}

var dockerfileTargetRE = regexp.MustCompile(`(?mi)^FROM\s+\S+\s+AS\s+([a-z0-9_-]{1,64})\s*$`)

func dockerfileTargets(dockerfile []byte) []string {
	var out []string
	for _, m := range dockerfileTargetRE.FindAllStringSubmatch(string(dockerfile), -1) {
		out = append(out, m[1])
	}
	return out
}

// unreleasedComposeImages lists "service (target)" for every compose image
// whose target is not in the release matrix or not a Dockerfile target.
func unreleasedComposeImages(compose map[string]string, matrix, dockerTargets []string) []string {
	var bad []string
	for svc, target := range compose {
		if !slices.Contains(matrix, target) {
			bad = append(bad, svc+" ("+target+"): not built by release.yml")
		}
		if !slices.Contains(dockerTargets, target) {
			bad = append(bad, svc+" ("+target+"): no such target in docker/Dockerfile")
		}
	}
	slices.Sort(bad)
	return bad
}

// arch-test:no-negative-fixture (RED proof: TestMeta_ProdComposeImagesGateDetects feeds the comparison a compose file with an unreleased worker and an unknown target)
func TestArch_ProdComposeImagesAreReleased(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) []byte {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		return b
	}
	compose := composeLeadkartTargets(t, read("docker/compose.prod.yml"))
	matrix := releaseMatrixTargets(t, read(".github/workflows/release.yml"))
	targets := dockerfileTargets(read("docker/Dockerfile"))

	// Floor: the gate is meaningless if it stops seeing the services.
	for _, want := range []string{"api", "worker", "migrate"} {
		if !slices.Contains(slices.Collect(maps.Values(compose)), want) {
			t.Errorf("compose.prod.yml no longer references the %s image; the scan or the file changed shape", want)
		}
	}
	if bad := unreleasedComposeImages(compose, matrix, targets); len(bad) > 0 {
		t.Fatalf("compose.prod.yml pulls images the release does not publish:\n  %s", strings.Join(bad, "\n  "))
	}
}

func TestMeta_ProdComposeImagesGateDetects(t *testing.T) {
	const composeDoc = `
services:
  postgres: {image: "postgres:17-alpine"}
  api: {image: "${API_IMAGE:-ghcr.io/leadkart/leadkart-go-api:latest}"}
  worker: {image: "${WORKER_IMAGE:-ghcr.io/leadkart/leadkart-go-worker:latest}"}
  odd: {image: "ghcr.io/leadkart/leadkart-go-reports:1.2"}
`
	got := composeLeadkartTargets(t, []byte(composeDoc))
	want := map[string]string{"api": "api", "worker": "worker", "odd": "reports"}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("targets[%s] = %q, want %q", k, got[k], v)
		}
	}
	const releaseDoc = `
jobs:
  build-and-push:
    strategy:
      matrix:
        target: [api, migrate]
`
	matrix := releaseMatrixTargets(t, []byte(releaseDoc))
	docker := dockerfileTargets([]byte("FROM x AS build\nFROM y AS api\nFROM y AS worker\n"))
	bad := unreleasedComposeImages(got, matrix, docker)
	joined := strings.Join(bad, "|")
	for _, frag := range []string{"worker (worker): not built by release.yml", "odd (reports): no such target"} {
		if !strings.Contains(joined, frag) {
			t.Errorf("gate missed %q in %v", frag, bad)
		}
	}
	if strings.Contains(joined, "api (api)") {
		t.Errorf("gate flagged a released image: %v", bad)
	}
	if got := unreleasedComposeImages(map[string]string{"api": "api"}, []string{"api"}, []string{"api"}); len(got) != 0 {
		t.Errorf("clean input flagged: %v", got)
	}
}
