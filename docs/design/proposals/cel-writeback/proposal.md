# KREP-021: Stateful Transitions in ResourceGraphDefinitions

**Author:** Rafael Panazzo ([@pnz1990](https://github.com/pnz1990))  
**Status:** Implemented (see §Validation)  
**Filed:** 2026-03-09  
**Updated:** 2026-03-12

---

## Problem statement

kro's CEL expressions are read-only. They evaluate against a snapshot of the
parent instance CR and produce child resources. There is no way for an RGD to
persist a computed value back into the instance across reconcile cycles.

This blocks a class of real controller patterns that are fundamentally
stateful: read the current state, compute the next state, write it somewhere
the next reconcile can see it. Every operator who needs this today writes
a bespoke controller to do the read-compute-patch cycle — which is exactly what
kro already does, minus the last step.

Some concrete examples where the gap shows up:

- A `DatabaseMigration` CR needs to track which migration step was last applied
  so it can pick up where it left off after a pod restart.
- A `CertificateBundle` CR needs to record `lastRotatedAt` after a rotation Job
  succeeds so future reconciles can compute when the next rotation is due.
- A `Deployment` CR with progressive delivery semantics needs to advance
  `phase: canary → stable` once a traffic metric crosses a threshold, and hold
  it there until a rollback signal arrives.
- A `WebhookSink` CR needs to count consecutive delivery failures and suspend
  itself after N failures — then reset the counter on success.

In each case the missing ingredient is the same: a way for kro to write a
computed value back into the instance CR so that subsequent reconcile cycles can
read it as input.

## Proposal

Add two new virtual node types to the RGD `resources` list:

- **`specPatch`** — evaluates CEL expressions and writes the computed values
  back into `spec` fields of the instance CR via Server-Side Apply.
- **`stateWrite`** — evaluates CEL expressions and writes the computed values
  into a dedicated `status.kstate` sub-object, leaving `spec` untouched.

Virtual here means these nodes do not create a Kubernetes resource. They
participate in the DAG normally — they can depend on child resource statuses,
and other nodes can depend on them — but their reconcile action is a patch to
the instance CR rather than an Apply to a child resource.

Both node types are idempotent. Each compares its computed output against the
current CR state before issuing a patch. If the values already match, no patch
is issued and no watch event is generated.

#### Overview

An RGD with write-back nodes looks like this:

```yaml
resources:
  - id: rotationJob
    template:
      apiVersion: batch/v1
      kind: Job
      # ...

  - id: recordRotation
    type: stateWrite
    includeWhen:
      - "${rotationJob.status.succeeded == 1}"
    state:
      lastRotatedAt:   "${rotationJob.status.completionTime}"
      nextRotationDue: "${rotationJob.status.completionTime + schema.spec.rotationIntervalSeconds}"
```

The `type` field on a resource entry is the discriminator. When `type` is
`specPatch` or `stateWrite`, the entry carries a `patch:` or `state:` map
of `fieldName: "${celExpression}"` pairs instead of a `template:`. The
`${...}` wrapper is the existing kro expression convention and is stripped
before CEL compilation.

The node fires on each reconcile cycle where `includeWhen` is true and the
computed values differ from the live CR. Once the values match, `includeWhen`
typically becomes false (because the trigger condition is no longer true) and
the node is skipped cleanly.

#### Design details

##### `specPatch`

Writes computed values into the instance CR's `spec` using Server-Side Apply
with field manager `kro.run/specpatch-<nodeID>`. Each node gets its own field
manager so that two specPatch nodes owning disjoint sets of fields do not
interfere with each other's ownership.

Because the written fields live in `spec`, they are visible to the next
reconcile cycle as ordinary spec fields — no special CEL context handling is
needed. The pattern for a convergent counter looks like:

```yaml
- id: incrementStep
  type: specPatch
  includeWhen:
    - "${schema.spec.currentStep < schema.spec.totalSteps}"
  patch:
    currentStep: "${schema.spec.currentStep + 1}"
```

Each reconcile cycle where `currentStep < totalSteps` increments the field by
one. When `currentStep == totalSteps`, `includeWhen` is false and the node
stops firing. The gate in `includeWhen` is what prevents an infinite loop —
without it, the node would fire forever.

The trade-off for `specPatch` is GitOps compatibility. A field written by
`specPatch` lives in `spec`, and a Git manifest for the same CR also declares
`spec` fields. GitOps tools that use non-SSA apply (the Argo CD default) will
overwrite `specPatch`-managed fields on every sync. Using `specPatch` requires
either configuring Argo CD in SSA mode or adding the managed fields to
`ignoreDifferences`. This is a real operational cost.

##### `stateWrite`

Writes computed values into `status.kstate.*` on the instance CR using the
`/status` subresource. GitOps tools never touch `status` — this is the core
reason `stateWrite` exists alongside `specPatch` rather than replacing it.

The `stateWrite` node reads prior kstate values via `schema.status.kstate.*`.
To support this, the CEL context for stateWrite nodes is built with `status`
re-injected (the standard kro context strips status from the instance). On
first reconcile, `status.kstate` does not exist yet, so expressions that
reference their own prior state must use `has()`:

```yaml
- id: trackStep
  type: stateWrite
  includeWhen:
    - "${!has(schema.status.kstate.step) || schema.status.kstate.step < schema.spec.totalSteps}"
  state:
    step: "${has(schema.status.kstate.step) ? schema.status.kstate.step + 1 : 1}"
```

The `has()` bootstrap is required whenever a stateWrite expression self-references
a kstate field. It is not needed for expressions that only read from `spec`.

The generated CRD for any RGD with stateWrite nodes automatically gains
`status.kstate: {type: object, x-kubernetes-preserve-unknown-fields: true}` in
its schema. Without this, the Kubernetes API server would silently strip kstate
fields from every UpdateStatus call. This injection happens at build time in
`InjectKstateField()`.

`stateWrite` expressions are compiled in parse-only mode (no type checking)
because the CEL environment is built from `schemaWithoutStatus` and has no
knowledge of `status.kstate.*`. Type errors in state expressions are caught at
runtime rather than at RGD validation time. This is a known trade-off.

When a stateWrite node mutates kstate, the reconciler schedules a requeue.
This is necessary because status changes do not advance the spec
`resourceVersion` and therefore do not automatically trigger a new reconcile.

##### How the two types complement each other

`specPatch` and `stateWrite` are not redundant. They address different ownership
semantics:

`specPatch` is for fields that need to be visible to the user and to other
systems — a phase string that drives `includeWhen` expressions in child resource
templates, a counter that a human operator can inspect with `kubectl get`, a
timestamp that an external system reads. The cost is GitOps friction.

`stateWrite` is for bookkeeping that belongs to the controller — step counters,
internal timestamps, derived flags that no one outside kro needs to set or read
directly. The benefit is that GitOps tooling is entirely unaware of these fields.

An RGD can use both in the same resource graph. For example: a `specPatch` node
advances a user-visible `phase` field that downstream resources gate on, while
a `stateWrite` node tracks the internal retry count that the phase transition
logic reads.

##### CEL extensions

Standard CEL lacks functions needed for common controller patterns. This
proposal includes three new libraries registered in kro's base CEL environment,
available in all expression contexts (templates, `readyWhen`, `includeWhen`,
`patch:`, `state:`):

**`random`** — deterministic pseudo-random generation seeded by a stable string.
Essential because kro reconciles continuously: a non-deterministic function
would compute a new value on every reconcile cycle and drive constant re-apply.

```
random.seededString(length int, seed string) → string
random.seededInt(seed string, max int) → int
```

`seededString` uses SHA-256; `seededInt` uses FNV-1a. Both produce the same
output for the same inputs regardless of when or how many times they are called.

Use this when you need a stable resource name suffix, a deterministic selection
from a pool, or any other random-looking-but-reproducible value derived from an
instance UID or resource name.

**`lists`** — functional mutation for integer arrays.

```
lists.set(arr list(int), index int, value int) → list(int)
```

CEL has no way to return a new list with a single element replaced. This fills
that gap. It produces a new list; the input is not modified.

**`csv`** — comma-separated value string manipulation.

```
csv.remove(csv string, item string) → string
csv.add(csv string, item string, cap int) → string
csv.contains(csv string, item string) → bool
```

CSV-encoded strings in spec fields are a common pattern for representing small
ordered sets without declaring an array field. These functions let specPatch
nodes add and remove elements from such strings declaratively.

**`cel.bind()` support in the AST inspector**

kro's AST inspector extracts resource dependencies from CEL expressions to wire
the DAG. It was not aware of `cel.bind()`, causing bound variable names to be
reported as unknown resource references. The fix scopes bound variable names as
loop-local identifiers, consistent with the existing handling of comprehension
variables. `ext.Bindings()` is also registered in the base CEL environment so
that `cel.bind` expressions pass type-checking.

`cel.bind` matters for specPatch and stateWrite because combat-math-style
expressions — any expression with more than two or three intermediate values —
become unreadable when every intermediate value must be inlined. Without
`cel.bind`, the alternative is splitting the computation across multiple nodes,
which adds reconcile latency for no semantic reason.

## Other solutions considered

**External bespoke controller (status quo)**

The current approach: write a separate controller that watches for a triggering
condition, reads the parent CR, computes the next state, and patches. This works
and is what every production kro user who needs stateful transitions is doing
today. The cost is that kro stops being a complete solution for those workflows.
For each new transition rule, you add Go code and deploy another binary.

**Alternative C: `function` — external HTTP/gRPC delegation**

A `function` virtual node type sends the full instance CR to an HTTP endpoint
and applies the returned patch. The endpoint can contain arbitrary logic.

```yaml
- id: computeTransition
  type: function
  endpoint: "http://transition-server.my-namespace.svc:8080/advance"
  includeWhen:
    - "${schema.spec.phase == 'pending'}"
```

This was implemented and validated as part of this work. The conclusion: it is
a useful escape hatch for logic that cannot be expressed in CEL, but the wrong
primary answer for the cases this proposal addresses. CEL arithmetic on integer
fields does not warrant a running HTTP service. The operational overhead
(deploying, versioning, and operating the function server) outweighs the benefit
for simple state transitions.

`function` is worth a separate proposal for users who need external data lookups,
complex algorithms, or probabilistic computation inside a kro RGD. It is out of
scope here.

## Scoping

#### What is in scope

- `specPatch` virtual node type: CEL expressions → SSA patch to instance `spec`.
- `stateWrite` virtual node type: CEL expressions → UpdateStatus patch to
  `status.kstate.*`.
- Per-node SSA field managers for specPatch (`kro.run/specpatch-<id>`).
- Automatic `status.kstate` CRD schema injection for RGDs with stateWrite nodes.
- `includeWhen` integration: existing conditional execution semantics apply
  unchanged.
- Idempotency: no patch when computed values already match live CR state.
- DAG ordering: write-back nodes participate in topological ordering normally.
- `random`, `lists`, `csv` CEL libraries registered in the base environment.
- `cel.bind()` support in the AST inspector and CEL environment.

#### What is not in scope

- Time-triggered transitions — kro's reconciler is event-driven; `requeueAfter`
  is a separate proposal.
- Cross-instance mutations — write-back nodes can only target the current
  instance CR.
- Transactional atomicity across multiple write-back nodes in a single reconcile
  cycle. The new value written by node A is not visible to node B in the same
  pass; it becomes visible on the next reconcile.
- `function` node type (HTTP/gRPC escape hatch) — separate proposal.
- Admission-time warning for non-convergent expressions (expression reads and
  writes the same field without an `includeWhen` gate). Useful but not required
  for correctness.

## Testing strategy

#### Requirements

- A running kro-enabled Kubernetes cluster with the custom controller image
  deployed.
- RGD YAML fixtures for each validation scenario.

#### Test plan

Unit tests cover: new node type dispatch in `GetDesired()`, `EvaluateSpecPatch()`,
`EvaluateStateWrite()`; nil-schema guard in `collectNodeSchemas`; dependency
extraction from `patch:` and `state:` expressions; `StripExpressionWrapper`
validation; `cel.bind` scoping in the AST inspector (two cases: single bind,
nested binds).

Integration tests cover: specPatch counter converges to target value then stops;
stateWrite counter converges to target value then stops; idempotency (second
reconcile after convergence issues no patch); `includeWhen = false` skips node
and issues no patch; specPatch and stateWrite nodes active simultaneously without
interference; all existing RGD integration tests continue to pass.

CEL library tests cover: `random.seededInt` and `random.seededString` produce
identical output across repeated calls with the same seed; `lists.set` produces
correct output and does not mutate the input; `csv.add/remove/contains` handle
empty string, cap enforcement, non-existent item, and repeated-item edge cases.

## Validation

This proposal was not just designed — it was fully implemented, deployed to a
production EKS cluster, and validated against a comprehensive integration test
suite before being submitted. What follows is an honest account of what that
process uncovered.

#### Implementation scope

The final implementation spans 21 files and roughly 1,600 lines of Go (net, after
accounting for deletions):

| Area | Files changed |
|---|---|
| RGD API types | `api/v1alpha1/resourcegraphdefinition_types.go` |
| Graph build | `pkg/graph/builder.go`, `pkg/graph/node.go`, `pkg/graph/parser/conditions.go`, `pkg/graph/crd/crd.go` |
| Runtime | `pkg/runtime/node.go`, `pkg/runtime/runtime.go` |
| Controller | `pkg/controller/instance/resources.go`, `pkg/controller/instance/context.go`, `pkg/controller/instance/status.go` |
| CEL environment | `pkg/cel/environment.go`, `pkg/cel/ast/inspector.go`, `pkg/cel/ast/inspector_test.go` |
| CEL libraries | `pkg/cel/library/random.go`, `pkg/cel/library/random_test.go`, `pkg/cel/library/lists.go`, `pkg/cel/library/lists_test.go`, `pkg/cel/library/csv.go`, `pkg/cel/library/csv_test.go` |
| CRD | `helm/crds/kro.run_resourcegraphdefinitions.yaml` |

#### Bugs found during implementation (documented honestly)

Several non-obvious problems surfaced during implementation that are worth
recording here — both because they inform the design and because they would
affect anyone implementing this independently.

**specPatch: shared SSA field manager drops ownership**

The initial implementation used a single field manager `kro.run/specpatch` for
all specPatch nodes. When nodeA fires and writes `{X, Y}` with that manager, and
later nodeB fires and writes `{Y, Z}` with the same manager, the API server
transfers ownership of `Y` from nodeA's reconcile to nodeB's — and drops
nodeA's ownership of `X`, reverting it to the schema default. The fix: each
specPatch node uses `kro.run/specpatch-<nodeID>` as its field manager. This is
not obvious from reading the SSA documentation.

**stateWrite: `updateStatus` was wiping kstate every reconcile**

kro's `updateStatus()` builds a completely fresh status map from scratch
(conditions + state + CEL-resolved fields) and calls
`cur.Object["status"] = status`. This overwrites the entire status object,
including any `kstate` written by stateWrite nodes earlier in the same
reconcile. The fix: before calling UpdateStatus, read `status.kstate` from the
in-memory instance, merge it into the new status map, and then write. This
required threading kstate preservation through two code paths: the normal
updateStatus path and the RetryOnConflict fetch-before-write path inside it.

**stateWrite: status never reconciles if `status.kstate` is not in the CRD schema**

The API server silently discards fields not declared in the CRD schema on
UpdateStatus calls. `status.kstate` is injected at runtime by kro — it does not
appear in the RGD's schema section — so it was being stripped silently. The fix:
`InjectKstateField()` in the build phase adds `status.kstate` with
`x-kubernetes-preserve-unknown-fields: true` to the generated CRD when any
stateWrite node is present. This is the primary architectural addition required
for `stateWrite` beyond what you would initially estimate.

**stateWrite: first reconcile crashes with "no such key: status"**

On a freshly created instance CR, `status` itself does not exist in the
unstructured object (the API server creates it as empty, but it may not be
populated in the in-memory representation kro uses). `buildContextWithState()`
tried to read `schema.status.kstate` and received a CEL "no such key" error.
The fix: always inject `status: {kstate: {}}` as a baseline before merging
actual kstate values into the CEL context.

**stateWrite: `includeWhen` uses the wrong CEL context**

`includeWhen` evaluation calls the standard `buildContext()` which strips
`status`. A stateWrite node whose `includeWhen` references `schema.status.kstate`
(which is the common case — checking whether a prior kstate value exists before
writing) was always evaluating as if kstate was empty. The fix: override
`IsIgnored()` for `NodeTypeStateWrite` to use `buildContextWithState()` instead
of `buildContext()`.

**stateWrite: reconcile does not re-trigger after kstate write**

kro watches child resources for changes to drive reconcile. A status subresource
update does not advance `spec.resourceVersion` and therefore does not produce a
watch event that triggers another reconcile. After kstate is written once, the
reconciler finished normally and the counter was stuck at 1. The fix: set
`StateWriteMutated = true` in `ReconcileContext` when a stateWrite node fires,
check it at the end of `ReconcileResources`, and call `delayedRequeue` if set.

**`[]byte` from Secret `.data` fields panics in `updateStatus`**

Kubernetes Secrets store `.data` values as `[]byte` in Go's unstructured
representation. When a Loot CR's status expression referenced `lootSecret.data.*`
and the value was placed into the desired status map, `unstructured.NestedMap`
(which deep-copies) panicked on `cannot deep copy []uint8`. The fix: replace
`NestedMap` with `NestedFieldNoCopy` and add a `sanitizeForJSON()` helper that
recursively converts `[]byte` → `string` before storing values in status. This
bug is not related to write-back but was discovered in the same test environment
and is included here because it would affect any kro user whose status
expressions reference Secret data fields.

**`cel.bind` initial AST inspector fix was incomplete**

The first inspector fix registered the bind variable in `loopVars` and inspected
the body but forgot to inspect the init expression (the second argument to
`cel.bind`). Any resource reference inside the init expression was therefore
invisible to the dependency graph. This was caught by the second test case
(nested binds with resource refs in init exprs).

#### Verification results

All tests were run against a live EKS cluster (Amazon EKS 1.34, `us-west-2`)
with the custom kro controller image deployed:

```
Integration tests:     30/30 pass
CEL library tests:     all pass (random, lists, csv, inspector)
Graph builder tests:   all pass
Production RGDs:       19/19 Active (no regressions)
```

Two test RGDs were deployed to verify convergence behavior:

**specpatch-counter-test** — a `spec.counter` field incremented from 0 to 5 by a
single specPatch node with `includeWhen: counter < 5`. After 5 reconcile cycles
(~5 seconds), counter reached 5, `includeWhen` became false, and no further
patches were issued. The SSA field manager `kro.run/specpatch-incrementCounter`
is visible in `managedFields`.

**statewrite-counter-test** — `status.kstate.counter` incremented from absent to
5 by a stateWrite node using the `has()` bootstrap pattern. After 5 requeue
cycles, counter reached 5, `includeWhen` became false, no further patches were
issued. The instance remained Active.

Both were run simultaneously to confirm the two node types do not interfere with
each other.

#### Reconcile amplification

Both node types require one reconcile cycle per state transition. A sequence of
N transitions takes approximately N seconds on a lightly loaded cluster (the
default `delayedRequeue` is 3 seconds). This is expected and documented. It is
inherent to kro's event-driven model and is not a regression.

For use cases that require multiple sequential transitions, the latency can be
reduced by chaining multiple specPatch or stateWrite nodes in a single reconcile
(each node fires once per cycle; N nodes in one cycle = N transitions per cycle).

## Discussion and notes

**On the choice between specPatch and stateWrite**

Both were implemented and both are included in this proposal because they solve
legitimately different problems. The decision of which to use for a given field
is a design question for the RGD author, not a choice the proposal should make
for them.

A rough rule of thumb: if the field appears in your Git manifest or if another
system reads it via `kubectl get`, use `specPatch`. If it is purely bookkeeping
that kro maintains for its own use, use `stateWrite`.

**On the `has()` bootstrap verbosity for stateWrite**

The required `has(schema.status.kstate.fieldName) ? ... : initialValue` pattern
is verbose, and it can be easy to forget. A future improvement could be a
`kstate()` helper function that handles the absent-on-first-reconcile case
transparently: `kstate("fieldName", defaultValue)`. This is out of scope for
this proposal.

**On CEL type safety for stateWrite**

stateWrite expressions are parse-only (no type checking) because the typed CEL
environment does not include `status.kstate.*`. This is a real trade-off: typos
in stateWrite field names are silent at RGD admission time and only manifest as
runtime nil values. A future improvement could maintain a separate partial type
environment for kstate fields derived from the `state:` map declarations of
stateWrite nodes. This is out of scope here.

**Precedent**

The `specPatch` pattern is analogous to Kubernetes Late Initialization (official
pattern: scheduler writes `spec.nodeName`, HPA writes `spec.replicas`). The
SSA field manager is the mechanism the ecosystem settled on for safe
controller-writes-spec.

The `stateWrite` pattern is analogous to Crossplane's `status.atProvider` — a
controller-owned sub-object within status that accumulates observed state without
conflicting with the user-owned `spec`.
