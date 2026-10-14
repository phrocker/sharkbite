# Lexicon bundle

Slice 1 of #499, under the vocabulary bundle in
[`local-language.md`](local-language.md). `pkg/lexicon` builds a lexicon from a
graph snapshot; `pkg/explorer/authorized` resolves mentions against it under the
caller's current authorization.

## What a bundle holds

- **Terms.** For each node: `name`, falling back to `title` when there is no
  name or the name yields no tokens; `shoal.ontology.entity_key`; and every
  `shoal.lexicon.alias.<n>` property (a reserved key prefix; any non-empty
  suffix). With `Input.DeriveInitialisms` set (off by default), a `name` or
  `title` of three or more tokens also gives an initialism, tagged
  `origin=derived`. Labels are not used yet. Each term keeps its node IDs, node
  kinds and strongest origin (name < title < entity_key < alias < derived).
- **Lookup templates.** From ontology `RelationshipDefinition`s: two per directed
  relation (`out`: what does X *rel*; `in`: what *rel* X), one per undirected
  relation (`both`). Each holds the relation key, subject and answer concept IDs,
  and a phrase key. The wording belongs in a renderer catalog, not in the bundle.
  Relationship keys must be unique, whatever their directedness. Only
  server-filtered bundles carry templates (see below).
- **Pin.** Snapshot `(ID, Frontier, AsOf)`, the normalization version, the
  Unicode table versions, the build flags (whether initialisms were derived),
  and the scope. Two builds that differ only in a flag have different IDs.

## Normalization

`Tokenize` (normalization version 2):

1. Removes default-ignorable code points: format characters (Cf, such as zero
   width joiners and soft hyphens), variation selectors and the other default
   ignorables. They neither split a word ("pay", soft hyphen, "ments" is `payments`) nor keep a
   mark from composing with its base.
2. Applies NFKC, full case folding, then NFKC again. Where x/text's folding is
   not idempotent (it swaps Cherokee case on every pass), each such character
   maps to the smaller of its two folded forms, which is the form Unicode case
   folding chooses, so `ᏣᎳᎩ` and `ꮳꮃꭹ` are one token.
3. Splits on maximal runs of letters, numbers and combining marks. Everything
   else separates tokens and is dropped. That includes punctuation (`payments-api`
   and `svc_v2.prod` split), emoji and other symbols. A name made only of emoji
   gives no tokens. Invalid UTF-8 bytes separate tokens too, but `Build` refuses
   them outright (below).

Fullwidth forms, ligatures and `ß` normalize (`ＡＢＣ` → `abc`, `ﬁle` → `file`,
`Straße` → `strasse`). Camel case is not split. Folding does not depend on
locale, so `İ` folds to `i` plus a combining dot, not to `i`. Every token is a
fixed point: tokenizing it again gives exactly it. `FuzzTokenize` checks this, and that each token's source span tokenizes back to exactly that token (or, for a span shared by two tokens of one compatibility expansion such as `¾`, still contains it).
`NormalizationVersion` and the x/text and stdlib Unicode versions are written
into every bundle, so a change to tokenization, or a toolchain that brings other
Unicode tables, gives a new bundle ID. A bundle built under other tables is
refused at load. Typos and inflections are not matched; they abstain.

## Determinism and the codec

`Build(Input, Limits)` gives byte-identical output for the same content in any
order of nodes, properties and relationships. The encoding is fixed-width
big-endian with length-prefixed strings: magic `shoal-lexicon-v1`, versions,
build flags, pin, scope, a sorted token table, a sorted node table, terms sorted
by token sequence with sorted postings, and sorted templates. The bundle ID is
the SHA-256 of those bytes. `Load` checks every ordering and reference invariant,
including that template relation keys are valid ontology keys and template
concept IDs are concept-namespace ontology IDs. It then re-encodes and refuses
anything that is not byte-for-byte the canonical encoding, and rebuilds the
matcher from the decoded terms. `LoadVerified` also checks the bytes against an
expected ID. `FuzzLoad` checks that `Load` never panics and accepts only
canonical bytes. A golden ID is pinned in `pkg/lexicon/testdata`. It depends on
the Unicode tables, which x/text selects by Go toolchain version (15.0.0 before
go1.27, 17.0.0 from go1.27), so the move to go1.27 changes it, and every bundle
ID, by design.

**Nothing is dropped.** A lexicon that quietly omitted an entity would make that
entity look unknown, and in a server-filtered bundle whether something was
dropped could depend on hidden nodes. So the build fails, naming the node and
property, when:

