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
	"fmt"
	"testing"
	"time"

	"istio.io/istio/pkg/http/headers"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/ambient"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/util/retry"
)

const (
	// headerRoutingVS creates a VirtualService + DestinationRule for header-based
	// routing. Requests with "end-user: jason" go to server-v2; all others to server-v1.
	// The %s placeholder is the namespace.
	headerRoutingVS = `
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: server-versions
spec:
  host: server.%s.svc.cluster.local
  trafficPolicy:
    connectionPool:
      http:
        h2UpgradePolicy: DO_NOT_UPGRADE
  subsets:
  - name: v1
    labels:
      version: v1
  - name: v2
    labels:
      version: v2
---
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: server-header-routing
spec:
  hosts:
  - server.%s.svc.cluster.local
  http:
  - match:
    - headers:
        end-user:
          exact: jason
    route:
    - destination:
        host: server.%s.svc.cluster.local
        subset: v2
  - route:
    - destination:
        host: server.%s.svc.cluster.local
        subset: v1
`

	// headerRoutingHTTPRoute is the Gateway API equivalent of headerRoutingVS.
	// The first %s is the server service name, %d is the service port,
	// remaining %s are the backend service names.
	headerRoutingHTTPRoute = `
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: server-header-routing
spec:
  parentRefs:
  - name: %s
    kind: Service
    group: ""
  rules:
  - matches:
    - headers:
      - name: end-user
        value: jason
    backendRefs:
    - name: server-v2
      port: %d
  - backendRefs:
    - name: server-v1
      port: %d
`
)

// TestHTTPRouteThroughWaypoint validates the VirtualService -> HTTPRoute migration path.
// It starts with sidecar-mode header-based routing via VirtualService + DestinationRule,
// then migrates to ambient mode with an equivalent HTTPRoute attached to a waypoint.
func TestHTTPRouteThroughWaypoint(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		env := newTestEnv(ctx, withVersionedServers())

		const waypointName = "routing-wp"

		httpPort := env.server.Config().Ports.MustForName("http")

		// Step 1: Apply VirtualService + DestinationRule for header-based routing.
		// Requests with "end-user: jason" -> server-v2, default -> server-v1.
		vsCfg := fmt.Sprintf(headerRoutingVS, env.ns.Name(), env.ns.Name(), env.ns.Name(), env.ns.Name())
		ctx.Log("Applying VirtualService + DestinationRule for header-based routing")
		ctx.ConfigIstio().YAML(env.ns.Name(), vsCfg).ApplyOrFail(ctx)

		// Step 2: Verify header-based routing works under sidecar mode.
		ctx.Log("Verifying header-based routing under sidecar mode")

		// Default traffic -> server-v1.
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.And(check.OK(), check.Hostname(serverPodByVersion(ctx, env.server, "v1"))),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))

		// Header "end-user: jason" -> server-v2.
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:   env.server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Headers: headers.New().With("end-user", "jason").Build(),
				},
				Count: 1,
				Check: check.And(check.OK(), check.Hostname(serverPodByVersion(ctx, env.server, "v2"))),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("Header-based routing works under sidecar mode")

		// Step 3: Deploy waypoint; create HTTPRoute with parentRefs to Service.
		ctx.Log("Deploying waypoint and creating HTTPRoute")
		deployWaypoint(ctx, env.ns, waypointName)
		httpRouteCfg := fmt.Sprintf(headerRoutingHTTPRoute, "server", httpPort.ServicePort, httpPort.ServicePort)
		ctx.ConfigIstio().YAML(env.ns.Name(), httpRouteCfg).ApplyOrFail(ctx)

		// Step 4: Delete the VirtualService + DestinationRule.
		ctx.Log("Deleting VirtualService + DestinationRule")
		ctx.ConfigIstio().YAML(env.ns.Name(), vsCfg).DeleteOrFail(ctx)

		// Step 5: Activate waypoint, enable ambient, remove injection, restart pods.
		ambient.SetWaypointForNamespace(ctx, env.ns, waypointName)
		migrateNSToAmbient(ctx, env.ns)
		restartWorkloads(ctx, env.server, env.serverV1, env.serverV2, env.client)

		// Wait for ambient connectivity.
		ctx.Log("Waiting for ambient connectivity through waypoint")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.And(check.OK(), isL7()),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(time.Second))

		// Step 6: Verify header-based routing works through the waypoint.
		ctx.Log("Verifying header-based routing through waypoint")

		// Default traffic -> server-v1.
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.And(check.OK(), isL7(), check.Hostname(env.serverV1.WorkloadsOrFail(ctx)[0].PodName())),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))

		// Header "end-user: jason" -> server-v2.
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:   env.server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Headers: headers.New().With("end-user", "jason").Build(),
				},
				Count: 1,
				Check: check.And(check.OK(), isL7(), check.Hostname(env.serverV2.WorkloadsOrFail(ctx)[0].PodName())),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("Header-based routing works through waypoint — HTTPRoute migration successful")
	})
}
