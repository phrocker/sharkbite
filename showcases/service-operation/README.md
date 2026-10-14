# Service-operation shadow picture

This small showcase models a proposed restart of a service from five separate
evidence classes: an approved troubleshooting guide, normative operation policy,
deployment-to-source mapping, source revision, observations, and historical
outcomes. Each record is sealed and the resulting picture is content-addressed.

`assess` is deliberately shadow-only. It returns an inspection priority,
missing prerequisites, or an explicit abstention, while `policy_effect` remains
false and `prediction.causal_proof` remains false. It never grants an operation,
changes policy, or treats a successful historical outcome as permission.

The tests cover the initial acceptance cases: an allowed operation, a missing
capacity prerequisite, stale deployment/source mapping, an unapproved or
misleading guide, conflicting history, permission revocation, and target
substitution. Evidence after the availability cutoff and identity tampering are
rejected before assessment.

This is a contract and shadow-boundary example, not a production collector,
execution fence, quality evaluation, or training grant. A production adapter
must supply authenticated current policy, source, target, actor, and observation
authority around the same separation.

```sh
python3 -m unittest discover -s showcases/service-operation -v
```
