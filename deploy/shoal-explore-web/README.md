# shoal-explore-web deployment

Single-instance Shoal Explorer workspace: one binary, one state directory, no
external datastore. See [`docs/shoal-explore-web-deploy.md`](../../docs/shoal-explore-web-deploy.md)
for the full guide (image, configuration seam, the unsafe-configuration guard,
persistence and the split-brain guard, and Azure options).

## Local, one command

```console
docker compose -f deploy/shoal-explore-web/docker-compose.yml up --build
```

Runs the loopback-gated development authenticator (`-dev-auth`) over host
networking. On Linux, open <http://127.0.0.1:8098>. On Docker Desktop, "host" is
the Docker VM — see the guide.

Files:

- `docker-compose.yml` — the local one-command profile (host networking,
  `-state-dir /var/lib/shoal`, single state-root volume `explorer-state`).
- `.env.example` — copy to `.env` to override `SHOAL_EXPLORE_LISTEN`.
- `shared.env.example` — placeholder-only OIDC, host-authority, and optional
  Fleet configuration for a shared instance.
- `demo-scenario.json.example` — deterministic two-user seed configuration;
  see [`docs/multi-user-demo.md`](../../docs/multi-user-demo.md).
- `azure/` — Bicep and guidance for a single shared **Azure** instance
  (App Service for Containers, one writer, Azure Files state root). See
  [`azure/README.md`](azure/README.md).

## Volume layout: one state root

Configure `-state-dir /var/lib/shoal` and mount that one directory. **Persist
`/var/lib/shoal` and both the corpus and the authorization catalog survive a
restart:**

- `/var/lib/shoal/corpus` — the document corpus.
- `/var/lib/shoal/policy` — the durable authorization catalog (#284 / PR #288). It
  is a **sibling** of the corpus, never inside it (the engine treats every corpus
  subdirectory as a table), and falls under the same single mount.

Mounting the corpus directory alone would drop every authorization registration
on restart; the command's split-brain guard then refuses to start (in production
mode) rather than serving an under-authorized corpus. `-data` and `-policy-dir`
remain available for legacy/custom layouts — see the guide.

The image (`Dockerfile.shoal-explore-web`, at the repo root) is the same artifact
a shared instance would run; only the flags differ. For a shared, non-loopback
instance, supply the provider-neutral OIDC authenticator instead of `-dev-auth`
(see [Provider-neutral OIDC authentication](../../docs/shoal-explore-web-deploy.md#provider-neutral-oidc-authentication))
and set `-allowed-host` / `SHOAL_ALLOWED_HOST` to the external name(s) the proxy
forwards.

## Host authority

The handler refuses any request whose `Host`/`:authority` does not exactly match
an allow-list (case-insensitive hostname, exact port), returning `421 Misdirected
Request`. It defaults to the resolved listen address, so the loopback compose
profile above needs no configuration: it is reached at `http://127.0.0.1:8098`,
whose `Host` is the bind authority. This local profile uses **host networking**,
so the container binds `127.0.0.1` directly in the host namespace and that
default authority is exactly what the browser sends.

A shared instance that binds a wildcard address (`0.0.0.0:<port>`) resolves to an
authority no real client `Host` carries, so it **fails closed** — every request
`421`s — until `-allowed-host` names the external authority (for example
`-allowed-host explorer.example.test`, comma-separated for more than one name).
`X-Forwarded-Host` is never trusted. See
[Host authority](../../docs/shoal-explore-web-deploy.md#host-authority-required-for-a-public-bind)
in the guide.
