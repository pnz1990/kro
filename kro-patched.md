# kro — Custom Patches and Extensions

This document describes every modification introduced in this fork of
[kubernetes-sigs/kro](https://github.com/kubernetes-sigs/kro). The changes
collectively enable **stateful, self-driving ResourceGraphDefinitions**: the
instance CR itself becomes a live state machine whose fields are computed and
mutated by the kro controller across reconcile cycles, without any external
operator code.

---

## Status summary

| Patch | Status |
|---|---|
| `specPatch` — CEL write-back to `spec` | **Still in fork** — pending upstream design discussion (#578) |
| `stateWrite` — CEL write-back to `status.kstate` | **Still in fork** — pending upstream design discussion (#578) |
| `InjectKstateField` — CRD auto-injection for `status.kstate` | **Still in fork** — depends on `stateWrite` |
| `StripExpressionWrapper` — build-time expression parser | **Still in fork** — depends on `specPatch`/`stateWrite` |
| `cel.bind()` / `ext.Bindings()` AST inspector fix | **Merged upstream** — PR #1145 |
| `random` CEL library (`random.seededString`, `random.seededInt`) | **Merged upstream** — earlier contributions |
| `lists` CEL library (`lists.setAtIndex`, `lists.insertAtIndex`, `lists.removeAtIndex`) | **Merged upstream** — PR #1148 (supersedes the old `lists.set` in this fork) |
| `csv` CEL library (`csv.add`, `csv.remove`, `csv.contains`) | **Removed from fork** — krombat migrated to `json.marshal`/`json.unmarshal`; no longer needed |

The current fork diff is exactly 13 files — all `specPatch`/`stateWrite` infrastructure:

```
api/v1alpha1/resourcegraphdefinition_types.go
helm/crds/kro.run_resourcegraphdefinitions.yaml
pkg/graph/builder.go
pkg/graph/node.go
pkg/graph/parser/conditions.go
pkg/graph/crd/crd.go
pkg/controller/instance/context.go
pkg/controller/instance/resources.go
pkg/controller/instance/status.go
pkg/runtime/node.go
pkg/runtime/runtime.go
docs/design/proposals/cel-writeback/proposal.md
Dockerfile.fix
```

Run `git diff cel-writeback-d upstream/main --name-only` to verify before opening any upstream PR.

---

## What we still need (fork-only)

### 1. `specPatch` — CEL write-back to `spec`

A `specPatch` node is a virtual node (it creates no Kubernetes resource) that
evaluates a set of CEL expressions and writes the computed values back into
the instance CR's `spec` fields via Server-Side Apply. The node fires when its
`includeWhen` condition is true and is idempotent: it compares computed values
against the live spec before issuing a patch, so it is a no-op when the
computed values already match.

Each specPatch node uses a dedicated SSA field manager
(`kro.run/specpatch-<id>`) so that multiple specPatch nodes in the same RGD do
not drop each other's field ownership.

Because the patched fields are in `spec`, the updated values are visible to the
next reconcile cycle immediately — spec changes trigger a watch event which
drives the controller to re-reconcile. This enables convergence loops where one
specPatch node computes an intermediate result that another node then reads and
transforms.

**Real-world use cases**

- **Multi-step approval workflow**: An `ApprovalRequest` CR passes through
  states `submitted → reviewing → approved → provisioned`. Each state
  transition is a specPatch node gated on the previous state value.
- **Retry counter with backoff**: A `JobRun` CR tracks `retryCount` and
  `nextRetryAt`. A specPatch node increments `retryCount` and computes the
  next retry timestamp after each failure, stopping when `retryCount >= maxRetries`.
- **Quota accumulation**: A `Namespace` CR tracks `usedCPU` and `usedMemory`.
  A specPatch node recalculates totals by summing over child resource specs each
  reconcile cycle.

**YAML syntax**

```yaml
- id: advanceStage
  type: specPatch
  includeWhen:
    - "${schema.spec.currentStage == 'reviewing' && schema.spec.approvedBy != ''}"
  patch:
    currentStage: "${schema.spec.approvedBy != '' ? 'approved' : schema.spec.currentStage}"
    approvedAt:   "${schema.metadata.name + '-' + string(schema.spec.approvalSeq)}"
    stageSeq:     "${schema.spec.stageSeq + 1}"
```

**Implementation notes**

- Located in: `pkg/graph/builder.go` (`buildSpecPatchNode`), `pkg/runtime/node.go`
  (`EvaluateSpecPatch`), `pkg/controller/instance/resources.go`
  (`processSpecPatchNode`)
- `patch:` values must use `${...}` expression syntax; bare strings are rejected
  at build time
- CEL expressions in `patch:` are fully type-checked against the instance schema
- The node participates in the DAG — any resource ID referenced in a `patch:`
  expression becomes a dependency, so the node only fires after those resources
  are ready
- `readyWhen:` and `forEach:` are not supported on specPatch nodes
- **Do NOT use `transformList` or `transformMap` inside `specPatch` nodes** —
  kro's dep-graph builder does not recognize the comprehension variable as a
  local binding and reports it as an unknown identifier. Use
  `lists.range(n).map(i, expr)` instead

---

### 2. `stateWrite` — CEL write-back to `status.kstate`

A `stateWrite` node is a virtual node that evaluates CEL expressions and writes
the computed values to `status.kstate.*` on the instance CR via the
`/status` subresource. Unlike `specPatch`, the written values live in `status`
and are therefore invisible to GitOps tooling (Argo CD, Flux) — they do not
appear in the Git manifest and do not cause drift detection to fire.

`status.kstate` is an opaque, schema-free object injected by kro:
`x-kubernetes-preserve-unknown-fields: true` is added to the CRD automatically
when any stateWrite node is present (see §3). Values written there persist
across reconcile cycles and can be read back by subsequent stateWrite
expressions via `schema.status.kstate.*`.

When a stateWrite node mutates kstate, the controller schedules a requeue so
that the next reconcile cycle reads the updated kstate and can fire further
nodes.

**Real-world use cases**

- **Convergence step counter**: A `DatabaseMigration` CR needs to run N
  migration scripts in sequence. A stateWrite node tracks which step has been
  applied in `status.kstate.lastAppliedStep`.
- **Observed generation bookkeeping**: A custom resource needs to detect when
  its `spec` was last changed and record it in a controller-private field
  without exposing it as a user-settable spec field.
- **Transient error accumulation**: A `WebhookSink` CR accumulates consecutive
  error counts in `status.kstate.consecutiveErrors`. A stateWrite node resets
  it to zero on success or increments it on failure, with a specPatch node
  setting `spec.suspended = true` when the threshold is crossed.

**YAML syntax**

```yaml
- id: trackMigrationStep
  type: stateWrite
  includeWhen:
    - "${!has(schema.status.kstate.lastStep) || schema.status.kstate.lastStep < schema.spec.totalSteps}"
  state:
    lastStep:  "${has(schema.status.kstate.lastStep) ? schema.status.kstate.lastStep + 1 : 1}"
    updatedAt: "${schema.metadata.resourceVersion}"
```

> **Note on the `has()` bootstrap pattern**: On the first reconcile, `status.kstate`
> is empty so any `schema.status.kstate.*` access will fail with "no such key"
> unless guarded by `has(schema.status.kstate.fieldName)`. This guard is
> required for any stateWrite expression that self-references a kstate field.

**Implementation notes**

- Located in: `pkg/graph/builder.go` (`buildStateWriteNode`), `pkg/runtime/node.go`
  (`EvaluateStateWrite`, `buildContextWithState`), `pkg/controller/instance/resources.go`
  (`processStateWriteNode`), `pkg/controller/instance/status.go` (kstate preservation),
  `pkg/graph/crd/crd.go` (`InjectKstateField`)
- `state:` expressions are compiled in **parse-only mode** (no type checking)
  because the typed CEL environment is built from `schemaWithoutStatus` and has
  no knowledge of `status.kstate.*`
- kstate is preserved across `updateStatus` calls by explicitly re-reading and
  re-injecting it during each status update cycle
- `readyWhen:` and `forEach:` are not supported on stateWrite nodes

---

### 3. `InjectKstateField` — CRD schema auto-injection for `status.kstate`

When the kro build phase detects that an RGD contains at least one `stateWrite`
node, it automatically injects a `status.kstate` field into the generated CRD
schema with `x-kubernetes-preserve-unknown-fields: true`. Without this, the
Kubernetes API server would silently strip any `kstate` data from `UpdateStatus`
calls because the field is not declared in the CRD schema.

- Located in: `pkg/graph/crd/crd.go` (`InjectKstateField`)

---

### 4. `StripExpressionWrapper` — build-time CEL expression parser

A utility function in the conditions parser that validates a string is in
`${...}` form and strips the wrapper, returning the bare CEL expression. Used
by the `specPatch` and `stateWrite` node builders when processing `patch:` and
`state:` map values.

- Located in: `pkg/graph/parser/conditions.go` (`StripExpressionWrapper`)

---

## What was merged upstream (no longer in fork diff)

### `cel.bind()` / `ext.Bindings()` — merged upstream PR #1145

kro's AST inspector now correctly handles `cel.bind()` let-bindings, and
`ext.Bindings()` is registered in the base CEL environment. Bound variable
names are no longer misreported as unknown resource identifiers in the
dependency graph.

This was the most impactful upstream contribution — without it, any expression
using `cel.bind()` to define intermediate computed values would produce
incorrect DAG dependencies.

### `random` CEL library — merged upstream (earlier contributions)

`random.seededString(length, seed)` and `random.seededInt(min, max, seed)` are
registered in the upstream base CEL environment. Deterministic pseudo-random
generation seeded by stable string identifiers, safe for use in continuously-
reconciling CEL expressions.

### `lists` CEL library — merged upstream PR #1148

`lists.setAtIndex`, `lists.insertAtIndex`, and `lists.removeAtIndex` are all
available in upstream kro. These supersede the old `lists.set` function that
was previously in this fork. The upstream PR uses the more descriptive `AtIndex`
naming convention.

---

## What was removed from the fork (no longer needed)

### `csv` CEL library — removed at commit `bad6490`

`csv.add`, `csv.remove`, and `csv.contains` for manipulating comma-separated
value strings. Removed because the sole consumer (krombat's inventory system)
has been migrated to use the upstream `json.marshal` / `json.unmarshal` CEL
functions instead. Inventory is now stored as a JSON array string
(e.g. `["weapon-common","armor-rare"]`) rather than a CSV string.

Files deleted: `pkg/cel/library/csv.go`, `pkg/cel/library/csv_test.go`.
One line removed from `pkg/cel/environment.go` (`library.CSV()`).

---

## Upstream path for the remaining fork patches

`specPatch` and `stateWrite` are the core value of this fork and the ones that
matter for upstreaming. The design is documented in
`docs/design/proposals/cel-writeback/proposal.md`. The plan:

1. Open a kro KREP or GitHub Discussion to get maintainer buy-in on the
   `specPatch` write-back design (issue #578 in krombat)
2. Once the design is agreed, open a PR with only the `specPatch`/`stateWrite`
   files — no krombat-private code, no game references
3. `StripExpressionWrapper` and `InjectKstateField` go in the same PR as they
   have no standalone value
4. The CRD schema changes (`api/v1alpha1/...`, `helm/crds/...`) follow the
   implementation PR

**Do not include in any upstream PR:**
- `kro-patched.md` (this file)
- `Dockerfile.fix`
- Any reference to krombat, game logic, dungeons, or combat

---

## File index

| File | Change type | Status |
|---|---|---|
| `api/v1alpha1/resourcegraphdefinition_types.go` | Modified | **Fork** — `Type`, `Patch`, `State` fields on `Resource` |
| `helm/crds/kro.run_resourcegraphdefinitions.yaml` | Modified | **Fork** — CRD schema for `type`, `patch`, `state` |
| `pkg/graph/node.go` | Modified | **Fork** — `NodeTypeSpecPatch`, `NodeTypeStateWrite` |
| `pkg/graph/builder.go` | Modified | **Fork** — `buildSpecPatchNode`, `buildStateWriteNode` |
| `pkg/graph/parser/conditions.go` | Modified | **Fork** — `StripExpressionWrapper` |
| `pkg/graph/crd/crd.go` | Modified | **Fork** — `InjectKstateField` |
| `pkg/runtime/node.go` | Modified | **Fork** — `EvaluateSpecPatch`, `EvaluateStateWrite` |
| `pkg/runtime/runtime.go` | Modified | **Fork** — `StateWriteMutated` requeue |
| `pkg/controller/instance/context.go` | Modified | **Fork** — `StateWriteMutated bool` |
| `pkg/controller/instance/resources.go` | Modified | **Fork** — `processSpecPatchNode`, `processStateWriteNode` |
| `pkg/controller/instance/status.go` | Modified | **Fork** — kstate preservation |
| `docs/design/proposals/cel-writeback/proposal.md` | New | **Fork** — design doc |
| `Dockerfile.fix` | New | **Fork** — build tooling for custom image |
| `pkg/cel/library/random.go` + `random_test.go` | ~~New~~ | **Upstream** — PR merged (seededString, seededInt) |
| `pkg/cel/library/lists.go` + `lists_test.go` | ~~New~~ | **Upstream** — PR #1148 (setAtIndex, insertAtIndex, removeAtIndex) |
| `pkg/cel/ast/inspector.go` + `inspector_test.go` | ~~Modified~~ | **Upstream** — PR #1145 (cel.bind scoping) |
| `pkg/cel/environment.go` | ~~Modified~~ | **Upstream** — ext.Bindings(), Random(), Lists() all registered upstream |
| `pkg/cel/library/csv.go` + `csv_test.go` | ~~New~~ | **Removed** — migrated to json.marshal/unmarshal (commit bad6490) |
