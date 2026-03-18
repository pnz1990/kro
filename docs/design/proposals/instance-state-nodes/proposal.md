# KREP-023: Instance State Nodes

**Author:** Rafael Panazzo ([@pnz1990](https://github.com/pnz1990))
**Status:** Draft
**Filed:** 2026-03-17

---

## Problem statement

kro's CEL expressions are strictly read-only projections. They evaluate against
a snapshot of the parent instance CR and produce child resources. The output of
one reconcile cycle cannot feed as input to the next. This makes it impossible
to express stateful transitions — patterns where the computed result of a
reconcile depends on the previous value of a field.

Concrete patterns that are blocked today:

- **Accumulation**: `retryCount = retryCount + 1`, `usedQuota = usedQuota + delta`
- **Mutation**: `items[i].status = "applied"`, `inventory = inventory.filter(x, x != usedItem)`
- **Lifecycle transitions**: `phase: pending → reviewing → approved → provisioned`, each gated on the previous phase value
- **Counters with TTL**: `backstabCooldown = backstabCooldown - 1` per turn, stop at zero

These patterns are common in platform engineering — cert rotation scheduling,
progressive delivery step tracking, autoscaling cooldown windows, quota
enforcement, retry budgets with backoff.

The missing ingredient in every case is the same: a way for kro to write a
computed value back into the instance CR so that subsequent reconcile cycles
(or subsequent nodes within the same cycle) can read it as input.

The closest existing primitives do not cover this:

- `schema.status` projections — read-only, no self-reference, no accumulation
- KREP-011 data nodes — ephemeral within one reconcile cycle, not persisted to the API server
- KREP-016 patch nodes — writes to resources kro does not own, not to self

## Proposal

Add a new virtual node type — `state:` — that evaluates CEL expressions and
writes the results to a named storage scope under `status` on the instance CR.
The written values persist across reconcile cycles and are readable by
subsequent CEL expressions in the same or future cycles.

```yaml
resources:
  - id: rotationJob
    template:
      apiVersion: batch/v1
      kind: Job
      # ...

  - id: recordRotation
    state:
      storeName: rotation
      fields:
        lastRotatedAt:    "${rotationJob.status.completionTime}"
        rotationCount:    "${has(schema.status.rotation.rotationCount) ? schema.status.rotation.rotationCount + 1 : 1}"
    includeWhen:
      - "${rotationJob.status.succeeded == 1}"
```

The written values are available to subsequent nodes in the same cycle and to the next reconcile cycle:

```yaml
  - id: notifyOnThirdRotation
    template:
      # ...
    includeWhen:
      - "${has(schema.status.rotation.rotationCount) && schema.status.rotation.rotationCount >= 3}"
```

#### Overview

A `state:` node is a virtual node — it creates no Kubernetes resource. It
participates in the DAG normally, can depend on child resource statuses, and
can be depended upon by other nodes. Its reconcile action is a status patch to
the instance CR rather than an Apply to a child resource.

After a state node writes its values, the controller refreshes the in-memory
CEL context with the new `status.<storeName>` values. This means that state
nodes later in the DAG within the same reconcile cycle can read the values just
written — they are not deferred to the next cycle. Only values that have not
yet been written in the current cycle require a requeue.

This within-cycle context refresh is what makes multi-step state machine
patterns efficient. N state nodes in one RGD can advance N fields in one
reconcile cycle rather than requiring N consecutive cycles.

#### Design details

##### Node structure

A `state:` block is mutually exclusive with `template:` and `externalRef:`.
It has two fields:

- `storeName` (string, required) — the name of the field written under
  `status`. Must be a valid Go identifier. kro auto-injects this field into the
  generated CRD schema with `x-kubernetes-preserve-unknown-fields: true` when
  any state node targeting it is present in the RGD. The name must not collide
  with `state`, `conditions`, or `managedResources` (reserved by kro's own
  status management), nor with any field name declared in the RGD's own
  `schema.status` block (user-defined projection fields). Either collision is
  rejected at admission time with a descriptive validation error. The
  schema.status collision rule prevents `updateStatus()` from silently
  overwriting state node values with CEL projection outputs (or vice versa)
  during the end-of-reconcile status merge.

- `fields` (map[string]CEL expression, required) — key-value pairs where every
  value is a `${...}` CEL expression. Bare strings (no `${}` wrapper) are a
  validation error.

`includeWhen` is supported. `readyWhen` is not applicable — a
state node is considered ready as soon as its expressions evaluate and the
status patch succeeds. `forEach` is not supported on state nodes in v1 — the
semantics of writing multiple `storeName`-scoped entries from a single node
are non-trivial and left for a follow-on proposal once the base primitive is
established. For example: does a `forEach` with three iterations produce
`status.migration.items = [{...}, {...}, {...}]` (one array field) or merge
all three iterations into a flat `status.migration = {step0: ..., step1: ...,
step2: ...}` (one map field per iteration)? Neither answer is obvious, and
the right design depends on use cases that are not yet established.

##### kstate() — bootstrap guard helper

State node expressions that self-reference a field they write require a
`has()` guard on first reconcile (see "Bootstrap guard" below). The verbosity
of the nested ternary pattern is mitigated by the built-in `kstate()` CEL
function, which ships with v1 with typed overloads:

```
kstate(scope map, field string, default int)    → int
kstate(scope map, field string, default string) → string
kstate(scope map, field string, default bool)   → bool
kstate(scope map, field string, default dyn)    → dyn   // fallback: selected only when no typed overload matches
```

CEL resolves the overload at compile time based on the type of `default`. When
`default` is a typed literal (e.g., `0`, `""`, `false`), the typed overload is
selected and the return type is known to the compiler — enabling v2 compile-time
type inference for expressions downstream of `kstate()`. When `default` has type
`dyn` (e.g., the result of another dynamic expression), the fallback overload is
selected and the return type is `dyn`. The `dyn` fallback exists as a safety valve
for edge cases; the typed overloads are preferred in all other situations.

`kstate` reads `fieldName` from `scope` and returns `default` if absent. It is
called as:

```yaml
fields:
  step: "${kstate(schema.status.migration, 'step', 0) + 1}"
```

Equivalent to the explicit `has()` form, which remains valid:

```yaml
fields:
  step: "${has(schema.status.migration.step) ? schema.status.migration.step + 1 : 1}"
```

`kstate` accepts any `map(string, dyn)` as its first argument — it is not
specific to state nodes and can be used in any CEL expression that reads an
optional map field. The typed overloads cover scalar types (`int`, `string`,
`bool`). Complex types — lists, maps, doubles, bytes, timestamps — fall through
to the `dyn` fallback overload and remain untyped at compile time. For example,
`kstate(schema.status.delivery, 'steps', [])` matches the `dyn` overload because
`[]` has type `list(dyn)`, not a scalar type. List-typed and map-typed overloads
are deferred to v2 alongside the type inference improvements. A macro form that
accepts a string storeName directly (avoiding the need to pass
`schema.status.<storeName>` as an argument) is also a v2 ergonomics improvement.

##### Within-cycle context refresh

After a state node successfully writes to `status.<storeName>`, the controller
updates the in-memory representation of the instance used by the CEL context
builder — replacing it with the object returned by the `UpdateStatus` call.
Any state node later in
topological order within the same reconcile cycle will read the updated values
from `schema.status.<storeName>.*` directly.

This is analogous to how Server-Side Apply field managers immediately expose
written values to subsequent operations: the write succeeds, the in-memory
state is refreshed, and the next operation reads the updated values without
requiring a round-trip to the API server.

The implication: for a chain of N state nodes where node B reads a field written
by node A, both fire in the same reconcile cycle. The chain advances N steps per
cycle rather than requiring N requeue cycles. The only case that requires a
requeue is when the same state node must read its own prior output (i.e., it
depends on itself across cycles), which is the bootstrap pattern described below.

##### CRD schema auto-injection

The Kubernetes API server silently discards fields not declared in the CRD
schema on `UpdateStatus` calls. For each distinct `storeName` referenced by any
state node in an RGD, kro's build phase automatically injects:

```yaml
status:
  properties:
    <storeName>:
      x-kubernetes-preserve-unknown-fields: true
```

into the generated CRD. This injection happens at build time so that the first
`UpdateStatus` call does not silently drop the written values. Multiple state
nodes targeting the same `storeName` result in exactly one injection.

##### Bootstrap guard

On the first reconcile, `status.<storeName>` does not exist. Any expression
that self-references a state field must guard with `has()` or use the
`kstate()` helper:

```yaml
# Cert rotation counter — increments on each successful rotation
- id: recordRotation
  state:
    storeName: rotation
    fields:
      count:         "${kstate(schema.status.rotation, 'count', 0) + 1}"
      lastRotatedAt: "${rotationJob.status.completionTime}"
  includeWhen:
    - "${rotationJob.status.succeeded == 1}"
    - "${!has(schema.status.rotation.lastRotatedAt) || schema.status.rotation.lastRotatedAt != rotationJob.status.completionTime}"
```

This pattern is consistent with existing kro practice for optional fields in
CEL expressions. The `has()` guard (or `kstate()` equivalent) is required only
for expressions that read a field they themselves write. Expressions that only
read from `spec` or from sibling resources do not need it.

On first reconcile, the controller ensures `status: {<storeName>: {}}` is
injected as a baseline into the CEL context before evaluating state node
expressions, preventing "no such key" errors.

The same `has()` guard requirement applies to template nodes that read
`schema.status.<storeName>.*` fields. If the state node that writes a field
has an `includeWhen` condition that may be false on the first reconcile, the
template node must guard against the absent field:
`${kstate(schema.status.rotation, 'count', 0)}` or
`${has(schema.status.rotation.count) ? schema.status.rotation.count : 0}`.
Failing to guard produces a silent nil value, which the template renders as
a zero or empty default.

For array-typed state fields, `has()` alone is insufficient to guard against
index-out-of-bounds errors: `has(schema.status.delivery.steps)` returns `true`
even when `steps` is an empty list, and an index access on an empty list panics.
Expressions that index into a state array field must also guard on size:

```yaml
fields:
  current: "${has(schema.status.delivery.steps) && size(schema.status.delivery.steps) > idx ? schema.status.delivery.steps[idx] : ''}"
```

The recommended pattern for state nodes that initialize array fields is a
sentinel-first design: a single initialization node with a strict
`includeWhen` gate writes the array and advances a scalar sentinel. All
dependent nodes gate on that sentinel. This is safe because the sentinel is
a scalar — guardable with `has()` alone — and ensures arrays are populated
before any downstream index access:

```yaml
# Progressive delivery — initialize step list on first reconcile
- id: initDelivery
  state:
    storeName: delivery
    fields:
      steps:      "${schema.spec.stages.map(s, {'name': s.name, 'status': 'pending'})}"
      currentIdx: "${0}"
      initSeq:    "${1}"
  includeWhen:
    - "${!has(schema.status.delivery.initSeq) || schema.status.delivery.initSeq == 0}"

# Advance current step when the deployed revision is ready
- id: advanceStep
  state:
    storeName: delivery
    fields:
      steps: "${lists.setAtIndex(schema.status.delivery.steps, schema.status.delivery.currentIdx, {'name': schema.status.delivery.steps[schema.status.delivery.currentIdx].name, 'status': 'complete'})}"
      currentIdx: "${schema.status.delivery.currentIdx + 1}"
  includeWhen:
    - "${has(schema.status.delivery.initSeq) && schema.status.delivery.initSeq > 0}"
    - "${deployedRevision.status.readyReplicas == deployedRevision.spec.replicas}"
    - "${schema.status.delivery.currentIdx < size(schema.status.delivery.steps)}"
```

##### Concurrent writes to the same `storeName`

When two or more state nodes target the same `storeName`, the within-cycle
context refresh ensures they are not independent overwriting operations. The
sequence is:

1. The DAG processes state nodes in topological order.
2. Node A evaluates its `fields` expressions, producing `{step: 1}`. It reads
   the current `status.<storeName>` from the in-memory instance (e.g., `{}`),
   merges its computed values into a copy of that map, and issues an
   `UpdateStatus` call with the merged result: `{step: 1}`.
3. After the write succeeds, the controller refreshes the in-memory instance
   with the returned updated object. The instance now reflects
   `status.<storeName> = {step: 1}`.
4. Node B evaluates its `fields` expressions, producing `{phase: "reviewing"}`.
   It reads the current in-memory `status.<storeName>` — which is now `{step: 1}`
   — merges its values in, and issues `UpdateStatus` with
   `{step: 1, phase: "reviewing"}`. Node A's fields are preserved because the
   merge starts from the current live state, not from an empty map.

The key invariant: each state node's `UpdateStatus` call contains the full
current contents of `status.<storeName>` plus the node's own computed fields.
No prior node's writes are dropped.

If the `UpdateStatus` call encounters a resource version conflict (a concurrent
external write to the same instance), the controller performs a full retry:
re-fetch the live CR, re-evaluate all CEL expressions in the node's `fields`
map against the freshly-fetched instance, merge the new results into the
current `status.<storeName>`, and retry the call. Re-evaluation is required,
not optional. If the retry only re-merges the originally computed values, a
stale result can be written silently: consider a node computing
`step = schema.status.migration.step + 1` where `step` was `0` at first
evaluation. If a concurrent reconcile wrote `step = 1` between the read and
the write, a merge-only retry would write `step = 1` again instead of `step = 2`.
The counter is off by one with no error. Re-evaluating against the fresh
instance produces `step = 2` — the correct result.

##### Within-cycle context refresh implementation

After a state node's `UpdateStatus` call returns successfully, the controller
replaces its in-memory instance object with the returned object, so that
subsequent nodes in the same reconcile cycle read the freshly written
`status.<storeName>.*` values without a round-trip to the API server.

**Implementation note — this is net-new infrastructure.** The current runtime
caches each node's evaluated result in `n.desired` (`node.go`: `if n.desired != nil { return n.desired, nil }`). State nodes do not use `GetDesired()` — they evaluate their `fields` CEL map directly against the current in-memory instance on each call. However, downstream template nodes that depend on a state node DO use `GetDesired()`, and their cached result would reflect the pre-write instance state. After a state node writes and updates the in-memory instance (via `SetObserved()` on the instance node), the implementation must invalidate the `desired` cache (`n.desired = nil`) on:

1. **The instance node itself.** The end-of-reconcile `updateStatus()` calls `Instance().GetDesired()`, which invokes `softResolve()` to evaluate `schema.status` projection expressions. If the instance node's `desired` is cached from before the state node write, the projections will not reflect the freshly written `storeName` values. Note: the instance node is excluded from the topological order and is NOT processed during `processNodes()` — its `GetDesired()` is first called in `updateStatus()`. In practice this means the instance node's cache is nil on the first call (no prior evaluation in this cycle), so invalidation is needed only when a state node writes AFTER another state node has already triggered an instance node evaluation in the same cycle (e.g., via a dependency check).
2. **All downstream template and forEach nodes** that reference `schema.status.<storeName>.*` — so they re-evaluate against the fresh in-memory instance on their next `GetDesired()` call.

This cache invalidation is new behavior — today `desired` is set once and never cleared within a reconcile cycle.

On a resource version conflict, the controller performs a full retry: re-fetch
the live CR, re-evaluate all CEL expressions in the node's `fields` map against
the freshly fetched instance, and retry the `UpdateStatus` call. The in-memory
instance is updated with the final committed state returned by the successful
retry call, not with any intermediate value. If the retry is exhausted, the
in-memory instance is not updated and the node is marked as error.

A requeue is scheduled after the full reconcile cycle completes whenever any
state node wrote new values during that cycle. This ensures cross-cycle
self-referencing patterns (bootstrap pattern) can advance in the next cycle.
For within-cycle chains (node A writes storeName X; node B reads X in the same
cycle), the requeue is not needed for forward progress — it is a safety net for
convergence of the `includeWhen` gate re-evaluation.

##### Idempotency

Before issuing an `UpdateStatus` call, the controller compares the computed
field values against the current `status.<storeName>.*` values in the in-memory
instance. If all values already match, no patch is issued and no requeue is
scheduled. This is an optimization to avoid redundant API calls when the RGD
re-reconciles due to unrelated watch events. It is NOT a correctness mechanism —
a stale informer cache may cause a false negative (cache reports different values
than what is on the API server), resulting in an unnecessary write. That write
either succeeds idempotently (the API server value was already the same) or
conflicts and triggers the re-evaluation retry. In neither case is state
corrupted. Correctness is provided by the conflict retry mechanism, not the
idempotency check.

##### Write rate limit

A state node without a terminating `includeWhen` gate fires on every reconcile
cycle. Each write triggers a MODIFIED watch event, which triggers a new
reconcile immediately — creating a tight write loop bounded only by API server
latency. This is an operational hazard: 100 instances of a runaway RGD can
generate thousands of `UpdateStatus` calls per second.

To prevent runaway loops from overloading the API server, the controller
enforces a minimum write interval per storeName per instance. If a state node
attempts to write to a `storeName` on an instance within `minStateNodeWriteInterval`
of the last successful write to that storeName on that instance, the write is
skipped and a `RequeueAfter(remaining interval)` is scheduled instead.

- **Default interval:** 100ms (configurable via controller flag
  `--min-state-node-write-interval`)
- **Scope:** per storeName per instance — independent instances are not affected
  by each other's write rate
- **Applies only when values changed:** if the idempotency check determines
  values are unchanged, no write is issued and no rate limit applies
- **In-memory:** the last write timestamp is stored in the controller's memory,
  not persisted to the API server. It resets on controller restart. During the
  first burst after restart, the rate limit is not enforced. This is acceptable
  — the workqueue's existing backoff behavior limits the post-restart burst
  naturally
- **Effect on runaway loops:** a counter that increments every cycle is limited
  to at most 10 writes per second per storeName per instance (100ms interval),
  making the runaway observable and bounded without preventing convergence
- **Effect on legitimate bursts:** a batch of state transitions that legitimately
  fires multiple writes in quick succession (e.g., 10 certs rotating simultaneously,
  each on a separate instance) is unaffected — the rate limit is per instance
- **Cycle continuation on skip:** when a state node write is skipped due to rate
  limiting, the rest of the reconcile cycle continues normally. Downstream
  template and forEach nodes that depend on the rate-limited state node read
  whatever values are present in `status.<storeName>.*` from prior reconcile
  cycles — the same values they would see between any two reconcile cycles.
  This creates a transient inconsistency window (template resources reflect
  stale state until the requeue fires, the state node writes, and the next
  reconcile applies templates with fresh values). This is acceptable: the
  inconsistency is identical to the gap between any two reconcile cycles in
  normal operation. The alternative — blocking downstream nodes on a rate
  limit — would delay all downstream work and make the feature unusable for
  RGDs where state nodes have dependents
- **Flag floor:** the `--min-state-node-write-interval` flag has a minimum
  enforced value of 10ms. Setting it to `0` is rejected at controller startup
  with an error: `"--min-state-node-write-interval must be >= 10ms; 0 disables
  the runaway write loop protection"`. This prevents operators from accidentally
  disabling the safety mechanism

##### End-of-reconcile updateStatus() and storeName preservation

kro's `updateStatus()` builds a completely fresh status map from scratch each
reconcile cycle and replaces the entire `status` object with a single
`UpdateStatus` call. Without explicit storeName preservation, this final call
would silently overwrite all values written by state nodes earlier in the same
cycle.

The preservation invariant: `updateStatus()` iterates over all `storeName`
values declared in the RGD, reads each from the current in-memory instance
(updated after the last state node write in the cycle), and
merges them into the freshly-built status map before issuing the final write.
The RGD build phase records the full set of declared storeNames so that the
reconciler context has this list at runtime.

On conflict retry inside `updateStatus()`, the preservation logic must
**re-read storeNames from the freshly-fetched live CR**, not from the stale
in-memory instance. The retry pseudocode:

```
newStatus = buildFreshStatus(rcx)      # once — conditions + cached CEL projections
RetryOnConflict:
  cur = GET live CR                    # fresh resource version
  for storeName in declaredStoreNames:
    newStatus[storeName] = cur.status[storeName]  # re-read storeNames from live, not stale
  cur.status = newStatus
  UpdateStatus(cur)
```

`buildFreshStatus()` is called once, outside the retry loop. It rebuilds
conditions and the instance state field, and merges CEL projection results from
the current reconcile cycle's cached evaluation. Conditions are computed from the
current reconcile's state manager and do not change between retry attempts — they
reflect THIS reconcile's outcome, not a concurrent reconcile's. CEL projections
are derived from `spec`, which does not change during a status conflict retry —
there is no need to re-evaluate them. Only the storeName scopes are re-read from
the freshly-fetched CR on each retry attempt, ensuring state node writes from
concurrent reconciles are not overwritten.

**Conflict retry budget:** state node `UpdateStatus` calls use `retry.DefaultRetry`
(up to 5 attempts with exponential backoff starting at 10ms). The end-of-reconcile
`updateStatus()` call uses the same budget. This is consistent with all other
`UpdateStatus` calls in kro today.

**Partial failure:** if a state node exhausts its retry budget and fails, its
writes are not persisted. Downstream state nodes that depend on it are not
evaluated. The instance is re-queued. On the next cycle, all state nodes
re-evaluate — the idempotency check means successfully-written state nodes from
the prior cycle are no-ops; only the failed node (and its dependents) re-fire.
This is eventually consistent: each state node writes or is a no-op, never
partially overwrites a prior successful write.

**Write amplification:** each state node issues one `UpdateStatus` call per
reconcile cycle in which it fires. An RGD with N state nodes generates up to N
additional `UpdateStatus` calls per reconcile cycle per instance, compared to
kro's current one-call-per-cycle model. The idempotency check (no write if
values unchanged) means steady-state reconciles (where no state has changed)
issue zero state node writes. Amplification occurs only on cycles where state
actually advances. RGD authors with large numbers of state nodes can minimize
amplification by grouping related fields into a single state node (one call)
rather than one field per node (N calls).

##### Reserved `storeName` validation

The following names are reserved and cannot be used as `storeName` values:

- `state` — used by kro's own instance state tracking
- `conditions` — standard Kubernetes status conditions
- `managedResources` — kro's managed resource list
- Any field name declared in the `schema.status` block of the same RGD (user-defined
  CEL projection fields). This prevents the end-of-reconcile `updateStatus()` merge
  from having undefined behavior when a storeName conflicts with a projection field name.

Attempting to use a reserved or colliding name produces an admission webhook
rejection with a descriptive message, e.g.:
`state.storeName "phase" conflicts with schema.status field "phase"; choose a different name`.

This is enforced at admission time (kubebuilder validation rule), not at runtime.

##### CEL type checking

State node expressions are compiled in parse-only mode (no type checking) for
v1. The typed CEL environment is built from the RGD's `schema` section, which
has no knowledge of `status.<storeName>.*` fields at compile time. Type errors
in state expressions are caught at runtime rather than at RGD admission time.
See the Discussion section for mitigations available to RGD authors and the v2
path to compile-time type inference.

In addition to parse-only compilation, the build phase performs a best-effort
static field reference check at admission time: for any
`schema.status.<storeName>.<fieldName>` reference found in a static AST walk
of a state node's `fields` or `includeWhen` expressions, it validates that
`<fieldName>` is declared in the `fields` map of at least one state node
targeting `<storeName>`. Statically-resolved references to undeclared field
names produce an admission warning. References that cannot be resolved
statically (e.g., dynamic map access via `schema.status.migration[varName]`)
are silently skipped. This check catches the most common class of typo — a
mistyped field name in a static reference — without requiring full type
inference and without producing false positives on dynamic access patterns.

The build phase also checks for **overlapping field declarations** across state
nodes targeting the same `storeName`. When multiple state nodes declare the same
field name in their `fields` maps for the same `storeName`, an admission warning
is emitted: e.g., `"field 'destination' in storeName 'routing' is declared by
state nodes ['normalPath', 'emergencyPath']; if both fire in the same cycle,
'emergencyPath' (later in topological order) wins"`. This is a warning, not an
error — overlapping declarations may be intentional when `includeWhen` gates are
mutually exclusive. The warning makes the last-writer-wins semantics explicit.

##### Requeue behavior

When a state node writes values and the within-cycle context refresh is
complete, the controller does not automatically schedule a requeue unless one
of the following conditions holds:

1. A state node's `includeWhen` is true and its values differ from the live CR
   (the normal case on first write — the write succeeds and triggers an
   immediate requeue so the next cycle can evaluate the updated `includeWhen`).
2. Any state node wrote to `status` during the current reconcile — the
   controller schedules a delayed requeue so that cross-cycle self-referencing
   expressions (bootstrap pattern) get a chance to advance.

Status writes generate a MODIFIED watch event that will re-trigger the
reconciler. The controller also schedules an explicit requeue as a convergence
safety net — this ensures the bootstrap pattern advances even in environments
where watch event delivery is delayed or the controller's informer cache has
not yet reflected the status write. The explicit requeue is not required for
correctness in all cases, but is the conservative and recommended behavior.

##### CEL evaluation context for state nodes

State nodes use a different CEL environment than all other node types in kro.

kro's standard CEL context deliberately strips `status` from the instance
object before building the context for `template:`, `readyWhen:`, and
`includeWhen:` expressions on ordinary resource nodes. The reasoning: child
resource templates should not depend on `status` (which can be stale or absent
on first reconcile) — they should depend on `spec` fields that the user controls.

State nodes invert this requirement. Their entire purpose is to read prior state
values from `status.<storeName>.*` and decide whether to fire based on them.
If the standard stripped context were used:

- `has(schema.status.migration.step)` would always return `false` (status is
  absent from the context), so `includeWhen` expressions that guard on prior
  state would always evaluate as if no state had ever been written.
- A state node with `includeWhen: "${!has(schema.status.foo.counter) || ...}"`
  would fire on every reconcile regardless of whether `counter` had already
  reached its target value — producing infinite requeue loops.

To prevent this, state nodes use a status-aware context variant for both their
`includeWhen` expressions and their `fields` expressions. This context includes
`schema.status.*` populated from the live instance CR, with all declared
`storeName` scopes pre-initialized to `{}` if absent (to avoid "no such key"
errors on first reconcile).

The implication for implementers: the `buildContext()` call site in the DAG
evaluation loop must branch on node type. State nodes call
`buildContextWithStatus()` (or equivalent); all other node types continue to
use the standard `buildContext()` that strips `status`. The two code paths share
everything except the status injection step — they are not separate environments,
just different context construction calls.

##### State fields in template, forEach, and schema.status projection expressions

Template nodes, `forEach` dimension expressions, and `schema.status` projection
expressions within an RGD may reference `schema.status.<storeName>.*` for any
`storeName` declared by a `state:` node in the same RGD. This is the only
exception to the rule that non-state-node expressions use a status-stripped CEL
context.

**Rationale:** `storeName` scopes are written exclusively by the kro controller
via `state:` nodes declared in the same RGD. They are pre-initialized to `{}`
at bootstrap (see "Bootstrap guard" above), preventing "no such key" errors on
first reconcile. They are controller-owned, not user-owned or externally written,
so the stale-read concern that motivates stripping `status` from template contexts
does not apply to them.

**DAG ordering:** The RGD build phase treats a template node's or `forEach`
expression's reference to `schema.status.<storeName>.*` as an implicit DAG
dependency edge from that node to the `state:` node declaring `storeName`. The
state node must reach `ready` before the dependent template node or `forEach`
expression is evaluated. Standard DAG cycle detection applies. When a cycle
involves an implicit storeName-derived edge, the cycle detection error message
must identify the implicit edge source: e.g., "cycle detected: node X depends
on node Y (implicit: X reads schema.status.<storeName> written by Y) which
depends on X." Generic "cycle detected between X and Y" messages are
insufficient — the implicit edge is invisible in the RGD YAML and authors
cannot diagnose it without this hint.

**Evaluation order within a reconcile cycle:**

1. State nodes evaluate in topological (DAG) order. After each state node
   writes, the in-memory context is refreshed so that state nodes later in the
   DAG read the freshly written values.
2. Template nodes that depend on a state node (via explicit resource-status
   references or via `schema.status.<storeName>.*` reads) evaluate after that
   state node in topological order.
3. `schema.status` projection expressions (the `status:` block in the RGD
   schema) evaluate last, after all template and state nodes. They may read from
   both child resource status fields (as today) and declared `storeName` scopes
   (`schema.status.<storeName>.*`).

**Constraint:** State node `includeWhen` and `fields` expressions may reference
`schema.status.<storeName>.*` from sibling state nodes (read via the
within-cycle context refresh). They may NOT reference `schema.status` projection
outputs (e.g., `schema.status.bossState`) because projections have not yet been
computed when state nodes execute. Attempting to do so is a build-time
validation error.

**Skipped state nodes and dependency gating — Satisfied vs. Ignored:** State
nodes require a distinct lifecycle from resource nodes. The full state transition
table for a state node:

| State node lifecycle | `IsIgnored()` return | Downstream node sees | Behavior |
|---|---|---|---|
| Not yet evaluated in this cycle | `(false, nil)` | Waits in topological order | Evaluated when its turn comes |
| `includeWhen` pending (dep not ready) | `(false, ErrDataPending)` | Pending — waits | Downstream propagates ErrDataPending |
| `includeWhen` = false | `(false, nil)` + `Satisfied` state set | Proceeds | Reads prior `storeName` values from status |
| `includeWhen` = true, write succeeded | `(false, nil)` + `Ready` state set | Proceeds | Reads freshly written `storeName` values |
| `includeWhen` = true, write failed | error | Skips; instance requeues | Error propagated; downstream blocked |

`Satisfied` is a terminal state distinct from `Skipped`/`Ignored`. A resource
node in `Ignored` state propagates ignore contagiously — its dependents are also
ignored. A state node in `Satisfied` state does NOT propagate ignore: its
`storeName` scope persists in `status` from prior reconcile cycles, and
downstream nodes proceed using those values.

**Implementation requirements for `IsIgnored()`:**

1. The contagious-ignore loop (`node.go:80-89`) must check dep node type: if a
   dep is a state node in `Satisfied` or `Ready` state, skip propagation (do NOT
   return `(true, nil)`).
2. Virtual (state node) deps must not be passed to `checkObservedReadiness()`.
   State nodes have no observed Kubernetes resource. Their readiness for
   dependency gating is determined by their lifecycle state, not by
   `checkObservedReadiness()`.
3. A state node whose `includeWhen` returns `ErrDataPending` (because one of
   its own deps is not yet ready) propagates that pending state normally —
   downstream nodes that depend on it also wait.

Template nodes reading a storeName field should use `has()` guards or `kstate()`
defaults if the writing state node's `includeWhen` may be false on the first
reconcile.

**Example:**

```yaml
resources:
  # State node tracks delivery progress in status.delivery
  - id: advanceStep
    state:
      storeName: delivery
      fields:
        completedSteps: "${kstate(schema.status.delivery, 'completedSteps', 0) + 1}"
        phase:          "${kstate(schema.status.delivery, 'completedSteps', 0) + 1 >= size(schema.spec.stages) ? 'complete' : 'in-progress'}"
        processedSeq:   "${schema.spec.rolloutSeq}"
    includeWhen:
      - "${schema.spec.rolloutSeq > kstate(schema.status.delivery, 'processedSeq', 0)}"
      - "${deployedRevision.status.readyReplicas == deployedRevision.spec.replicas}"

  # Template node reads from status.delivery — permitted because 'delivery' is a declared storeName
  - id: progressCM
    template:
      apiVersion: v1
      kind: ConfigMap
      data:
        phase:          "${kstate(schema.status.delivery, 'phase', 'pending')}"
        completedSteps: "${string(kstate(schema.status.delivery, 'completedSteps', 0))}"

  # forEach dimension reads from status.delivery — same scoped access
  - id: stageCRs
    forEach:
      - idx: "${has(schema.status.delivery.stageResults) ? lists.range(size(schema.status.delivery.stageResults)) : []}"
    template:
      spec:
        result: "${schema.status.delivery.stageResults[idx]}"  # reads from declared storeName ✓

# schema.status projection reads from declared storeName — permitted, evaluated last
schema:
  status:
    deliveryComplete: "${has(schema.status.delivery.phase) && schema.status.delivery.phase == 'complete'}"
```

## Usage patterns

#### Trigger-sentinel pattern (replacing write-back triggers)

A common stateful workflow uses a trigger field in `spec` to signal an action
to the controller, with the controller writing a result field back to `spec` to
clear the trigger and signal completion. This write-back-to-spec approach
conflicts with GitOps controllers (Argo CD, Flux) which detect spec mutations
as drift.

`state:` nodes eliminate the need for spec write-backs. The pattern is:

1. The user or external controller writes a monotonically increasing sequence
   number to `spec.triggerSeq` alongside the trigger payload fields.
2. A `state:` node gates on `spec.triggerSeq > status.<storeName>.processedSeq`.
   When the gate is true, it processes the action and writes
   `processedSeq: spec.triggerSeq` to advance the sentinel. The gate closes.
3. The external controller polls `status.<storeName>.processedSeq >= N` to
   detect completion. All action results are available in `status.<storeName>.*`.

```yaml
# Quota enforcement: approve or deny a quota request, write result to status
- id: processQuotaRequest
  state:
    storeName: quota
    fields:
      approved:     "${schema.spec.requestedCPU <= schema.spec.cpuLimit && schema.spec.requestedMemory <= schema.spec.memoryLimit}"
      allocatedCPU: "${schema.spec.requestedCPU <= schema.spec.cpuLimit ? schema.spec.requestedCPU : 0}"
      processedSeq: "${schema.spec.requestSeq}"
  includeWhen:
    - "${schema.spec.requestSeq > kstate(schema.status.quota, 'processedSeq', 0)}"
```

When multiple action types share the same sequence counter, the trigger payload
fields distinguish the action type. The external controller must write ALL
trigger payload fields on every trigger, explicitly blanking non-applicable
fields:

```
# Approve trigger: set requestedCPU/requestedMemory, blank revokeTarget
spec.requestSeq = N, spec.requestedCPU = 4, spec.requestedMemory = "8Gi", spec.revokeTarget = ""

# Revoke trigger: set revokeTarget, blank request fields
spec.requestSeq = N, spec.revokeTarget = "tenant-a", spec.requestedCPU = 0, spec.requestedMemory = ""
```

The `includeWhen` gate on each state node then uses BOTH the sentinel comparison
AND the payload discriminator:

```yaml
- id: processApproval
  state:
    storeName: quota
    fields:
      approved:     "${schema.spec.requestedCPU <= schema.spec.cpuLimit}"
      processedSeq: "${schema.spec.requestSeq}"
  includeWhen:
    - "${schema.spec.requestSeq > kstate(schema.status.quota, 'processedSeq', 0)}"
    - "${schema.spec.revokeTarget == ''}"

- id: processRevocation
  state:
    storeName: quota
    fields:
      revoked:      "${true}"
      revokedTenant: "${schema.spec.revokeTarget}"
      processedSeq: "${schema.spec.requestSeq}"
  includeWhen:
    - "${schema.spec.requestSeq > kstate(schema.status.quota, 'processedSeq', 0)}"
    - "${schema.spec.revokeTarget != ''}"
```

Because the external controller blanks `revokeTarget` on every approval trigger
and blanks the request fields on every revocation trigger, exactly one of the
two nodes fires per sequence number advance. No spec write-back from kro is needed.

This pattern is NOT a requirement of the `state:` node primitive — it is a
usage pattern for RGD authors who need trigger-driven workflows without
GitOps-conflicting spec mutations.

## Other solutions considered

**External bespoke controller (status quo)**

Write a separate controller that watches for a triggering condition, reads the
parent CR, computes the next state, and patches. This works and is what every
production kro user who needs stateful transitions is doing today. The cost: kro
stops being a complete solution for those workflows. For each new transition
rule, a separate controller binary must be written, deployed, and operated.

**KREP-011 data nodes**

Data nodes solve within-cycle reuse but are ephemeral — their values are not
persisted to the API server and are not visible in the next reconcile cycle.
They cannot express accumulation or any pattern that requires reading the result
of a prior reconcile.

**KREP-016 patch nodes targeting self**

KREP-016 patch nodes write to resources kro does not own. Extending them to
support self-targeting is possible but conflates two distinct semantics:
contributing fields to a peer resource vs. persisting computed state on self.
Keeping them separate maintains a cleaner mental model and avoids complexity in
the field manager assignment logic.

**External ConfigMap as scratch pad**

A kro-managed ConfigMap can hold arbitrary key-value data and would survive
across reconcile cycles. However, this pollutes the cluster with
controller-private implementation details, requires consumers to know about a
second resource, and makes the ownership model harder to reason about. State
belongs on the instance CR itself.

**Write to `spec` instead of `status`**

Writing to `spec` triggers a watch event which re-drives the controller
immediately — enabling a multi-step chain to resolve in a single logical
transaction without an explicit requeue. However, `spec` is the GitOps source
of truth. kro writing to `spec` creates a conflict with Argo CD and Flux, which
detect the mutation as drift and attempt to revert it. `ignoreDifferences` can
paper over this, but it requires per-field configuration and is fragile.
Writing to `status` avoids this entirely. The within-cycle context refresh
described in this proposal recovers most of the convergence speed benefit of
spec writes without the GitOps conflict.

## Scoping

#### What is in scope

- `state:` virtual node type with `storeName` and `fields`
- Within-cycle CEL context refresh after each state node write
- CRD schema auto-injection (`x-kubernetes-preserve-unknown-fields: true`) for each declared `storeName`
- `includeWhen` support on state nodes (with status-aware CEL context)
- `kstate(scope, fieldName, defaultValue)` built-in CEL helper for bootstrap guard ergonomics
- Best-effort static field reference check at admission time (warns on statically-detectable typos in `schema.status.<storeName>.<field>` references)
- Idempotency: no patch when computed values already match live CR
- Reserved `storeName` validation at admission time (including schema.status projection field collision)
- Requeue-on-write for cross-cycle bootstrap cases
- DAG participation: state nodes can depend on child resource statuses and can be depended upon
- Metrics: `kro_state_node_writes_total` (labels: `node_id`, `store_name`, `rgd`), `kro_state_node_conflicts_total` (labels: `node_id`, `rgd`), `kro_state_node_evaluation_errors_total` (labels: `node_id`, `rgd`). The `node_id` label is required — without per-node cardinality, a runaway state node (missing `includeWhen` gate) is indistinguishable from normal activity in aggregate metrics
- Failure propagation: a state node that exhausts its retry budget sets `ResourcesReady=False` on the instance with a descriptive reason
- Kubernetes Events on state node failure (CEL evaluation error or exhausted retries); no events on successful writes (too noisy for high-frequency state nodes)

#### What is not in scope

- Writing to `spec` — GitOps conflict; tracked separately
- Writing to other instance CRs or resources kro does not own — covered by KREP-016
- Transactional atomicity across multiple state nodes in a single `UpdateStatus` call
- Time-triggered transitions (no timer/TTL primitive)
- Compile-time type checking for `status.<storeName>.*` fields (v2 improvement; see Discussion)
- `forEach` on state nodes — the semantics of iterating a state write (one entry per iteration vs. merged result) are non-trivial and deferred to a follow-on proposal
- Explicit lifecycle management for `storeName` data when a state node is removed
  from an RGD. When a state node is removed, its `storeName` is no longer in
  `declaredStoreNames`, so the end-of-reconcile `updateStatus()` preservation loop
  no longer merges it — the data is silently dropped on the next reconcile cycle.
  This is the natural behavior and is intentional: removing a state node is a
  destructive operation on the corresponding status scope. RGD authors who need
  to preserve the data across a state node removal must declare a new state node
  with the same `storeName`. The CRD schema retains `x-kubernetes-preserve-unknown-fields: true` on the storeName scope even after the state node is removed (the CRD schema is not automatically updated when an RGD changes), which means the field continues to be accepted by the API server but is no longer written or preserved by the controller.
- Admission-time warning for non-convergent expressions (expression reads and writes the same field without a terminating `includeWhen` gate)
- `kstate()` macro form with string storeName argument (v2 ergonomics improvement)
- Allowing template nodes, `forEach` expressions, or `schema.status` projections to reference `status.*` fields beyond declared `storeName` scopes. The scoped extension described in this proposal is specifically for controller-owned state written by `state:` nodes in the same RGD. Extending it to arbitrary `status.*` fields would reintroduce the stale-read problem that motivates the standard context's status stripping.

## Testing strategy

#### Requirements

- A running kro-enabled Kubernetes cluster with the updated controller image
- RGD YAML fixtures for each validation scenario

#### Test plan

**Unit tests:**

- State node CEL evaluation: `EvaluateStateNode()` produces correct output for arithmetic, array mutation, and conditional expressions
- `kstate()` helper: returns default value when field absent; returns stored value when present
- Within-cycle context refresh: after a state node fires, subsequent nodes in the same reconcile see the updated `status.<storeName>.*` values
- `includeWhen` uses status-aware CEL context: `has(schema.status.<storeName>.field)` evaluates correctly
- CRD schema injection: `InjectStateField()` adds `x-kubernetes-preserve-unknown-fields: true` for each declared `storeName`, exactly once per name
- Reserved `storeName` rejection: kubebuilder validation rule fires for `state`, `conditions`, `managedResources`, and schema.status projection field names
- storeName/projection collision: RGD with both a state node `storeName: phase` and a `schema.status.phase` projection field is rejected at admission
- Static cross-reference check: a `schema.status.<storeName>.<undeclaredField>` reference in a static expression produces an admission warning
- Idempotency: second call with identical computed values issues no `UpdateStatus` call
- Conflict re-evaluation: mock `UpdateStatus` to return a resource version conflict
  on the first attempt; provide a modified live CR on the re-fetch where a self-
  referencing field (e.g., `step`) has advanced from 0 to 1; assert that the
  re-evaluated result is `step = 2` (not `step = 1` from the stale evaluation).
  This test must verify the computed values changed, not just that the retry
  succeeded. A merge-only retry implementation would produce `step = 1` — the
  wrong answer — and this test must catch it.
- Metrics: `kro_state_node_writes_total` increments on successful write (labels: `node_id`, `store_name`, `rgd`); `kro_state_node_conflicts_total` increments on retry (labels: `node_id`, `rgd`); `kro_state_node_evaluation_errors_total` increments on CEL error (labels: `node_id`, `rgd`)
- Write rate limit enforcement: mock a state node that fires twice within `minStateNodeWriteInterval` (100ms default) with changed values. Assert the second write is skipped, no `UpdateStatus` call is issued, and a `RequeueAfter` is returned with the correct remaining interval (100ms minus elapsed time since last write). Also verify: when the idempotency check passes (values unchanged), no rate limit is applied regardless of timing

**Integration tests (Chainsaw):**

1. **Bootstrap**: first reconcile with empty `status.<storeName>` writes initial values correctly; `has()` guard evaluates false, expression uses default value; `kstate()` returns default value
2. **Cross-cycle accumulation**: a counter field increments correctly across N reconcile cycles; after reaching `totalSteps`, `includeWhen` becomes false and no further patches are issued
3. **Within-cycle chaining**: two state nodes A → B in DAG order, where B reads a field written by A; both fire in the same reconcile cycle; the instance reaches the expected state in one cycle
4. **`includeWhen: false`**: state node is skipped; existing `status.<storeName>` values are preserved unchanged
5. **Concurrent `storeName` writes**: two state nodes targeting the same `storeName` write disjoint fields; the retry-on-conflict path ensures both sets of fields are present in the final status without one node's write dropping the other's fields
6. **Reserved `storeName` rejection**: RGD with `storeName: conditions` is rejected at admission with the expected error message
7. **storeName/projection collision**: RGD with a state node `storeName` matching a `schema.status` field is rejected at admission
8. **Multi-`storeName`**: two state nodes targeting different `storeNames` coexist without collision; CRD schema contains both injected fields
9. **Requeue correctness**: a self-referencing counter (bootstrap pattern) advances once per cycle and converges to the target value
10. **Partial failure**: a state node that exhausts retries sets `ResourcesReady=False` on the instance; on requeue, successfully-written state nodes are no-ops (idempotency); the failed node re-fires
11. **End-of-reconcile preservation**: state node values written during a cycle are not overwritten by the final `updateStatus()` call; on conflict retry in `updateStatus()`, storeNames are re-read from the freshly-fetched CR, not the stale in-memory instance
12. **Failure event**: a state node CEL evaluation error emits a Kubernetes Event on the instance with the error details
13. **Runaway observability**: a state node with no `includeWhen` gate and a
    self-incrementing field writes on every reconcile; verify that
    `kro_state_node_writes_total{node_id="..."}` increments monotonically and
    is distinguishable from other nodes in the same RGD via the `node_id` label
14. **Write rate limit**: a state node that writes changed values on every cycle is rate-limited to at most `1 / minStateNodeWriteInterval` writes per second per storeName per instance (10/sec at 100ms default). Verify the counter does not advance faster than the rate limit over a 5-second observation window

## Discussion and notes

**On reconcile amplification**

The within-cycle context refresh addresses the primary latency concern for
chained state transitions. For a chain of N state nodes where the DAG can be
resolved in topological order within a single reconcile, the chain converges
in one cycle regardless of N. The amplification concern only applies to
self-referencing (cross-cycle) patterns, where each step requires one requeue.
For platform use cases — cert rotation scheduling, retry counters, progressive
delivery step tracking — the cross-cycle case is typical, and the latency
(~100–500ms per step on a lightly loaded cluster) is well within the acceptable
range. These transitions happen over minutes or hours, not milliseconds.

**On `x-kubernetes-preserve-unknown-fields` as a one-way door**

The use of `x-kubernetes-preserve-unknown-fields: true` on each `storeName`
scope is an explicit trade-off. It means:

- The API server performs no validation on fields written under
  `status.<storeName>`. A state node can write any structure — nested objects,
  arrays, mixed types — and the API server will accept it silently.
- There are no schema evolution guarantees for state fields. If an RGD author
  changes a state field type (e.g., from `int` to `string`) between RGD versions,
  existing instances retain the old type in `status.<storeName>`. CEL expressions
  that perform type-specific operations on the field will fail at runtime.
- **Field rename produces a silent counter reset.** If an RGD author renames a
  state field (e.g., `step` → `currentStep`) in a new RGD version, existing
  instances have the old field name (`step: 3`) in `status.<storeName>`. Expressions
  using `kstate(schema.status.migration, 'currentStep', 0)` return 0 (the
  default) — the counter silently resets to zero. There is no error, no warning,
  no event. The static field reference check does not catch this: `currentStep`
  is declared in the new RGD's `fields` map, so it passes validation. To mitigate
  silent resets, the `updateStatus()` preservation loop logs a warning when it
  finds fields in a `status.<storeName>` scope that no current state node declares —
  specifically, when preserving `storeName` values, if the live CR contains
  fields under that scope that are not in any current state node's `fields` map,
  a warning is logged: "storeName '<name>' contains undeclared fields: [field1,
  field2] — possible stale data from a prior RGD version."