- a limit is exceeded (defaults: 8 tokens per term and 8 postings per term; the
  hard caps are 16 and 64);
- a property it reads is not valid UTF-8 (replacing bytes with U+FFFD could
  merge distinct names);
- a node has a name or title but neither yields a token;
- an entity key or alias yields no token;
- a token does not normalize stably.

A node with no name, title, entity key or alias contributes no terms, and that
is not an error.

Initialisms would make this rule fail in practice: short names collide quickly
(every "Payments API" and "Public Access" gives `pa`), so a large graph would
exceed 8 postings per term on derived terms alone. Derivation is therefore
opt-in, and when enabled it uses only names of three or more tokens. A collision
beyond the limit among those still fails the build.

## Matching

The matcher is an Aho-Corasick automaton over token IDs, in-repo. `Candidates`
returns every term occurrence, overlapping ones included, with token and byte
spans and all node IDs and kinds. `Select(candidates, visible)` first drops
nodes that are not visible, then candidates left with none, and only then
chooses leftmost-longest spans. A span with two or more visible nodes is marked
`Ambiguous` and lists them all; it is never resolved to one.

## Scope and disclosure

`Bundle.Scope()` is one of:

- `ScopeServerFiltered`: built from `Input.Nodes`, of any visibility. It is used
  only through `ResolveMentions`, which filters every candidate against current
  policy. Its bytes cannot be exported: the package exposes bytes only through
  `Shippable`, which only `ForShipping` on a pinned bundle returns.
- `ScopePinned`: built only from a `lexicon.ScopedNodes`, passed as
  `Input.Scoped`. Only `Client.LexiconScopeNodes` can make a non-zero
  `ScopedNodes`. It goes through a module-internal hook
  (`internal/lexiconscope`), so code outside this module cannot forge one, and
  `Build` refuses the zero value. A pinned build takes its nodes and snapshot
  from the scoped set alone: extra `Input.Nodes` are refused. The digest is not
  supplied by the caller. It is SHA-256 over the caller's authorization
  fingerprint (`auth.AuthorizationFingerprint`: domain, identity, operations,
  sources, policies, generation), the policy generation and the snapshot. So
  two callers' bundles carry different scopes, and the scope round-trips through
  `Load`. Filtering at match time cannot protect bytes someone already holds, so
  only this kind ships.

Inside the module, the hook is guarded twice. The sealer is set once:
`pkg/lexicon` installs it at init, and any later `Install` panics. A test
walks every Go file in the module and fails if any package other than
`pkg/lexicon` and `pkg/explorer/authorized` imports `internal/lexiconscope`.
The walk skips only `.git` and nested modules. It scans `testdata/`,
`node_modules/`, `vendor/` and dot directories, because Go builds a package
there when something imports it by path. A synthetic-module test proves an
importer in each of those places is caught.

**Only a freshly built bundle ships.** `ForShipping` succeeds only for a bundle
that `Build` made from `ScopedNodes` in this process. A bundle obtained through
`Load` never ships, even when its bytes say `ScopePinned`. The digest in bundle
bytes is an **unauthenticated label**: anyone holding bytes can write any digest
there, for example one copied from another caller's `Shippable.Scope()`. A
recipient that needs to know a bundle's scope must get it from the authorized
party that built the bundle, not from the bytes. `Load` also refuses pinned
bytes that carry templates, which `Build` never produces.

A pinned bundle carries **no lookup templates**: `Build` refuses relationships
for it. Templates reveal relation types, and no authorized filter for published
ontology exists yet (see Deferred).

`Bundle` and `Shippable` print only their ID and scope, for every `fmt` verb
(`%s`, `%v`, `%+v`, `%#v`, `%x`, `%q`, ...). The state behind a `Bundle` is
reachable only through a function value, which `fmt` prints as an address. So
a bundle held in an unexported field of another struct cannot print its
contents either.

Visibility, for both `ResolveMentions` and `LexiconScopeNodes`, is what
`Neighborhood` applies to a seed:

- the node's current rule must allow `OperationNeighborhood`. `Read` is neither
  required nor enough.
- for a document-section node, the cataloged revision must still be the base's
  current one. A node whose document the base no longer holds at that revision
  is dropped.

The canonical check runs only on nodes the caller may already see, so its base
reads never depend on hidden or unknown names.

