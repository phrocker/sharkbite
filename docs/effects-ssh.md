# effects-ssh: one SSH command as one dispatch action

Slice 1 of #450. `extensions/effects-ssh` runs operator-registered commands
over SSH for the effects gateway (`docs/effects-gateway-deploy.md`). An
agent that needs a command run on a host enqueues one action. The gateway
claims it, sends one HTTP request to this service, and completes the action
from the answer. This service runs the command at most once per action and
says what it knows about how the command ended.

The design is the last comment on #450. In short, this service is an HTTP
target of the existing gateway, not a second worker. Claiming, fencing, claim
extension, completion, ambiguity reports, drain and the unrecorded log all stay
in `internal/effectsgateway`. The service adds three things:

- running a command over SSH, with pinned host keys;
- a durable journal that makes the command run once per idempotency key;
- an answer contract that the gateway's route turns into the right record.

```
agent ──enqueue──> explorer <──pull/claim/complete── shoal-gateway
                                                         │ POST /v1/commands/{name}
                                                         │ Idempotency-Key: <ExecutorKey>
                                                         v
                                              shoal-effects-ssh ──ssh exec──> host
                                                 journal (fsync'd)
```

## What it does, and what it will not do

- **Nothing comes from the request.** The operator registers each command by
  name, together with its host and port, pinned host keys, user, private key
  file and the exact command string. The request names the command in its
  path. Its body must be empty or `{}`, and anything else is refused. v1 has
  no parameters.
- **An unregistered command is refused before any connection is opened**, and
  nothing is journaled for it. So are requests with a bad credential, method,
  path, query, body or key.
- **Host keys are pinned.** There is no `known_hosts` file, no trust on first
  use and no `InsecureIgnoreHostKey`. The client offers only the pinned key
  types, so a host that holds several keys presents a pinned one, and
  certificates are refused. A host whose key is not pinned gets no credential:
  the handshake ends before authentication.
- **The SSH session runs exec only.** The client makes no PTY request, sets no
  environment variables, requests no agent or X11 forwarding, opens no port
  forward and uses no subsystem. It rejects every channel the server opens
  toward it and answers every global request false. Stdin is closed at once.
  A test asserts that `exec` is the only request the server ever sees.
- **Authentication uses one unencrypted private key.** There is no password,
  no keyboard-interactive login and no agent. The key file is read on every
  run, so a rotated Secret takes effect without a restart.
- **Everything is bounded.** The connect timeout covers TCP, the handshake and
  authentication together, and then bounds the session open. The run
  timeout covers the command from the exec request to its exit: when it ends,
  the connection is closed whatever the client is waiting on, including a host
  that never replies to exec. Stdout and stderr are each kept up to `max_output_bytes`. The rest is
  read and counted, so the host is never stalled, and truncation is recorded.
  A keepalive every 30 seconds ends a run on a dead connection as unknown.
- **No session-gateway state.** Nothing here depends on #447. The SSH client
  is the package `extensions/effects-ssh/sshclient`, which imports only the
  standard library and `golang.org/x/crypto/ssh`
  (`TestSSHClientIsSelfContained`). The session gateway can share it, either
  by moving it to a shared module once a second consumer exists, or by
  importing it once the import boundary allows extension-to-extension imports.
  Both are #447's decision. Until then it stays inside the only module allowed
  to import `x/crypto/ssh`, which `internal/importboundary`'s
  `TestSSHIsImportedOnlyByItsModules` and CI's "Core links no SSH" step
  enforce.

## The once-only argument

