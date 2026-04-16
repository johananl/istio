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
	"testing"
	"time"

	"istio.io/api/label"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test/echo/common/scheme"
	"istio.io/istio/pkg/test/framework"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/echo/check"
	"istio.io/istio/pkg/test/framework/components/echo/util/traffic"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/util/retry"
)

const peerAuthenticationStrict = `
apiVersion: security.istio.io/v1
kind: PeerAuthentication
metadata:
  name: strict-mtls
spec:
  mtls:
    mode: STRICT
`

// TestSidecarToAmbientMigration verifies that migrating workloads from sidecar to ambient mode
// does not cause significant packet loss or latency.
func TestSidecarToAmbientMigration(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ctx.Cleanup(func() { resetToSidecarMode(ctx) })

		// Run the migration with PeerAuthentication in permissive mode. Running in permissive mode
		// may result in a brief window of unencrypted traffic during the transition to ambient.
		ctx.NewSubTest("permissive").Run(func(ctx framework.TestContext) {
			runMigrationTest(ctx, 1.0)
		})

		// Reset the namespace back to sidecar mode so the next sub-test starts from the same
		// initial state.
		resetToSidecarMode(ctx)

		// Run the migration with PeerAuthentication in strict mode. No plaintext traffic is
		// allowed in this mode. This might reveal failures which don't show in permissive mode.
		ctx.NewSubTest("strict-mtls").Run(func(ctx framework.TestContext) {
			ctx.ConfigIstio().YAML(ns.Name(), peerAuthenticationStrict).ApplyOrFail(ctx)

			// Let the policy propagate before starting traffic.
			ctx.Log("Waiting for strict PeerAuthentication to propagate")
			retry.UntilSuccessOrFail(ctx, func() error {
				_, err := client.Call(echo.CallOptions{
					To:    server,
					Port:  echo.Port{Name: "http"},
					Count: 1,
					Check: check.OK(),
				})
				return err
			}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
			ctx.Log("Strict PeerAuthentication active — sidecar mTLS verified")

			runMigrationTest(ctx, 1.0)
		})
	})
}

// ingressGatewayConfig creates an Istio Gateway and VirtualService to route ingress traffic
// through the default istio-ingressgateway to the backend service.
// The first %s is the backend service host, %d is the backend port.
const ingressGatewayConfig = `
apiVersion: networking.istio.io/v1
kind: Gateway
metadata:
  name: server-gateway
spec:
  selector:
    istio: ingressgateway
  servers:
  - port:
      number: 80
      name: http
      protocol: HTTP
    hosts: ["*"]
---
apiVersion: networking.istio.io/v1
kind: VirtualService
metadata:
  name: server-vs
spec:
  gateways:
  - server-gateway
  hosts:
  - "*"
  http:
  - route:
    - destination:
        host: "%s"
        port:
          number: %d
`

