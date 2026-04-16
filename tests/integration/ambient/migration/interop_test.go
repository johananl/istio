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

// TestMixedModeCrossNamespace validates sidecar↔ambient interop and the documented
// waypoint-bypass limitation. When a sidecar client calls a server in an ambient namespace
// with a waypoint, the traffic succeeds but bypasses the waypoint — L7 policies are not
// enforced. Traffic from an ambient client in the same namespace does go through the waypoint.
func TestMixedModeCrossNamespace(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		env := newTestEnv(ctx, withCrossClient())

		const waypointName = "mixed-mode-wp"

		// Step 1: Migrate ns to ambient with a waypoint; nsSidecar stays sidecar-injected.
		// Migrate before setting the waypoint so that SetWaypointForNamespace captures
		// the ambient label in its snapshot; its LIFO cleanup will then preserve it,
		// allowing namespace cleanup to work reliably.
		migrateNSToAmbient(ctx, env.ns)
		deployWaypoint(ctx, env.ns, waypointName)
		ambient.SetWaypointForNamespace(ctx, env.ns, waypointName)

		// Apply an L7 ALLOW policy on the waypoint so we can detect whether it is enforced.
		waypointPolicy := fmt.Sprintf(l7AuthzPolicyWaypoint, waypointName)
		ctx.ConfigIstio().YAML(env.ns.Name(), waypointPolicy).ApplyOrFail(ctx)
		restartWorkloads(ctx, env.server, env.client)

		ctx.Log("Waiting for ambient client connectivity through waypoint")
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

		// Step 2: crossClient (sidecar) calls server (ambient + waypoint) — traffic
		// succeeds but bypasses the waypoint.
		ctx.Log("Verifying sidecar crossClient can reach ambient server (bypasses waypoint)")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.crossClient.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))

		// Step 3: Verify L7 waypoint policy is NOT enforced for sidecar→ambient traffic.
		// The waypoint ALLOW policy only allows GET /allowed*. A POST should succeed if
		// the waypoint is bypassed (because ztunnel doesn't enforce L7 rules).
		ctx.Log("Verifying L7 policy is NOT enforced for sidecar→ambient (waypoint bypassed)")
		env.crossClient.CallOrFail(ctx, echo.CallOptions{
			To:   env.server,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "POST",
				Path:   "/allowed",
			},
			Count: 1,
			Check: check.OK(),
		})
		ctx.Log("Confirmed: sidecar→ambient traffic bypasses waypoint (POST allowed, no L7)")

		// Step 4: client (ambient) calls server — verify L7 waypoint policy IS enforced.
		ctx.Log("Verifying L7 policy IS enforced for ambient→ambient (through waypoint)")
		env.client.CallOrFail(ctx, echo.CallOptions{
			To:   env.server,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "GET",
				Path:   "/allowed",
			},
			Count: 1,
			Check: check.And(check.OK(), isL7()),
		})
		// POST should be denied by the waypoint.
		env.client.CallOrFail(ctx, echo.CallOptions{
			To:   env.server,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "POST",
				Path:   "/allowed",
			},
			Count: 1,
			Check: check.Status(403),
		})
		ctx.Log("Confirmed: ambient→ambient traffic goes through waypoint (POST denied)")

		// Step 5: Verify mTLS works in both directions.
		ctx.Log("Verifying mTLS for both traffic paths")
		// ambient client → server should work (already verified above).
		env.client.CallOrFail(ctx, echo.CallOptions{
			To:   env.server,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "GET",
				Path:   "/allowed",
			},
			Count: 1,
			Check: check.OK(),
		})
		// sidecar crossClient → server should work.
		env.crossClient.CallOrFail(ctx, echo.CallOptions{
			To:    env.server,
			Port:  echo.Port{Name: "http"},
			Count: 1,
			Check: check.OK(),
		})
		ctx.Log("mTLS verified for both sidecar→ambient and ambient→ambient paths")
	})
}
