# Sharkbite v2 migration

This repository is the next generation of Sharkbite. The original Sharkbite
project was an Accumulo native client; that implementation is preserved in its
historical tags and will not be part of the active v2 build.

Sharkbite v2 uses Shoal as its decision and evidence engine. Shoal's Go
packages, persisted records, snapshot identities, authorization boundaries,
and fail-closed behavior remain the implementation contract during the
migration. The Accumulo compatibility and storage packages remain part of the
platform where they are useful.

## Boundaries

- **Sharkbite** is the user-facing product: commands, services, integrations,
  deployment, and documentation.
- **Shoal** is the engine: evidence, graph and ontology contracts, decisions,
  provenance, local model execution, persistence, and evaluation.
- **Accumulo** is a supported storage and compatibility boundary, not a
  requirement for every deployment.

The first migration keeps the Go module path `github.com/phrocker/shoal-oss`
so existing consumers continue to build. A later compatibility release can
introduce a Sharkbite module path after all consumers have migrated.

## Migration invariants

1. Existing Shoal tests remain green.
2. Persisted record formats and snapshot identities do not change as a branding
   operation.
3. Existing authorization and fail-closed behavior remains unchanged.
4. The legacy native client is not reintroduced into the active v2 build.
5. Each consumer moves explicitly; no silent import-path break is introduced.

## Release transition

The migration should be released in stages:

1. establish the Sharkbite v2 product shell and documentation;
2. move the known Shoal consumer and update its imports/configuration;
3. add compatibility aliases or a new module path only after the consumer and
   validation suite are green;
4. archive the legacy native client separately and publish the v2 release.
