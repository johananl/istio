//go:build integ

// Copyright Istio Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package migration

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"istio.io/api/label"
	"istio.io/istio/pkg/config/constants"
	"istio.io/istio/pkg/config/protocol"
	echoClient "istio.io/istio/pkg/test/echo"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/ambient"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/framework/components/echo/deployment"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/tests/integration/security/util/cert"
)

var ist istio.Instance

// testEnv holds per-test isolated namespaces and echo deployments, preventing cross-test
// interference and enabling parallel execution.
type testEnv struct {
	ns            namespace.Instance
	nsSidecar     namespace.Instance // Nil unless withSidecarClient is passed to newTestEnv.
	client        echo.Instance
	server        echo.Instance
	serverV1      echo.Instance // Nil unless withVersionedServers is passed to newTestEnv.
	serverV2      echo.Instance // Nil unless withVersionedServers is passed to newTestEnv.
	sidecarClient echo.Instance // Nil unless withSidecarClient is passed to newTestEnv.
}

// testEnvOption configures optional deployments in newTestEnv.
type testEnvOption func(*testEnvConfig)

type testEnvConfig struct {
	sidecarClient   bool
	versionedServer bool
}

// withSidecarClient deploys a sidecar-injected client in a separate namespace.
func withSidecarClient() testEnvOption {
	return func(c *testEnvConfig) { c.sidecarClient = true }
}

// withVersionedServers deploys server-v1 and server-v2 single-subset echo instances.
func withVersionedServers() testEnvOption {
	return func(c *testEnvConfig) { c.versionedServer = true }
}

// newTestEnv creates isolated namespaces and echo deployments for a single test. Resources are
// automatically cleaned up when the test context completes.
func newTestEnv(ctx framework.TestContext, opts ...testEnvOption) *testEnv {
	ctx.Helper()
	var cfg testEnvConfig
	for _, o := range opts {
		o(&cfg)
	}

	env := &testEnv{}
	var err error
	env.ns, err = namespace.New(ctx, namespace.Config{
		Prefix: "sidecar-to-ambient",
		Inject: true,
	})
	if err != nil {
		ctx.Fatal(err)
	}

	serverCfg := echo.Config{
		Service:   "server",
		Namespace: env.ns,
		Ports: []echo.Port{
			{
				Name:         "http",
				Protocol:     protocol.HTTP,
				WorkloadPort: 8090,
			},
		},
	}
	if cfg.versionedServer {
		serverCfg.Subsets = []echo.SubsetConfig{
			{Version: "v1"},
			{Version: "v2"},
		}
	}

	builder := deployment.New(ctx).
		With(&env.client, echo.Config{
			Service:   "client",
			Namespace: env.ns,
			Ports:     []echo.Port{},
		}).
		With(&env.server, serverCfg)

	if cfg.versionedServer {
		builder = builder.
			With(&env.serverV1, echo.Config{
				Service:   "server-v1",
				Namespace: env.ns,
				Ports: []echo.Port{
					{
						Name:         "http",
						Protocol:     protocol.HTTP,
						WorkloadPort: 8090,
					},
				},
				Subsets: []echo.SubsetConfig{
					{
						Version: "v1",
						Labels: map[string]string{
							"app":     "server-v1",
							"version": "v1",
						},
					},
				},
			}).
			With(&env.serverV2, echo.Config{
				Service:   "server-v2",
				Namespace: env.ns,
				Ports: []echo.Port{
					{
						Name:         "http",
						Protocol:     protocol.HTTP,
						WorkloadPort: 8090,
					},
				},
				Subsets: []echo.SubsetConfig{
					{
						Version: "v2",
						Labels: map[string]string{
							"app":     "server-v2",
							"version": "v2",
						},
					},
				},
			})
	}

	if cfg.sidecarClient {
		env.nsSidecar, err = namespace.New(ctx, namespace.Config{
			Prefix: "sidecar-client",
			Inject: true,
		})
		if err != nil {
			ctx.Fatal(err)
		}
		builder = builder.With(&env.sidecarClient, echo.Config{
			Service:   "sidecar-client",
			Namespace: env.nsSidecar,
			Ports:     []echo.Port{},
		})
	}

	if _, err := builder.Build(); err != nil {
		ctx.Fatal(err)
	}
	return env
}

const ambientControlPlaneValues = `
values:
  cni:
    repair:
      enabled: false
  ztunnel:
    terminationGracePeriodSeconds: 5
    env:
      SECRET_TTL: 5m
`

