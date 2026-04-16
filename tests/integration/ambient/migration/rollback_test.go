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

	"istio.io/api/label"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/util/retry"
)

// TestRollbackFromAmbient validates reversibility at the critical post-restart stage.
// After a full migration to ambient, the test rolls back by re-enabling sidecar injection,
// removing the ambient label, and restarting pods.
func TestRollbackFromAmbient(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		env := newTestEnv(ctx, withCrossClient())

		// Step 1: Migrate namespace fully to ambient (labels + restart, no waypoint).
		migrateNSToAmbient(ctx, env.ns)
		restartWorkloads(ctx, env.server, env.client)

		// Step 2: Verify ambient connectivity.
		ctx.Log("Verifying ambient connectivity")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(time.Second))
		ctx.Log("Ambient connectivity confirmed")

		// Apply an L4 policy to verify it survives the rollback.
		policy := fmt.Sprintf(l4AuthzPolicy, env.ns.Name(), "default")
		ctx.Log("Applying L4 policy to verify it survives rollback")
		ctx.ConfigIstio().YAML(env.ns.Name(), policy).ApplyOrFail(ctx)

		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))

		// Step 3: Re-add istio-injection=enabled, remove dataplane-mode=ambient.
		ctx.Log("Rolling back to sidecar mode")
		if err := env.ns.RemoveLabel(label.IoIstioDataplaneMode.Name); err != nil {
			ctx.Fatalf("removing ambient label: %v", err)
		}
		if err := env.ns.SetLabel("istio-injection", "enabled"); err != nil {
			ctx.Fatalf("re-enabling sidecar injection: %v", err)
		}

		// Step 4: Restart pods.
		restartWorkloads(ctx, env.server, env.client)

		// Step 5: Verify sidecars are re-injected and traffic works.
		ctx.Log("Verifying sidecar connectivity after rollback")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(time.Second))
		ctx.Log("Sidecar connectivity restored after rollback")

		// Step 6: Verify L4 policy is still enforced post-rollback.
		ctx.Log("Verifying L4 policy still enforced after rollback")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.crossClient.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.NotOK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("L4 policy still enforced after rollback — crossClient denied")
	})
}

// forceMtlsDestinationRule forces the client sidecar to always send mTLS to the
// server service, overriding auto-mTLS. The %s placeholder is the namespace.
const forceMtlsDestinationRule = `
apiVersion: networking.istio.io/v1
kind: DestinationRule
metadata:
  name: force-mtls-server
spec:
  host: server.%s.svc.cluster.local
  trafficPolicy:
    tls:
      mode: ISTIO_MUTUAL
`

// TestWrongOrderingDisruption validates the ordering requirement stated in the migration PR.
// Removing sidecar injection without enabling ambient first leaves pods with neither sidecar
// nor ztunnel capture, disrupting traffic.
func TestWrongOrderingDisruption(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		env := newTestEnv(ctx)

		// Apply a DestinationRule that forces mTLS from the client to the server,
		// overriding auto-mTLS. Without this, auto-mTLS detects the server has no
		// proxy after the restart and silently falls back to plaintext, hiding the
		// disruption. In a real deployment with strict mTLS DestinationRules, this
		// is the configuration that causes breakage.
		dr := fmt.Sprintf(forceMtlsDestinationRule, env.ns.Name())
		ctx.Log("Applying DestinationRule to force mTLS to server")
		ctx.ConfigIstio().YAML(env.ns.Name(), dr).ApplyOrFail(ctx)

		ctx.Log("Waiting for DestinationRule to propagate")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 1,
				Check: check.OK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))

		// Step 1: Disable sidecar injection WITHOUT enabling ambient first.
		ctx.Log("Disabling sidecar injection without enabling ambient")
		if err := env.ns.SetLabel("istio-injection", "disabled"); err != nil {
			ctx.Fatalf("failed to disable sidecar injection: %v", err)
		}

		// Step 2: Restart server so it comes up without a sidecar and without ztunnel capture.
		restartWorkloads(ctx, env.server)

		// The client still has a sidecar (not restarted). The DestinationRule forces
		// mTLS, but the server has no proxy to terminate it — traffic should fail.

		// Step 3: Verify traffic is disrupted.
		ctx.Log("Verifying traffic disruption due to wrong ordering")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := env.client.Call(echo.CallOptions{
				To:    env.server,
				Port:  echo.Port{Name: "http"},
				Count: 3,
				Check: check.NotOK(),
			})
			return err
		}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
		ctx.Log("Traffic disrupted as expected — server has neither sidecar nor ztunnel capture")
	})
}
