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

// TestSidecarToAmbientBypassesWaypoint validates sidecar-ambient interop. When a sidecar client
// calls a server in an ambient namespace with a waypoint, the traffic succeeds but bypasses the
// waypoint. L7 policies are not enforced. Traffic from an ambient client in the same namespace
// does go through the waypoint.
func TestSidecarToAmbientBypassesWaypoint(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		env := newTestEnv(ctx, withSidecarClient())

		const waypointName = "interop-waypoint"

		// Migrate ns to ambient with a waypoint. The sidecar client's namespace stays on sidecar.
		// Migrate before setting the waypoint so that SetWaypointForNamespace's label snapshot
		// includes the ambient label, otherwise its cleanup would restore pre-ambient labels.
		migrateNSToAmbient(ctx, env.ns)
		deployWaypoint(ctx, env.ns, waypointName)
		ambient.SetWaypointForNamespace(ctx, env.ns, waypointName)

		// Apply an L7 ALLOW policy on the waypoint so we can detect whether traffic goes through
		// the waypoint.
		policy := fmt.Sprintf(l7AuthzPolicyWaypoint, waypointName)
		ctx.ConfigIstio().YAML(env.ns.Name(), policy).ApplyOrFail(ctx)
		restartWorkloads(ctx, env.server, env.client)

		ctx.Log("Waiting for ambient client connectivity (through waypoint)")
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

		ctx.Log("Waiting for sidecar client connectivity (not through waypoint)")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.sidecarClient.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))

		// Verify L7 policy is not enforced for sidecar->ambient traffic. The policy only allows
		// GET requests to /allowed* so if a POST request succeeds it means the waypoint is
		// bypassed.
		ctx.Log("Verifying L7 policy is NOT enforced for sidecar->ambient (waypoint bypassed)")
		env.sidecarClient.CallOrFail(ctx, echo.CallOptions{
			To:   env.server,
			Port: echo.Port{Name: "http"},
			HTTP: echo.HTTP{
				Method: "POST",
				Path:   "/allowed",
			},
			Count: 1,
			Check: check.OK(),
		})
		ctx.Log("Confirmed: sidecar->ambient traffic bypasses waypoint")

		ctx.Log("Verifying L7 policy is enforced for ambient->ambient (through waypoint)")
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
		ctx.Log("Confirmed: ambient->ambient traffic goes through waypoint (POST denied)")
	})
}