The gateway sends the action's `ExecutorKey` in its `-idempotency-header` on
every attempt: the first send, a resend after a lost answer, and a resend
after a re-claim by a restarted gateway. The key belongs to the action record,
so every attempt carries the same key (#391).

The service keeps a journal keyed by that value:

1. **`started` is fsync'd before any connection is opened.** If the record
   cannot be made durable, the request is refused (`journal_unavailable`) and
   nothing is dialed. `TestSyncsPrecedeTheDial` checks the fsync order, and `TestStartedIsDurableBeforeTheRun` reads the file from
   inside the run.
2. **A key with a `started` record is never dialed again.** A repeat is
   answered from the journal:
   - The key's `finished` record is durable: the recorded result is replayed.
   - This process is still running the key: the repeat waits up to
     `-attach-wait` for the result, then answers `in_progress`.
   - Otherwise (the process restarted, or the finish could not be written):
     `outcome_unknown`. It is never run again.
3. **`finished` is written when the run ends**, and every answer is a function
   of what is durable. If the finish cannot be written, the request is
   answered `outcome_unknown`, and so is every repeat from this process. The
   journal is then failed, no new key starts, and the process exits 3. A
   finish whose write reached the page cache and whose fsync failed may still
   be read back after a restart, so a later repeat can replay it: each answer
   is honest, but they need not agree across restarts.
4. **A different request under a known key runs nothing** and is answered
   `key_conflict`, and the key's entry is left as it was. The fingerprint is
   the command name. The gateway's binder is pure, so the same action sends
   the same request unless its route changed between attempts (a gateway
   restarted with a new `-routes` while the action was in flight). The key's
   command may then have run, so `key_conflict` is a 409 that the gateway
   records exactly as `outcome_unknown`, never as a rejection.
5. **Stopping does not refuse known keys.** From SIGTERM on, a new key is
   answered `busy` (nothing journaled, retried), while a known key is still
   answered from the journal: replayed, attached to its run, or
   `outcome_unknown`. Refusing it would read as "nothing ran".

So a command is dialed at most once per key while the journal remembers the
key, whatever happens to the gateway: a worker restart, a lost fence, a
duplicate delivery or a lost answer. When nothing establishes how the command
ended, the record says `outcome_unknown`, with `EffectPossible` set by the
claim.

**Crash windows.** A process killed before `started` is durable dialed
nothing. The torn line it may leave is dropped when the journal next opens
(`TestTornStartedTailIsDropped`). A process killed after `started` and before
`finished` leaves the key answering `outcome_unknown`. A torn `finished` line
is dropped, which leaves exactly that state
(`TestTornFinishedTailLeavesUnknown`). `TestKillBetweenStartedAndFinishedNeverRunsAgain`
does this with a real `SIGKILL` of a child process while its command runs,
then restarts on the same journal: the repeat answers `outcome_unknown` and
the host sees one exec. Only a final fragment without its newline is a torn
tail. If a complete line fails its checksum, or a record contradicts the
records before it, the journal is corrupt; that includes a final line that
ends in a newline but whose earlier pages a node crash never wrote, which
fails closed. It refuses to open
and nothing runs until an operator has looked. Do not delete a journal to get
past this: deleting it forgets every key it holds.

**Where it stops holding.**

- **Retention.** A key is forgotten `-journal-retention` after its last record.
  The gateway sends a key only within `-idempotency-retention` of the action's
  creation (its PRECHECK), so set the journal's retention to at least the
  gateway's retention plus any clock skew between the explorer and this host.
  The gateway cannot read this service's retention. Keeping the two in step is
  a deployment rule, written down here. Retention uses the wall clock, so a
  clock that jumps forward can prune a key early. A key past retention that
  the process still holds is replayed, not re-run; only a key already
  forgotten starts again, and its older records are then superseded, even if
  the retention or the clock has changed since
  (`TestKeyReusedAfterRetentionStillOpens`).
- **The journal is the guarantee.** It does not hold if the journal volume is
  lost or replaced, if the journal is moved aside, or if two instances share
  one journal on a filesystem where `flock` does not exclude them (a network
  filesystem). The journal directory is held under an exclusive `flock`, so a
  second process on the same node is refused. Use a `ReadWriteOnce` volume and
  one replica.
- **Once-only applies to the exec request, not to what the command does.** A
  command interrupted by the run timeout, a lost connection, or a host that
  closes the session without an exit status ends as `outcome_unknown`. The
  service does not retry it, and it does not kill it. Closing the session
  does not stop a command already running on the host. If the exec request may
  have been sent and its reply was lost, the outcome is unknown and the command
  is not run again.
- **`not_run` is claimed only when no exec request was sent**: the
  connect, handshake, host key or authentication failed, the session open was
  refused, or the key file could not be loaded. The exec request was never
  sent.
- **A command that leaves a background child holding stdout** keeps the
  session open until the run timeout, and is recorded `outcome_unknown` even
  if it exited. Redirect such children's output on the host.
- **Platforms.** The journal runs on Linux and the BSDs. It refuses to open on
  macOS, whose `fsync` does not flush the drive's write cache (only
  `F_FULLFSYNC` does), and on any platform without `flock`.
