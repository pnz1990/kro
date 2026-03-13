# kro — Custom Patches and Extensions

This document describes every modification introduced in this fork of
[kubernetes-sigs/kro](https://github.com/kubernetes-sigs/kro). The changes
collectively enable **stateful, self-driving ResourceGraphDefinitions**: the
instance CR itself becomes a live state machine whose fields are computed and
mutated by the kro controller across reconcile cycles, without any external
operator code.

---

## Overview

Stock kro is a one-way projection engine: CEL expressions read from a parent
instance CR and produce child Kubernetes resources. There is no way for kro
itself to write back to the instance. Any field that needs to change over time
(counters, phase flags, accumulated state) must be managed by an external
controller or by patching the CR manually.

This fork adds two new virtual node types (`specPatch`, `stateWrite`), a suite
of CEL library extensions, and supporting infrastructure that together allow an
RGD author to express full state-machine logic declaratively — driving
multi-step workflows, convergence loops, and reactive state transitions purely
from the RGD YAML.

---

## 1. `specPatch` — CEL write-back to `spec`

### What it does

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

### Real-world use cases

- **Multi-step approval workflow**: An `ApprovalRequest` CR passes through
  states `submitted → reviewing → approved → provisioned`. Each state
  transition is a specPatch node gated on the previous state value.
- **Retry counter with backoff**: A `JobRun` CR tracks `retryCount` and
  `nextRetryAt`. A specPatch node increments `retryCount` and computes the
  next retry timestamp after each failure, stopping when `retryCount >= maxRetries`.
- **Quota accumulation**: A `Namespace` CR tracks `usedCPU` and `usedMemory`.
  A specPatch node recalculates totals by summing over child resource specs each
  reconcile cycle.
- **Certificate rotation readiness**: A `TLSBundle` CR has a `renewedAt`
  timestamp. A specPatch node computes `daysUntilExpiry` from the certificate's
  `notAfter` field so downstream `readyWhen` expressions can gate on the
  derived value.

### YAML syntax

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

### Implementation notes

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

---

## 2. `stateWrite` — CEL write-back to `status.kstate`

### What it does

A `stateWrite` node is a virtual node that evaluates CEL expressions and writes
the computed values to `status.kstate.*` on the instance CR via the
`/status` subresource. Unlike `specPatch`, the written values live in `status`
and are therefore invisible to GitOps tooling (Argo CD, Flux) — they do not
appear in the Git manifest and do not cause drift detection to fire.

`status.kstate` is an opaque, schema-free object injected by kro:
`x-kubernetes-preserve-unknown-fields: true` is added to the CRD automatically
when any stateWrite node is present (see §4). Values written there persist
across reconcile cycles and can be read back by subsequent stateWrite
expressions via `schema.status.kstate.*`.

When a stateWrite node mutates kstate, the controller schedules a requeue so
that the next reconcile cycle reads the updated kstate and can fire further
nodes.

### Real-world use cases

- **Convergence step counter**: A `DatabaseMigration` CR needs to run N
  migration scripts in sequence. A stateWrite node tracks which step has been
  applied in `status.kstate.lastAppliedStep`. Each reconcile cycle applies the
  next script and increments the counter.
- **Observed generation bookkeeping**: A custom resource needs to detect when
  its `spec` was last changed and record it in a controller-private field
  without exposing it as a user-settable spec field.
- **Leader election state**: A `ClusterScan` CR has multiple replicas competing
  to run. A stateWrite node records `status.kstate.leaseHolder` and
  `status.kstate.leaseExpiry` as internal bookkeeping without polluting the
  user-facing spec.
- **Transient error accumulation**: A `WebhookSink` CR accumulates consecutive
  error counts in `status.kstate.consecutiveErrors`. A stateWrite node resets
  it to zero on success or increments it on failure, with a specPatch node
  setting `spec.suspended = true` when the threshold is crossed.

### YAML syntax

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

### Implementation notes

- Located in: `pkg/graph/builder.go` (`buildStateWriteNode`), `pkg/runtime/node.go`
  (`EvaluateStateWrite`, `buildContextWithState`), `pkg/controller/instance/resources.go`
  (`processStateWriteNode`), `pkg/controller/instance/status.go` (kstate preservation),
  `pkg/graph/crd/crd.go` (`InjectKstateField`)
- `state:` expressions are compiled in **parse-only mode** (no type checking)
  because the typed CEL environment is built from `schemaWithoutStatus` and has
  no knowledge of `status.kstate.*`. This is a known trade-off: type errors in
  `state:` expressions are caught at runtime, not at RGD validation time
- kstate is preserved across `updateStatus` calls by explicitly re-reading and
  re-injecting it during each status update cycle
- `readyWhen:` and `forEach:` are not supported on stateWrite nodes

---

## 3. `InjectKstateField` — CRD schema auto-injection for `status.kstate`

### What it does

When the kro build phase detects that an RGD contains at least one `stateWrite`
node, it automatically injects a `status.kstate` field into the generated CRD
schema with `x-kubernetes-preserve-unknown-fields: true`. Without this, the
Kubernetes API server would silently strip any `kstate` data from `UpdateStatus`
calls because the field is not declared in the CRD schema.

This is transparent to the RGD author — no manual CRD modification is needed.

### Implementation notes

- Located in: `pkg/graph/crd/crd.go` (`InjectKstateField`)
- Called once per build pass if any stateWrite node is present
- The injected field is always at `status.kstate` and uses open-schema
  (`x-kubernetes-preserve-unknown-fields: true`) so any shape of nested data
  is accepted without a pre-declared schema

---

## 4. `StripExpressionWrapper` — build-time CEL expression parser

### What it does

A utility function in the conditions parser that validates a string is in
`${...}` form and strips the wrapper, returning the bare CEL expression. Used
by the `specPatch` and `stateWrite` node builders when processing `patch:` and
`state:` map values.

This enforces the kro expression convention consistently: map values that look
like `${schema.spec.counter + 1}` are validated to be complete, standalone
`${...}` expressions rather than template strings with embedded fragments.

### Implementation notes

- Located in: `pkg/graph/parser/conditions.go` (`StripExpressionWrapper`)
- Reuses the existing `isStandaloneExpression` validator
- Returns an error if the string is a partial expression like
  `"prefix-${schema.spec.name}"` (template interpolation is not supported in
  `patch:` / `state:` values)

---

## 5. `cel.bind()` support in the AST inspector

### What it does

kro's AST inspector analyzes CEL expressions to extract resource dependencies
— determining which resource IDs an expression references so the DAG can be
wired correctly. The inspector was not aware of `cel.bind()` (the CEL
let-binding extension), causing it to report bound variable names as unknown
resource identifiers and produce incorrect dependency graphs.

This patch adds special handling for `cel.bind(varName, initExpr, bodyExpr)`:
the bound variable is registered as a loop-local identifier (scoped to the body
expression), preventing it from being reported as an unknown resource reference.

### Real-world impact

Without this fix, any RGD expression using `cel.bind()` to define intermediate
computed values would cause the dependency graph to include spurious entries,
potentially making nodes appear to depend on resources that do not exist.

### Example

```yaml
# Without cel.bind: all intermediate values must be inlined (verbose, error-prone)
heroFinalHP: >-
  ${schema.spec.heroHP
    - (schema.spec.poisonTurns > 0 ? 5 : 0)
    - (schema.spec.burnTurns  > 0 ? 8 : 0)}

# With cel.bind: intermediate values are named and reused cleanly
heroFinalHP: >-
  ${cel.bind(poisonDmg, schema.spec.poisonTurns > 0 ? 5 : 0,
     cel.bind(burnDmg,  schema.spec.burnTurns  > 0 ? 8 : 0,
       schema.spec.heroHP - poisonDmg - burnDmg))}
```

### Implementation notes

- Located in: `pkg/cel/ast/inspector.go` (`inspectCall`)
- `ext.Bindings()` is also registered in `BaseDeclarations()` in
  `pkg/cel/environment.go` — without this registration, `cel.bind` parses but
  fails type-checking
- Two new test cases in `pkg/cel/ast/inspector_test.go` cover single and nested
  `cel.bind` usage

---

## 6. `random` CEL library

### What it does

Provides **deterministic pseudo-random generation** seeded by stable string
identifiers. Because kro reconciles continuously, any random value in a CEL
expression must produce the same output for the same inputs — otherwise each
reconcile cycle would compute a different value and the resource would be
continuously re-applied.

Two functions are provided:

| Function | Signature | Description |
|---|---|---|
| `random.seededString` | `(length int, seed string) → string` | Alphanumeric string of `length` chars, deterministically derived from `seed` via SHA-256 |
| `random.seededInt` | `(seed string, max int) → int` | Integer in `[0, max)`, deterministically derived from `seed` via FNV-1a hash |

### Real-world use cases

- **Stable resource name generation**: A `TenantEnvironment` CR provisions N
  named child resources. `random.seededString(8, schema.metadata.uid + "-db")`
  generates a stable, unique 8-char suffix for each resource that remains
  constant across reconciles.
- **Deterministic selection from a pool**: A `LoadTest` CR selects a target
  endpoint from a known list. `random.seededInt(schema.metadata.uid, size(endpoints))`
  produces a stable index that does not change on re-reconcile.
- **Distributed hashing / sharding**: A `CachePartition` CR computes which
  shard owns a given key. `random.seededInt(key, shardCount)` is a stable,
  deterministic shard assignment that can be expressed in CEL without an
  external sidecar.

### YAML syntax

```yaml
# Stable unique suffix for a child resource name
- id: database
  template:
    metadata:
      name: "${schema.metadata.name + '-db-' + random.seededString(6, schema.metadata.uid)}"

# Deterministic assignment of a replica index to a zone
- id: replicaConfig
  template:
    spec:
      zone: "${zones[random.seededInt(schema.metadata.uid + '-zone', size(zones))]}"
```

### Implementation notes

- Located in: `pkg/cel/library/random.go`
- `seededString` uses `crypto/sha256`; when the 32-byte hash is exhausted it
  re-hashes `hash + result_so_far` to produce additional bytes
- `seededInt` uses FNV-1a (`offset=14695981039346656037`, `prime=1099511628211`)
  with a right-shift by 1 to guarantee a non-negative `int64` before taking
  `% max`
- Registered in the base CEL environment via `library.Random()`

---

## 7. `lists` CEL library

### What it does

Provides index-mutation functions for lists. CEL's built-in list operations are
all read-only — there is no way to return a new list with a single element
replaced, inserted, or removed at a specific index. All functions are pure:
they return a new list and do not modify the input.

| Function | Signature | Description |
|---|---|---|
| `lists.set` | `(arr list(int), index int, value int) → list(int)` | Legacy. Returns a new list with `arr[index]` replaced by `value`. Typed `list(int)` only — kept for backwards compatibility with existing RGDs. New code should use `lists.setIndex`. |
| `lists.setIndex` | `(arr list(dyn), index int, value dyn) → list(dyn)` | Returns a new list with the element at `index` replaced by `value`. Works on any element type. |
| `lists.insertAt` | `(arr list(dyn), index int, value dyn) → list(dyn)` | Returns a new list with `value` inserted before `index`. `index == size(arr)` appends. |
| `lists.removeAt` | `(arr list(dyn), index int) → list(dyn)` | Returns a new list with the element at `index` removed. |

### Real-world use cases

- **Per-replica HP tracking**: An array of monster HP values is updated on each
  attack by calling `lists.setIndex(schema.spec.monsterHP, idx, newHP)`.
- **Ordered task queue**: A `Pipeline` CR dequeues the front element on
  completion with `lists.removeAt(schema.spec.pending, 0)` and enqueues new
  work with `lists.insertAt(schema.spec.pending, size(...), newTask)`.
- **Slot-based resource tracking**: A `NodePool` CR tracks available slots as
  an integer array indexed by zone. `lists.setIndex` updates a single zone's
  count without rebuilding the entire array.

### YAML syntax

```yaml
# Replace monster HP at a specific index
- id: applyDamage
  type: specPatch
  patch:
    monsterHP: "${lists.setIndex(schema.spec.monsterHP, idx, newHP)}"

# Remove the first pending task when it completes
- id: dequeue
  type: specPatch
  includeWhen:
    - "${size(schema.spec.pending) > 0}"
  patch:
    pending: "${lists.removeAt(schema.spec.pending, 0)}"
```

### Implementation notes

- Located in: `pkg/cel/library/lists.go`
- `lists.setIndex`, `lists.insertAt`, `lists.removeAt` use `list(dyn)` — any
  element type is accepted; index must be `int`
- `lists.setIndex` and `lists.removeAt` require index in `[0, size(arr))`; out-of-range returns an error
- `lists.insertAt` accepts index in `[0, size(arr)]`; `index == size(arr)` is a valid append
- New lists are built with `types.NewRefValList` — values are not copied through
  native Go slices, so mixed-type lists round-trip correctly
- `lists.set` (legacy) is kept as-is: typed `list(int)`, builds `[]int64` via
  `NativeToValue`, unchanged behaviour for existing callers
- Registered in the base CEL environment via `library.Lists()`

---

## 8. `csv` CEL library

### What it does

Provides manipulation of **comma-separated value strings** — a common pattern
for encoding simple ordered lists in a single Kubernetes string field when a
full array field is impractical or when the field must remain a scalar type.

Three functions are provided:

| Function | Signature | Description |
|---|---|---|
| `csv.remove` | `(csv string, item string) → string` | Removes the first exact occurrence of `item`; returns original if not found |
| `csv.add` | `(csv string, item string, cap int) → string` | Appends `item` if current element count is below `cap`; otherwise returns unchanged |
| `csv.contains` | `(csv string, item string) → bool` | Returns `true` if `item` appears as an exact element in the CSV |

### Real-world use cases

- **Feature flag sets**: A `FeatureGate` CR stores enabled flags as a CSV string
  `"dark-mode,beta-api,new-dashboard"`. `csv.add` and `csv.remove` toggle
  individual flags. `csv.contains` gates downstream resource creation on flag
  presence.
- **Role membership tracking**: A `ClusterMember` CR tracks active role
  assignments as a CSV string. A specPatch node appends a role on grant and
  removes it on revoke, with a capacity limit enforced by `csv.add`.
- **Ordered task queue (bounded)**: A `Pipeline` CR has a `pendingStages` CSV
  field. A specPatch node uses `csv.remove` to dequeue the front element when
  a stage completes, and `csv.add` to enqueue new stages up to a configured cap.

### YAML syntax

```yaml
# Add a role to a member (max 10 roles)
- id: grantRole
  type: specPatch
  includeWhen:
    - "${schema.spec.pendingGrant != '' && !csv.contains(schema.spec.roles, schema.spec.pendingGrant)}"
  patch:
    roles:        "${csv.add(schema.spec.roles, schema.spec.pendingGrant, 10)}"
    pendingGrant: "${''}"

# Remove a role from a member
- id: revokeRole
  type: specPatch
  includeWhen:
    - "${schema.spec.pendingRevoke != ''}"
  patch:
    roles:         "${csv.remove(schema.spec.roles, schema.spec.pendingRevoke)}"
    pendingRevoke: "${''}"
```

### Implementation notes

- Located in: `pkg/cel/library/csv.go`
- An empty string is treated as zero elements (not a single empty element)
- `csv.remove` removes only the first occurrence
- `csv.contains` is exact per-element match, not a substring search
- Registered in the base CEL environment via `library.CSV()`

---

## 9. CEL environment — new library registrations

### What changed

`pkg/cel/environment.go` — `BaseDeclarations()` now registers the following in
addition to the libraries that were already present:

| Added registration | Provides |
|---|---|
| `ext.Bindings()` | `cel.bind(var, init, body)` let-binding syntax |
| `library.Random()` | `random.seededString`, `random.seededInt` |
| `library.Lists()` | `lists.set` (legacy), `lists.setIndex`, `lists.insertAt`, `lists.removeAt` |
| `library.CSV()` | `csv.remove`, `csv.add`, `csv.contains` |

The base environment is computed once via `sync.Once` and cached. All
per-expression environments extend the base via `base.Extend()`, so every CEL
expression evaluated anywhere in kro — templates, `readyWhen`, `includeWhen`,
`patch`, `state`, status projections — has access to the full library set.

Because kro's AST inspector derives its known-function set from the CEL
environment (`env.Functions()`), the new functions are automatically
recognized as known (non-unknown) identifiers without any additional
hardcoding in the inspector.

---

## Combining the features: a complete example

The following RGD sketch illustrates how the features compose. A
`DatabaseCluster` CR drives its own provisioning through four phases using
only kro — no external operator required.

```yaml
apiVersion: kro.run/v1alpha1
kind: ResourceGraphDefinition
metadata:
  name: database-cluster
spec:
  schema:
    apiVersion: v1alpha1
    kind: DatabaseCluster
    spec:
      replicas:    { type: integer }
      tier:        { type: string }   # "bronze" | "silver" | "gold"
      phase:       { type: string, default: "initializing" }
      phaseSeq:    { type: integer, default: 0 }
      instanceIds: { type: string }   # CSV of provisioned instance IDs

  resources:

    # ── Phase transition: initializing → provisioning ─────────────────────
    - id: startProvisioning
      type: specPatch
      includeWhen:
        - "${schema.spec.phase == 'initializing'}"
      patch:
        phase:    "${'provisioning'}"
        phaseSeq: "${schema.spec.phaseSeq + 1}"

    # ── Provision each replica with a stable seeded ID ────────────────────
    - id: provisionReplica
      type: specPatch
      includeWhen:
        - "${schema.spec.phase == 'provisioning' &&
             size(schema.spec.instanceIds) == 0}"
      patch:
        instanceIds: >-
          ${csv.add(
            schema.spec.instanceIds,
            random.seededString(12, schema.metadata.uid + '-r0'),
            schema.spec.replicas
          )}

    # ── Track provisioning progress in controller-private state ──────────
    - id: trackProgress
      type: stateWrite
      includeWhen:
        - "${schema.spec.phase == 'provisioning'}"
      state:
        provisionedCount: >-
          ${has(schema.status.kstate.provisionedCount)
            ? schema.status.kstate.provisionedCount + 1
            : 1}
        lastProvisionedAt: "${schema.metadata.resourceVersion}"

    # ── Transition to ready once all replicas are provisioned ─────────────
    - id: markReady
      type: specPatch
      includeWhen:
        - "${schema.spec.phase == 'provisioning' &&
             has(schema.status.kstate.provisionedCount) &&
             schema.status.kstate.provisionedCount >= schema.spec.replicas}"
      patch:
        phase:    "${'ready'}"
        phaseSeq: "${schema.spec.phaseSeq + 1}"

    # ── Actual StatefulSet — only created once phase is ready ─────────────
    - id: statefulSet
      template:
        apiVersion: apps/v1
        kind: StatefulSet
        metadata:
          name: "${schema.metadata.name}"
        spec:
          replicas: "${schema.spec.replicas}"
      readyWhen:
        - "${statefulSet.status.readyReplicas == schema.spec.replicas}"
      includeWhen:
        - "${schema.spec.phase == 'ready'}"
```

---

## File index

| File | Change type | Summary |
|---|---|---|
| `api/v1alpha1/resourcegraphdefinition_types.go` | Modified | Added `Type`, `Patch`, `State` fields to `Resource`; updated XValidation rule |
| `pkg/graph/node.go` | Modified | Added `NodeTypeSpecPatch`, `NodeTypeStateWrite`; `SpecPatch`, `StateWrite`, `CompiledSpecPatch`, `CompiledStateWrite` fields |
| `pkg/graph/builder.go` | Modified | `buildSpecPatchNode`, `buildStateWriteNode`, dependency extractors, nil-schema guard in `collectNodeSchemas` |
| `pkg/graph/parser/conditions.go` | Modified | Added `StripExpressionWrapper` |
| `pkg/graph/crd/crd.go` | Modified | Added `InjectKstateField` |
| `pkg/runtime/node.go` | Modified | `EvaluateSpecPatch`, `EvaluateStateWrite`, `buildContextWithState`; nil returns for virtual node types |
| `pkg/runtime/runtime.go` | Modified | `StateWriteMutated` requeue handling |
| `pkg/controller/instance/context.go` | Modified | Added `StateWriteMutated bool` to `ReconcileContext` |
| `pkg/controller/instance/resources.go` | Modified | `processSpecPatchNode`, `processStateWriteNode`; per-node SSA field manager; apply-results skip for virtual nodes |
| `pkg/controller/instance/status.go` | Modified | kstate preservation in `updateStatus`; `sanitizeForJSON` to handle `[]byte` from Secret `.data` fields |
| `pkg/cel/environment.go` | Modified | Registered `ext.Bindings()`, `library.Random()`, `library.Lists()`, `library.CSV()` |
| `pkg/cel/ast/inspector.go` | Modified | `cel.bind` variable scoping in `inspectCall` |
| `pkg/cel/ast/inspector_test.go` | Modified | Two new `cel.bind` test cases |
| `pkg/cel/library/random.go` | New | `random.seededString`, `random.seededInt` |
| `pkg/cel/library/random_test.go` | New | Tests for random library |
| `pkg/cel/library/lists.go` | Modified | Added `lists.setIndex`, `lists.insertAt`, `lists.removeAt` (dyn); `lists.set` kept as legacy |
| `pkg/cel/library/lists_test.go` | Modified | Tests for all four list functions including composition cases |
| `pkg/cel/library/csv.go` | New | `csv.remove`, `csv.add`, `csv.contains` |
| `pkg/cel/library/csv_test.go` | New | Tests for csv library |
| `helm/crds/kro.run_resourcegraphdefinitions.yaml` | Modified | Updated CRD schema to include `type`, `patch`, `state` fields on `spec.resources.items` |
| `Dockerfile.fix` | New | Minimal distroless Dockerfile for building custom kro controller images |