- Tooling (kubectl explain, Lens, k9s, IDE integrations) cannot provide
  completion or validation for fields under `status.<storeName>`.
- When a state node is removed from an RGD, its storeName data is silently
  dropped on the next reconcile (the preservation loop no longer includes it).
  See "What is not in scope" for the full behavior.

This is a known permanent cost of the v1 design. The v2 path to typed schemas
(described below) mitigates it for new fields, but cannot retroactively fix the
schema of fields written under v1.

**On strategic merge patch and `storeName` scopes**

External tools that modify `status.<storeName>` must use JSON merge patch
(`application/merge-patch+json`) or full status replacement, not strategic merge
patch (`application/strategic-merge-patch+json`). Strategic merge patch treats
`x-kubernetes-preserve-unknown-fields: true` scopes as opaque blobs and replaces
the entire scope wholesale rather than merging sub-fields. An external controller
that does a strategic merge patch including `<storeName>: {someField: "value"}`
will replace the entire storeName scope, deleting all other fields that state
nodes wrote. This is consistent with how `preserve-unknown-fields` works in
Kubernetes generally, but is a sharp edge that RGD authors and cooperating
controllers must be aware of.

**On the `has()` bootstrap verbosity and `kstate()`**

The `kstate(scope, fieldName, defaultValue)` helper (in scope for v1) eliminates
the nested ternary guard required for self-referencing expressions. The pattern:

```cel
kstate(schema.status.rotation, "count", 0) + 1
```

is equivalent to the explicit form:

```cel
has(schema.status.rotation.count) ? schema.status.rotation.count + 1 : 1
```

A macro form accepting a string storeName directly — `kstate("rotation", "count", 0)` —
requires CEL macro support and is deferred to v2.

**On CEL type safety**

The parse-only compilation for state expressions is a real trade-off. In v1,
a typo in a state field name — `schema.status.migration.stpe` instead of
`schema.status.migration.step` — passes admission and produces a silent `nil`
value at runtime. The best-effort static cross-reference check (in scope for v1)
catches this for the common case of static field access. Dynamic access patterns
are not checked.

Two additional mitigations available to RGD authors in v1:

1. **Test with a unit test fixture** that applies the RGD to a known instance
   state and asserts the expected `status.<storeName>.*` values after
   reconciliation. This is the same pattern used for testing `readyWhen` and
   `includeWhen` expressions and catches typos before production deployment.

2. **Use `kstate()` defensively**: the helper returns the default value on
   absent fields, preventing nil dereference panics at the cost of silently
   masking the typo as "field not yet written."

The v2 path to compile-time type checking is feasible: at RGD admission time,
derive a partial type schema from the declared `fields` map across all state
nodes targeting the same `storeName`. The `fields` map is static (declared in
the RGD YAML, not computed at runtime), so inferred types from the declared
expressions (integer arithmetic → `int`, string concatenation → `string`) can
populate a supplementary CEL type environment entry for
`schema.status.<storeName>.*`. This would not require changes to the RGD API
surface and could be implemented as a follow-on without breaking existing RGDs.