- **The lock is a file in the journal directory.** Deleting `LOCK` while the
  server runs lets a second process lock a new one. Do not.
- **Once per action, not once per intent.** A new action is a new key. An
  agent that enqueues a second action for the same command runs it again.

## The contract

`extensions/effects-ssh/testdata/contract` pins both halves.
`route.json` is the gateway route, and `answers.json` holds every answer's
exact status, headers and body, with the gateway classification each one must
get.

- The service's tests assert that it writes exactly those bytes in every
  scenario (`TestAnswersAreThePinnedBytes` and the scenario tests in
  `service/`).
- The gateway's tests (`internal/effectsgateway/effectsssh_contract_test.go`)
  parse the same `route.json` and classify every answer with it. They also
  run the real worker loop against a target that serves the pinned bytes:
  a lost answer that is replayed, a restart mid-run that ends
  `outcome_unknown` and is not sent again, in-progress then done, busy,
  refusals, and a worker killed with the request written and replaced.
- `TestEffectsSSHRouteWithoutItsRulesIsUnsafe` shows the route's `conflict`
  and `retryable` rules are load-bearing. Without them, `outcome_unknown` and
  `in_progress` would be recorded as definite rejections.

| answer | status | body | the gateway records |
|---|---|---|---|
| completed | 200 | `{"state":"completed","reference":"exit-0"}` | succeeded |
| completed, replayed | 200, `Idempotent-Replayed: true` | the same | succeeded, `idempotency: replayed` |
| outcome unknown | 409 | `{"state":"outcome_unknown"}` | failed, `outcome_unknown` |
| in progress | 503, `Retry-After: 1` | `{"state":"in_progress"}` | retried under the same key |
| busy | 503, `Retry-After: 5` | `{"state":"busy"}` | retried; nothing was journaled |
| key conflict | 409 | `{"state":"key_conflict"}` | failed, `outcome_unknown` |
| not run | 502 | `{"state":"not_run"}` | failed, `target_rejected_502` |
| refused | 400, 401, 404, 405, 500, 507 | `{"state":"refused","reason":"…"}` | failed, `target_rejected_NNN` |

Why these statuses:

- **409 outcome unknown** relies on the route's conflict rule (`status: [409]`,
  `pointer: /state`, `equals: ["completed"]`). In the gateway's table, a
  conflict status whose body is not the done marker, and that is not
  retryable, is `outcome_unknown`. It is never a rejection and never retried.
  The service never sends 409 with `state: completed`. The marker is in the
  rule because the rule requires one. A 409 carrying it would truthfully mean
  "already done".
- **503 is the only retryable status.** The service sends it only when a
  retry is safe: the key is running in this process, or it was refused before
  anything was journaled. Every retry goes back through the journal, which is
  what makes key-route retries safe. A command that outlives every attempt
  ends as `retry_exhausted`, which records that the request was written.
- **A completed command is a success whatever its exit status.** The command
  ran, so the action happened. A non-zero exit or a signal is in `reference`:
  `exit-3`, `signal-KILL` (any name outside `[A-Z0-9]{1,16}` becomes `OTHER`),
  with `-truncated` appended when either stream exceeded its bound. Recording
  a non-zero exit as `target_rejected` would claim that nothing happened.
- **`not_run` and the refusals are definite failures**, because nothing ran.
  Every refusal is for a key the journal does not hold: an unregistered
  command, a credential or input refusal, or a full or failed journal, all
  before any dial. A request the service would refuse whose key the journal
  does hold, in any header, is answered `outcome_unknown` instead: a resend
  after the credential was rotated, the key header was renamed or duplicated,
  or the route's method or path changed. A resend that carries no key at all
  cannot be recognised, so do not change a route to send no key, or change
  `-idempotency-header` on both sides, while its actions are in flight.