`Client.ResolveMentions(ctx, bundle, text)` makes a hidden node match as nothing,
the same as an unknown name:

1. It authorizes the caller for `OperationNeighborhood` (a dedicated resolve
   operation is planned).
2. It bounds the input to `MaxMentionBytes` (4096) and `MaxMentionTokens` (32).
   Every caller gets the same generic `InvalidArgument` for over-bound input.
3. It requires the bundle's worst case for 32 tokens to fit in
   `MaxCandidateIDs` (2048). That bound depends only on the bundle.
4. It reads the policy store in one `Nodes` call of exactly `MaxCandidateIDs`
   IDs, padded with sentinel IDs under a prefix `Build` refuses. The number of
   calls and the batch size do not depend on what matched or what is hidden.
5. It drops candidates the store does not know, whose current rule denies, or
   that fail the canonical check. It then runs `Select` over the visible nodes,
   and checks the policy generation again. A generation change during the call
   fails it.

Tests compose the real client with memory and durable policy stores. They check
that results, errors, store-call counts and batch sizes for a hidden name equal
those for an unknown name of the same shape. They also cover:

- a shared alias with one hidden node (unambiguous);
- a hidden longer alias over a visible shorter one (the shorter matches);
- a rule change or revocation after the snapshot (dropped);
- a base that diverged from the catalog (dropped);
- a caller with only `Neighborhood` (resolves);
- a generation change during the call (fails);
- scoped builds: no hidden bytes, different scopes per caller, generation and
  snapshot, and no way to widen or forge a scope;
- relabeling the admin's bundle bytes with alice's digest: it loads, claims
  alice's scope, and cannot ship;
- reloading a freshly built bundle: it does not ship either.

A mutation pass (one change at a time) checked that the security-relevant checks
are covered. Four changes survive, each because a second check covers the
same case:

- marking every built bundle as minted (`ForShipping` also requires a pinned
  scope, which only a `ScopedNodes` build has);

- dropping the generation term from the scope digest (the fingerprint already
  includes the generation);
- the zero-digest check in `Build` (`Load`'s scope check refuses the result
  too);
- the build-time token-stability check (no current input reaches it, and
  `FuzzTokenize` checks the property directly).

## Disclosure residuals

Two known ways a hidden node still differs from an unknown name. Both are
accepted for slice 1 and belong with the residue #374 measures.

- **Timing.** Store traffic is fixed (one call, 2048 IDs), but per-ID work is
  not. The memory store clones each registration it finds, and the client
  evaluates its rule; a sentinel or unknown ID is a miss. On the benchmark host
  a matched hidden node costs a few microseconds more than an unknown name.
  Closing this needs a store read whose cost does not depend on presence.
- **Corrupt rules.** If evaluating a found node's rule fails with anything
  other than "unauthorized", `ResolveMentions` returns the existing internal
  "inconsistent data" error, as every other authorized read does. A node the
  store does not know never reaches that check, so with a corrupt catalog a
  hidden node and an unknown name can produce different errors.

## Measurements

Go 1.26.4, linux/amd64, AMD Ryzen 9 7950X 16-Core Processor. Run with
`go test -bench . -benchtime 2s`, once, after normalization version 2.

| Benchmark | Terms | ns/op | B/op | allocs/op |
|---|---|---|---|---|
| `BenchmarkCandidates` (24-token question) | 10,000 | 2,289 | 3,832 | 43 |
| `BenchmarkCandidates` | 100,000 | 2,008 | 3,832 | 43 |
| `BenchmarkCandidates` | 1,000,000 | 1,924 | 3,832 | 43 |
| `BenchmarkResolveMentions` (memory store, 12-token question) | 9 | 148,400 | 291,091 | 712 |

Matching is about 2 µs and does not grow with the lexicon. Authorized
resolution is about 150 µs. Most of that is the padded batch: 2048 lookups in
the memory store, each with its own context check. Of the rest, cloning the
registrations that matched is the main cost. Shrinking `MaxCandidateIDs`
(through a smaller token bound or tighter per-term limits) cuts the padded
batch proportionally.

## Deferred

- An authorized published-ontology filter, so a pinned bundle can carry lookup
  templates for the relation types its scope may see.
- An authorized node-enumeration API for builds.
- Bundle retention and promotion (#405/#406).
- The #373 question gate, which should consume `ResolveMentions`.
- #374 residue.
- Typo and inflection tolerance (abstain instead).
- Renderer wording for template phrase keys.