**On the CEL context construction paths**

This proposal introduces two additional CEL context construction variants
alongside the existing standard context:

1. **Status-aware context** (`buildContextWithStatus()`): used for state node
   `fields` and `includeWhen` expressions. Includes `schema.status.*` with all
   declared storeNames pre-initialized to `{}`.
2. **Scoped status context** (modified `buildContext()`): used for template,
   forEach, and schema.status projection expressions that reference a declared
   storeName. Identical to the standard context except that declared storeName
   scopes from the live instance status are injected alongside spec/metadata.

The standard context (which strips status entirely) remains unchanged for all
other expressions. The three paths must be kept in sync as the CEL environment
evolves. The clean alternative — a unified context with explicit scope
annotations — would require refactoring the existing status-stripping behavior
that all current RGDs depend on. The scoped injection approach minimizes the
delta to existing behavior while enabling state node access to prior written
values.

**On the `status` write mechanics**

kro's `updateStatus()` builds a completely fresh status map from scratch
(conditions + instance state + CEL-resolved fields) and replaces the entire
`status` object. Without explicit preservation, this overwrites any
`<storeName>` values written by state nodes earlier in the same reconcile.
The Design Details section "End-of-reconcile updateStatus() and storeName
preservation" specifies the exact merge and retry behavior. The key invariant:
on conflict retry, storeNames must be re-read from the freshly-fetched CR, not
from the stale in-memory instance.

