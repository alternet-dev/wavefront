# wavefront v0.2 — multi-contract generator design

Status: **proposed — ready for review** · 2026-05-18 · open questions resolved against stated constraints; implementation gated on the v0.2 transform runtime

## Context

v0.1's `bundlegen` is deliberately a single-contract slice: exactly one
operation, one `info.version` → one `contracts:` entry. Production reality is
the opposite — one wavefront bundle must serve *every still-pinned external
contract version at once*: `versions.yaml` with N `contracts:` entries and
`descriptors.binpb` carrying each version's proto package/messages, all
mapping onto the *current* internal backend. So the generator must, across
releases, **accumulate** new frozen contracts into the one committed bundle
and let the consumer **prune** a version when its client cohort is gone.

This is the lifecycle gap flagged during v0.1 ("seems like it could get
unwieldy"). This doc proposes how v0.2's generator closes it. It is a design
proposal for review — no implementation here.

## Constraints (must hold)

- **Frozen shapes are immutable.** Once a contract version's external message
  shapes are generated, they never change — that immutability *is* the
  anti-corruption guarantee. Re-running the generator must not mutate a
  prior version's descriptors.
- **Byte-reproducible output.** The accumulated `descriptors.binpb` must be
  deterministic (sorted, canonical marshal) so a rebuild is a no-op diff and
  `buf breaking` can gate a frozen version's shape in the consumer's CI.
- **Zero runtime inference / no runtime policy.** wavefront keeps reading the
  binding verbatim; retention is a consumer/generator-side decision, never
  wavefront-binary behavior (AGENTS.md: no policy in the binary).
- **1:1 cardinality unchanged.** Each `contracts:` entry still maps to exactly
  one current internal route/method — no fan-out.
- **The bundle is the committed source of truth** for frozen shapes (it lives
  in the consumer's repo); the generator must be able to re-emit from it
  without needing archived per-version OpenAPI snapshots.

## Proposed approach

1. **Accumulate, don't regenerate.** A build emits the *current* contract
   version's descriptors + binding and **merges** it into the existing
   committed bundle, copying every previously-frozen version's descriptors
   through **verbatim**. Generation = "merge current into the accumulated
   bundle," not "rebuild from scratch."
2. **Disjoint packages make the merge well-defined.** v0.1 already namespaces
   each contract version as its own proto package (package == sanitized
   version). Merging FileDescriptorSets across versions is therefore
   collision-free; the merge is a sorted union of files keyed by package.
3. **Multi-entry `versions.yaml`.** One entry per still-supported version,
   each binding that frozen external contract to *today's* internal
   route/method/messages. The external shape is frozen; the internal target
   is current — the generator scrapes the current OpenAPI and re-points old
   versions' bindings at the present backend.
4. **Retention/prune is declarative + explicit.** The consumer maintains a
   supported-versions list; pruning a version removes its `contracts:` entry
   *and* its descriptors. It is an auditable deprecation decision (gated in
   the consumer's CI), never automatic.

## Coupling to v0.2 transforms

Re-pointing an old external contract onto a changed internal surface is
exactly what the v0.2 mechanical transform vocabulary
(rename/default/optionalize/coerce/route) exists for. So multi-contract
assembly and transform-stanza emission are the same v0.2 effort: the
generator must emit, per old version, the transforms that bridge that
version's frozen external shape ↔ the current internal shape. This design
should land in lockstep with the v0.2 transform runtime, not before it.

## Decisions

Each prior open question resolves directly against a stated constraint or
the proposed approach — none is a free design choice:

- **Supported-versions declaration: a checked-in declarative list in the
  consumer's repo, not generator flags.** Approach #4 already requires the
  retention decision be *auditable* and *CI-gated*; flags are ephemeral and
  unreviewable, so a committed list is the only form that satisfies the
  constraint. (Its exact schema is build-time mechanics — see below.)
- **Frozen-shape source on re-emit: read back from the committed bundle.**
  The "bundle is the committed source of truth" constraint already mandates
  re-emit *without* archived per-version OpenAPI; this is settled, not open.
- **Per-version transform stanzas: consumer-authored, not generator-diffed.**
  A shape delta can carry semantic intent the generator cannot infer
  (AGENTS.md: semantic logic is consumer-side). The generator may scaffold
  the *mechanical* stanzas, but authorship and sign-off stay with the
  consumer.
- **`buf breaking` gating: one gate per frozen proto package, in the
  consumer's CI.** The byte-reproducible + disjoint-package constraints make
  per-package breaking checks well-defined; this gate is how the
  immutability constraint is *enforced*, not a new policy.

## Deferred to implementation

Not design questions — build-time mechanics to settle alongside the v0.2
transform runtime: the supported-versions list's concrete schema/location,
and the exact `buf` config wiring for the per-package breaking gate.

## Out of scope

Runtime changes (the runtime already reads a multi-entry `versions.yaml`
verbatim — no inference added); the transform *runtime* itself (separate
v0.2 work this depends on); any automatic/heuristic version pruning.