// TestMain sets up Istio with both sidecar injection and ambient mode (CNI + ztunnel).
func TestMain(m *testing.M) {
	framework.
		NewSuite(m).
		RequireMinVersion(24).
		Setup(func(t resource.Context) error {
			t.Settings().Ambient = true
			return nil
		}).
		Setup(istio.Setup(&ist, func(_ resource.Context, cfg *istio.Config) {
			cfg.EnableCNI = true
			cfg.DeployEastWestGW = false
			cfg.ControlPlaneValues = ambientControlPlaneValues
		}, cert.CreateCASecretAlt)).
		Run()
}

const (
	// l7AuthzPolicyWaypoint is the equivalent L7 policy using targetRefs to attach to a waypoint
	// Gateway, as required after migration to ambient. The %s placeholder is replaced with the
	// waypoint Gateway name.
	l7AuthzPolicyWaypoint = `
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: l7-allow-get-waypoint
spec:
  targetRefs:
  - kind: Gateway
    group: gateway.networking.k8s.io
    name: %s
  action: ALLOW
  rules:
  - to:
    - operation:
        methods: ["GET"]
        paths: ["/allowed*"]
`

	// l4AuthzPolicy is an L4 AuthorizationPolicy using a selector. Only the specified service
	// account principal is allowed. The first %s is the namespace and the second %s is the service
	// account name.
	l4AuthzPolicy = `
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: l4-allow-client
spec:
  selector:
    matchLabels:
      app: server
  action: ALLOW
  rules:
  - from:
    - source:
        principals: ["cluster.local/ns/%s/sa/%s"]
`
)

// serverPodByVersion returns the pod name of the server workload matching the given version (e.g.
// "v1" or "v2"). It searches the given server echo instance's workloads.
func serverPodByVersion(ctx framework.TestContext, srv echo.Instance, version string) string {
	prefix := "server-" + version + "-"
	for _, w := range srv.WorkloadsOrFail(ctx) {
		if strings.HasPrefix(w.PodName(), prefix) {
			return w.PodName()
		}
	}
	ctx.Fatalf("no server workload found for version %s", version)
	return ""
}

// deployWaypoint deploys a service-traffic waypoint proxy into the given namespace and waits for
// it to be ready.
func deployWaypoint(ctx framework.TestContext, targetNS namespace.Instance, waypointName string) ambient.Waypoints {
	ctx.Helper()
	ctx.Logf("Deploying waypoint %q in namespace %s", waypointName, targetNS.Name())
	wps, err := ambient.NewWaypointProxyWithTrafficType(ctx, targetNS, waypointName, constants.ServiceTraffic)
	if err != nil {
		ctx.Fatalf("failed to deploy waypoint %q: %v", waypointName, err)
	}
	return wps
}

// migrateNSToAmbient switches the given namespace from sidecar injection to ambient dataplane mode
// (labels only). The caller is responsible for restarting whichever workloads need the change.
func migrateNSToAmbient(ctx framework.TestContext, targetNS namespace.Instance) {
	ctx.Helper()
	ctx.Log("Migrating namespace to ambient mode")
	if err := targetNS.RemoveLabel("istio-injection"); err != nil {
		ctx.Fatalf("failed to remove sidecar injection label: %v", err)
	}
	if err := targetNS.SetLabel(label.IoIstioDataplaneMode.Name, "ambient"); err != nil {
		ctx.Fatalf("failed to set ambient dataplane mode: %v", err)
	}
}

// restartWorkloads restarts the given echo instances concurrently and waits for all of them to
// come back.
func restartWorkloads(ctx framework.TestContext, instances ...echo.Instance) {
	ctx.Helper()
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for _, inst := range instances {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := inst.Restart(); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("restarting %s: %w", inst.Config().Service, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		ctx.Fatalf("failed to restart workloads: %v", errors.Join(errs...))
	}
}

// isL7 returns a checker that verifies responses were processed by an L7 proxy (waypoint) by
// looking for the X-Request-Id header.
func isL7() echo.Checker {
	return check.Each(func(r echoClient.Response) error {
		if _, ok := r.RequestHeaders[http.CanonicalHeaderKey("X-Request-Id")]; !ok {
			return fmt.Errorf("X-Request-Id not set — traffic did not traverse L7 waypoint")
		}
		return nil
	})
}