// TestNorthSouthMigration verifies that north-south traffic (ingress gateway → server) is not
// significantly disrupted when migrating the server workload from sidecar to ambient mode.
func TestNorthSouthMigration(t *testing.T) {
	framework.NewTest(t).Run(func(ctx framework.TestContext) {
		ctx.Cleanup(func() { resetToSidecarMode(ctx) })

		// Apply Istio Gateway + VirtualService routing ingress traffic to the server.
		httpPort := server.Config().Ports.MustForName("http")
		gwCfg := fmt.Sprintf(ingressGatewayConfig, server.Config().Service, httpPort.ServicePort)
		ctx.ConfigIstio().YAML(ns.Name(), gwCfg).ApplyOrFail(ctx)

		ingress := istio.DefaultIngressOrFail(ctx, ctx)

		// Wait for ingress to be able to reach the server.
		ctx.Log("Waiting for ingress connectivity to server")
		retry.UntilSuccessOrFail(ctx, func() error {
			_, err := ingress.Call(echo.CallOptions{
				Port: echo.Port{
					Protocol:    protocol.HTTP,
					ServicePort: 80,
				},
				Scheme: scheme.HTTP,
				Count:  1,
				Check:  check.OK(),
			})
			return err
		}, retry.Timeout(2*time.Minute), retry.Delay(time.Second))
		ctx.Log("Ingress connectivity verified under sidecar mode")

		ctx.NewSubTest("permissive").Run(func(ctx framework.TestContext) {
			runNorthSouthMigrationTest(ctx, ingress, 1.0)
		})

		resetToSidecarMode(ctx)

		ctx.NewSubTest("strict-mtls").Run(func(ctx framework.TestContext) {
			ctx.ConfigIstio().YAML(ns.Name(), peerAuthenticationStrict).ApplyOrFail(ctx)

			ctx.Log("Waiting for strict PeerAuthentication to propagate")
			retry.UntilSuccessOrFail(ctx, func() error {
				_, err := ingress.Call(echo.CallOptions{
					Port: echo.Port{
						Protocol:    protocol.HTTP,
						ServicePort: 80,
					},
					Scheme: scheme.HTTP,
					Count:  1,
					Check:  check.OK(),
				})
				return err
			}, retry.Timeout(30*time.Second), retry.Delay(time.Second))
			ctx.Log("Strict PeerAuthentication active — ingress mTLS verified")

			runNorthSouthMigrationTest(ctx, ingress, 1.0)
		})
	})
}

// runMigrationTest executes the sidecar-to-ambient migration flow with a continuous traffic
// generator measuring disruption.
func runMigrationTest(ctx framework.TestContext, minSuccessRate float64) {
	const (
		preMigrationDuration  = 5 * time.Second
		postMigrationDuration = 30 * time.Second
		requestsPerRound      = 5
		interval              = 500 * time.Millisecond
	)

	ctx.Log("Verifying sidecar connectivity")
	client.CallOrFail(ctx, echo.CallOptions{
		To:    server,
		Port:  echo.Port{Name: "http"},
		Count: 5,
		Check: check.OK(),
	})
	ctx.Log("Sidecar connectivity verified")

	trafficCfg := traffic.Config{
		Source: client,
		Options: echo.CallOptions{
			To:    server,
			Port:  echo.Port{Name: "http"},
			Count: requestsPerRound,
			// Short per-request timeout so that a hung connection during pod restart is counted as
			// a failure quickly rather than blocking the generator and hiding missed requests.
			Timeout: 3 * time.Second,
		},
		Interval: interval,
		// StopTimeout must exceed the per-request Timeout so that the last in-flight call can
		// finish before Stop() gives up.
		StopTimeout: 10 * time.Second,
	}

	ctx.Log("Starting traffic generator")
	gen := traffic.NewGenerator(ctx, trafficCfg).Start()

	ctx.Log("Running baseline traffic under sidecar mode")
	time.Sleep(preMigrationDuration)

	ctx.Log("Migrating namespace to ambient mode")
	if err := ns.RemoveLabel("istio-injection"); err != nil {
		ctx.Fatalf("failed to remove sidecar injection label: %v", err)
	}
	if err := ns.SetLabel(label.IoIstioDataplaneMode.Name, "ambient"); err != nil {
		ctx.Fatalf("failed to set ambient dataplane mode: %v", err)
	}

	ctx.Log("Restarting server for switching to ambient")
	// Restart() is blocking. The traffic generator keeps running and records every success/failure
	// during this window.
	if err := server.Restart(); err != nil {
		ctx.Fatalf("failed to restart server: %v", err)
	}

	// Stop the generator before restarting the client. The generator sends traffic FROM the
	// client, so it cannot produce meaningful results while the client pod is being replaced.
	// Stopping here avoids a race in the test framework where the pod informer disconnects the
	// client mid-call, producing spurious "disconnected client" errors.
	migrationResult := gen.Stop()
	ctx.Log("Migration phase traffic results")
	ctx.Logf("  %s", migrationResult)

	ctx.Log("Restarting client for switching to ambient")
	if err := client.Restart(); err != nil {
		ctx.Fatalf("failed to restart client: %v", err)
	}
	ctx.Log("Workloads restarted — pods now running without sidecars")

	ctx.Log("Waiting for ambient connectivity")
	retry.UntilSuccessOrFail(ctx, func() error {
		_, err := client.Call(echo.CallOptions{
			To:    server,
			Port:  echo.Port{Name: "http"},
			Count: 1,
			Check: check.OK(),
		})
		return err
	}, retry.Timeout(5*time.Minute), retry.Delay(time.Second))
	ctx.Log("Ambient connectivity confirmed")

	ctx.Log("Running stabilization traffic under ambient mode")
	gen = traffic.NewGenerator(ctx, trafficCfg).Start()
	time.Sleep(postMigrationDuration)
	postResult := gen.Stop()
	ctx.Log("Stabilization phase traffic results")
	ctx.Logf("  %s", postResult)

	combined := traffic.Result{
		TotalRequests:      migrationResult.TotalRequests + postResult.TotalRequests,
		SuccessfulRequests: migrationResult.SuccessfulRequests + postResult.SuccessfulRequests,
		Error:              errors.Join(migrationResult.Error, postResult.Error),
	}
	ctx.Log("Sidecar-to-ambient migration traffic results (combined)")
	ctx.Logf("  %s", combined)
	combined.CheckSuccessRate(ctx, minSuccessRate)
}

