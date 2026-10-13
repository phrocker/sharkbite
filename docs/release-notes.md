# Release notes

## Unreleased

### Re-ingesting an earlier revision says so (#674)

Re-ingesting content byte-identical to an earlier revision of a document used
to report success while the document kept serving its current revision. An
operator undoing a relabel (secret, then public, then the secret content
again) was told it worked.

- `explorer.Ingest` reports a new disposition, `historical`, when the content
  matches a non-current revision. It is still a success and never makes the
  old revision current again. There is no restore operation; change the
  content by any byte to make it current.
- The authorized client refuses that case with a `conflict` error wrapping
  `authorized.ErrHistoricalRevision`, and changes nothing.
- The browser upload route returns it naming the file (a 409 when it is the
  first file of the batch; see the next item otherwise): `upload file
  "<name>" matches an earlier revision of its document, so the document was
  left unchanged; change the content to upload it`. This is common when a
  directory is re-uploaded after switching branches back.
- A multi-file upload that fails on any file after the first now reports an
  indeterminate commit outcome, as the remote client already did, because
  earlier files of the batch may have been applied (an earlier file that was
  unchanged wrote nothing). Like every indeterminate write it is a 503
  `unavailable` with the `Shoal-Commit-Outcome: indeterminate` header and
  `"indeterminate": true`, and it keeps the file's message. This covers the
  historical refusal and the missing-reason 400. A failure on the first file
  keeps its own status. Those two refusals are deterministic: retrying the
  same batch returns the same response. Change the named file's content, or
  supply a reason, before retrying.

### Declassification reason codes and an offline listing (#664)

Every relabel that removes visibility labels has been audited since #630. You
could not send a reason with one, though, and nothing shipped could read the
records.

- The browser upload route, the MCP `shoal.ingest` tool and the remote web
  API client take an optional `declassification_reason`. It is a multipart
  form field on the route, with an optional input in the workspace upload
  form, and a tool argument on MCP. The reason is recorded
  on the audit. A malformed reason refuses the batch with a 400 /
  `invalid_argument` before anything is ingested.
- `-require-declassification-reason` (chart:
  `explorer.declassification.requireReason`) refuses such a relabel unless
  it carries a reason. It is off by default.
- `shoal-explore-web -list-declassifications` prints the records, with the
  labels each one removed, and exits. It is offline only, like
  `-list-untranslatable-labels`. No online read of the records exists.
- Retention is not decided. The records are append-only and loaded on every
  start. `docs/shoal-explore-web-deploy.md` gives the expected volume, and the
  decision is open on #664.

### The LLM gateway's listener serves TLS (#424)

`shoal-llm-gateway` refused to send a prompt to a remote provider over
plaintext but accepted the same prompt over plaintext on any address. The
listener now follows the rule the process applies to the hops it makes: TLS
anywhere, plaintext on loopback only.

- `-tls-cert-file` and `-tls-key-file` serve TLS, both or neither. A missing
  file, a malformed certificate or a key that is not the certificate's is
  refused at startup. The pair is re-read on every handshake, so a rotated
  cert-manager Secret is served without a restart; a pair caught halfway
  through a rotation keeps the previous one serving.
- A plaintext listener on a non-loopback address is refused at startup unless
  `-allow-plaintext-listener` acknowledges a mesh sidecar that terminates mTLS.
  It opens the listener only, not the provider hop.
- The chart serves TLS by default from `llmGateway.tls.secretName`, a
  `kubernetes.io/tls` Secret, and names the port `https`. `tls.enabled: false`
  renders the acknowledgement instead. A non-ClusterIP Service with a plaintext
  listener is refused unless `llmGateway.service.allowPlaintext: true`
  acknowledges a terminating load balancer.

**Upgrade action required for every chart deployment of the LLM gateway.** An
existing values file stops rendering with `llmGateway.tls.secretName is
required`. Either name a TLS Secret and move callers to `https://`, or set
`llmGateway.tls.enabled: false` behind an mTLS mesh. A gateway run outside the
chart with a non-loopback `-listen` and neither TLS nor the acknowledgement now
exits at startup. See "Upgrading" in `docs/llm-gateway-deploy.md`.

### A non-sub subject claim has its own identity namespace (#546)

