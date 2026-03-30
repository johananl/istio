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

	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/ambient"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/util/retry"
)

// l7AuthzPolicySidecar is an L7 AuthorizationPolicy using a workload selector,
// as would be used in sidecar mode. It allows only GET on /allowed.
const l7AuthzPolicySidecar = `
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: l7-allow-get-sidecar
spec:
  selector:
    matchLabels:
      app: server
  action: ALLOW
  rules:
  - to:
    - operation:
        methods: ["GET"]
        paths: ["/allowed*"]
`

// TestL7AuthorizationPolicyMigration exercises the most dangerous migration path: an L7
// AuthorizationPolicy with a workload selector becomes a blanket DENY once sidecars are
// removed because ztunnel cannot enforce L7 rules. The test validates the documented
// migration path of switching from selector-based to targetRefs-based policy.
func TestL7AuthorizationPolicyMigration(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		const waypointName = "l7-migration-wp"

		// Step 1: Apply L7 AuthorizationPolicy with selector (sidecar-style).
		ctx.Log("Applying selector-based L7 AuthorizationPolicy")
		ctx.ConfigIstio().YAML(ns.Name(), l7AuthzPolicySidecar).ApplyOrFail(ctx)

		// Step 2: Verify enforcement under sidecar mode.
		ctx.Log("Verifying L7 policy enforcement under sidecar mode")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := client.Call(echo.CallOptions{
				To:   server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Method: "GET",
					Path:   "/allowed",
				},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(2*time.Second))

		// POST should be denied.
		client.CallOrFail(ctx, echo.CallOptions{
			To:   server,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "POST",
				Path:   "/allowed",
			},
			Count: 1,
			Check: check.Status(403),
		})
		ctx.Log("L7 policy enforced under sidecar: GET allowed, POST denied")

		// Step 3: Deploy waypoint and apply targetRefs-based L7 policy.
		ctx.Log("Deploying waypoint and applying targetRefs-based L7 policy")
		deployWaypoint(ctx, waypointName)
		waypointPolicy := fmt.Sprintf(l7AuthzPolicyWaypoint, waypointName)
		ctx.ConfigIstio().YAML(ns.Name(), waypointPolicy).ApplyOrFail(ctx)

		// Step 4: Activate waypoint for the namespace.
		ambient.SetWaypointForNamespace(ctx, ns, waypointName)

		// Register cleanup AFTER SetWaypointForNamespace. Cleanups run in LIFO
		// order, so this executes before SetWaypointForNamespace's internal cleanup
		// that restores pre-waypoint namespace labels (which lack the ambient label
		// added by migrateNSToAmbient below).
		ctx.Cleanup(func() {
			ambient.DeleteWaypoint(ctx, ns, waypointName)
			resetToSidecarMode(ctx)
		})

		// Step 5: Delete the old selector-based L7 policy before migrating.
		// In ambient mode ztunnel cannot evaluate L7 ALLOW rules and applies a
		// blanket DENY, so the selector-based policy must be removed first.
		ctx.Log("Deleting old selector-based L7 policy before ambient migration")
		ctx.ConfigIstio().YAML(ns.Name(), l7AuthzPolicySidecar).DeleteOrFail(ctx)

		// Step 6: Enable ambient mode, remove sidecar injection, restart server.
		migrateNSToAmbient(ctx)
		restartWorkloads(ctx, server, client)

		ctx.Log("Waiting for ambient connectivity through waypoint")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := client.Call(echo.CallOptions{
				To:   server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Method: "GET",
					Path:   "/allowed",
				},
				Count: 1,
				Check: check.And(check.OK(), isL7()),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(2*time.Second))

		// Step 7: Verify enforcement via waypoint.
		ctx.Log("Verifying L7 policy enforcement via waypoint")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := client.Call(echo.CallOptions{
				To:   server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Method: "GET",
					Path:   "/allowed",
				},
				Count: 5,
				Check: check.And(check.OK(), isL7()),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(2*time.Second))

		// POST should still be denied by waypoint.
		client.CallOrFail(ctx, echo.CallOptions{
			To:   server,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "POST",
				Path:   "/allowed",
			},
			Count: 1,
			Check: check.Status(403),
		})
		ctx.Log("Waypoint L7 policy enforced: GET allowed, POST denied")

		// Step 8: Verify no ztunnel blanket DENY (GET on an unrestricted path
		// that the ALLOW rule covers should succeed).
		client.CallOrFail(ctx, echo.CallOptions{
			To:   server,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "GET",
				Path:   "/allowed/subpath",
			},
			Count: 1,
			Check: check.And(check.OK(), isL7()),
		})
		ctx.Log("No ztunnel blanket DENY — migrated policy works correctly")
	})
}

