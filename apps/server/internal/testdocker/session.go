// Package testdocker isolates integration-test container lifetimes by process.
package testdocker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
)

const sessionLabel = "org.testcontainers.sessionId"

var reaperOnce sync.Once
var reaperErr error

// WithProcessSession avoids the shared-reaper startup race in testcontainers-go
// v0.43.0: reused reapers can accept TCP before they can complete a handshake,
// leaving a package's database subject to cleanup when another package exits.
func WithProcessSession() testcontainers.ContainerCustomizer {
	return processSession(testcontainers.SessionID(), os.Getpid())
}
func processSession(runID string, pid int) testcontainers.ContainerCustomizer {
	session := runID + "-" + strconv.Itoa(pid)
	return testcontainers.CustomizeRequestOption(func(req *testcontainers.GenericContainerRequest) error {
		previous := req.ConfigModifier
		req.ConfigModifier = func(cfg *container.Config) {
			if previous != nil {
				previous(cfg)
			}
			if cfg.Labels == nil {
				cfg.Labels = map[string]string{}
			}
			// CreateContainer overwrites ordinary WithLabels before this hook. Set
			// the final Docker label here so it matches our private reaper's filter.
			cfg.Labels[sessionLabel] = session
		}
		req.LifecycleHooks = append(req.LifecycleHooks, testcontainers.ContainerLifecycleHooks{PreCreates: []testcontainers.ContainerRequestHook{
			func(ctx context.Context, _ testcontainers.ContainerRequest) error {
				reaperOnce.Do(func() {
					provider, err := testcontainers.NewDockerProvider()
					if err != nil {
						reaperErr = err
						return
					}
					defer provider.Close()
					if provider.Config().RyukDisabled {
						return
					}
					// Initialize the process-local reaper before CreateContainer's normal
					// connection step. The library then reuses this ready private instance.
					reaper, err := testcontainers.NewReaper(ctx, session, provider, "")
					if err != nil {
						reaperErr = err
						return
					}
					if reaper.SessionID != session {
						reaperErr = fmt.Errorf("test container reaper already belongs to another session")
					}
				})
				return reaperErr
			},
		}})
		return nil
	})
}