- **No hop between the gateway and this service may answer for it.** A proxy
  that answered 502 or 504 after forwarding would be recorded as a
  rejection while the command may have run. The sidecar shape has no such
  hop.

### What reaches `ActionRecord.Output`

On success, exactly the gateway's closed output and nothing else:

```json
{"status": 200, "idempotency": "key", "reference": "exit-0"}
```

`idempotency` is `replayed` when the answer replays an earlier request's
result. `reference` matches
`(exit-[0-9]{1,10}|signal-[A-Z0-9]{1,16})(-truncated)?`; the gateway checks the
pattern again before copying it. On failure, the record carries only the error
code (`outcome_unknown`, `retry_exhausted`, `target_rejected_NNN`,
`request_not_sent` or `input_invalid`). `Effected` stays empty for a loopback
target, which egresses nothing.

**Stdout and stderr never leave this service.** They are kept, bounded, in the
journal for the operator (`shoal-effects-ssh journal show`), and are not in
the HTTP answer, so the gateway's `-max-response-bytes` never truncates the
reference away. The service's log carries a 12-hex-digit digest of the key,
the command name, the outcome, the reference and byte counts. It never carries
the key, the command string, output or the credential.

## The gateway route and the action

Register each command as one action whose route has a literal path, so the
agent's input chooses nothing. A templated `/v1/commands/{name}` path would
let the input pick any registered command. Do not configure one.

```json
[
  {
    "action": "restart-web",
    "method": "POST",
    "path": "/v1/commands/restart-web",
    "effects": ["external"],
    "idempotency": "key",
    "conflict": {"status": [409], "pointer": "/state", "equals": ["completed"]},
    "retryable": [503],
    "reference": {"pointer": "/reference", "pattern": "(exit-[0-9]{1,10}|signal-[A-Z0-9]{1,16})(-truncated)?"}
  }
]
```

The route must be `idempotency: key`. The service refuses any request without
a key (`idempotency_key_invalid`), so a route misconfigured as `unprotected`
fails every action before anything runs. `effects` is `["external"]` for a
loopback target and `["external", "egresses-content"]` for a remote one; the
gateway derives it from `-target-base-url` and refuses a mismatch.

The descriptor's action must match the route, as `VerifyDescriptor` checks at
startup:

- `effects`: the route's.
- `input_schema`: the route's `InputSchema()`, which for a literal POST path is
  `{"type":"object","properties":{"body":{}},"additionalProperties":false}`.
  The agent enqueues `{}`. An input with any `body` other than `{}` binds and
  is then refused by the service as `body_not_allowed`, with nothing run.
- `output_schema`: the gateway's canonical schema.

The executor reference must be in `explorer.fleet.externalExecutorRefs` (for a
loopback target) or `externalEgressExecutorRefs` (for a remote one).

## Configuration

`-config` is strict JSON. Duplicate keys at any depth, keys not spelled
exactly, unknown fields and trailing data are refused. Every private key file
is loaded at startup, so a missing or encrypted key refuses the start.

```json
{
  "commands": {
    "restart-web": {
      "address": "web-1.internal:22",
      "host_keys": ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA... root@web-1"],
      "user": "deploy",
      "private_key_file": "/etc/effects-ssh/keys/deploy",
      "command": "sudo systemctl restart web",
      "connect_timeout": "10s",
      "run_timeout": "2m",
      "max_output_bytes": 16384
    }
  }
}
```

| field | rule |
|---|---|
| name (the key) | `[A-Za-z0-9][A-Za-z0-9._-]{0,63}`; the route path's last segment; at most 256 commands |
| `address` | `host:port`, required |
| `host_keys` | 1 to 8 `authorized_keys` lines with no options; ed25519, ECDSA or RSA (offered with SHA-2 only); no certificates |
| `user` | required, at most 64 bytes, no spaces or control characters |
| `private_key_file` | required; one unencrypted private key, at most 64 KiB |
| `command` | required; one line, at most 4096 bytes, no control characters. A multi-line script belongs in a file on the host |
| `connect_timeout` | default `10s`, at most `60s` |
| `run_timeout` | required, at most `1h` |
| `max_output_bytes` | per stream, default 16384, at most 262144 |

