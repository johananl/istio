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

// l7AuthzPolicySidecar is an L7 AuthorizationPolicy using a workload selector, as would be used in
// sidecar mode. It allows only GET on /allowed.
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

// TestL7AuthorizationPolicyMigration validates L7 AuthorizationPolicy behavior during a migration
// from sidecar to ambient. When both a selector-based L7 ALLOW policy (for sidecars) and a
// targetRefs-based L7 ALLOW policy (for the waypoint) coexist, migrating to ambient causes ztunnel
// to see the selector-based L7 rules it cannot evaluate and apply a blanket DENY, temporarily
// dropping ALL traffic — including authorized requests. This is currently an expected tradeoff: a
// brief availability disruption in exchange for zero enforcement gap.
//
// The test verifies the following properties of the recommended migration path:
//   - Enforcement is never lost: unauthorized traffic is denied at every step.
//   - The blanket DENY is observable after migration while the old sidecar policy still exists.
//   - Traffic recovers once the stale sidecar policy is deleted and only the waypoint-targeted
//     policy remains.
func TestL7AuthorizationPolicyMigration(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		env := newTestEnv(ctx)

		const waypointName = "l7-migration-wp"

		ctx.Log("Applying selector-based L7 AuthorizationPolicy")
		ctx.ConfigIstio().YAML(env.ns.Name(), l7AuthzPolicySidecar).ApplyOrFail(ctx)

		ctx.Log("Waiting for sidecar policy to allow GET /allowed")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:   env.server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Method: "GET",
					Path:   "/allowed",
				},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("Checking enforcement after applying sidecar policy")
		checkL7Enforcement(ctx, env.client, env.server)

		ctx.Log("Deploying waypoint and applying targetRefs-based L7 policy")
		deployWaypoint(ctx, env.ns, waypointName)
		waypointPolicy := fmt.Sprintf(l7AuthzPolicyWaypoint, waypointName)
		ctx.ConfigIstio().YAML(env.ns.Name(), waypointPolicy).ApplyOrFail(ctx)
		ctx.Log("Checking enforcement after deploying waypoint and applying new policy")
		checkL7Enforcement(ctx, env.client, env.server)

		ctx.Log("Activating waypoint for namespace")
		ambient.SetWaypointForNamespace(ctx, env.ns, waypointName)
		ctx.Log("Checking enforcement after activating waypoint")
		checkL7Enforcement(ctx, env.client, env.server)

		// Keep both policies during migration: Ztunnel will blanket-DENY since it can't evaluate
		// the L7 selector policy.
		ctx.Log("Migrating namespace to ambient and restarting workloads")
		migrateNSToAmbient(ctx, env.ns)
		restartWorkloads(ctx, env.server, env.client)

		// Verify the expected blanket DENY: Even authorized GET /allowed should be rejected
		// while the stale sidecar L7 policy exists. This confirms the traffic drop users will
		// observe during migration.
		ctx.Log("Verifying blanket DENY affects authorized traffic")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:   env.server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Method: "GET",
					Path:   "/allowed",
				},
				Count: 5,
				Check: check.NotOK(),
				Retry: echo.Retry{NoRetry: true},
			})
			return err
		}, retry.Timeout(15*time.Second), retry.Delay(time.Second))
		ctx.Log("Blanket DENY confirmed — authorized traffic is temporarily dropped")

		// Delete the old selector-based L7 policy. This clears the rule that ztunnel could not
		// evaluate, lifting the blanket DENY. Traffic should recover through the waypoint, which
		// enforces the targetRefs policy.
		ctx.Log("Deleting old selector-based L7 policy to lift blanket DENY")
		ctx.ConfigIstio().YAML(env.ns.Name(), l7AuthzPolicySidecar).DeleteOrFail(ctx)

		ctx.Log("Waiting for traffic to recover through waypoint")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:   env.server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Method: "GET",
					Path:   "/allowed",
				},
				Count: 1,
				Check: check.And(check.OK(), isL7()),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(time.Second))
		ctx.Log("Traffic recovered through waypoint")

		ctx.Log("Checking enforcement after deleting old sidecar policy")
		checkL7Enforcement(ctx, env.client, env.server)

		ctx.Log("Verifying L7 policy enforcement via waypoint")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:   env.server,
				Port: echo.Port{Name: "http"},
				HTTP: echo.HTTP{
					Method: "GET",
					Path:   "/allowed",
				},
				Count: 5,
				Check: check.And(check.OK(), isL7()),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))

		// Verify no residual blanket DENY (GET on a subpath covered by the ALLOW rule should
		// succeed).
		env.client.CallOrFail(ctx, echo.CallOptions{
			To:   env.server,
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

// checkL7Enforcement verifies that POST /allowed is denied, catching any enforcement gap
// introduced by a migration step. It retries briefly to allow config propagation, then asserts
// that all requests are rejected.
func checkL7Enforcement(ctx framework.TestContext, from echo.Instance, to echo.Instance) {
	ctx.Helper()
	retry.UntilSuccessOrFail(ctx, func() error {
		_, err := from.Call(echo.CallOptions{
			To:   to,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "POST",
				Path:   "/allowed",
			},
			Count: 5,
			Check: check.NotOK(),
			Retry: echo.Retry{NoRetry: true},
		})
		return err
	}, retry.Timeout(15*time.Second), retry.Delay(time.Second))
}

// TestL4AuthorizationPolicySurvivesMigration verifies that an L4-only AuthorizationPolicy
// continues to work without modification when a namespace is migrated from sidecar mode to ambient
// mode. Because ztunnel can enforce L4 rules natively, no waypoint is required and the policy
// needs no changes.
func TestL4AuthorizationPolicySurvivesMigration(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		env := newTestEnv(ctx, withSidecarClient())

		// Apply an L4 AuthorizationPolicy that allows only the client's service account. The
		// client echo instance does not set ServiceAccount, so its pod runs under the "default"
		// service account.
		policy := fmt.Sprintf(l4AuthzPolicy, env.ns.Name(), "default")
		ctx.Log("Applying L4 AuthorizationPolicy allowing client SA")
		ctx.ConfigIstio().YAML(env.ns.Name(), policy).ApplyOrFail(ctx)

		// Verify enforcement under sidecar mode.
		ctx.Log("Verifying L4 policy enforcement under sidecar mode")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		// Sidecar client (different namespace, different SA) should be denied.
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.sidecarClient.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.NotOK(),
				Retry: echo.Retry{NoRetry: true},
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("L4 policy enforced under sidecar: client allowed, sidecar client denied")

		// Migrate namespace to ambient (no waypoint needed for L4).
		migrateNSToAmbient(ctx, env.ns)
		restartWorkloads(ctx, env.server, env.client)

		ctx.Log("Verifying L4 policy enforcement under ambient mode")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(time.Second))
		// Sidecar client should still be denied after migration.
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.sidecarClient.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.NotOK(),
				Retry: echo.Retry{NoRetry: true},
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("L4 policy survives migration: client allowed, sidecar client denied by ztunnel")
	})
}

