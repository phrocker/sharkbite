# Durable target outcome inventory

The target inventory is an internal persistence boundary for a trusted admission
coordinator. It does not authenticate callers, discover outcomes, or establish
source or training rights. Hosts must register a target and an explicit coverage
boundary before using it. An absent row is not a complete empty inventory.

Coverage must describe an operator-controlled namespace where every future
outcome admission passes through the coordinator. Existing outcomes require an
explicit migration or reconciliation barrier before coverage can be asserted.
A new inventory cannot silently certify historical or bypassed writes.

## Admission ordering

1. Authenticate and authorize the canonical target and outcome under the actual
   resolved caller identity.
2. Persist a pending intent binding the exact outcome receipt identity and
   canonical observation.
3. Recheck authorization after inventory IO, then attempt the original immutable
   outcome write.
4. Reconcile the exact original outcome receipt, including its original
   attribution and receipt time, and publish its digest in the inventory.
5. Recheck current authorization before disclosing the result.

A pending intent makes the inventory incomplete. Crashes and lost acknowledgments
leave it incomplete until exact retry reconciles the original outcome. There is
no timeout, automatic abandonment, or inference of rollback from a missing read.
Idempotent retries do not advance the version; a new intent and its publication
each advance it. All reporters for the same target share the inventory, while
ordinary outcome receipt access remains principal scoped.

## Snapshot use

A registered inventory with no pending intents describes its declared coverage,
not universal completeness. This is a per-target snapshot; sequential reads of
several targets do not establish an atomic cohort snapshot. A production host
must supply the cohort-wide synchronization or epoch protocol required by its
export authority. Pending intents are never filtered away using client-provided
observation times.

Adjudication and export authorities must load the
exact receipts, validate the coverage assertion, and jointly recheck current
inventory generations and permissions after all IO. A changed generation fails
that export. Newly published outcomes require a fresh adjudication before an old
verified label can become eligible again.

The inventory's checksum and receipt digests bind bytes; the trusted coordinator
must establish that those bytes are authentic. Source access and training-purpose
permission remain separate, current checks. This primitive alone does not make
an operational authority or train a model.