**Precedent**

The `state:` node pattern is analogous to Crossplane's `status.atProvider` — a
controller-owned sub-object within `status` that accumulates observed state
without conflicting with the user-owned `spec`. The named `storeName` scoping
is consistent with how Crossplane isolates provider-managed state from
user-visible conditions.

**On the category shift: kro as a state machine runtime**

This proposal changes kro's fundamental contract. Today, kro is a declarative
resource graph: CEL expressions are pure functions of the current spec and child
resource statuses. The system is convergent by design — given the same inputs,
it always produces the same outputs. Debugging is straightforward: the state at
any point is fully determined by `spec` and observed child resource statuses.

State nodes make kro imperative. The output of a reconcile depends on the
previous output. The system has memory. Three consequences:

1. **Convergence is the RGD author's responsibility.** A declarative RGD always
   converges — the desired state is expressed, and kro drives toward it. A
   stateful RGD converges only if every state node has a terminating `includeWhen`
   gate. A missing gate creates a runaway write loop: the state node fires every
   reconcile, each write triggers a MODIFIED watch event, which triggers another
   reconcile, indefinitely. Monitor `kro_state_node_writes_total{node_id="..."}` —
   a monotonically increasing rate for a specific node is the signal. Always gate
   state nodes with `includeWhen`.

