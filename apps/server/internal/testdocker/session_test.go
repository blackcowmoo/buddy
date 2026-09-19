package testdocker

import (
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
)

func TestProcessSessionSurvivesDefaultLabelsAndSeparatesCleanup(t *testing.T) {
	makeConfig := func(pid int) *container.Config {
		t.Helper()
		req := testcontainers.GenericContainerRequest{ContainerRequest: testcontainers.ContainerRequest{ConfigModifier: func(c *container.Config) { c.Hostname = "preserved" }}}
		if err := processSession("same-go-test-run", pid).Customize(&req); err != nil {
			t.Fatal(err)
		}
		// The provider assigns generic labels after options, before ConfigModifier.
		cfg := &container.Config{Labels: testcontainers.GenericLabels()}
		req.ConfigModifier(cfg)
		if cfg.Hostname != "preserved" {
			t.Fatal("lost existing config modifier")
		}
		if len(req.LifecycleHooks) != 1 || len(req.LifecycleHooks[0].PreCreates) != 1 {
			t.Fatal("private reaper must initialize before container creation")
		}
		return cfg
	}
	first, second, other := makeConfig(100), makeConfig(100), makeConfig(200)
	if first.Labels[sessionLabel] != "same-go-test-run-100" || first.Labels[sessionLabel] != second.Labels[sessionLabel] {
		t.Fatal("same-process cleanup label not preserved")
	}
	if first.Labels[sessionLabel] == other.Labels[sessionLabel] {
		t.Fatal("parallel packages can reap each other's active containers")
	}
	req := testcontainers.GenericContainerRequest{}
	if err := WithProcessSession().Customize(&req); err != nil || req.ConfigModifier == nil {
		t.Fatalf("runtime session: %v", err)
	}
}