`-oidc-subject-claim X` (any claim but `sub`) used to name principals
`oidc:<iss>#<value>`, the same namespace as `sub`-derived principals. After
a switch from `sub` to `X`, or from one claim to another, the old identities
were still inside the namespace in force, so the approval service's
namespace rule (#553) could not refuse them. A human's old registrations
then counted as current identities under the new scheme, which is the same
self-approval shape #553 closed for `oidcid:`. Now:

- A non-default subject claim names principals `oidc:<iss>#<tag>#<value>`,
  where the tag is 16 hex digits of a digest of the claim name. Its actor,
  client and delegation values take the same prefix. Its requests are
  stamped with its scheme.
- The `sub` namespace `oidc:<iss>#` is flat (legacy Entra excepted). A `sub`, actor, client or
  delegation value containing `#` is refused on both branches, and an
  identity with a `#` after the issuer's is foreign under `sub`. So
  `oidc:<iss>#<sub>` and `oidc:<iss>#<tag>#<value>` cannot be confused.
- The default `sub` scheme and legacy Entra mode keep the same recorded
  scheme digest and start without any flag. Every identity without a `#`
  is unchanged.

**Behaviour change on the default `sub` scheme: `#` is refused.** A token
whose `sub`, or whose `-oidc-actor-claim`, `-oidc-client-id-claim` or
`-oidc-delegation-claim` value, contains `#` is now refused with `401`, on
the workspace branch and on the approver branch. The scheme digest does not
change, so **nothing at startup warns about this**. If the client-ID or
actor claim you configured routinely carries `#`, every user is locked out.
Before upgrading, check that none of these claims can contain `#`. If one
can, configure a different claim. The first refusal for each claim is
logged at WARN, naming the claim but never the value, and the log repeats
at most once a minute per claim. `shoal-demo-seed` likewise refuses an
`oidc_subject_id` containing `#`, and an issuer containing one.

**Upgrade action required for deployments that set `-oidc-subject-claim`.**
The scheme digest covers the identity format, so such a deployment's
recorded scheme no longer matches, and the server refuses to start. The
refusal explains this and prints the digest to pass as
`-oidc-identity-scheme-migrate`. After the migration, identities minted
before the upgrade are refused wherever they are involved in an approval
until #526's adoption route lands, and requests pending at the upgrade can
only expire. See "Upgrading a deployment that sets `-oidc-subject-claim`" in
`docs/shoal-explore-web-deploy.md`.

### Free-form visibility labels are enforced (#570)

`metadata["shoal.visibility"]` labels used to be stored on a document's nodes
and nowhere else, so every holder of the document's source could read it. They
are now enforced on every read path by translating each label into a
structured policy on the document's source and conjoining it into the
document's access rule. This ships as three changes that must be released
together:

- **Ingest** (#585). A labelled ingest is registered under the source policy
  and one policy per label; the ingester must hold every label it writes.
- **Grants** (#590). Labels are granted only by the operator: the
  `shoal.label-grants/v1` file (`-oidc-label-grants-file`), `-dev-auth-labels`
  for local development, and `shoal-mcp -identity-labels`. A label no grant
  names is visible to nobody.
- **Migration** (this change). On every start, before serving,
  `shoal-explore-web` and `shoal-mcp` narrow the catalog rule of each document
  labelled under an earlier release, including its historical revisions,
  extracted entities and relations, and source claim. A document whose labels
  cannot be translated becomes readable by nobody and is listed by
  `shoal-explore-web -list-untranslatable-labels`. It runs on every start, so
  documents labelled by an older binary after a rollback are closed on the
  next start; any error refuses to start. A document whose corpus revision
  is newer than its registered one (a failed registration) is narrowed by
  its registered labels and reported for a retried ingest, never locked.

**Upgrade action required.** Write the label grant file before starting the
new release, then review the untranslatable list and rebuild lexicon bundles.
The steps are in "Upgrading: labelled documents are tightened at startup" in
`docs/shoal-explore-web-deploy.md`.

This differs from earlier tightenings such as #544, which applied at the next
registration and left stored records resolving. Labelled documents already in
the corpus would stay readable under that model, so this one rewrites stored
authorization at startup instead.