// allowOnlyWaypointPolicy is an ALLOW policy that permits only traffic from the waypoint's service
// account. Sidecar traffic bypasses the waypoint and is implicitly denied. We use ALLOW+principals
// rather than DENY+notPrincipals because ztunnel does not extract the peer identity on the inbound
// passthrough path (non-HBONE sidecar traffic), so a DENY+notPrincipals rule would be skipped and
// traffic would be allowed. With ALLOW+principals, an unknown identity matches no rule and is
// denied by default. The first %s is the namespace and the second %s is the waypoint service
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

// TestWaypointBypassPrevention validates the recommended ALLOW-only-waypoint security hardening
// pattern that prevents sidecar clients from bypassing the waypoint.
func TestWaypointBypassPrevention(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		env := newTestEnv(ctx, withSidecarClient())

		const waypointName = "bypass-prevention-wp"

		// Migrate ns to ambient with a waypoint activated.
		deployWaypoint(ctx, env.ns, waypointName)
		ambient.SetWaypointForNamespace(ctx, env.ns, waypointName)
		migrateNSToAmbient(ctx, env.ns)
		restartWorkloads(ctx, env.server, env.client)

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

		// Apply ALLOW policy permitting only the waypoint SA.
		policy := fmt.Sprintf(allowOnlyWaypointPolicy, env.ns.Name(), waypointName)
		ctx.Log("Applying waypoint bypass prevention ALLOW policy")
		ctx.ConfigIstio().YAML(env.ns.Name(), policy).ApplyOrFail(ctx)

		// Ambient client in the same namespace should succeed because its traffic is routed
		// through the waypoint whose SA matches the ALLOW rule.
		ctx.Log("Verifying ambient client can reach server through waypoint")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.And(check.OK(), isL7()),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("Ambient client allowed (traffic through waypoint)")

		// Sidecar client from a different namespace should be denied because it bypasses the
		// waypoint so its principal does not match the ALLOW rule.
		ctx.Log("Verifying sidecar client is denied (bypasses waypoint)")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.sidecarClient.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.NotOK(),
				Retry: echo.Retry{NoRetry: true},
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("Sidecar client denied — waypoint bypass prevention works")
	})
}