// runNorthSouthMigrationTest executes the sidecar-to-ambient migration flow with continuous
// north-south traffic (ingress gateway → server) measuring disruption.
func runNorthSouthMigrationTest(ctx framework.TestContext, ingress echo.Caller, minSuccessRate float64) {
	const (
		preMigrationDuration  = 5 * time.Second
		postMigrationDuration = 30 * time.Second
		requestsPerRound      = 5
		interval              = 500 * time.Millisecond
	)

	trafficCfg := traffic.Config{
		Source: ingress,
		Options: echo.CallOptions{
			Port: echo.Port{
				Protocol:    protocol.HTTP,
				ServicePort: 80,
			},
			Scheme:  scheme.HTTP,
			Count:   requestsPerRound,
			Timeout: 3 * time.Second,
		},
		Interval:    interval,
		StopTimeout: 10 * time.Second,
	}

	ctx.Log("Starting north-south traffic generator")
	gen := traffic.NewGenerator(ctx, trafficCfg).Start()

	ctx.Log("Running baseline north-south traffic under sidecar mode")
	time.Sleep(preMigrationDuration)

	ctx.Log("Migrating namespace to ambient mode")
	if err := ns.RemoveLabel("istio-injection"); err != nil {
		ctx.Fatalf("failed to remove sidecar injection label: %v", err)
	}
	if err := ns.SetLabel(label.IoIstioDataplaneMode.Name, "ambient"); err != nil {
		ctx.Fatalf("failed to set ambient dataplane mode: %v", err)
	}

	ctx.Log("Restarting server for switching to ambient")
	if err := server.Restart(); err != nil {
		ctx.Fatalf("failed to restart server: %v", err)
	}

	// The ingress gateway is external to the namespace — it keeps running, so we can
	// continue measuring north-south traffic through the entire migration window.
	ctx.Log("Waiting for ambient north-south connectivity")
	retry.UntilSuccessOrFail(ctx, func() error {
		_, err := ingress.Call(echo.CallOptions{
			Port: echo.Port{
				Protocol:    protocol.HTTP,
				ServicePort: 80,
			},
			Scheme: scheme.HTTP,
			Count:  1,
			Check:  check.OK(),
		})
		return err
	}, retry.Timeout(5*time.Minute), retry.Delay(time.Second))
	ctx.Log("Ambient north-south connectivity confirmed")

	ctx.Log("Running stabilization north-south traffic under ambient mode")
	time.Sleep(postMigrationDuration)
	result := gen.Stop()
	ctx.Log("North-south migration traffic results")
	ctx.Logf("  %s", result)
	result.CheckSuccessRate(ctx, minSuccessRate)
}