Get a host's key out of band, for example from
`/etc/ssh/ssh_host_ed25519_key.pub` on the host itself or from your
provisioning system. `ssh-keyscan` is trust on first use, the thing pinning
exists to avoid.

### Flags (`shoal-effects-ssh serve`)

| flag | default | rule |
|---|---|---|
| `-config` | | required |
| `-listen` | | required, `host:port` |
| `-tls-cert-file`, `-tls-key-file` | | both, or neither with `-allow-plaintext-loopback` |
| `-allow-plaintext-loopback` | false | plaintext HTTP, refused unless `-listen` is a loopback address and the bound address is loopback |
| `-caller-token-file` | | required; at least 32 printable bytes; read on every request |
| `-idempotency-header` | `Idempotency-Key` | the gateway's `-idempotency-header` |
| `-journal-dir` | | required; must hold a journal made by `journal init` (serve never creates one) |
| `-journal-id` | | required; the identity `journal init` printed. A directory with no journal, or another journal, is refused |
| `-journal-retention` | | required, at least `1m`; at least the gateway's `-idempotency-retention` plus clock skew |
| `-journal-max-entries` | 10000 | 1 to 1048576 keys, and with the largest `max_output_bytes` at most 512 MiB of live records (below); a new key beyond it is refused `journal_full` |
| `-max-concurrent` | 4 | 1 to 64 runs; a new key beyond it is answered `busy` |
| `-attach-wait` | `30s` | below the gateway's `-operation-timeout` |
| `-shutdown-grace` | `60s` | on SIGTERM, how long runs may finish; then their connections are closed and they are journaled `unknown` |

**The journal's size is declared.** With `-journal-max-entries` N and the
largest `max_output_bytes` M, one key's records take at most
`2 × base64(M) + 2 KiB`. The file holds at most `2N + 1024` keys' worth before
a compaction rewrites it. The start line logs both figures
(`journal_max_entry_bytes`, `journal_bound_bytes`). With the defaults, that is
about 960 MB at most, and far less in practice.

**Compaction stalls requests, boundedly.** Compaction copies the live records
to a new file while holding the journal's lock, so requests wait for it. The
start refuses an `-journal-max-entries` and largest `max_output_bytes` whose
live records could exceed 512 MiB (`N × (2 × base64(M) + 2 KiB)`), which
bounds the stall to that copy, about a second on a local SSD. The defaults
allow about 460 MB. During a stall the gateway sees only latency: a timed-out
attempt is resent under the same key and answered from the journal.