2. **Debugging requires understanding history.** For declarative kro, answering
   "why is my instance in this state?" requires inspecting `spec` and child
   resource statuses. For stateful kro, the answer also requires understanding
   the sequence of state transitions that wrote the current `status.<storeName>.*`
   values. That history is not recorded — only the current values are visible.
   `kubectl get <instance> -o jsonpath='{.status.<storeName>}'` shows the current
   state; the path that got there is opaque.

3. **State nodes are a power-user feature.** Before using state nodes, ask: can
   this be solved with an external controller that watches the instance and patches
   it? For many use cases, the answer is yes — and an external controller is more
   debuggable, testable, and auditable than embedded CEL state transitions. State
   nodes are the right choice when the overhead of a separate controller binary
   outweighs the simplicity of keeping the logic in the RGD. They are not a
   replacement for purpose-built controllers in complex domains.

## Open questions

1. **`storeName` required or optional?** This proposal requires it explicitly.
   Defaulting to a kro-chosen name (e.g., `kstate`) would reduce verbosity for
   simple cases but creates a shared namespace collision risk when multiple
   independent features in one RGD each need their own state. Explicit names
   are self-documenting and prevent accidental cross-node field clobbering.

2. **Type-checked `fields` at admission time?** This proposal uses parse-only
   for v1 with a documented v2 path (see Discussion). If maintainers prefer to
   require type checking before the feature ships, the partial type inference
   approach described in Discussion is the recommended path — it does not
   require changes to the RGD API surface.