// TestL4AuthorizationPolicySurvivesMigration validates the "no change needed" claim for
// L4-only policies during sidecar-to-ambient migration.
func TestL4AuthorizationPolicySurvivesMigration(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ctx.Cleanup(func() { resetToSidecarMode(ctx) })

		// Step 1: Apply L4 AuthorizationPolicy allowing only client's SA.
		// The echo framework uses the service name as the SA when ServiceAccount is
		// not explicitly set — for the default SA we use "default".
		policy := fmt.Sprintf(l4AuthzPolicy, ns.Name(), "default")
		ctx.Log("Applying L4 AuthorizationPolicy allowing client SA")
		ctx.ConfigIstio().YAML(ns.Name(), policy).ApplyOrFail(ctx)

		// Step 2: Verify enforcement under sidecar mode.
		ctx.Log("Verifying L4 policy enforcement under sidecar mode")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := client.Call(echo.CallOptions{
				To:    server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(2*time.Second))

		// crossClient (different namespace, different SA) should be denied.
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := crossClient.Call(echo.CallOptions{
				To:    server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.NotOK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(2*time.Second))
		ctx.Log("L4 policy enforced under sidecar: client allowed, crossClient denied")

		// Step 3: Migrate namespace to ambient (no waypoint needed for L4).
		migrateNSToAmbient(ctx)

		// Step 4: Restart server + client.
		restartWorkloads(ctx, server, client)

		// Step 5: Verify same L4 policy still enforced by ztunnel.
		ctx.Log("Verifying L4 policy enforcement under ambient mode")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := client.Call(echo.CallOptions{
				To:    server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(2*time.Second))

		// crossClient should still be denied after migration.
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := crossClient.Call(echo.CallOptions{
				To:    server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.NotOK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(2*time.Second))
		ctx.Log("L4 policy survives migration: client allowed, crossClient denied by ztunnel")
	})
}

// allowOnlyWaypointPolicy is an ALLOW policy that permits only traffic whose
// source principal matches the waypoint's service account. Sidecar traffic
// bypasses the waypoint, so it presents its own (non-waypoint) principal and
// is implicitly denied. We use ALLOW+principals instead of DENY+notPrincipals
// because ztunnel may not extract the peer identity on the inbound passthrough
// path (non-HBONE traffic from sidecars), making notPrincipals unevaluable;
// with an ALLOW policy, unrecognised principals simply have no matching rule
// and are denied by default.
// The first %s is the namespace and the second %s is the waypoint service
// account name.
const allowOnlyWaypointPolicy = `
apiVersion: security.istio.io/v1
kind: AuthorizationPolicy
metadata:
  name: allow-only-waypoint
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

// TestWaypointBypassPrevention validates the recommended ALLOW-only-waypoint security
// hardening pattern that prevents sidecar clients from bypassing the waypoint.
func TestWaypointBypassPrevention(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		const waypointName = "bypass-prevention-wp"

		// Step 1: Migrate ns to ambient with a waypoint activated.
		deployWaypoint(ctx, waypointName)
		ambient.SetWaypointForNamespace(ctx, ns, waypointName)

		// Register cleanup AFTER SetWaypointForNamespace so LIFO ordering
		// ensures we reset the namespace before the framework restores
		// pre-waypoint labels.
		ctx.Cleanup(func() {
			ambient.DeleteWaypoint(ctx, ns, waypointName)
			resetToSidecarMode(ctx)
		})
		migrateNSToAmbient(ctx)
		restartWorkloads(ctx, server, client)

		ctx.Log("Waiting for ambient connectivity through waypoint")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := client.Call(echo.CallOptions{
				To:    server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.And(check.OK(), isL7()),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(2*time.Second))

		// Step 2: Apply ALLOW policy permitting only the waypoint SA.
		policy := fmt.Sprintf(allowOnlyWaypointPolicy, ns.Name(), waypointName)
		ctx.Log("Applying waypoint bypass prevention ALLOW policy")
		ctx.ConfigIstio().YAML(ns.Name(), policy).ApplyOrFail(ctx)

		// Step 3: client (ambient, same namespace) calls server — succeeds
		// (traffic goes through the waypoint, so the source principal is the
		// waypoint SA which matches the ALLOW rule).
		ctx.Log("Verifying ambient client can reach server through waypoint")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := client.Call(echo.CallOptions{
				To:    server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.And(check.OK(), isL7()),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(2*time.Second))
		ctx.Log("Ambient client allowed (traffic through waypoint)")

		// Step 4: crossClient (sidecar, different namespace) calls server — denied
		// because sidecar traffic bypasses the waypoint and its principal (or
		// lack thereof on the passthrough path) does not match the ALLOW rule.
		ctx.Log("Verifying sidecar crossClient is denied (bypasses waypoint)")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := crossClient.Call(echo.CallOptions{
				To:    server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.NotOK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(2*time.Second))
		ctx.Log("Sidecar crossClient denied — waypoint bypass prevention works")
	})
}
