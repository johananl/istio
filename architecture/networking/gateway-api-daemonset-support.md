# Gateway API DaemonSet Support

This document describes a plan for adding DaemonSet workload support to Istio's automated Gateway API deployment controller.

## Motivation

Istio's Gateway API implementation automatically provisions a `Service` and `Deployment` for managed `Gateway` resources. This works well for most ingress deployments, but some environments need a gateway pod on every node.

Common use cases include:

- Preserving real client IPs with `externalTrafficPolicy: Local`.
- Using `NodePort` or load balancers that target cluster nodes directly.
- Avoiding Deployment replica placement issues where multiple gateway pods can land on the same node.
- Reducing disruption during planned rollouts by maintaining one gateway pod per eligible node.

A DaemonSet supports these patterns, but it does not guarantee a ready gateway pod on every node a load balancer may target. Taints, node selectors, affinity, unschedulable nodes, resource pressure, and rollout state can all leave a node without a local endpoint. Users still need to align Service configuration, node eligibility, and load balancer behavior.

The standalone Istio gateway Helm chart already supports `kind: DaemonSet`. The automated Gateway API path should provide a comparable capability while preserving its current default behavior.

Related context:

- [Istio issue #56658](https://github.com/istio/istio/issues/56658)
- [agentgateway issue #1592](https://github.com/agentgateway/agentgateway/issues/1592)
- [agentgateway PR #2208](https://github.com/agentgateway/agentgateway/pull/2208)
- [Istio Gateway API deployment methods](https://istio.io/latest/docs/tasks/traffic-management/ingress/gateway-api/#deployment-methods)

## Goals

- Allow a managed Gateway API `Gateway` to select `Deployment` or `DaemonSet` as its generated workload kind.
- Keep `Deployment` as the default for backward compatibility.
- Support DaemonSet customization through the existing `Gateway.spec.infrastructure.parametersRef` ConfigMap mechanism.
- Support DaemonSet defaults through the existing `values.gatewayClasses` installation mechanism.
- Prevent unsupported combinations such as `horizontalPodAutoscaler` overlays with DaemonSet workloads.
- Avoid duplicate-serving states when a Gateway changes workload kind.

## Non-Goals

- Introducing a new Istio CRD for Gateway parameters.
- Changing manual gateway deployment behavior.
- Enabling DaemonSet waypoints without a separate design discussion.
- Supporting HPA for DaemonSet workloads.
- Guaranteeing zero-downtime transitions when changing an existing Gateway between workload kinds.
- Guaranteeing local endpoints on every node a user or cloud load balancer may target.

## User API

Use the existing Gateway infrastructure parameters model. Add a controller-only `workload` key and a new `daemonSet` overlay key.

Example per-Gateway configuration:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: edge
  namespace: istio-ingress
spec:
  gatewayClassName: istio
  infrastructure:
    parametersRef:
      group: ""
      kind: ConfigMap
      name: edge-gateway-options
  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: All
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: edge-gateway-options
  namespace: istio-ingress
data:
  workload: |
    kind: DaemonSet

  daemonSet: |
    spec:
      updateStrategy:
        type: RollingUpdate

  service: |
    spec:
      type: NodePort
      externalTrafficPolicy: Local
```

Example GatewayClass default configuration:

```yaml
apiVersion: install.istio.io/v1alpha1
kind: IstioOperator
spec:
  values:
    gatewayClasses:
      istio:
        workload:
          kind: DaemonSet
        service:
          spec:
            type: NodePort
            externalTrafficPolicy: Local
        daemonSet:
          spec:
            updateStrategy:
              type: RollingUpdate
```

The `workload.kind` field should accept only `Deployment` and `DaemonSet`. If omitted, it defaults to `Deployment`.

Not every Istio-managed GatewayClass should automatically get this option. Each GatewayClass maps to an internal `ClassInfo`, and each `ClassInfo` points at a specific generated-resource template. The mechanism should be generic, but enablement should be per GatewayClass/template pair. The controller should honor `workload.kind` only for classes whose template has been updated and tested to render both workload kinds.

This design enables workload selection only for the standard managed ingress GatewayClass, `istio`, which uses the `kube-gateway` template. Other Istio-managed GatewayClasses, including waypoint-style classes and the in-repository agentgateway classes, should reject `workload` configuration with a visible error because their templates and semantics are not part of this design. This scope does not require a hard-coded `GatewayClassName == istio` special case; it should be represented as a capability on `ClassInfo`, so another class can opt in coherently if its template and tests are added in a separate design.

## ConfigMap Data Contract

The Gateway API infrastructure parameters are carried in `ConfigMap.data`, so every value is a string containing YAML. The controller should continue to interpret the existing object overlay keys as strategic merge patches against generated Kubernetes resources:

- `deployment`
- `daemonSet`
- `service`
- `serviceAccount`
- `horizontalPodAutoscaler`
- `podDisruptionBudget`

The new `workload` key is different. It is controller configuration, not an overlay for a Kubernetes object. Its initial schema should be intentionally small:

```yaml
kind: DaemonSet
```

The same YAML shape should be accepted through GatewayClass defaults. When supplied through `values.gatewayClasses`, Helm renders the structured values into `ConfigMap.data` strings, so these two inputs should be equivalent after rendering:

```yaml
gatewayClasses:
  istio:
    workload:
      kind: DaemonSet
```

```yaml
data:
  workload: |
    kind: DaemonSet
```

Because this is ConfigMap data rather than a typed CRD, malformed values cannot be rejected by Kubernetes admission. The deployment controller should parse and validate this data during reconciliation and produce clear errors for unknown fields, invalid workload kinds, or incompatible overlay combinations.

## Precedence and Inheritance

The existing overlay model composes GatewayClass defaults and Gateway-level parameters by applying GatewayClass overlays first, then Gateway overlays. Workload selection needs a more precise rule because a Gateway may inherit Deployment-oriented defaults from its GatewayClass while opting into a DaemonSet.

Resolve parameters in two phases:

1. Resolve the final workload kind from ordered sources. The last explicit `workload.kind` wins. If no source sets it, the final kind is `Deployment`.
1. Apply only overlays compatible with the final workload kind.

Compatibility is evaluated with inheritance in mind:

- If a lower-precedence source, such as GatewayClass defaults, supplied `deployment` or `horizontalPodAutoscaler` overlays, and a higher-precedence Gateway source selects `DaemonSet`, ignore those inherited Deployment-only overlays and emit a warning signal.
- If the source that selects `DaemonSet` also supplies a `deployment` or `horizontalPodAutoscaler` overlay, reject the configuration.
- If a GatewayClass itself selects `DaemonSet`, `deployment` and `horizontalPodAutoscaler` overlays in that same GatewayClass default are invalid.
- Apply the equivalent rules in the other direction for `daemonSet` overlays when the final workload kind is `Deployment`.
- Workload-neutral overlays, such as `service`, `serviceAccount`, and `podDisruptionBudget`, continue to compose in source order.

This preserves existing additive overlay behavior for normal Deployment gateways while giving per-Gateway workload overrides a defined way to escape inherited Deployment-only defaults.

## Validation Rules

Apply validation while resolving and rendering overlays:

- `workload.kind` must be empty, `Deployment`, or `DaemonSet`.
- `deployment` overlays are applied only when the selected workload is `Deployment`; otherwise they are either ignored as inherited lower-precedence defaults or rejected when supplied by the source selecting the incompatible workload.
- `daemonSet` overlays are applied only when the selected workload is `DaemonSet`; otherwise they are either ignored as inherited lower-precedence defaults or rejected when supplied by the source selecting the incompatible workload.
- `horizontalPodAutoscaler` overlays are applied only when the selected workload is `Deployment`; otherwise they are either ignored as inherited lower-precedence defaults or rejected when supplied by the source selecting the incompatible workload.
- `podDisruptionBudget` overlays are valid for both `Deployment` and `DaemonSet` because they select pods by labels.
- `workload` is valid only for GatewayClasses that explicitly support workload selection.

Istio's current parameters are ConfigMap data rather than typed API fields, so these checks should produce user-visible controller errors rather than relying on Kubernetes schema validation.

## Implementation Plan

### 1. Resolve Workload Kind and Overlays

Update `pilot/pkg/config/kube/gatewaycommon/deploymentcontroller.go` to resolve workload configuration from the same overlay sources already used by `render`:

1. GatewayClass defaults ConfigMap in the root namespace.
1. Gateway `spec.infrastructure.parametersRef` ConfigMap in the Gateway namespace.

Gateway-level workload configuration should override GatewayClass defaults. Object overlays should still compose in source order after filtering incompatible inherited workload-specific overlays as described in the precedence model.

Extend `TemplateInput` with fields such as:

```go
WorkloadKind     string
WorkloadResource string
```

For example:

- `Deployment` maps to `deployments`.
- `DaemonSet` maps to `daemonsets`.

These fields allow templates to set both the Kubernetes kind and metadata such as `ISTIO_META_OWNER` without hard-coding Deployment paths.

The resolver should return both the effective workload kind and a filtered overlay list. The filtered list is what `applyOverlay` consumes, so ignored inherited Deployment-only overlays do not cause DaemonSet renders to produce HPAs or Deployment patches.

### 2. Add DaemonSet Overlay Support

Extend `supportedOverlays` in `deploymentcontroller.go` to include:

```go
"workload"
"daemonSet"
```

Handle `daemonSet` in `applyOverlay` with `appsv1.DaemonSet{}` as the strategic merge patch schema.

Treat `workload` as controller configuration, not as an object patch. It should be parsed before object overlays are applied and skipped by the object overlay loop.

Add a `SupportsWorkloadSelection` field to `ClassInfo`. The deployment controller should reject `workload` configuration for classes without this flag. This design sets the flag only for the standard managed ingress GatewayClass, `istio`.

### 3. Render the Selected Workload

Update `manifests/charts/istio-control/istio-discovery/files/kube-gateway.yaml`:

- Replace the hard-coded workload `kind: Deployment` with the selected workload kind.
- Keep the common `spec.selector` and `spec.template` structure, which is valid for both Deployments and DaemonSets.
- Set `ISTIO_META_OWNER` using the selected workload resource, for example `deployments` or `daemonsets`.
- Render an HPA only for Deployment workloads and only when a compatible HPA overlay remains after overlay resolution.
- Keep the PDB selector unchanged so it continues to target generated gateway pods by `gateway.networking.k8s.io/gateway-name`.

This design updates only `kube-gateway.yaml`. Do not update `agentgateway.yaml`, `waypoint.yaml`, or `agentgateway-waypoint.yaml` unless the design is expanded to include those classes and their class-specific tests.

### 4. Watch DaemonSet Children

Add a DaemonSet informer to `DeploymentController`:

```go
daemonSets kclient.Client[*appsv1.DaemonSet]
```

Register it the same way as Deployments:

- Create it with `kclient.NewFiltered[*appsv1.DaemonSet](client, filter)`.
- Attach `parentHandler` so child changes requeue the parent Gateway.
- Add `d.clients[gvr.DaemonSet] = NewUntypedWrapper(d.daemonSets)` so `canManage` can protect existing resources.
- Include `d.daemonSets.HasSynced` in `Run`.
- Include `d.daemonSets` in controller shutdown.

The generated schema package already defines `gvr.DaemonSet` and `gvk.DaemonSet`, so this should not require schema generation work.

### 5. Handle Workload-Kind Transitions

Workload-kind transitions need an explicit trade-off decision. Applying the new workload first minimizes downtime, but can temporarily route traffic to both the old Deployment pods and the new DaemonSet pods, and a later prune failure would leave the cluster in that state. Deleting the old workload first avoids duplicate serving, but can cause downtime while the new workload becomes ready.

The implementation should document the chosen transition behavior and the reason for it. Given the motivating DaemonSet use cases depend on node-local endpoint semantics, this plan prefers avoiding duplicate serving over minimizing downtime during workload-kind migration.

Use a conservative two-phase transition when the selected workload kind differs from an existing managed workload:

1. Detect managed stale workloads of the other kind before applying the desired workload.
1. Delete stale workload-scoped resources first, including the old workload and its HPA if present.
1. Requeue and wait until the stale workload is absent from the informer cache. For the strongest no-duplicate guarantee, also wait until pods owned by that stale workload are gone or no longer selected by the Gateway Service.
1. Only then apply the desired workload kind and workload-neutral resources.

If pre-pruning fails, abort reconciliation, leave the existing workload in place, and surface a user-visible error. This favors availability of the existing gateway over creating a duplicate-serving state. The tradeoff is that changing workload kind is not guaranteed to be zero downtime; the implementation should document that users may see a gap while the old workload is removed and the new workload becomes ready.

After normal apply succeeds, prune generated resources that are no longer desired but do not create duplicate-serving risk, such as removed PDBs or stale autoscaling resources.

Build the desired resource set from the rendered objects. Then list managed generated resources in the Gateway namespace for these kinds:

- `Deployment`
- `DaemonSet`
- `HorizontalPodAutoscaler`
- `PodDisruptionBudget`

Use labels to find candidate resources:

- `gateway.networking.k8s.io/gateway-name=<gateway name>`
- `gateway.istio.io/managed`

For workload resources, also require a Gateway owner reference before deleting. This avoids deleting user-managed workloads that copied the Gateway label for policy attachment or observability.

Delete candidates that are not in the desired resource set. This also handles removal of HPA or PDB overlays after they were previously configured.

### 6. Surface Reconciliation Errors

Malformed `workload` data, incompatible overlays, failed applies, and failed transition pruning should be visible to users. Log-only failures are not sufficient because ConfigMap data cannot be validated by admission.

Add a user-visible signal for deployment-controller failures. Preferred behavior is to update the Gateway `Programmed` condition to `False` with a reason such as `Invalid` for invalid parameters or an implementation-specific reason for apply/prune failures, and a message naming the offending ConfigMap key. If wiring these errors into the existing Gateway status collection is too invasive, emit Kubernetes warning events as an initial implementation and track status integration as required follow-up before documenting the feature as stable.

Clear the failure signal after a subsequent successful reconciliation.

### 7. Update RBAC

Update `manifests/charts/istio-control/istio-discovery/templates/clusterrole.yaml` so the gateway deployment controller can manage DaemonSets:

```yaml
- apiGroups: ["apps"]
  verbs: ["get", "watch", "list", "update", "patch", "create", "delete"]
  resources: ["deployments", "daemonsets"]
```

Existing permissions for Services, ServiceAccounts, HPAs, and PDBs can remain unchanged.

### 8. Update Documentation

Update the Istio Gateway API deployment methods documentation to include:

- `workload.kind: DaemonSet` examples.
- A `NodePort` plus `externalTrafficPolicy: Local` example.
- The new valid ConfigMap keys: `workload` and `daemonSet`.
- A note that HPA overlays are unsupported for DaemonSet workloads.
- A note that `Deployment` remains the default when `workload.kind` is omitted.
- A note that DaemonSet plus `externalTrafficPolicy: Local` requires suitable node scheduling and load balancer configuration, and does not guarantee every targeted node has a ready local endpoint.
- A note that changing an existing Gateway between workload kinds may be disruptive because Istio must avoid a duplicate-serving state.

Add a release note under `releasenotes/notes` when the implementation lands because this is user-facing behavior and configuration surface.

## Test Plan

Add focused unit coverage in `pilot/pkg/config/kube/gatewaycommon/deploymentcontroller_test.go`:

- Default Gateway API reconciliation still renders a `Deployment`.
- `workload.kind: DaemonSet` renders a `DaemonSet`.
- `daemonSet` overlays apply with strategic merge patch semantics.
- Gateway-level workload configuration overrides GatewayClass defaults.
- Gateway-level DaemonSet selection ignores inherited GatewayClass `deployment` and `horizontalPodAutoscaler` overlays with a warning signal.
- `service.externalTrafficPolicy: Local` can be combined with DaemonSet workloads.
- `horizontalPodAutoscaler` overlays are rejected for DaemonSet workloads.
- `deployment` overlays are rejected for DaemonSet workloads.
- `daemonSet` overlays are rejected for Deployment workloads.
- `workload` configuration is rejected for managed GatewayClasses that do not set `SupportsWorkloadSelection`, including waypoint-style and in-repository agentgateway classes.
- Invalid `workload` data and incompatible overlays produce user-visible status or events.
- Switching from Deployment to DaemonSet deletes the old Deployment before creating the new DaemonSet.
- Switching from DaemonSet to Deployment deletes the old DaemonSet before creating the new Deployment.
- `TestApplySafeguards` accepts `DaemonSet` as a valid managed kind.

Update Helm/operator golden output for RBAC and embedded template changes.

Useful focused validation commands:

```bash
go test ./pilot/pkg/config/kube/gatewaycommon
go test ./operator/pkg/helm
```

## Rollout Considerations

This change is backward compatible when `workload.kind` is omitted. The primary operational risk is stale or duplicate-serving resources during workload-kind transitions, so pre-pruning should be implemented with conservative ownership checks before the feature is documented as supported.

The mechanism should remain generic, but this design enables it only for the standard managed ingress GatewayClass, `istio`. Support for waypoint, agentgateway, or other specialized GatewayClasses should require updating their templates, enabling the same `ClassInfo` capability, and adding class-specific tests in the design that includes them.