**Caller authentication is mandatory.** The gateway sends
`-target-credential-file` verbatim in `-target-auth-header` (`Authorization`).
Store `Bearer <token>` in the gateway's file and `<token>` in this service's
`-caller-token-file`. The token is compared in constant time. An unreadable
token file refuses every request; it never falls open. There is no
unauthenticated mode. mTLS is not offered, because the gateway's target client
presents no client certificate today. When it does, a pinned client identity
can replace the token. Plaintext is allowed only on loopback, and only with
the flag, as for the LLM gateway (#424).

### Timing

- `2 × connect_timeout + run_timeout + 10s` (connect, then the session open, each bounded by the connect timeout, then the run, the close and the fsyncs) must be below the gateway's
  `-operation-timeout` (T), so the first attempt normally gets the answer.
  If it does not, the resend attaches to the running command (`-attach-wait`)
  and gets the result or `in_progress`. Nothing runs twice, but the gateway
  spends its attempts.
- Long commands need the gateway's `-renew`, which extends the claim while the
  request is outstanding (#430). If the fence is lost mid-command, the gateway
  reports an ambiguity (#438). The re-claim resends the same key, and the
  journal answers it.
- Set `-shutdown-grace` to at least the longest `run_timeout`, and the pod's
  `terminationGracePeriodSeconds` above it.

## Deploying

The chart does not render this extension; extensions ship their own images
(`docs/gateways.md`). Two shapes:

**Sidecar (recommended).** Run it in the gateway's pod on loopback:

```
# once, on the journal's volume; prints the journal's identity
shoal-effects-ssh journal init -journal-dir /var/lib/effects-ssh

shoal-effects-ssh serve -config /etc/effects-ssh/commands.json \
    -listen 127.0.0.1:8443 -allow-plaintext-loopback \
    -caller-token-file /run/secrets/effects-ssh/token \
    -journal-dir /var/lib/effects-ssh -journal-id <identity> -journal-retention 48h

shoal-gateway run -capability effects.ssh -surface-name web-fleet \
    -target-base-url http://127.0.0.1:8443 -target-allow-private \
    -target-credential-file /run/secrets/effects-ssh/authorization \
    -idempotency-header Idempotency-Key -idempotency-retention 24h \
    -operation-timeout 3m -renew -routes "$(cat routes.json)" ...
```

The effects are `{external}`. Give the journal its own `ReadWriteOnce`
persistent volume, separate from the gateway's `-unrecorded-dir`, and run one
replica.

**Initialize the journal once, and pin it.** `serve` never creates a journal.
`journal init` writes the first line, a header with a random identity and
the creation time, fsyncs it and its directory entry, and prints the
identity; it refuses a directory that already holds a journal file, even an
empty one. Put the identity in the deployment as `-journal-id`. Then a
volume that fails to mount, leaving the image's empty directory, refuses the
start instead of serving a fresh journal that has forgotten every key, which
would re-run a command whose outcome was unknown. A different journal (another
volume, or a wiped and re-initialized one) is refused by the identity. Run
`journal init` as a one-off job against the volume, not in the pod's start
script, or the protection is gone. Mount the volume at `-journal-dir` itself:
a `journal.log` that is a symlink is refused, because compaction's rename
would replace the link with a file off the volume and leave the old journal
behind the link. A symlinked directory is fine. A journal whose identity is
not the pinned one is refused at its first line and left untouched. The pod's egress policy must allow the SSH hosts' addresses and
ports. The chart's per-entry NetworkPolicy assumes a single target, so a
sidecar deployment writes its own policy.

**Separate Deployment.** Serve TLS (`-tls-cert-file`, `-tls-key-file`) and
point the gateway at `https://effects-ssh.<namespace>.svc:8443`. The gateway
verifies the certificate against its system roots, so issue it from a CA
those roots contain, or add that CA to the gateway image's trust store
(`SSL_CERT_FILE`). The effects are `{external, egresses-content}`, because
the gateway cannot tell that this target is in the cluster. The route and the
descriptor must declare both.

### Operating it

```
shoal-effects-ssh journal list -journal-dir DIR
shoal-effects-ssh journal show -journal-dir DIR -key <key or 12-hex digest>
```

`list` prints the header (the journal's identity and creation time) and
every record's metadata: key, digest, command, times, outcome, reference,
byte counts and truncation. `show` adds the kept output for one key. Neither
takes the lock or changes the file, so both are safe while the server runs.
Both check the records by the same rules the server does. On a corrupt
journal, which the server refuses to open, they print every record before the
bad line, then its line number, byte offset and reason, and exit 1: that is
the operator's view of the journal in the one state where nothing runs until
someone has looked. A file with no complete header is reported as uninitialized, which is how the server treats it, and a read error is reported as such, not as corruption.

| exit code | meaning |
|---|---|
| 0 | stopped cleanly |
| 1 | the listener failed, or closing the journal failed |
| 2 | the command line or configuration is wrong, or the journal is missing, uninitialized, not the pinned one, locked by another process, or corrupt |
| 3 | a journal write failed. No new key was started after it, and the process drained and exited. A restart re-reads the file |

## Deferred

- Parameters. v1 runs fixed strings only. Parameters need a schema, an
  encoding that cannot reach the shell, and a fingerprint that covers them.
- A chart entry and an image for the extension.
- mTLS to the gateway, once the gateway's target client can present a client
  certificate.
- A shared module for `sshclient`, with #447.
