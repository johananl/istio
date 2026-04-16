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
	"time"

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
	"istio.io/istio/pkg/test/util/retry"
	"istio.io/istio/tests/integration/security/util/cert"
)

var (
	ist         istio.Instance
	ns          namespace.Instance
	nsSidecar   namespace.Instance
	client      echo.Instance
	server      echo.Instance
	serverV1    echo.Instance
	serverV2    echo.Instance
	crossClient echo.Instance
)

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

// TestMain sets up Istio with both sidecar injection and ambient mode (CNI + ztunnel)
// deployed in parallel, then creates a namespace with sidecar injection enabled.
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
		Setup(func(ctx resource.Context) error {
			var err error
			ns, err = namespace.New(ctx, namespace.Config{
				Prefix: "sidecar-to-ambient",
				Inject: true,
			})
			if err != nil {
				return err
			}
			nsSidecar, err = namespace.New(ctx, namespace.Config{
				Prefix: "sidecar-cross",
				Inject: true,
			})
			return err
		}).
		Setup(setupEcho).
		Run()
}

func setupEcho(ctx resource.Context) error {
	_, err := deployment.New(ctx).
		With(&client, echo.Config{
			Service:   "client",
			Namespace: ns,
			Ports:     []echo.Port{},
		}).
		With(&server, echo.Config{
			Service:   "server",
			Namespace: ns,
			Ports: []echo.Port{
				{
					Name:         "http",
					Protocol:     protocol.HTTP,
					WorkloadPort: 8090,
				},
			},
			Subsets: []echo.SubsetConfig{
				{Version: "v1"},
				{Version: "v2"},
			},
		}).
		With(&serverV1, echo.Config{
			Service:   "server-v1",
			Namespace: ns,
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
		With(&serverV2, echo.Config{
			Service:   "server-v2",
			Namespace: ns,
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
		}).
		With(&crossClient, echo.Config{
			Service:   "cross-client",
			Namespace: nsSidecar,
			Ports:     []echo.Port{},
		}).
		Build()
	return err
}

const (
	// l7AuthzPolicyWaypoint is the equivalent L7 policy using targetRefs to
	// attach to a waypoint Gateway, as required after migration to ambient.
	// The %s placeholder is replaced with the waypoint Gateway name.
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

	// l4AuthzPolicy is an L4 AuthorizationPolicy using a selector. Only the
	// specified service account principal is allowed. The first %s is the
	// namespace and the second %s is the service account name.
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

// resetToSidecarMode restores the shared namespace and workloads to sidecar injection mode so that
// a subsequent sub-test starts from a clean state.
func resetToSidecarMode(ctx framework.TestContext) {
	ctx.Log("Resetting namespace to sidecar injection mode")
	if err := ns.RemoveLabel(label.IoIstioDataplaneMode.Name); err != nil {
		ctx.Logf("removing ambient label (may not exist yet): %v", err)
	}
	if err := ns.SetLabel("istio-injection", "enabled"); err != nil {
		ctx.Fatalf("re-enabling sidecar injection: %v", err)
	}

	ctx.Log("Restarting workloads to re-inject sidecars")
	restartWorkloadsConcurrent(ctx, server, serverV1, serverV2, client)

	// Verify sidecars are back.
	retry.UntilSuccessOrFail(ctx, func() error {
		_, err := client.Call(echo.CallOptions{
			To:    server,
			Port:  echo.Port{Name: "http"},
			Count: 1,
			Check: check.OK(),
		})
		return err
	}, retry.Timeout(2*time.Minute), retry.Delay(time.Second))
	ctx.Log("Namespace reset to sidecar mode")
}

// serverPodByVersion returns the pod name of the server workload matching the given version
// (e.g. "v1" or "v2"). It searches the server echo instance's workloads.
func serverPodByVersion(ctx framework.TestContext, version string) string {
	prefix := "server-" + version + "-"
	for _, w := range server.WorkloadsOrFail(ctx) {
		if strings.HasPrefix(w.PodName(), prefix) {
			return w.PodName()
		}
	}
	ctx.Fatalf("no server workload found for version %s", version)
	return ""
}

// deployWaypoint deploys a service-traffic waypoint proxy into ns and waits for it to be ready.
// It is intended to be called mid-test (not in TestMain) since waypoints are only needed after
// migration begins.
func deployWaypoint(ctx framework.TestContext, waypointName string) ambient.Waypoints {
	ctx.Helper()
	ctx.Logf("Deploying waypoint %q in namespace %s", waypointName, ns.Name())
	wps, err := ambient.NewWaypointProxyWithTrafficType(ctx, ns, waypointName, constants.ServiceTraffic)
	if err != nil {
		ctx.Fatalf("failed to deploy waypoint %q: %v", waypointName, err)
	}
	return wps
}

// migrateNSToAmbient switches the shared namespace from sidecar injection to
// ambient dataplane mode (labels only). The caller is responsible for
// restarting whichever workloads need the change.
func migrateNSToAmbient(ctx framework.TestContext) {
	ctx.Helper()
	ctx.Log("Migrating namespace to ambient mode")
	if err := ns.RemoveLabel("istio-injection"); err != nil {
		ctx.Fatalf("failed to remove sidecar injection label: %v", err)
	}
	if err := ns.SetLabel(label.IoIstioDataplaneMode.Name, "ambient"); err != nil {
		ctx.Fatalf("failed to set ambient dataplane mode: %v", err)
	}
}

// restartWorkloads restarts the given echo instances sequentially and waits for
// each to come back.
func restartWorkloads(ctx framework.TestContext, instances ...echo.Instance) {
	ctx.Helper()
	for _, inst := range instances {
		ctx.Logf("Restarting %s", inst.Config().Service)
		if err := inst.Restart(); err != nil {
			ctx.Fatalf("failed to restart %s: %v", inst.Config().Service, err)
		}
	}
}

// restartWorkloadsConcurrent restarts the given echo instances concurrently and
// waits for all of them to come back. This is significantly faster than
// restartWorkloads when multiple independent instances need to be restarted.
func restartWorkloadsConcurrent(ctx framework.TestContext, instances ...echo.Instance) {
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

// isL7 returns a checker that verifies responses were processed by an L7
// proxy (waypoint) by looking for the X-Request-Id header.
func isL7() echo.Checker {
	return check.Each(func(r echoClient.Response) error {
		if _, ok := r.RequestHeaders[http.CanonicalHeaderKey("X-Request-Id")]; !ok {
			return fmt.Errorf("X-Request-Id not set — traffic did not traverse L7 waypoint")
		}
		return nil
	})
}
