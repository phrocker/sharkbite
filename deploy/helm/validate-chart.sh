#!/usr/bin/env bash
#
# Chart checks that run locally and in CI.
#
# Three things are verified, and the third is the one that matters. Rendering
# and schema-validating the chart proves it produces well-formed Kubernetes
# objects. But the explorer and llm-gateway templates exist to make a
# misconfiguration fail at render rather than at runtime in a pod log, and a
# guard that stops firing is invisible: the chart renders, installs, and
# produces a workspace that denies everything or answers nothing — or a gateway
# that passes every probe and refuses every call. So each guard is asserted to
# refuse, and each valid configuration is asserted to render.
#
# These assertions are themselves mutation-tested: each guard is removed in
# turn and the case that must then fail is confirmed to fail. A check that
# cannot fail is worse than no check, and adding one here without that
# confirmation is how two chart guards have shipped broken before.
#
#   deploy/helm/validate-chart.sh
#
# Requires helm, python3 with PyYAML, and optionally kubeconform for schema
# validation. Each missing tool is reported rather than quietly skipped, so a
# run that checked less than it looks like says so.
set -euo pipefail

# Hermetic: no cluster, ever, unless asked for. An ambient kubeconfig (the
# operator's own, or a CI runner's) can point at a real cluster, and Helm
# contacts it from `helm install --dry-run=client` and every server dry run.
# The chart's lookup calls read from it too. So the script's own kubeconfig
# is a path that does not exist, and the HELM_KUBE* overrides are cleared.
# Only the opt-in lookup section reaches a cluster, by passing
# --kubeconfig "$SHOAL_LOOKUP_KUBECONFIG" explicitly, and it accepts only a
# kind or k3d context.
hermetic_kubeconfig=/nonexistent/shoal-validate-chart
export KUBECONFIG="$hermetic_kubeconfig"
unset HELM_KUBECONTEXT HELM_KUBEAPISERVER HELM_KUBETOKEN HELM_KUBEASGROUPS \
  HELM_KUBEASUSER HELM_KUBECAFILE HELM_KUBETLS_SERVER_NAME HELM_KUBEINSECURE_SKIP_TLS_VERIFY

chart="$(cd "$(dirname "${BASH_SOURCE[0]}")/shoal" && pwd)"
failures=0

note() { printf '%s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*"; failures=$((failures + 1)); }

# A valid explorer configuration, as the preset supplies it. The refusal cases
# below each break exactly one thing in it, so a refusal can only be caused by
# the value under test.
valid_explorer=(
  --set explorer.enabled=true
  --set explorer.auth.mode=oidc
  --set explorer.auth.oidc.issuer=https://issuer.example.test/
  --set 'explorer.auth.oidc.audiences={shoal}'
  --set explorer.auth.oidc.authorizationClaim=groups
  --set 'explorer.auth.oidc.readerValues={readers}'
  --set 'explorer.allowedHosts={shoal.example.test}'
)

# The shipped profile leaves every required value empty on purpose, so it does
# not install as-is. Filling it is what an operator does; this is that.
explorer_base=(-f "$chart/values-explorer.yaml" "${valid_explorer[@]}")

# A valid LLM gateway configuration, on the same principle: every refusal case
# below breaks exactly one thing in it.
#
# The scope identities are real base64url ("source" and "policy"), because the
# chart checks the encoding — a stand-in like "src" would make the valid case
# fail for a reason that has nothing to do with the case under test.
valid_llm_gateway=(
  --set llmGateway.enabled=true
  --set 'llmGateway.allowedHosts={llm.example.test}'
  --set llmGateway.admission.url=https://shoal.example.test
  --set llmGateway.admission.credentialSecretName=shoal-admission-token
  --set llmGateway.upstream.baseURL=https://api.example.test/v1
  --set llmGateway.upstream.credentialSecretName=shoal-upstream-key
  --set llmGateway.identity.agentID=Z292ZXJuZWQtcHJveHk
  --set llmGateway.identity.capability=chat.completions
  --set llmGateway.identity.action=complete
  --set llmGateway.identity.sourceID=c291cmNl
  --set llmGateway.identity.policyID=cG9saWN5
  --set llmGateway.tls.secretName=shoal-llm-gateway-tls
)
llm_gateway_base=(-f "$chart/values-llm-gateway.yaml" "${valid_llm_gateway[@]}")

# The same configuration with the admission token coming from a file instead of
# an environment variable, as a projected ServiceAccount token.
#
# It is a separate fixture rather than one more --set on the base, because the
# two forms require different things and a fixture that carried both could not
# express either. The admission Secret is cleared on purpose: naming one here
# is refused, since nothing in the projected form reads it, and leaving it set
# would make every case below pass or fail for that reason instead of its own.
valid_token_file=(
  --set llmGateway.admission.credentialSecretName=
  --set llmGateway.admission.tokenFile=/var/run/secrets/shoal/token
  --set llmGateway.admission.tokenAudience=shoal
  --set llmGateway.serviceAccountName=shoal-llm-gateway
)
token_file_base=("${llm_gateway_base[@]}" "${valid_token_file[@]}")

# A valid effects gateway entry (#391), on the same principle. It is a values
# file rather than --set flags because the route table is a list of maps. The
# dispatch URL is loopback so the binary-acceptance check below can run the
# rendered arguments against it and get as far as the descriptor resolve.
effects_fixtures="$(mktemp -d)"
trap 'rm -rf "$effects_fixtures"' EXIT
cat > "$effects_fixtures/gateway.yaml" <<'GATEWAY'
effectsGateways:
  - name: stripe
    enabled: true
    agentID: c3RyaXBlLWdhdGV3YXk
    surfaceName: stripe
    executorRef: stripe
    dispatch:
      url: http://127.0.0.1:1
      tokenAudience: shoal-executors
    target:
      baseURL: https://api.example.test
      credentialSecretName: stripe-api-key
      idempotencyHeader: Idempotency-Key
      idempotencyRetention: 24h
    routes:
      - action: charge
        method: POST
        path: /v1/accounts/{account}/charges
        effects: [external, egresses-content]
        idempotency: key
        conflict: {status: [400], pointer: /error/type, equals: [idempotency_error]}
        retryable: [429, 503]
        reference: {pointer: /id, pattern: "ch_[A-Za-z0-9]+"}
GATEWAY
effects_gateway_base=(-f "$effects_fixtures/gateway.yaml")
# A second, distinct surface beside the first: every identity differs. The
# identity-sharing cases below each make one of them the same.
cat > "$effects_fixtures/two-gateways.yaml" <<'GATEWAYS'
effectsGateways:
  - name: stripe
    enabled: true
    agentID: c3RyaXBlLWdhdGV3YXk
    surfaceName: stripe
    executorRef: stripe
    dispatch: {url: "http://127.0.0.1:1", tokenAudience: shoal-executors}
    target: {baseURL: "https://api.example.test", credentialSecretName: stripe-api-key}
    routes:
      - {action: charge, method: POST, path: /v1/charges, effects: [external, egresses-content], idempotency: natural}
  - name: ledger
    enabled: true
    agentID: bGVkZ2VyLWdhdGV3YXk
    surfaceName: ledger
    executorRef: ledger
    dispatch: {url: "http://127.0.0.1:1", tokenAudience: shoal-executors}
    target: {baseURL: "https://ledger.example.test", credentialSecretName: ledger-api-key}
    routes:
      - {action: post, method: PUT, path: "/v1/entries/{id}", effects: [external, egresses-content], idempotency: natural}
GATEWAYS
two_gateways=(-f "$effects_fixtures/two-gateways.yaml")
# The explorer in the same release, with the executor mapping that binds the
# fixture's ServiceAccount (release shoal, namespace default) to stripe.
cat > "$effects_fixtures/explorer-mapping.yaml" <<'MAPPING'
explorer:
  fleet:
    executorRefs: [stripe]
    externalEgressExecutorRefs: [stripe]
  auth:
    oidc:
      executorMapping:
        version: shoal.executors/v1
        issuer: https://oidc.cluster.example.test/id/0123
        audience: shoal-executors
        service_assertion:
          claim: [kubernetes.io, namespace]
          equals: default
        executors:
          - subject: "system:serviceaccount:default:shoal-gw-stripe"
            executor_ref: stripe
MAPPING
explorer_mapping=(-f "$effects_fixtures/explorer-mapping.yaml")
# A loopback target: a sidecar, admitted with allowPrivate, needing no
# credential, and whose derived effects are {external} alone.
loopback_target=(
  --set 'effectsGateways[0].target.baseURL=http://127.0.0.1:9000,effectsGateways[0].target.allowPrivate=true,effectsGateways[0].target.credentialSecretName='
  --set 'effectsGateways[0].routes[0].effects={external}'
)

# A valid effects-ssh entry (#450, #662) and the gateway entry that calls it,
# on the same principle: every refusal case below breaks exactly one thing.
# The gateway targets the entry's Service, verifies it with the in-cluster CA,
# presents a client certificate and a bearer token; the entry pins the
# gateway's certificate and requires the token too. journal.id is set, so the
# Deployment renders; the init case clears it. The host key is a real
# ed25519 key, because the binary parses it.
cat > "$effects_fixtures/ssh.yaml" <<'SSH'
effectsGateways:
  - name: web-fleet
    enabled: true
    agentID: d2ViLWZsZWV0
    capability: effects.ssh
    surfaceName: web-fleet
    executorRef: web-fleet
    dispatch:
      url: http://127.0.0.1:1
      tokenAudience: shoal-executors
    target:
      baseURL: https://shoal-ssh-web.default.svc:8443
      allowPrivate: true
      credentialSecretName: effects-ssh-web-authorization
      caSecretName: effects-ssh-web-ca
      clientCertificate:
        secretName: shoal-gw-web-fleet-client
      idempotencyHeader: Idempotency-Key
      idempotencyRetention: 24h
    routes:
      - action: restart-web
        method: POST
        path: /v1/commands/restart-web
        effects: [external, egresses-content]
        idempotency: key
        conflict: {status: [409], pointer: /state, equals: [completed]}
        retryable: [503]
        reference: {pointer: /reference, pattern: "(exit-[0-9]{1,10}|signal-[A-Z0-9]{1,16})(-truncated)?"}
    networkPolicy:
      enabled: true
effectsSsh:
  - name: web
    enabled: true
    gateway: web-fleet
    tls: {secretName: effects-ssh-web-tls}
    callerToken: {secretName: effects-ssh-web-token}
    clientAuth:
      caSecretName: effects-ssh-web-client-ca
      identities: ["uri:spiffe://cluster.local/ns/default/sa/shoal-gw-web-fleet"]
    sshKeys: {secretName: effects-ssh-web-keys}
    journal: {id: 0123456789abcdef0123456789abcdef}
    commands:
      restart-web:
        address: 10.0.4.17:22
        hostKeys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIRh5PXk+0rm7CbKyH/ViV5hPII8CP+XNiZ58vWMgteW root@web-1"]
        user: deploy
        privateKeyKey: deploy
        command: sudo systemctl restart web
        runTimeout: 2m
SSH
ssh_base=(-f "$effects_fixtures/ssh.yaml")

renders() {
  local description="$1"; shift
  if ! helm template shoal "$chart" "$@" >/dev/null 2>&1; then
    fail "should render but was refused: $description"
    helm template shoal "$chart" "$@" 2>&1 | grep -oE 'execution error.*|Error: .*' | head -1 | sed 's/^/      /' || true
  fi
}

refuses() {
  local description="$1"; shift
  if helm template shoal "$chart" "$@" >/dev/null 2>&1; then
    fail "should be refused but rendered: $description"
  fi
}

# A refusal is only evidence of a guard when it is *that* guard's refusal.
#
# Several values below are refused by something downstream as well: an unknown
# token file source also trips the audience rule, a non-numeric token lifetime
# also casts to 0 and trips the floor, and an empty operator volume renders YAML
# Helm itself rejects. A case that only asserted "did not render" would keep
# passing with the guard it is named for deleted — which is the defect this
# script exists to prevent, arriving through the assertion instead of the
# template. These pin the sentence.
# A pattern that must appear in the rendered output. "renders" only proves a
# values file was not refused, which cannot see whether what the API server and
# the binary actually receive says what the operator asked for — and a list
# joined into one argument is exactly where a blank element disappears quietly
# or arrives as an empty name.
#
# Every match below reads a here-string, never `printf ... | grep -q`. grep -q
# exits at the first match, and once the input is larger than the pipe buffer
# the printf still writing gets SIGPIPE. Under pipefail the pipeline then
# exits 141, so a match reads as no match: a presence check fails spuriously,
# and an absence check passes when the pattern is there.
assert_renders() {
  local description="$1" pattern="$2"; shift 2
  local rendered
  if ! rendered=$(helm template shoal "$chart" "$@" 2>&1); then
    fail "should render but was refused: $description"
    return
  fi
  if ! grep -qE -- "$pattern" <<<"$rendered"; then
    fail "the rendered output does not match /$pattern/: $description"
  fi
}

# The mirror, for a flag that must not appear unless an operator asked for it.
# "renders" cannot see this and neither can assert_renders: a key that widened
# an effect ceiling by default would render, validate, install, and pass every
# positive assertion in this file. The absence is the property.
assert_absent() {
  local description="$1" pattern="$2"; shift 2
  local rendered
  if ! rendered=$(helm template shoal "$chart" "$@" 2>&1); then
    fail "should render but was refused: $description"
    return
  fi
  if grep -qE -- "$pattern" <<<"$rendered"; then
    fail "the rendered output matches /$pattern/ and must not: $description"
    { grep -E -- "$pattern" <<<"$rendered" | head -3 | sed 's/^/      /'; } || true
  fi
}

assert_absent_or_refused() {
  local description="$1" pattern="$2"; shift 2
  local rendered
  if ! rendered=$(helm template shoal "$chart" "$@" 2>&1); then
    return 0
  fi
  if grep -qE -- "$pattern" <<<"$rendered"; then
    fail "the rendered output matches /$pattern/ and must not: $description"
    { grep -E -- "$pattern" <<<"$rendered" | head -3 | sed 's/^/      /'; } || true
  fi
}

refuses_citing() {
  local expected="$1" description="$2"; shift 2
  local output
  if output="$(helm template shoal "$chart" "$@" 2>&1)"; then
    fail "should be refused but rendered: $description"
  elif ! grep -qF -- "$expected" <<<"$output"; then
    fail "refused, but not by the guard under test: $description"
    # Helm's own YAML errors carry no "execution error", and under pipefail a
    # grep that finds nothing would end the whole run here, silently skipping
    # every check after it. So any error line is shown, and none is not fatal.
    printf '%s' "$output" | grep -oE 'execution error.*|Error: .*' | head -1 |
      cut -c1-200 | sed 's/^/      /' || true
  fi
}

note "== tools =="
command -v helm >/dev/null 2>&1 || { printf 'FAIL  helm is required\n'; exit 1; }
if ! python3 -c 'import yaml' 2>/dev/null; then
  fail "python3 with PyYAML is required: the rendered-object checks parse Helm output"
  note "  install it with: python3 -m pip install pyyaml"
fi

note "== a match near the start of a large render is still a match =="
# The assertions' grep reads a here-string (see assert_renders). This pins the
# property on an input well past any pipe buffer: the form it replaced,
# printf | grep -q, exits 141 here under pipefail and reads the match as absent.
large_render="sentinel-near-the-start
$(head -c 3000000 /dev/zero | tr '\0' 'x' | fold -w 100)"
grep -qE -- '^sentinel-near-the-start$' <<<"$large_render" ||
  fail "a presence check misses a match near the start of a 3 MB render"
if ! grep -qF -- 'sentinel-near-the-start' <<<"$large_render"; then
  fail "an absence check reports a present match near the start of a 3 MB render as absent"
fi
unset large_render

note "== hermetic: no cluster is reachable =="
# The guard above, checked rather than trusted: with no lookup cluster asked
# for, Helm must see no context and the nonexistent kubeconfig. If either
# fails, every later dry run may have reached whatever the environment points
# at, so the run stops here.
if [ -z "${SHOAL_LOOKUP_KUBECONFIG:-}" ]; then
  # helm env reports the HELM_KUBE* settings but not the kubeconfig path, so
  # the path is checked in the environment Helm inherits.
  if [ -n "$(helm env HELM_KUBECONTEXT)" ] || [ -n "$(helm env HELM_KUBEAPISERVER)" ] ||
    [ -n "$(helm env HELM_KUBETOKEN)" ] || [ -n "$(helm env HELM_KUBEASGROUPS)" ] ||
    [ "${KUBECONFIG:-}" != "$hermetic_kubeconfig" ] || [ -e "$hermetic_kubeconfig" ]; then
    printf 'FAIL  the run is not hermetic: KUBECONFIG=%s, helm sees context %q and API server %q\n' \
      "${KUBECONFIG:-}" "$(helm env HELM_KUBECONTEXT)" "$(helm env HELM_KUBEAPISERVER)"
    exit 1
  fi
else
  # The lookup section creates a namespace and a Deployment, so it is held to
  # a throwaway local cluster: the kubeconfig's current context must be a kind
  # or k3d one. A production kubeconfig named here by mistake is refused
  # before anything touches it.
  lookup_context="$(kubectl --kubeconfig "$SHOAL_LOOKUP_KUBECONFIG" config current-context 2>/dev/null || true)"
  case "$lookup_context" in
    kind-* | k3d-*) note "  lookup cluster: $lookup_context" ;;
    *)
      printf 'FAIL  SHOAL_LOOKUP_KUBECONFIG must name a kind or k3d cluster (current context %q): the lookup section creates objects in it\n' "$lookup_context"
      exit 1
      ;;
  esac
fi

note "== lint =="
for values in values.yaml values-single.yaml values-distributed.yaml values-accumulo.yaml; do
  helm lint "$chart" -f "$chart/$values" >/dev/null || fail "helm lint $values"
done
# The explorer profile is linted with the overrides that make it valid.
# Linting it as shipped lints nothing: its required values are empty on
# purpose, so the templates refuse, and `helm lint` reports a template failure
# as INFO and still exits 0 — a check that cannot fail and would also break
# outright under a helm version that treats it as an error.
helm lint "$chart" "${explorer_base[@]}" >/dev/null || fail "helm lint values-explorer.yaml"
helm lint "$chart" "${llm_gateway_base[@]}" >/dev/null || fail "helm lint values-llm-gateway.yaml"

note "== every profile renders =="
for values in values.yaml values-single.yaml values-distributed.yaml values-accumulo.yaml; do
  renders "$values" -f "$chart/$values"
done
renders "values-explorer.yaml, filled in" "${explorer_base[@]}"
renders "storage tier plus explorer" -f "$chart/values.yaml" "${valid_explorer[@]}"
refuses "values-explorer.yaml as shipped" -f "$chart/values-explorer.yaml"
renders "values-llm-gateway.yaml, filled in" "${llm_gateway_base[@]}"
renders "storage tier plus llm gateway" -f "$chart/values.yaml" "${valid_llm_gateway[@]}"
renders "explorer plus llm gateway" "${explorer_base[@]}" "${valid_llm_gateway[@]}"
refuses "values-llm-gateway.yaml as shipped" -f "$chart/values-llm-gateway.yaml"

note "== the storage profiles are unchanged by the enforcement plane =="
# The standing guarantee for this chart: work on the explorer and the gateway
# changes nothing for an operator who has not enabled them. Both are off by
# default and independent of `mode`, so each storage profile must render
# byte-identically to the baseline — not merely "still render", which is what
# the section above checks and what a stray always-rendered key would pass.
#
# The baseline is origin/main. A clone without it reports a skip rather than
# quietly checking nothing, as the schema section does for kubeconform.
baseline="${SHOAL_CHART_BASELINE:-origin/main}"
# From the repository root, because an `archive` pathspec is resolved against
# the working directory and the chart is three levels down.
repository="$(git -C "$chart" rev-parse --show-toplevel 2>/dev/null || true)"
if [ -n "$repository" ] &&
  git -C "$repository" rev-parse --verify --quiet "$baseline" >/dev/null 2>&1; then
  reference="$(mktemp -d)"
  trap 'rm -rf "$reference" "$effects_fixtures"' EXIT
  if git -C "$repository" archive "$baseline" deploy/helm/shoal | tar -x -C "$reference"; then
    for values in values.yaml values-single.yaml values-distributed.yaml values-accumulo.yaml; do
      if ! diff -u \
        <(helm template shoal "$reference/deploy/helm/shoal" -f "$reference/deploy/helm/shoal/$values" 2>&1) \
        <(helm template shoal "$chart" -f "$chart/$values" 2>&1) > "$reference/diff"; then
        fail "$values no longer renders byte-identically to $baseline: the enforcement plane is off by default and must change nothing for anyone who has not enabled it"
        head -20 "$reference/diff" | sed 's/^/      /'
      fi
    done
    # The explorer and gateway profiles may change bytes — quoting a scalar
    # does — but never meaning: every object must parse to exactly what the
    # baseline's did. This is what lets a template outside the storage
    # profiles be quoted without a reviewer diffing manifests by eye. The
    # explorer case names a storage class and a chat credential, so the
    # optional fields it quotes are rendered too.
    reference_chart="$reference/deploy/helm/shoal"
    explorer_parsed=(--set explorer.storageClassName=fast --set explorer.chat.provider=openai-compatible,explorer.chat.model=m,explorer.chat.baseURL=https://api.example.test,explorer.chat.credentialSecretName=chat --set explorer.embedding.provider=voyage,explorer.embedding.model=v3,explorer.embedding.credentialSecretName=voyage)
    for profile in explorer llm-gateway; do
      if [ "$profile" = explorer ]; then
        overrides=("${valid_explorer[@]}" "${explorer_parsed[@]}")
      else
        overrides=("${valid_llm_gateway[@]}")
      fi
      # The gateway's listener serves TLS by default since #424, which is a
      # change of meaning on purpose. Its plaintext mode is the old behaviour,
      # so that is what is compared, and the one argument it adds, the
      # acknowledgement the binary now requires for a 0.0.0.0 bind, is the
      # only difference allowed. The TLS rendering is asserted on its own below.
      #
      # That holds only while the baseline predates #424. Once the baseline has
      # the TLS listener too (it has since #583 merged, so origin/main compares
      # against itself), forcing plaintext on one side only compares TLS against
      # plaintext and always fails. Then the two sides get the same values.
      current=("${overrides[@]}")
      if [ "$profile" = llm-gateway ] &&
        ! grep -qF -- '-allow-plaintext-listener' "$reference_chart/templates/llm-gateway-deployment.yaml"; then
        current+=(--set llmGateway.tls.enabled=false,llmGateway.tls.secretName=)
      fi
      # Both must render, or two identical error messages would compare equal.
      if ! helm template shoal "$reference_chart" -f "$reference_chart/values-$profile.yaml" "${overrides[@]}" > "$reference/before" 2>&1 ||
        ! helm template shoal "$chart" -f "$chart/values-$profile.yaml" "${current[@]}" > "$reference/after" 2>&1; then
        fail "values-$profile.yaml, filled in, does not render on both $baseline and this tree, so it cannot be compared"
      elif ! python3 - "$reference/before" "$reference/after" <<'PARSED'
import sys, yaml
before, after = (list(yaml.safe_load_all(open(path))) for path in sys.argv[1:])
acknowledgement = "-allow-plaintext-listener=true"
for document in after:
    if (document or {}).get("kind") != "Deployment":
        continue
    for container in document["spec"]["template"]["spec"]["containers"]:
        arguments = container.get("args") or []
        if acknowledgement in arguments and not any(
                acknowledgement in (c.get("args") or [])
                for d in before if (d or {}).get("kind") == "Deployment"
                for c in d["spec"]["template"]["spec"]["containers"]):
            arguments.remove(acknowledgement)


def gateway_pod_metadata(documents):
    """The llm gateway Deployment's pod template metadata, or None."""
    for document in documents:
        if (document or {}).get("kind") == "Deployment" and document["metadata"].get(
            "labels", {}
        ).get("app.kubernetes.io/component") == "llm-gateway":
            return document["spec"]["template"]["metadata"]
    return None


# The one meaning change this comparison is told about: #425 added scrape
# annotations naming /metrics on the gateway's health port. They are set aside
# only while the baseline has none, so once that change is on the baseline the
# comparison is exact again and this branch does nothing.
old, new = gateway_pod_metadata(before), gateway_pod_metadata(after)
if old is not None and new is not None and "annotations" not in old:
    annotations = new.get("annotations") or {}
    for key in [k for k in annotations if k.startswith("prometheus.io/")]:
        del annotations[key]
    if annotations == {}:
        new.pop("annotations", None)
raise SystemExit(0 if before == after else 1)
PARSED
      then
        fail "values-$profile.yaml, filled in, no longer parses to the same objects as $baseline: quoting may change bytes, never meaning"
      fi
    done
  else
    fail "could not export the chart at $baseline to compare against"
  fi
else
  note "  skipped: $baseline is not in this clone (set SHOAL_CHART_BASELINE)"
fi

note "== schema =="
if command -v kubeconform >/dev/null 2>&1; then
  for values in values.yaml values-single.yaml values-distributed.yaml values-accumulo.yaml; do
    helm template shoal "$chart" -f "$chart/$values" |
      kubeconform -strict -summary - >/dev/null || fail "kubeconform $values"
  done
  helm template shoal "$chart" "${explorer_base[@]}" |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform values-explorer.yaml"
  helm template shoal "$chart" -f "$chart/values.yaml" "${valid_explorer[@]}" |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform storage tier plus explorer"
  helm template shoal "$chart" "${llm_gateway_base[@]}" |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform values-llm-gateway.yaml"
  helm template shoal "$chart" "${explorer_base[@]}" "${valid_llm_gateway[@]}" |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform explorer plus llm gateway"
  # The file form renders a volume, a mount and an fsGroup the env form does
  # not, so it is a different object shape and needs its own schema pass.
  helm template shoal "$chart" "${token_file_base[@]}" |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform llm gateway with a projected token"
  # Every object effects-ssh can render, in both halves of an install.
  helm template shoal "$chart" "${ssh_base[@]}" |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform effects-ssh serving"
  helm template shoal "$chart" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id=' |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform effects-ssh journal init"
  # Every object the effects gateway can render: the claim, the NetworkPolicy,
  # the Secret token source and the attestation volume.
  helm template shoal "$chart" "${effects_gateway_base[@]}" \
    --set 'effectsGateways[0].networkPolicy.enabled=true' --set 'effectsGateways[0].networkPolicy.targetCIDRs={203.0.113.0/24}' \
    --set 'effectsGateways[0].attestation.secretName=stripe-attestation' |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform effects gateway"
  helm template shoal "$chart" "${effects_gateway_base[@]}" \
    --set 'effectsGateways[0].dispatch.tokenSource=secret,effectsGateways[0].dispatch.tokenAudience=,effectsGateways[0].dispatch.credentialSecretName=stripe-dispatch' \
    --set 'effectsGateways[0].unrecorded.storage=existingClaim,effectsGateways[0].unrecorded.existingClaim=stripe-log' |
    kubeconform -strict -summary - >/dev/null || fail "kubeconform effects gateway with a Secret token and an existing claim"
else
  note "  skipped: kubeconform is not on PATH"
fi

note "== guards refuse =="
refuses "authenticator unset"           "${explorer_base[@]}" --set explorer.auth.mode=
refuses "dev-auth requested"            "${explorer_base[@]}" --set explorer.auth.mode=dev-unsafe
refuses "no OIDC issuer"                "${explorer_base[@]}" --set explorer.auth.oidc.issuer=
refuses "no OIDC audience"              "${explorer_base[@]}" --set explorer.auth.oidc.audiences=null
refuses "no authorization claim"        "${explorer_base[@]}" --set explorer.auth.oidc.authorizationClaim=
refuses "claim mapped to no values"     "${explorer_base[@]}" --set explorer.auth.oidc.readerValues=null,explorer.auth.oidc.contributorValues=null,explorer.auth.oidc.fleetValues=null
refuses "no allowed hosts"              "${explorer_base[@]}" --set explorer.allowedHosts=null
refuses "more than one replica"         "${explorer_base[@]}" --set explorer.replicas=2
refuses "mosaic budget with no window"  "${explorer_base[@]}" --set explorer.disclosure.mosaic.maxDomains=3,explorer.disclosure.mosaic.window=
refuses "remote chat with no credential" "${explorer_base[@]}" --set explorer.chat.provider=openai-compatible,explorer.chat.model=m,explorer.chat.baseURL=https://api.example.test
refuses "chat provider with no model"   "${explorer_base[@]}" --set explorer.chat.provider=ollama,explorer.chat.baseURL=http://localhost:11434
refuses "voyage with no credential"     "${explorer_base[@]}" --set explorer.embedding.provider=voyage,explorer.embedding.model=v3
refuses "ask executor not allowlisted"  "${explorer_base[@]}" --set explorer.fleet.askExecutorRef=ask,explorer.chat.provider=ollama,explorer.chat.model=m,explorer.chat.baseURL=http://localhost:11434
refuses "ask executor with no chat"     "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={ask}',explorer.fleet.askExecutorRef=ask
refuses "health port collides"          "${explorer_base[@]}" --set explorer.healthPort=8098
# Present-but-blank is not configured. The workspace drops empty entries and
# then reports the setting missing, so these render a pod that exits at startup.
refuses "blank issuer"                  "${explorer_base[@]}" --set explorer.auth.oidc.issuer=" "
refuses "blank audience only"           "${explorer_base[@]}" --set 'explorer.auth.oidc.audiences={ }'
refuses "blank authorization claim"     "${explorer_base[@]}" --set explorer.auth.oidc.authorizationClaim=" "
refuses "blank role value only"         "${explorer_base[@]}" --set 'explorer.auth.oidc.readerValues={ }'
refuses "blank allowed host only"       "${explorer_base[@]}" --set 'explorer.allowedHosts={ }'
refuses "placeholder issuer"            "${explorer_base[@]}" --set explorer.auth.oidc.issuer=https://REPLACE_ME/
refuses "placeholder allowed host"      "${explorer_base[@]}" --set 'explorer.allowedHosts={REPLACE_ME.example.test}'
# A placeholder in a role mapping is worse than a non-working deployment: it is
# a working one that authorizes the literal claim "REPLACE_ME", denying every
# real caller while the chart reports success.
refuses "placeholder reader value"      "${explorer_base[@]}" --set 'explorer.auth.oidc.readerValues={REPLACE_ME}'
refuses "placeholder contributor value" "${explorer_base[@]}" --set 'explorer.auth.oidc.contributorValues={REPLACE_ME}'
refuses "placeholder fleet value"       "${explorer_base[@]}" --set 'explorer.auth.oidc.fleetValues={REPLACE_ME}'

note "== valid configurations still render =="
renders "loopback chat needs no credential" "${explorer_base[@]}" --set explorer.chat.provider=ollama,explorer.chat.model=llama3,explorer.chat.baseURL=http://localhost:11434
renders "remote chat with a credential"     "${explorer_base[@]}" --set explorer.chat.provider=openai-compatible,explorer.chat.model=m,explorer.chat.baseURL=https://api.example.test,explorer.chat.credentialSecretName=chat
renders "mosaic budget enabled"             "${explorer_base[@]}" --set explorer.disclosure.mosaic.maxDomains=3
renders "withholding concealed"             "${explorer_base[@]}" --set explorer.disclosure.concealWithholding=true
assert_renders "declassification reason required" '^ +- -require-declassification-reason$' "${explorer_base[@]}" --set explorer.declassification.requireReason=true
assert_absent "declassification reason optional by default" 'require-declassification-reason' "${explorer_base[@]}"
renders "ask executor wired"                "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={ask}',explorer.fleet.askExecutorRef=ask,explorer.chat.provider=ollama,explorer.chat.model=m,explorer.chat.baseURL=http://localhost:11434
renders "lexical embedding"                 "${explorer_base[@]}" --set explorer.embedding.provider=lexical,explorer.embedding.dimensions=256
renders "scaled to zero"                    "${explorer_base[@]}" --set explorer.replicas=0
assert_renders "every allowed host is trimmed" '^ +- "-allowed-host=shoal\.example\.test,other\.example\.test"$' "${explorer_base[@]}" --set 'explorer.allowedHosts={ shoal.example.test , other.example.test }'

note "== explorer host, bind port, and provider URL guards (#423) =="
refuses_citing "explorer.allowedHosts contains a blank-after-trim element" "a blank host beside a valid host" "${explorer_base[@]}" --set 'explorer.allowedHosts={shoal.example.test, }'
refuses_citing "explorer.allowedHosts contains a blank-after-trim element" "an empty host beside a valid host" "${explorer_base[@]}" --set 'explorer.allowedHosts={,shoal.example.test}'
for field in containerPort healthPort; do
  for port in 80 443 1023; do
    refuses_citing "explorer.$field must be 1024 or above" "explorer $field=$port" "${explorer_base[@]}" --set "explorer.$field=$port"
  done
  renders "explorer $field at the unprivileged boundary" "${explorer_base[@]}" --set "explorer.$field=1024"
done
renders "a privileged explorer Service port is not a bind port" "${explorer_base[@]}" --set explorer.servicePort=80
renders "disabled explorer bind ports are ignored" --set explorer.containerPort=80,explorer.healthPort=443
# A comma inside one entry splits it in the binary, and a blank piece is then
# dropped there silently: the shortened list the blank check refuses, reached
# around it. Whitespace inside an authority matches no Host header.
# (--set-json is applied before --set, so it could not override the base's
# list; an escaped comma keeps the separator inside one --set entry.)
refuses_citing "which holds a comma" "a trailing comma inside one entry" "${explorer_base[@]}" --set 'explorer.allowedHosts={shoal.example.test\, }'
refuses_citing "which holds a comma" "two authorities written as one entry" "${explorer_base[@]}" --set 'explorer.allowedHosts={shoal.example.test\,other.example.test}'
refuses_citing "which holds whitespace inside the authority" "a space inside an authority" "${explorer_base[@]}" --set 'explorer.allowedHosts={shoal .example.test}'
refuses_citing "explorer.allowedHosts contains a blank-after-trim element" "a null entry beside a valid host" "${explorer_base[@]}" --set 'explorer.allowedHosts[1]=null'
# Entries are authorities as normalizeAuthority reads them. A URL splits at its
# last colon into host "https" and matches nothing; an unbracketed IPv6 address
# is refused by the workspace at startup.
refuses_citing "which holds a \"/\"" "a URL instead of an authority" "${explorer_base[@]}" --set-string 'explorer.allowedHosts[0]=https://shoal.example.test'
refuses_citing "which holds a \"/\"" "an authority with a path" "${explorer_base[@]}" --set-string 'explorer.allowedHosts[0]=shoal.example.test/'
for host in '::1' 'fe80::1' 'a:b:c' '::1]'; do
  refuses_citing "an unbracketed host with more than one colon" "unbracketed $host" "${explorer_base[@]}" --set-string "explorer.allowedHosts[0]=$host"
done
for host in ':8443' '.'; do
  refuses_citing "which has no host" "an empty host in $host" "${explorer_base[@]}" --set-string "explorer.allowedHosts[0]=$host"
done
for host in '[foo]' '[foo]:80' '[1:2:3:4:5:6:7:8:9]' '[:::]'; do
  refuses_citing "whose bracketed host is not an IP address" "brackets around $host" "${explorer_base[@]}" --set-string "explorer.allowedHosts[0]=$host"
done
for host in '[::1' '[]:80' 'shoal.example.test:https'; do
  refuses_citing "which is not an authority the workspace accepts" "malformed $host" "${explorer_base[@]}" --set-string "explorer.allowedHosts[0]=$host"
done
for host in '[::1]' '[::1]:8443' '[0:0:0:0:0:0:0:1]' '[fe80::1]:80' '[::]' '[127.0.0.1]' '127.0.0.1:80' 'SHOAL.example.test.' 'localhost'; do
  escaped=$(printf '%s' "$host" | sed -e 's/[].[]/\\&/g')
  assert_renders "authority $host renders as written" "^ +- \"-allowed-host=${escaped}\"\$" "${explorer_base[@]}" --set-string "explorer.allowedHosts[0]=$host"
done
assert_renders "an authority with a port is kept as written" '^ +- "-allowed-host=shoal\.example\.test:8443"$' "${explorer_base[@]}" --set 'explorer.allowedHosts={shoal.example.test:8443}'

# The read fleet binds readFleet.listen and readFleet.metricsPort; its
# servicePort only names the container port.
read_base=(-f "$chart/values-distributed.yaml")
for port in 80 443 1023; do
  refuses_citing "readFleet.metricsPort must be 1024 or above" "readFleet metricsPort=$port" "${read_base[@]}" --set "readFleet.metricsPort=$port"
  refuses_citing "readFleet.listen must bind a port of 1024 or above" "readFleet listen :$port" "${read_base[@]}" --set "readFleet.listen=:$port"
done
refuses_citing "readFleet.listen must bind a port of 1024 or above" "readFleet listen on an address" "${read_base[@]}" --set "readFleet.listen=0.0.0.0:443"
renders "readFleet metricsPort at the unprivileged boundary" "${read_base[@]}" --set readFleet.metricsPort=1024
renders "readFleet listen at the unprivileged boundary" "${read_base[@]}" --set readFleet.listen=:1024,readFleet.servicePort=1024
renders "disabled readFleet bind ports are ignored" "${read_base[@]}" --set readFleet.enabled=false,readFleet.metricsPort=80,readFleet.listen=:80

for role in writeTier tserver compactor; do
  fields=(thriftPort metricsPort)
  role_base=(-f "$chart/values-accumulo.yaml")
  if [ "$role" = writeTier ]; then
    fields=(grpcPort metricsPort)
    role_base=(--set mode=single)
  fi
  for field in "${fields[@]}"; do
    for port in 80 443 1023; do
      refuses_citing "$role.$field must be 1024 or above" "$role $field=$port" "${role_base[@]}" --set "$role.$field=$port"
    done
    renders "$role $field at the unprivileged boundary" "${role_base[@]}" --set "$role.$field=1024"
    renders "disabled $role $field is ignored" "${role_base[@]}" --set "$role.enabled=false,$role.$field=80"
  done
done

chat_base=("${explorer_base[@]}" --set explorer.chat.provider=ollama,explorer.chat.model=m)
embedding_base=("${explorer_base[@]}" --set explorer.embedding.provider=ollama,explorer.embedding.model=m,explorer.embedding.dimensions=256)
for url in http://LOCALHOST:11434 HTTP://LOCALHOST:11434 http://127.0.0.2:11434 http://127.255.255.255:11434 'http://[::1]:11434' 'http://[0:0:0:0:0:0:0:1]:11434'; do
  renders "loopback chat $url needs no credential" "${chat_base[@]}" --set "explorer.chat.baseURL=$url"
  renders "loopback embedding $url renders" "${embedding_base[@]}" --set "explorer.embedding.baseURL=$url"
done
for url in https://localhost.example https://127.example.test; do
  refuses_citing "explorer.chat.credentialSecretName is required for a remote chat base URL" "a loopback-looking remote chat host $url" "${chat_base[@]}" --set "explorer.chat.baseURL=$url"
  renders "remote chat $url with a credential" "${chat_base[@]}" --set "explorer.chat.baseURL=$url,explorer.chat.credentialSecretName=chat"
done
for url in http://localhost.example http://127.example.test http://127.0.0.256 http://127.1; do
  refuses_citing "explorer.chat.baseURL must use HTTPS unless the host is loopback" "remote plaintext chat $url" "${chat_base[@]}" --set "explorer.chat.baseURL=$url,explorer.chat.credentialSecretName=chat"
  refuses_citing "explorer.embedding.baseURL must use HTTPS unless the host is loopback" "remote plaintext embedding $url" "${embedding_base[@]}" --set "explorer.embedding.baseURL=$url"
done
for url in '' https:// http:// /relative ftp://localhost; do
  refuses_citing "explorer.chat.baseURL must be an absolute http(s) URL with a host" "invalid chat URL $url" "${chat_base[@]}" --set "explorer.chat.baseURL=$url,explorer.chat.credentialSecretName=chat"
  for provider in ollama openai voyage; do
    # Only Voyage has a built-in URL default.
    if [ -z "$url" ] && [ "$provider" = voyage ]; then continue; fi
    refuses_citing "explorer.embedding.baseURL must be an absolute http(s) URL with a host" "invalid $provider embedding URL $url" "${embedding_base[@]}" --set "explorer.embedding.provider=$provider,explorer.embedding.baseURL=$url,explorer.embedding.credentialSecretName=embedding"
  done
done
for provider in ollama openai voyage; do
  renders "remote HTTPS $provider embedding with a credential" "${embedding_base[@]}" --set "explorer.embedding.provider=$provider,explorer.embedding.baseURL=https://models.example.test,explorer.embedding.credentialSecretName=embedding"
done
# What pkg/model refuses beyond the scheme and host: userinfo, a query, a
# fragment, and any path but the root, for every provider the explorer offers.
for url in 'https://user:pass@models.example.test' 'https://models.example.test/?x=1' 'https://models.example.test?' 'https://models.example.test/#top' 'https://models.example.test#' 'https://models.example.test/v1' 'http://localhost:11434/api'; do
  case "$url" in
    *@*) expected="must not carry userinfo" ;;
    *\?*|*\#*) expected="must not carry a query or fragment" ;;
    *) expected="must be the provider's root, with no path" ;;
  esac
  for provider in ollama openai; do
    refuses_citing "explorer.chat.baseURL $expected" "a $provider chat URL $url" "${chat_base[@]}" --set "explorer.chat.provider=$provider,explorer.chat.baseURL=$url,explorer.chat.credentialSecretName=chat"
  done
  for provider in ollama openai voyage; do
    refuses_citing "explorer.embedding.baseURL $expected" "a $provider embedding URL $url" "${embedding_base[@]}" --set "explorer.embedding.provider=$provider,explorer.embedding.baseURL=$url,explorer.embedding.credentialSecretName=embedding"
  done
done
renders "a provider root with a trailing slash" "${chat_base[@]}" --set explorer.chat.provider=openai,explorer.chat.baseURL=https://models.example.test/,explorer.chat.credentialSecretName=chat
renders "a provider URL with surrounding space, which the workspace trims" "${embedding_base[@]}" --set 'explorer.embedding.baseURL= http://localhost:11434 '
renders "Voyage retains its built-in URL default" "${embedding_base[@]}" --set explorer.embedding.provider=voyage,explorer.embedding.baseURL=,explorer.embedding.credentialSecretName=embedding
renders "fake embedding ignores provider URLs" "${embedding_base[@]}" --set explorer.embedding.provider=fake,explorer.embedding.baseURL=https://
renders "lexical embedding ignores provider URLs" "${embedding_base[@]}" --set explorer.embedding.provider=lexical,explorer.embedding.baseURL=https://
renders "disabled providers ignore their URLs" "${explorer_base[@]}" --set explorer.chat.baseURL=https://,explorer.embedding.baseURL=https://

note "== the approver mapping and the stable identity claim (#451, #526) =="
# The mapping is a document the workspace reads from disk, so it is the one
# setting the chart renders into a ConfigMap; the identity claim is an argument
# like every other. Neither appears unless asked for, the mapping must restate
# the claim exactly when it is set (the workspace refuses to start otherwise),
# and the claim is a list of segments, never a dotted string.
approvers_file="$(mktemp)"
cat > "$approvers_file" <<'APPROVERS'
explorer:
  auth:
    oidc:
      identityClaim: [oid]
      approverMapping:
        version: shoal.approvers/v1
        issuer: https://issuer.example.test/
        audience: https://shoal.example.test/approvals
        client_ids: [shoal-console]
        claim: [roles]
        values: [shoal-approvers]
        max_values: 64
        human_assertion:
          claim: [principal_type]
          equals: human
        identity_claim: [oid]
APPROVERS
approvers_file_both="$(mktemp)"
cp "$approvers_file" "$approvers_file_both"
assert_absent "no approver mapping unless configured" 'oidc-approver-mapping-file|approver-mapping|approvers.json' "${explorer_base[@]}"
assert_absent "no identity claim unless configured" 'oidc-identity-claim' "${explorer_base[@]}"
# Migrating the scheme is one-shot: it names the recorded scheme it replaces,
# never rendered by default, and held to the digest's shape so a boolean (the
# old, advisory form) cannot be passed.
scheme_digest=5a35ffaec1b6aafc2af35ed289def9bae8728339a3984691e2ad9818a1896a79
assert_absent "no scheme migration unless asked for" 'oidc-identity-scheme-migrate' "${explorer_base[@]}"
assert_renders "a scheme migration names the scheme it replaces" '^ +- "-oidc-identity-scheme-migrate='"$scheme_digest"'"$' "${explorer_base[@]}" --set-string explorer.auth.oidc.identitySchemeMigrateFrom="$scheme_digest"
refuses_citing "must be the 64 lowercase hex digits" "a boolean scheme migration" "${explorer_base[@]}" --set-string explorer.auth.oidc.identitySchemeMigrateFrom=true
refuses_citing "must be the 64 lowercase hex digits" "an upper-case digest" "${explorer_base[@]}" --set-string explorer.auth.oidc.identitySchemeMigrateFrom=5A35FFAEC1B6AAFC2AF35ED289DEF9BAE8728339A3984691E2AD9818A1896A79
assert_renders "the mapping file is passed" '^ +- "-oidc-approver-mapping-file=/etc/shoal/approvers/approvers.json"$' "${explorer_base[@]}" -f "$approvers_file"
assert_renders "the identity claim is passed as one JSON argument" '^ +- "-oidc-identity-claim=\[\\"oid\\"\]"$' "${explorer_base[@]}" -f "$approvers_file"
assert_renders "the mapping is mounted read-only" '^ +readOnly: true$' "${explorer_base[@]}" -f "$approvers_file"
assert_renders "the mapping is rendered as JSON" '^  approvers.json: ".*\\"identity_claim\\":\[\\"oid\\"\].*"$' "${explorer_base[@]}" -f "$approvers_file"
assert_renders "a mapping change rolls the pod" '^ +checksum/approver-mapping: "[0-9a-f]{64}"$' "${explorer_base[@]}" -f "$approvers_file"
renders "a mapping without the stable claim" "${explorer_base[@]}" -f "$approvers_file" --set explorer.auth.oidc.identityClaim=null --set explorer.auth.oidc.approverMapping.identity_claim=null
renders "the stable claim without a mapping" "${explorer_base[@]}" --set 'explorer.auth.oidc.identityClaim={oid}'
refuses_citing "must restate explorer.auth.oidc.identityClaim" "the claim without the mapping restating it" "${explorer_base[@]}" -f "$approvers_file" --set explorer.auth.oidc.approverMapping.identity_claim=null
refuses_citing "must restate explorer.auth.oidc.identityClaim" "the mapping restating a claim that is not set" "${explorer_base[@]}" -f "$approvers_file" --set explorer.auth.oidc.identityClaim=null
refuses_citing "must restate explorer.auth.oidc.identityClaim" "a mapping restating another claim" "${explorer_base[@]}" -f "$approvers_file" --set 'explorer.auth.oidc.approverMapping.identity_claim={sub_id}'
refuses_citing "must restate explorer.auth.oidc.identityClaim" "a dotted restatement of a nested claim" "${explorer_base[@]}" -f "$approvers_file" --set 'explorer.auth.oidc.identityClaim={ext,oid}' --set 'explorer.auth.oidc.approverMapping.identity_claim={ext.oid}'
refuses_citing "must be a list of path segments" "a dotted string as the claim" "${explorer_base[@]}" --set explorer.auth.oidc.identityClaim=ext.oid
refuses_citing "blank or non-string segment" "a blank claim segment" "${explorer_base[@]}" --set 'explorer.auth.oidc.identityClaim={ }'
refuses_citing "must be the shoal.approvers/v1 document as a map" "a mapping that is not a document" "${explorer_base[@]}" --set explorer.auth.oidc.approverMapping=approvers.json
# The control-character guard: both reach the pod inside a quoted scalar, and
# the line-break walk refuses the character so a quoted remainder cannot name
# a different claim.
refuses_citing 'explorer.auth.oidc.identityClaim[0] holds "\n"' "a newline in a claim segment" "${explorer_base[@]}" --set-string 'explorer.auth.oidc.identityClaim[0]=oid
            - -conceal-withholding=false'
refuses_citing 'explorer.auth.oidc.identityClaim[0] holds "\u2028"' "U+2028 in a claim segment" "${explorer_base[@]}" --set-string "explorer.auth.oidc.identityClaim[0]=oid"$' '"x"
refuses_citing 'explorer.auth.oidc.approverMapping.values[0] holds "\n"' "a newline in a mapped value" "${explorer_base[@]}" -f "$approvers_file" --set-string 'explorer.auth.oidc.approverMapping.values[0]=shoal-approvers
x'
rm -f "$approvers_file"

note "== the label grants (#570) =="
# Like the approver mapping, the grant file is a document the workspace reads
# from disk once at startup, so it is a chart-owned ConfigMap with a checksum
# that rolls the pod. It never appears unless configured, and the shapes the
# workspace would refuse at startup are refused at render: the version, the
# issuer, a source other than the workspace's one, a label outside the label
# charset, and a placeholder claim value.
grants_file="$(mktemp)"
cat > "$grants_file" <<'GRANTS'
explorer:
  auth:
    oidc:
      labelGrants:
        version: shoal.label-grants/v1
        issuer: https://issuer.example.test/
        claim: [groups]
        max_values: 64
        grants:
          secret-readers:
            - source: shoal-explore-web/workspace
              label: secret
            - source: shoal-explore-web/workspace
              label: "pii:eu"
GRANTS
assert_absent "no label grants unless configured" 'oidc-label-grants-file|label-grants' "${explorer_base[@]}"
assert_renders "the grant file is passed" '^ +- "-oidc-label-grants-file=/etc/shoal/label-grants/label-grants.json"$' "${explorer_base[@]}" -f "$grants_file"
assert_renders "the grant file is mounted read-only" '^ +mountPath: "/etc/shoal/label-grants"$' "${explorer_base[@]}" -f "$grants_file"
assert_renders "the grant file is rendered as JSON" '^  label-grants.json: ".*\\"label\\":\\"pii:eu\\".*"$' "${explorer_base[@]}" -f "$grants_file"
assert_renders "a grant change rolls the pod" '^ +checksum/label-grants: "[0-9a-f]{64}"$' "${explorer_base[@]}" -f "$grants_file"
assert_renders "the ConfigMap is chart-owned" '^  name: "[a-z0-9-]+-label-grants"$' "${explorer_base[@]}" -f "$grants_file"
renders "label grants beside an approver mapping" "${explorer_base[@]}" -f "$grants_file" -f "$approvers_file_both"
refuses_citing "must be the shoal.label-grants/v1 document as a map" "grants that are not a document" "${explorer_base[@]}" --set explorer.auth.oidc.labelGrants=label-grants.json
refuses_citing "labelGrants.version must be shoal.label-grants/v1" "another version" "${explorer_base[@]}" -f "$grants_file" --set explorer.auth.oidc.labelGrants.version=shoal.label-grants/v2
refuses_citing "must equal explorer.auth.oidc.issuer" "another issuer" "${explorer_base[@]}" -f "$grants_file" --set explorer.auth.oidc.labelGrants.issuer=https://issuer.example.test
refuses_citing "must map at least one claim value" "no grants" "${explorer_base[@]}" -f "$grants_file" --set explorer.auth.oidc.labelGrants.grants=null
refuses_citing "configures one source" "a grant on another source" "${explorer_base[@]}" -f "$grants_file" --set 'explorer.auth.oidc.labelGrants.grants.secret-readers[0].source=other/source'
refuses_citing "is not a visibility label" "a label with a slash" "${explorer_base[@]}" -f "$grants_file" --set 'explorer.auth.oidc.labelGrants.grants.secret-readers[0].label=a/b'
refuses_citing "is not a visibility label" "an empty label" "${explorer_base[@]}" -f "$grants_file" --set 'explorer.auth.oidc.labelGrants.grants.secret-readers[0].label='
refuses_citing "placeholder" "a placeholder claim value" "${explorer_base[@]}" -f "$grants_file" --set 'explorer.auth.oidc.labelGrants.grants.REPLACE_ME[0].source=shoal-explore-web/workspace' --set 'explorer.auth.oidc.labelGrants.grants.REPLACE_ME[0].label=secret'
refuses_citing "is not a visibility label" "a newline in a label" "${explorer_base[@]}" -f "$grants_file" --set-string 'explorer.auth.oidc.labelGrants.grants.secret-readers[0].label=secret
x'
refuses_citing 'explorer.auth.oidc.labelGrants.claim[0] holds "\n"' "a newline in the claim path" "${explorer_base[@]}" -f "$grants_file" --set-string 'explorer.auth.oidc.labelGrants.claim[0]=groups
x'
rm -f "$grants_file" "$approvers_file_both"

note "== the executor mapping (#391) =="
# The only source of the execute operation. Like the approver mapping it is a
# document the workspace reads from disk once at startup, so it is a
# chart-owned ConfigMap with a checksum that rolls the pod, and it never
# appears unless configured. The shapes the workspace would refuse at startup
# are refused at render, and so is a reference no descriptor could register
# against.
executors_file="$(mktemp)"
cat > "$executors_file" <<'EXECUTORS'
explorer:
  fleet:
    executorRefs: [stripe, ledger]
  auth:
    oidc:
      executorMapping:
        version: shoal.executors/v1
        issuer: https://oidc.cluster.example.test/id/0123
        audience: shoal-executors
        service_assertion:
          claim: [kubernetes.io, namespace]
          equals: shoal-gateways
        executors:
          - subject: "system:serviceaccount:shoal-gateways:stripe"
            executor_ref: stripe
          - subject: "system:serviceaccount:shoal-gateways:ledger"
            executor_ref: ledger
EXECUTORS
assert_absent "no executor mapping unless configured" 'oidc-executor-mapping-file|executor-mapping|executors.json' "${explorer_base[@]}"
assert_renders "the executor mapping is passed" '^ +- "-oidc-executor-mapping-file=/etc/shoal/executors/executors.json"$' "${explorer_base[@]}" -f "$executors_file"
assert_renders "the executor mapping is mounted read-only" '^ +mountPath: "/etc/shoal/executors"$' "${explorer_base[@]}" -f "$executors_file"
assert_renders "the executor mapping is rendered as JSON" '^  executors.json: ".*\\"executor_ref\\":\\"stripe\\".*"$' "${explorer_base[@]}" -f "$executors_file"
assert_renders "an executor mapping change rolls the pod" '^ +checksum/executor-mapping: "[0-9a-f]{64}"$' "${explorer_base[@]}" -f "$executors_file"
assert_renders "the executor ConfigMap is chart-owned" '^  name: "[a-z0-9-]+-executors"$' "${explorer_base[@]}" -f "$executors_file"
refuses_citing "must be the shoal.executors/v1 document as a map" "a mapping that is not a document" "${explorer_base[@]}" --set explorer.auth.oidc.executorMapping=executors.json
refuses_citing "executorMapping.version must be shoal.executors/v1" "another version" "${explorer_base[@]}" -f "$executors_file" --set explorer.auth.oidc.executorMapping.version=shoal.executors/v2
refuses_citing "must be a canonical https URL" "an http issuer" "${explorer_base[@]}" -f "$executors_file" --set explorer.auth.oidc.executorMapping.issuer=http://oidc.cluster.example.test
refuses_citing "must be a canonical https URL" "an issuer with an empty fragment" "${explorer_base[@]}" -f "$executors_file" --set 'explorer.auth.oidc.executorMapping.issuer=https://x/#'
refuses_citing "must be a canonical https URL" "an issuer with a query" "${explorer_base[@]}" -f "$executors_file" --set 'explorer.auth.oidc.executorMapping.issuer=https://x/?a=b'
refuses_citing "must be a canonical https URL" "an upper-case issuer host" "${explorer_base[@]}" -f "$executors_file" --set 'explorer.auth.oidc.executorMapping.issuer=https://Cluster.example.test'
refuses_citing "audience is required" "no audience" "${explorer_base[@]}" -f "$executors_file" --set explorer.auth.oidc.executorMapping.audience=
refuses_citing "must differ from explorer.auth.oidc.audiences" "the workspace audience" "${explorer_base[@]}" -f "$executors_file" --set explorer.auth.oidc.executorMapping.audience=shoal
refuses_citing "service_assertion is required" "no service assertion" "${explorer_base[@]}" -f "$executors_file" --set explorer.auth.oidc.executorMapping.service_assertion=null
refuses_citing "service_assertion.equals is required" "an assertion with no value" "${explorer_base[@]}" -f "$executors_file" --set explorer.auth.oidc.executorMapping.service_assertion.equals=null
refuses_citing "service_assertion.claim must be a non-empty list" "a dotted assertion claim" "${explorer_base[@]}" -f "$executors_file" --set explorer.auth.oidc.executorMapping.service_assertion.claim=kubernetes.io.namespace
refuses_citing "must list at least one" "no executors" "${explorer_base[@]}" -f "$executors_file" --set explorer.auth.oidc.executorMapping.executors=null
refuses_citing "executors[1].subject is mapped twice" "one subject, two references" "${explorer_base[@]}" -f "$executors_file" --set 'explorer.auth.oidc.executorMapping.executors[1].subject=system:serviceaccount:shoal-gateways:stripe'
refuses_citing "executors[1].executor_ref is mapped twice" "two subjects, one reference" "${explorer_base[@]}" -f "$executors_file" --set 'explorer.auth.oidc.executorMapping.executors[1].executor_ref=stripe'
refuses_citing "executors[0].executor_ref is outside the executor-reference charset" "a reference with a space" "${explorer_base[@]}" -f "$executors_file" --set-string 'explorer.auth.oidc.executorMapping.executors[0].executor_ref=str ipe'
refuses_citing "executors[0].executor_ref is not in explorer.fleet.executorRefs" "a reference the workspace does not configure" "${explorer_base[@]}" -f "$executors_file" --set 'explorer.fleet.executorRefs={ledger}'
refuses_citing "executors[0].subject must be a non-empty string" "a blank subject" "${explorer_base[@]}" -f "$executors_file" --set-string 'explorer.auth.oidc.executorMapping.executors[0].subject='
refuses_citing "placeholder" "a placeholder issuer" "${explorer_base[@]}" -f "$executors_file" --set 'explorer.auth.oidc.executorMapping.issuer=https://replace-me.example.test/REPLACE_ME'
refuses_citing 'explorer.auth.oidc.executorMapping.executors[0].subject holds "\n"' "a newline in a subject" "${explorer_base[@]}" -f "$executors_file" --set-string 'explorer.auth.oidc.executorMapping.executors[0].subject=system:serviceaccount:shoal-gateways:stripe
x'
rm -f "$executors_file"

note "== a reference cannot carry a character that changes what the argument means =="
# Every executor reference reaches the container inside one argument, and three
# of the four settings are comma-joined into it. Two characters therefore cannot
# appear in a reference without changing what the argument says.
#
# A newline was the serious one. The container arguments were unquoted YAML
# scalars, so a newline in any list entry ended the scalar and the remainder
# became a new sequence entry — an arbitrary argument of the explorer
# container. The arguments are quoted now and the characters are refused where
# they are written, which are two halves of one fix: quoting stops the
# injection, and the guard stops the quoted remainder from silently matching no
# executor while the pod starts and serves.
refuses_citing "holds a control or line-separator character" "a newline in an allowlisted reference" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=deploy
            - -conceal-withholding=false'
refuses_citing "holds a control or line-separator character" "a newline in an external reference" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=deploy' --set-string 'explorer.fleet.externalExecutorRefs[0]=deploy
            - -conceal-withholding=false'
refuses_citing "holds a control or line-separator character" "a newline in a transmitting reference" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=notify' --set-string 'explorer.fleet.externalEgressExecutorRefs[0]=notify
            - -conceal-withholding=false'
refuses_citing "holds a control or line-separator character" "a newline in the ask reference" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=ask' --set explorer.chat.provider=ollama,explorer.chat.model=m,explorer.chat.baseURL=http://localhost:11434 --set-string 'explorer.fleet.askExecutorRef=ask
            - -conceal-withholding=false'
# A comma is the separator. One entry of "a,b" reaches the workspace as two
# references, so a single element silently becomes two bindings — and the
# collision guards compare the unsplit string, which is how an entry of "a,b"
# passes a check against an askExecutorRef of "b". The Go side refuses that at
# startup, so this is defence in depth rather than the only line.
# Not every line break is ASCII. go-yaml breaks lines on U+0085, U+2028 and
# U+2029 as well as on LF, and RE2's [[:cntrl:]] is ASCII-only -- so the guard
# these three cover used to pass them. The arguments are quoted, so none of
# them injects anything; what they do is reach the workspace inside the
# reference, match no registered executor, and bind nothing while the pod
# starts and serves normally. That is the silent failure the guard exists to
# refuse, which is why it is refused for the same reason a newline is.
refuses_citing "holds a control or line-separator character" "U+0085 in an allowlisted reference" "${explorer_base[@]}" --set-string "explorer.fleet.executorRefs[0]=deploy"$''"- -conceal-withholding=false"
refuses_citing "holds a control or line-separator character" "U+2028 in an allowlisted reference" "${explorer_base[@]}" --set-string "explorer.fleet.executorRefs[0]=deploy"$' '"- -conceal-withholding=false"
refuses_citing "holds a control or line-separator character" "U+2029 in an external reference" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=deploy' --set-string "explorer.fleet.externalExecutorRefs[0]=deploy"$' '"- -conceal-withholding=false"
# A non-ASCII reference used to render here. Since #391 every executor
# reference is held to one ASCII charset (pkg/executorref), because Unicode
# look-alikes and invisibles would register, bind and attest as a different
# executor; the explorer refuses one at startup and the chart refuses it here.
refuses_citing "entry 1 is outside the executor-reference charset" "a non-ASCII allowlisted reference" "${explorer_base[@]}" --set-string "explorer.fleet.executorRefs[0]=d"$'é'"ploy"
refuses_citing "entry 1 is outside the executor-reference charset" "a Cyrillic look-alike external reference" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=deploy' --set-string "explorer.fleet.externalExecutorRefs[0]=depl"$'о'"y"
refuses_citing "entry 2 is outside the executor-reference charset" "an inner space, at its position" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=deploy' --set-string 'explorer.fleet.executorRefs[1]=de ploy'
refuses_citing "entry 1 is outside the executor-reference charset" "a leading dash" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=-deploy'
assert_renders "every charset punctuation renders" '^ +- "-fleet-executor-refs=a.b_c:d/e@f-g"$' "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=a.b_c:d/e@f-g'

refuses_citing "holds a comma" "a comma inside one allowlisted entry" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=deploy\,restart'
refuses_citing "holds a comma" "a comma inside one external entry" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=deploy' --set-string 'explorer.fleet.externalExecutorRefs[0]=deploy\,restart'
# And the injection itself, asserted as an absence rather than as a refusal.
# This is the assertion that would fail if the guard above were deleted AND the
# arguments were unquoted again, which is the state the chart was in: the guard
# alone makes it unreachable, so without this the quoting could be reverted
# with every check still passing.
# The payload has to be in the value for this to mean anything: an earlier
# version of this check passed a reference of "deploy" and so asserted the
# absence of an argument nothing had tried to inject. It cannot use
# refuses_citing, because the point is the state with the guard gone — so it
# asserts the absence under a value the guard currently refuses, which is why
# it is written as an absence and not as a render.
assert_absent_or_refused "a newline cannot become a container argument" "^ +- -conceal-withholding=false$" "${explorer_base[@]}" --set-string 'explorer.fleet.executorRefs[0]=deploy
            - -conceal-withholding=false'
# Every explorer argument is a quoted scalar, which is what makes that true for
# values this file does not enumerate. Pinned positively so the property is
# asserted rather than assumed from the refusals above.
assert_renders "explorer arguments are quoted scalars" '^ +- "-allowed-host=shoal\.example\.test"$' "${explorer_base[@]}"

note "== the external-effect executor bindings =="
# explorer.fleet.externalExecutorRefs and externalEgressExecutorRefs are the
# only keys in this chart that raise an executor's effect ceiling to admit work
# whose consequences land outside Shoal. Everything else in the enforcement
# plane is fail-closed by omission; this is the one place an operator says "I
# accept that Shoal will not perform this and will not undo it".
#
# So the first thing asserted is the absence, not a refusal: nothing an
# operator leaves unset may produce that ceiling, and no positive assertion in
# this file can see that.
#
# The refusals after it each cover a values file that reads like the opt-in
# while not being one, or being one twice over. Every one is pinned to its own
# sentence rather than asserted as "did not render": a blank entry is also
# refused by the allow-list guard downstream of it, so that case would keep
# passing with the guard it is named for deleted — and the other three are
# cheap to pin once the helper is being used anyway.
assert_absent "no configuration produces an external ceiling by omission" "fleet-external" "${explorer_base[@]}"
assert_absent "nor does allowlisting a reference without naming it" "fleet-external" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={deploy}'
refuses_citing "is not in explorer.fleet.executorRefs" "an external reference that is not allowlisted" "${explorer_base[@]}" --set 'explorer.fleet.externalExecutorRefs={deploy}'
refuses_citing "is not in explorer.fleet.executorRefs" "a transmitting external reference that is not allowlisted" "${explorer_base[@]}" --set 'explorer.fleet.externalEgressExecutorRefs={notify}'
# Present but blank binds nothing, and the pod starts and serves anyway — so
# the operator learns about it when the gateway descriptor is refused.
refuses_citing "contains a blank entry" "a blank external reference only" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={deploy}' --set 'explorer.fleet.externalExecutorRefs={ }'
refuses_citing "contains a blank entry" "a blank transmitting reference only" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={notify}' --set 'explorer.fleet.externalEgressExecutorRefs={ }'
# The grounded-reasoning executor's floor equals its ceiling and excludes
# external mutation. One reference cannot carry both bindings, and the loser is
# decided by startup order.
refuses_citing "is also explorer.fleet.askExecutorRef" "the ask reference also bound for external mutation" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={ask}',explorer.fleet.askExecutorRef=ask,explorer.chat.provider=ollama,explorer.chat.model=m,explorer.chat.baseURL=http://localhost:11434 --set 'explorer.fleet.externalExecutorRefs={ask}'
refuses_citing "is also explorer.fleet.askExecutorRef" "the ask reference also bound for transmitting external work" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={ask}',explorer.fleet.askExecutorRef=ask,explorer.chat.provider=ollama,explorer.chat.model=m,explorer.chat.baseURL=http://localhost:11434 --set 'explorer.fleet.externalEgressExecutorRefs={ask}'
# Naming one reference in both lists is two different ceilings for it. The wider
# one winning would mean egress authority is acquired by listing it twice.
refuses_citing "already names" "one reference in both effect lists" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={deploy}' --set 'explorer.fleet.externalExecutorRefs={deploy}' --set 'explorer.fleet.externalEgressExecutorRefs={deploy}'

# And the configurations that must still render. The flag value is asserted,
# not just the absence of a refusal: a list joined into one argument is where a
# dropped or mistyped element disappears quietly, and the whole point of the
# pair of keys is that the two ceilings reach the process as different flags.
assert_renders "an external gateway reference is bound" "\-fleet-external-executor-refs=deploy\"$" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={deploy}' --set 'explorer.fleet.externalExecutorRefs={deploy}'
assert_renders "a transmitting external gateway is bound" "\-fleet-external-egress-executor-refs=notify\"$" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={notify}' --set 'explorer.fleet.externalEgressExecutorRefs={notify}'
assert_absent "a mutating reference does not acquire the egress flag" "fleet-external-egress-executor-refs" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={deploy}' --set 'explorer.fleet.externalExecutorRefs={deploy}'
assert_renders "several external references join into one argument" "\-fleet-external-executor-refs=deploy,restart\"$" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={deploy,restart}' --set 'explorer.fleet.externalExecutorRefs={deploy,restart}'
# Distinct references in the two lists are not a collision: a deployment can
# have one gateway that only mutates and another that also transmits.
assert_renders "both ceilings on separate references" "\-fleet-external-egress-executor-refs=notify\"$" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={deploy,notify}' --set 'explorer.fleet.externalExecutorRefs={deploy}' --set 'explorer.fleet.externalEgressExecutorRefs={notify}'
# The reasoning executor and a gateway coexist on separate references, which is
# the configuration #391 needs: Shoal answers from the corpus in process and
# dispatches the external work.
assert_renders "the ask executor beside a gateway" "\-fleet-external-executor-refs=deploy\"$" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={ask,deploy}',explorer.fleet.askExecutorRef=ask,explorer.chat.provider=ollama,explorer.chat.model=m,explorer.chat.baseURL=http://localhost:11434 --set 'explorer.fleet.externalExecutorRefs={deploy}'
assert_renders "and the ask binding survives it" "\-fleet-ask-executor-ref=ask\"$" "${explorer_base[@]}" --set 'explorer.fleet.executorRefs={ask,deploy}',explorer.fleet.askExecutorRef=ask,explorer.chat.provider=ollama,explorer.chat.model=m,explorer.chat.baseURL=http://localhost:11434 --set 'explorer.fleet.externalExecutorRefs={deploy}'

note "== the llm gateway's metrics are on its health port =="
# #425: the counters are served at /metrics on the -health-address listener,
# not on a listener of their own. The scrape annotation follows healthPort
# rather than naming a fixed number, and nothing renders a separate metrics
# flag, container port or Service port: a second port would be a second
# unauthenticated surface, and publishing it on the Service would make it
# routable in-cluster for no reason. The end-to-end check below asserts the
# annotation names the port -health-address actually serves.
assert_renders "the scrape annotation names the health port" '^ +prometheus\.io/port: "8101"$' "${llm_gateway_base[@]}"
assert_renders "the scrape annotation follows a moved health port" '^ +prometheus\.io/port: "9101"$' "${llm_gateway_base[@]}" --set llmGateway.healthPort=9101
assert_renders "the scrape annotation names /metrics" '^ +prometheus\.io/path: "/metrics"$' "${llm_gateway_base[@]}"
# The base renders the traffic listener as TLS (#424); the health port stays
# plaintext, so the scrape must too. A prometheus.io/scheme: https here would
# have every scrape fail the handshake against a plaintext listener.
assert_absent "the scrape is not told to use TLS against the plaintext health port" 'prometheus\.io/scheme' "${llm_gateway_base[@]}"
assert_renders "a plaintext traffic listener scrapes the same health port" '^ +prometheus\.io/port: "8101"$' "${llm_gateway_base[@]}" --set llmGateway.tls.enabled=false,llmGateway.tls.secretName=
assert_absent "no separate metrics listener is rendered" '(metrics-address|metricsPort|name: "?metrics"?$|targetPort: "?metrics"?$)' "${llm_gateway_base[@]}" --show-only templates/llm-gateway-deployment.yaml --show-only templates/llm-gateway-service.yaml

note "== a gateway value cannot carry a character that changes what the pod is given =="
# Every string the gateway pod is rendered from is a quoted scalar now, and a
# control character in one is refused where it is written. Before the quoting, a
# newline in identity.capability appended an argument of the operator's choosing
# — -allow-plaintext-admission=true among them.
assert_renders "gateway arguments are quoted scalars" '^ +- "-capability=chat\.completions"$' "${llm_gateway_base[@]}"
refuses_citing "holds a control character" "a newline in the capability" "${llm_gateway_base[@]}" --set-string 'llmGateway.identity.capability=chat
            - -allow-plaintext-admission=true'
refuses_citing "holds a control character" "a newline in the admission token variable" "${llm_gateway_base[@]}" --set-string 'llmGateway.admission.tokenEnv=TOKEN
            - name: OTHER'
refuses_citing "holds a control character" "a newline in an allowed host" "${llm_gateway_base[@]}" --set-string 'llmGateway.allowedHosts[0]=llm.example.test
            - -allow-plaintext-admission=true'
refuses_citing "holds a control character" "a newline in a model" "${llm_gateway_base[@]}" --set-string 'llmGateway.models[0]=gpt-4o
            - -allow-plaintext-admission=true'
refuses_citing "holds a control character" "a newline in the image tag" "${llm_gateway_base[@]}" --set-string 'llmGateway.image.tag=v1
          securityContext: {}'
refuses_citing "holds a comma" "a comma inside one allowed host" "${llm_gateway_base[@]}" --set-string 'llmGateway.allowedHosts[0]=a.example.test\,b.example.test'
refuses_citing "holds a comma" "a comma inside one model" "${llm_gateway_base[@]}" --set-string 'llmGateway.models[0]=gpt-4o\,claude-opus-5'
# Structural, so a newly added field or argument cannot be left out of it: in
# the rendered gateway Deployment no container argument is a bare scalar, and
# every value-shaped field is double-quoted unless it is one of the template's
# own constants.
gateway_scalars_quoted() {
  local description="$1"; shift
  local rendered stray
  if ! rendered=$(helm template shoal "$chart" "$@" -s templates/llm-gateway-deployment.yaml 2>&1); then
    fail "should render but was refused: $description"
    return
  fi
  stray=$(printf '%s\n' "$rendered" | grep -E '^ +- -' || true)
  stray+=$'\n'$(printf '%s\n' "$rendered" |
    grep -E '^ +(- )?(name|key|secretName|mountPath|audience|path|image|imagePullPolicy|serviceAccountName|type): [^"]' |
    grep -vE ': (shoal-llm-gateway|RollingUpdate|RuntimeDefault|http|https|health|admission-token|upstream-api-key|listener-tls|/readyz|/healthz)$' || true)
  stray=$(printf '%s\n' "$stray" | sed '/^$/d')
  if [ -n "$stray" ]; then
    fail "an unquoted value in the gateway Deployment: $description"
    { head -3 <<<"$stray" | sed 's/^/      /'; } || true
  fi
}
gateway_scalars_quoted "env-form credentials" "${llm_gateway_base[@]}"

# Each class of rendered value is asserted quoted, so reverting the quoting of
# any one of them fails here rather than only behind the guard.
both_files=("${llm_gateway_base[@]}" "${valid_token_file[@]}" --set llmGateway.upstream.apiKeyFile=/etc/shoal/upstream/api-key --set llmGateway.upstream.apiKeyFileSource=secret)
assert_renders "the env variable name is quoted" '^ +- name: "SHOAL_ADMISSION_TOKEN"$' "${llm_gateway_base[@]}"
assert_renders "the Secret reference is quoted" '^ +name: "shoal-admission-token"$' "${llm_gateway_base[@]}"
assert_renders "the Secret key is quoted" '^ +key: "token"$' "${llm_gateway_base[@]}"
assert_renders "the image is quoted" '^ +image: "[^"]+"$' "${llm_gateway_base[@]}"
assert_renders "the pull policy is quoted" '^ +imagePullPolicy: "IfNotPresent"$' "${llm_gateway_base[@]}"
assert_renders "the Service type is quoted" '^ +type: "ClusterIP"$' "${llm_gateway_base[@]}"
assert_renders "the ServiceAccount is quoted" '^ +serviceAccountName: "shoal-llm-gateway"$' "${both_files[@]}"
assert_renders "a mount path is quoted" '^ +mountPath: "/var/run/secrets/shoal"$' "${both_files[@]}"
assert_renders "a projected token audience is quoted" '^ +audience: "shoal"$' "${both_files[@]}"
assert_renders "a projected token path is quoted" '^ +path: "token"$' "${both_files[@]}"
assert_renders "a credential volume's Secret is quoted" '^ +secretName: "shoal-upstream-key"$' "${both_files[@]}"
assert_renders "a credential volume's item key is quoted" '^ +- key: "api-key"$' "${both_files[@]}"
gateway_scalars_quoted "file-form credentials" "${both_files[@]}"
# The credential volumes are built by a helper the arguments do not pass
# through, so its inputs carry the payload too.
refuses_citing "holds a control character" "a newline in a credential volume's Secret key" "${both_files[@]}" --set-string 'llmGateway.upstream.credentialSecretKey=other-key
                path: decoy
              - key: api-key'
refuses_citing "holds a control character" "a newline in the projected token audience" "${both_files[@]}" --set-string 'llmGateway.admission.tokenAudience=shoal
              - secret:
                  name: injected-secret'
# YAML breaks lines on NEL and the Unicode line and paragraph separators as
# well, and [[:cntrl:]] is ASCII only.
refuses_citing "holds a control character" "a NEL in the capability" "${llm_gateway_base[@]}" --set-string "llmGateway.identity.capability=chat$(printf '\u0085')- -allow-plaintext-admission=true"
refuses_citing "holds a control character" "a line separator in an allowed host" "${llm_gateway_base[@]}" --set-string "llmGateway.allowedHosts[0]=llm.example.test$(printf '\u2028')x"
refuses_citing "holds a control character" "a paragraph separator in a model" "${llm_gateway_base[@]}" --set-string "llmGateway.models[0]=gpt-4o$(printf '\u2029')x"
# The contrast: a non-ASCII character that is not a line break still renders,
# so widening the class did not start refusing ordinary values.
assert_renders "a non-ASCII model name still renders" '\-model=modèle-é"$' "${llm_gateway_base[@]}" --set-string 'llmGateway.models[0]=modèle-é'
renders "a trailing newline the template trims is not a control character in the value" "${llm_gateway_base[@]}" --set-string 'llmGateway.identity.action=complete
'

note "== a name override cannot carry YAML of its own =="
# Both overrides are rendered unquoted into names and labels on every object.
# A line break in either used to end the scalar and inject the remainder:
# fullnameOverride "x<newline>  namespace: kube-system" moved the gateway's
# objects into kube-system.
refuses_citing "fullnameOverride" "a newline in fullnameOverride" "${llm_gateway_base[@]}" --set-string 'fullnameOverride=x
  namespace: kube-system'
refuses_citing "nameOverride" "a newline in nameOverride" -f "$chart/values.yaml" --set-string 'nameOverride=x
  namespace: kube-system'
refuses_citing "fullnameOverride" "a NEL in fullnameOverride" -f "$chart/values.yaml" --set-string "fullnameOverride=x$(printf '\u0085')y"
refuses_citing "fullnameOverride" "a full name that cannot be a Service name" -f "$chart/values.yaml" --set-string fullnameOverride=9shoal
refuses_citing "nameOverride" "an upper-case name override" -f "$chart/values.yaml" --set-string nameOverride=Shoal
refuses_citing "fullnameOverride" "a full name over 63 characters" -f "$chart/values.yaml" --set-string fullnameOverride=$(printf 'a%.0s' $(seq 1 64))
refuses_citing "nameOverride" "a non-string name override" -f "$chart/values.yaml" --set nameOverride=7
# The contrast: ordinary overrides still render, and an empty one is unset.
assert_renders "a valid fullnameOverride names the gateway" '^  name: platform-llm-gateway$' "${llm_gateway_base[@]}" --set-string fullnameOverride=platform
assert_renders "a valid nameOverride names the release's objects" 'app.kubernetes.io/name: "lake"' -f "$chart/values.yaml" --set-string nameOverride=lake
assert_renders "beside a full name, a label-valid name override still renders" 'app.kubernetes.io/name: "Shoal.App_v2"' -f "$chart/values.yaml" --set-string fullnameOverride=shoal --set-string nameOverride=Shoal.App_v2
refuses_citing "nameOverride" "beside a full name, a newline in the name override" -f "$chart/values.yaml" --set-string fullnameOverride=shoal --set-string 'nameOverride=x
  namespace: kube-system'
# A label must be a string: a name override that YAML would read as a number
# or boolean stays one once quoted.
assert_renders "a numeric-looking name override stays a string label" 'app.kubernetes.io/name: "1.5"' -f "$chart/values.yaml" --set-string fullnameOverride=shoal --set-string nameOverride=1.5
renders "an empty fullnameOverride is unset" -f "$chart/values.yaml" --set-string fullnameOverride=
renders "a 63-character fullnameOverride still renders" -f "$chart/values.yaml" --set-string fullnameOverride=$(printf 'a%.0s' $(seq 1 63))

note "== no chart value can carry a line break (#468) =="
# Most of the storage tier's values are rendered as bare YAML scalars, and so
# were several of the explorer's. A newline in any of them ended the scalar and
# the remainder became YAML of the operator's choosing: explorer.service.type
# "ClusterIP<newline>  externalIPs: [6.6.6.6]" gave the Service an external IP.
# The storage profiles must stay byte-identical, so their templates cannot be
# quoted; validate.yaml instead walks every value and refuses the character,
# naming the key.
#
# First the values the issue found injecting, each in a configuration that
# renders it, so the refusal is the one standing between the payload and a
# manifest. Each is pinned to the walk's own sentence for that key: several are
# also shape-checked, and a refusal from the shape check would keep this
# passing with the walk gone.
line_break_cases=(
  "storage image.pullPolicy IfNotPresent"
  "storage writeTier.storageSize 50Gi"
  "storage writeTier.dataDir /var/lib/shoal"
  "storage writeTier.quiesceDelay 10s"
  "storage objectStorage.credentialsSecretName shoal-object-storage-credentials"
  "write-tls writeTier.tls.secretName shoal-tls"
  "distributed image.pullPolicy IfNotPresent"
  "distributed objectStorage.credentialsSecretName shoal-object-storage-credentials"
  "read-tls readFleet.tls.secretName shoal-tls"
  "accumulo tserver.group default"
  "accumulo tserver.drainTimeout 30s"
  "accumulo tserver.walStorageSize 20Gi"
  "accumulo tserver.credentialsSecretName shoal-accumulo-write-credentials"
  "accumulo-tls tserver.tls.secretName shoal-tls"
  "accumulo compactor.group shoal_default"
  "accumulo compactor.hdfsNamenode hdfs://namenode:8020"
  "accumulo compactor.shutdownTimeout 30s"
  "accumulo compactor.stateStorageSize 5Gi"
  "accumulo compactor.credentialsSecretName shoal-accumulo-write-credentials"
  "accumulo-tls compactor.tls.secretName shoal-tls"
  "explorer explorer.storageSize 20Gi"
  "explorer explorer.storageClassName fast"
  "explorer explorer.stateDir /var/lib/shoal"
  "explorer explorer.image.pullPolicy IfNotPresent"
  "explorer explorer.service.type ClusterIP"
)
for line_break_case in "${line_break_cases[@]}"; do
  read -r profile key value <<<"$line_break_case"
  case "$profile" in
    storage) base=(-f "$chart/values.yaml") ;;
    write-tls) base=(-f "$chart/values.yaml" --set writeTier.tls.enabled=true) ;;
    distributed) base=(-f "$chart/values-distributed.yaml") ;;
    read-tls) base=(-f "$chart/values-distributed.yaml" --set readFleet.tls.enabled=true) ;;
    accumulo) base=(-f "$chart/values-accumulo.yaml") ;;
    accumulo-tls) base=(-f "$chart/values-accumulo.yaml" --set tserver.tls.enabled=true --set compactor.tls.enabled=true --set-string tserver.tls.secretName=shoal-tls --set-string compactor.tls.secretName=shoal-tls) ;;
    explorer) base=("${explorer_base[@]}") ;;
  esac
  # The valid value renders in that configuration, so the refusals below can
  # only be about the character.
  renders "$key=$value ($profile)" "${base[@]}" --set-string "$key=$value"
  refuses_citing "$key holds \"\\n\"" "a newline in $key ($profile)" "${base[@]}" --set-string "$key=$value
  injected: true"
  refuses_citing "$key holds \"\\u0085\"" "a NEL in $key ($profile)" "${base[@]}" --set-string "$key=${value}$(printf '\u0085')injected: true"
  refuses_citing "$key holds \"\\u2028\"" "a line separator in $key ($profile)" "${base[@]}" --set-string "$key=${value}$(printf '\u2028')injected: true"
done
# The injections themselves, asserted as absences under the payload, in the
# manner of the explorer arguments above: these are what the walk prevents, and
# they would fail if it and the explorer's quoting were both reverted.
assert_absent_or_refused "a newline in the explorer Service type cannot add externalIPs" "externalIPs" "${explorer_base[@]}" --set-string 'explorer.service.type=ClusterIP
  externalIPs: [6.6.6.6]'
assert_absent_or_refused "a newline in the write tier's storage request cannot name a storage class" "storageClassName: attacker" -f "$chart/values.yaml" --set-string 'writeTier.storageSize=1Gi
        storageClassName: attacker'
assert_absent_or_refused "a newline in a Secret name cannot add a volume" "name: injected" -f "$chart/values.yaml" --set-string 'objectStorage.credentialsSecretName=creds
        - name: injected'
# Every explorer scalar a value reaches is quoted, so the property holds for a
# field this file does not list. Structural, in the manner of the gateway's:
# no bare container argument, and every value-shaped field double-quoted unless
# it is one of the template's own constants. The configuration names a storage
# class and a credential, so the optional fields are rendered and inspected.
explorer_scalars_quoted() {
  local rendered stray
  if ! rendered=$(helm template shoal "$chart" "$@" -s templates/explorer-statefulset.yaml -s templates/explorer-service.yaml 2>&1); then
    fail "should render but was refused: the explorer's quoted scalars"
    return
  fi
  stray=$(printf '%s\n' "$rendered" | grep -E '^ +- -' || true)
  stray+=$'\n'$(printf '%s\n' "$rendered" |
    grep -E '^ +(- )?(name|key|secretName|mountPath|image|imagePullPolicy|type|storage|storageClassName): [^"]' |
    grep -vE ': (shoal-explore-web|shoal-explorer|shoal-explorer-headless|state|tmp|http|health|/tmp|RollingUpdate|RuntimeDefault)$' || true)
  stray=$(printf '%s\n' "$stray" | sed '/^$/d')
  if [ -n "$stray" ]; then
    fail "an unquoted value in the explorer's StatefulSet or Service"
    { head -3 <<<"$stray" | sed 's/^/      /'; } || true
  fi
}
explorer_scalars_quoted "${explorer_base[@]}" --set explorer.storageClassName=fast --set explorer.chat.provider=openai-compatible,explorer.chat.model=m,explorer.chat.baseURL=https://api.example.test,explorer.chat.credentialSecretName=chat --set explorer.embedding.provider=voyage,explorer.embedding.model=v3,explorer.embedding.credentialSecretName=voyage
# The contrasts. A non-ASCII character that is not a line break still renders,
# in a value rendered bare and in one the explorer now quotes.
assert_renders "a non-ASCII data directory still renders" '^ +mountPath: /var/lib/données$' -f "$chart/values.yaml" --set-string 'writeTier.dataDir=/var/lib/données'
assert_renders "a non-ASCII explorer state root still renders" '^ +mountPath: "/var/lib/données"$' "${explorer_base[@]}" --set-string 'explorer.stateDir=/var/lib/données'
# An annotation may span lines with LF: toYaml keeps that inside its own block
# scalar. (Its other line breaks, and any in a key, are refused; see the toYaml
# section below.)
assert_renders "a multi-line Service annotation still renders, as one value" '^    note: \|-$' "${explorer_base[@]}" --set-string 'explorer.service.annotations.note=first
second'
# The service-account key file is the one value that is multi-line by design.
# LF, CRLF and tab stay inside its block scalar; a bare CR or a NEL is a line
# break YAML sees and `indent` does not, so it would leave the block.
key_file="$(mktemp)"
printf 'objectStorage:\n  gcsKeyJson: "{\\r\\n\\t\\"type\\": \\"service_account\\"\\n}\\n"\n' > "$key_file"
assert_renders "a multi-line key file stays inside its block scalar" '^    	"type": "service_account"' -f "$chart/values.yaml" -f "$key_file"
printf 'objectStorage:\n  gcsKeyJson: "{}\\rkind: Injected"\n' > "$key_file"
refuses_citing 'objectStorage.gcsKeyJson holds "\r"' "a bare CR in the key file" -f "$chart/values.yaml" -f "$key_file"
printf 'objectStorage:\n  gcsKeyJson: "{}\\Nkind: Injected"\n' > "$key_file"
refuses_citing 'objectStorage.gcsKeyJson holds "\u0085"' "a NEL in the key file" -f "$chart/values.yaml" -f "$key_file"
rm -f "$key_file"
# The walk covers values the chart's values.yaml does not list, so a key added
# to a template before its default is covered from the start.
refuses_citing 'explorer.extra holds "\n"' "a newline in a key values.yaml does not list" -f "$chart/values.yaml" --set-string 'explorer.extra=x
y'
# And the refusal names the character, never the value: the walk reaches the
# accumulo password and the key file, and refusals are printed to CI logs.
password_refusal=$(helm template shoal "$chart" -f "$chart/values.yaml" --set-string 'readFleet.accumuloPassword=hunter2-secret
' 2>&1 || true)
if grep -qF 'hunter2-secret' <<<"$password_refusal"; then
  fail "a refused password is printed in the refusal"
fi
if ! grep -qF 'readFleet.accumuloPassword holds' <<<"$password_refusal"; then
  fail "a trailing newline in the accumulo password is not refused by the walk"
fi

# Then every value in values.yaml, enumerated rather than listed, so a key added
# there later is fuzzed without anyone adding a case for it. Each leaf (an empty
# list's first element, an empty map's new key) gets a newline payload and a NEL
# payload on the default values, where every optional component is off — so no
# component's own guard can be what refuses it — and the refusal has to be the
# walk's own sentence for that key (or, for mode, the mode guard's), since a
# shape check names the key too and must not stand in for the walk. A non-ASCII
# value that is not a line break must render for every string-valued leaf
# except those held to a shape.
if ! python3 - "$chart" <<'FUZZ'
import concurrent.futures, subprocess, sys, yaml

chart = sys.argv[1]
values = yaml.safe_load(open(chart + "/values.yaml"))
# Mirrors $lineBreakMultiline in validate.yaml: values under these may hold LF
# (and CRLF and tab), and nothing else of the class. They are still walked.
multiline = ("objectStorage.gcsKeyJson", "explorer.service.annotations",
             "llmGateway.service.annotations", "llmGateway.admission.tokenVolume",
             "llmGateway.upstream.apiKeyVolume")
# Held to a shape (or, for mode, an enumeration) on the default values, so a
# non-ASCII value there is refused for that reason and not this one.
shaped = {"mode", "image.pullPolicy", "objectStorage.credentialsSecretName",
          "writeTier.storageSize", "writeTier.quiesceDelay"}

leaves = []
def walk(path, value):
    if isinstance(value, dict):
        if not value:
            leaves.append((path + ".fuzz", None))
        for key, child in value.items():
            walk(f"{path}.{key}" if path else key, child)
    elif isinstance(value, list):
        if not value:
            leaves.append((path + "[0]", None))
        for index, child in enumerate(value):
            walk(f"{path}[{index}]", child)
    else:
        leaves.append((path, value))
walk("", values)

def helm(path, payload):
    return subprocess.run(
        ["helm", "template", "shoal", chart, "-f", chart + "/values.yaml",
         "--set-string", f"{path}={payload}"],
        capture_output=True, text=True)

# The walk's own sentence, so a shape check that also names the key cannot
# stand in for it. mode is the exception: its guard runs first and names it.
def expected(path):
    return "mode must be one of" if path == "mode" else f"{path} holds "

def check(leaf):
    path, default = leaf
    problems = []
    payloads = [("a NEL", "x\u0085injected: true")]
    if not any(path == m or path.startswith(m + ".") for m in multiline):
        payloads.append(("a newline", "x\n  injected: true"))
    for label, payload in payloads:
        result = helm(path, payload)
        if result.returncode == 0:
            problems.append(f"{label} in {path} renders")
        elif expected(path) not in result.stderr:
            problems.append(f"{label} in {path} is refused, but not by the walk: "
                            + result.stderr.strip().splitlines()[0][:160])
    if isinstance(default, str) and path not in shaped:
        result = helm(path, "x\u00e9y")
        if result.returncode != 0:
            problems.append(f"a non-ASCII value in {path} is refused: "
                            + result.stderr.strip().splitlines()[0][:160])
    return problems

with concurrent.futures.ThreadPoolExecutor(8) as pool:
    problems = [p for found in pool.map(check, leaves) for p in found]
for problem in problems:
    print("      " + problem)
if len(leaves) < 150:
    print(f"      only {len(leaves)} leaves enumerated: the walk over values.yaml is broken")
    raise SystemExit(1)
raise SystemExit(1 if problems else 0)
FUZZ
then
  fail "a value in values.yaml can carry a line break into the manifests, or a valid one is refused (see above)"
fi

note "== unquoted values are held to their shape =="
# The storage templates cannot be quoted, and a bare scalar is cut short by
# " #" or turned into a map by ": " without any line break; in the accumulo
# templates' flow mappings a comma ends it. Values with a fixed form are held
# to it, which refuses nothing the API server or the binary would accept.
refuses_citing "writeTier.storageSize must be a Kubernetes quantity" "a storage request that is not a quantity" -f "$chart/values.yaml" --set-string 'writeTier.storageSize=50Gi #'
refuses_citing "explorer.storageSize must be a Kubernetes quantity" "an explorer storage request that is not a quantity" "${explorer_base[@]}" --set-string 'explorer.storageSize=fast: 20Gi'
refuses_citing "tserver.walStorageSize must be a Kubernetes quantity" "a WAL storage request that is not a quantity" -f "$chart/values-accumulo.yaml" --set-string 'tserver.walStorageSize=20Gi}'
refuses_citing "compactor.stateStorageSize must be a Kubernetes quantity" "a compactor storage request that is not a quantity" -f "$chart/values-accumulo.yaml" --set-string 'compactor.stateStorageSize=5Gi}'
refuses_citing "writeTier.quiesceDelay must be a Go duration" "a bare number as the quiesce delay" -f "$chart/values.yaml" --set-string writeTier.quiesceDelay=10
refuses_citing "readFleet.drainTimeout must be a Go duration" "a read-fleet drain timeout with no unit" -f "$chart/values-distributed.yaml" --set-string readFleet.drainTimeout=30
refuses_citing "tserver.drainTimeout must be a Go duration" "a tserver drain timeout that is not a duration" -f "$chart/values-accumulo.yaml" --set-string 'tserver.drainTimeout=30 seconds'
refuses_citing "compactor.shutdownTimeout must be a Go duration" "a compactor timeout that is not a duration" -f "$chart/values-accumulo.yaml" --set-string 'compactor.shutdownTimeout=30s #'
refuses_citing "explorer.auth.oidc.clockSkew must be a Go duration" "a clock skew that is not a duration" "${explorer_base[@]}" --set-string explorer.auth.oidc.clockSkew=1minute
refuses_citing "explorer.disclosure.mosaic.window must be a Go duration" "a mosaic window that is not a duration" "${explorer_base[@]}" --set explorer.disclosure.mosaic.maxDomains=3 --set-string explorer.disclosure.mosaic.window=hourly
# The flow-mapping injection a line break is not needed for: a comma ends the
# Secret name inside {name: ..., key: ...} and the rest names another key.
refuses_citing "tserver.credentialsSecretName must be a Secret name" "a comma in the tserver's Secret name" -f "$chart/values-accumulo.yaml" --set-string 'tserver.credentialsSecretName=creds\, key: other'
refuses_citing "compactor.credentialsSecretName must be a Secret name" "a brace in the compactor's Secret name" -f "$chart/values-accumulo.yaml" --set-string 'compactor.credentialsSecretName=creds}'
refuses_citing "compactor.tls.secretName must be a Secret name" "a brace in the compactor's TLS Secret" -f "$chart/values-accumulo.yaml" --set compactor.tls.enabled=true --set-string 'compactor.tls.secretName=tls}}'
refuses_citing "objectStorage.credentialsSecretName must be a Secret name" "an upper-case Secret name" -f "$chart/values.yaml" --set-string objectStorage.credentialsSecretName=Shoal
refuses_citing "writeTier.tls.secretName must be a Secret name" "a write-tier TLS Secret that is not a name" -f "$chart/values.yaml" --set writeTier.tls.enabled=true --set-string 'writeTier.tls.secretName=tls #'
refuses_citing "readFleet.tls.secretName must be a Secret name" "a read-fleet TLS Secret that is not a name" -f "$chart/values-distributed.yaml" --set readFleet.tls.enabled=true --set-string 'readFleet.tls.secretName=tls: x'
refuses_citing "explorer.chat.credentialSecretName must be a Secret name" "a chat credential Secret that is not a name" "${explorer_base[@]}" --set explorer.chat.provider=openai-compatible,explorer.chat.model=m,explorer.chat.baseURL=https://api.example.test --set-string 'explorer.chat.credentialSecretName=Chat Key'
refuses_citing "image.pullPolicy must be one of Always, IfNotPresent or Never" "an unknown pull policy" -f "$chart/values.yaml" --set-string image.pullPolicy=Sometimes
refuses_citing "explorer.image.pullPolicy must be one of Always, IfNotPresent or Never" "an unknown explorer pull policy" "${explorer_base[@]}" --set-string explorer.image.pullPolicy=always
refuses_citing "explorer.service.type must be one of ClusterIP, NodePort, LoadBalancer or ExternalName" "an unknown Service type" "${explorer_base[@]}" --set-string explorer.service.type=Internal
# The contrasts: every form those fields legitimately take still renders.
renders "decimal, binary-SI and exponent quantities" -f "$chart/values-accumulo.yaml" --set-string writeTier.storageSize=1.5Gi,tserver.walStorageSize=500M,compactor.stateStorageSize=5e9
renders "an integer storage request" -f "$chart/values.yaml" --set writeTier.storageSize=53687091200
renders "compound and fractional durations" -f "$chart/values-accumulo.yaml" --set-string tserver.drainTimeout=1m30s,compactor.shutdownTimeout=1.5s
renders "zero and sub-second durations" -f "$chart/values-distributed.yaml" --set-string readFleet.quiesceDelay=0,readFleet.drainTimeout=250ms,readFleet.readinessInterval=1h
renders "dotted Secret names" -f "$chart/values-accumulo.yaml" --set-string tserver.credentialsSecretName=shoal.accumulo-write,compactor.credentialsSecretName=shoal.accumulo-write
renders "every pull policy" -f "$chart/values.yaml" --set-string image.pullPolicy=Never
renders "an empty pull policy is left to the API server's default" -f "$chart/values.yaml" --set-string image.pullPolicy=
renders "every Service type" "${explorer_base[@]}" --set-string explorer.service.type=LoadBalancer
# Checked only where rendered: a disabled component's leftovers do not stop an
# install that never uses them.
renders "a disabled component's shapes are not checked" -f "$chart/values.yaml" --set-string tserver.drainTimeout=later,compactor.stateStorageSize=big,explorer.service.type=Internal

note "== llm gateway guards refuse =="
# The gateway's failure mode is not a crash. It is required to fail closed, so
# nearly every misconfiguration below renders a pod that passes every probe and
# denies every call — which from outside is a total outage of whatever is
# configured to go through it. That is what these move to render time.
refuses_citing "llmProxy has been renamed to llmGateway" \
  "the pre-rename llmProxy key still enabled" -f "$chart/values.yaml" --set llmProxy.enabled=true
refuses_citing "llmProxy has been renamed to llmGateway" \
  "settings left under llmProxy beside an enabled llmGateway" "${llm_gateway_base[@]}" \
  --set llmProxy.upstream.requestTimeout=30s
refuses_citing "llmProxy has been renamed to llmGateway" \
  "a scalar llmProxy" -f "$chart/values.yaml" --set llmProxy=true
renders "a null llmProxy is ignored"     "${llm_gateway_base[@]}" --set llmProxy=null
renders "an empty llmProxy is ignored"   "${llm_gateway_base[@]}" --set-json 'llmProxy={}'
renders "a disabled llmProxy is ignored" "${llm_gateway_base[@]}" --set llmProxy.enabled=false
renders "an old values dump with llmProxy disabled is ignored" "${llm_gateway_base[@]}" \
  --set llmProxy.enabled=false,llmProxy.replicas=2,llmProxy.upstream.requestTimeout=30s
refuses_citing "llmProxy has been renamed to llmGateway" \
  "a string enabled under llmProxy is not a disabled one" -f "$chart/values.yaml" \
  --set-string llmProxy.enabled=false
refuses "no admission URL"              "${llm_gateway_base[@]}" --set llmGateway.admission.url=
refuses "blank admission URL"           "${llm_gateway_base[@]}" --set llmGateway.admission.url=" "
refuses "admission URL with no scheme"  "${llm_gateway_base[@]}" --set llmGateway.admission.url=shoal.example.test
refuses "plaintext remote admission"    "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://shoal.example.test
refuses "no admission token env"        "${llm_gateway_base[@]}" --set llmGateway.admission.tokenEnv=
refuses "blank admission token env"     "${llm_gateway_base[@]}" --set llmGateway.admission.tokenEnv=" "
refuses "no admission credential"       "${llm_gateway_base[@]}" --set llmGateway.admission.credentialSecretName=
refuses "blank admission credential"    "${llm_gateway_base[@]}" --set llmGateway.admission.credentialSecretName=" "
refuses "no admission credential key"   "${llm_gateway_base[@]}" --set llmGateway.admission.credentialSecretKey=
# MaxActionClaimTTL is five minutes and the admission surface refuses a request
# outside it, so a longer lease is not a longer lease — it is every call denied.
refuses "lease above MaxActionClaimTTL" "${llm_gateway_base[@]}" --set llmGateway.admission.lease=10m
refuses "lease at 5m plus a second"     "${llm_gateway_base[@]}" --set llmGateway.admission.lease=5m1s
refuses "zero lease"                    "${llm_gateway_base[@]}" --set llmGateway.admission.lease=0s
# The duration parser, from both sides. Each of these is a value whose leading
# characters do parse — "30sec" scans as 30s, "1.5m" as 5m — so dropping the
# parser's refusal makes them render rather than trip a neighbouring guard.
# That is what makes them attribute to the parser and not to something else.
refuses "unparseable lease"             "${llm_gateway_base[@]}" --set llmGateway.admission.lease=30sec
refuses "fractional lease"              "${llm_gateway_base[@]}" --set llmGateway.admission.lease=1.5m
refuses "unparseable request timeout"   "${llm_gateway_base[@]}" --set llmGateway.upstream.requestTimeout=30sec
refuses "unitless lease"                "${llm_gateway_base[@]}" --set llmGateway.admission.lease=30
refuses "no upstream base URL"          "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=
refuses "blank upstream base URL"       "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=" "
refuses "upstream URL with no scheme"   "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=api.example.test/v1
refuses "no upstream api key env"       "${llm_gateway_base[@]}" --set llmGateway.upstream.apiKeyEnv=
refuses "remote upstream, no credential" "${llm_gateway_base[@]}" --set llmGateway.upstream.credentialSecretName=
refuses "upstream credential, no key"   "${llm_gateway_base[@]}" --set llmGateway.upstream.credentialSecretKey=
# A call that outlives its lease is performed under a token that can no longer
# be reported against: the admission is abandoned, not resolved.
refuses "timeout outlives the lease"    "${llm_gateway_base[@]}" --set llmGateway.upstream.requestTimeout=90s
refuses "unbounded upstream request"    "${llm_gateway_base[@]}" --set llmGateway.upstream.requestTimeout=0s
refuses "no allowed hosts"              "${llm_gateway_base[@]}" --set llmGateway.allowedHosts=null
refuses "blank allowed host only"       "${llm_gateway_base[@]}" --set 'llmGateway.allowedHosts={ }'
refuses "no agent id"                   "${llm_gateway_base[@]}" --set llmGateway.identity.agentID=
refuses "blank agent id"                "${llm_gateway_base[@]}" --set llmGateway.identity.agentID=" "
refuses "no capability"                 "${llm_gateway_base[@]}" --set llmGateway.identity.capability=
refuses "no action"                     "${llm_gateway_base[@]}" --set llmGateway.identity.action=
refuses "no source id"                  "${llm_gateway_base[@]}" --set llmGateway.identity.sourceID=
refuses "no policy id"                  "${llm_gateway_base[@]}" --set llmGateway.identity.policyID=
refuses "zero agent generation"         "${llm_gateway_base[@]}" --set llmGateway.identity.agentGeneration=0
refuses "negative agent generation"     "${llm_gateway_base[@]}" --set llmGateway.identity.agentGeneration=-1
refuses "source id is not base64url"    "${llm_gateway_base[@]}" --set 'llmGateway.identity.sourceID=source/one'
refuses "padded policy id"              "${llm_gateway_base[@]}" --set 'llmGateway.identity.policyID=cG9saWN5=='
refuses "truncated source id"           "${llm_gateway_base[@]}" --set llmGateway.identity.sourceID=c291cmNlZ
refuses "placeholder admission URL"     "${llm_gateway_base[@]}" --set llmGateway.admission.url=https://REPLACE_ME/
refuses "placeholder allowed host"      "${llm_gateway_base[@]}" --set 'llmGateway.allowedHosts={REPLACE_ME.example.test}'
# A placeholder identity is the dangerous case: the gateway comes up, passes every
# probe, and is refused by the plane on every request while the chart reports
# success.
refuses "placeholder capability"        "${llm_gateway_base[@]}" --set llmGateway.identity.capability=REPLACE_ME
refuses "placeholder upstream URL"      "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=https://REPLACE_ME/v1
refuses "health port collides"          "${llm_gateway_base[@]}" --set llmGateway.healthPort=8100
refuses "privileged listen port"        "${llm_gateway_base[@]}" --set llmGateway.containerPort=80
refuses "privileged health port"        "${llm_gateway_base[@]}" --set llmGateway.healthPort=81
refuses "port out of range"             "${llm_gateway_base[@]}" --set llmGateway.servicePort=70000
refuses "negative replicas"             "${llm_gateway_base[@]}" --set llmGateway.replicas=-1
refuses "fractional replicas"           "${llm_gateway_base[@]}" --set llmGateway.replicas=1.5
refuses "null replicas"                 "${llm_gateway_base[@]}" --set llmGateway.replicas=null
# The mirror image of the explorer's rule. A budget that refuses every eviction
# protects a singleton; this is not one, so it only wedges drains.
refuses "singleton disruption budget"   "${llm_gateway_base[@]}" --set llmGateway.podDisruptionBudget.maxUnavailable=0
# A key written with nothing after it is nil, not "". `toString nil` is the
# string "<nil>", which is non-blank — so a required-value check that does not
# normalize first reads an absent value as configured and renders the pod with
# an empty flag. These were observed rendering before `default ""` went in
# ahead of every `trim`, which is the whole reason they are pinned here.
refuses "null admission URL"            "${llm_gateway_base[@]}" --set llmGateway.admission.url=null
refuses "null admission token env"      "${llm_gateway_base[@]}" --set llmGateway.admission.tokenEnv=null
refuses "null admission credential"     "${llm_gateway_base[@]}" --set llmGateway.admission.credentialSecretName=null
refuses "null lease"                    "${llm_gateway_base[@]}" --set llmGateway.admission.lease=null
refuses "null upstream base URL"        "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=null
refuses "null upstream api key env"     "${llm_gateway_base[@]}" --set llmGateway.upstream.apiKeyEnv=null
refuses "null request timeout"          "${llm_gateway_base[@]}" --set llmGateway.upstream.requestTimeout=null
refuses "null agent id"                 "${llm_gateway_base[@]}" --set llmGateway.identity.agentID=null
refuses "null capability"               "${llm_gateway_base[@]}" --set llmGateway.identity.capability=null
refuses "null action"                   "${llm_gateway_base[@]}" --set llmGateway.identity.action=null
refuses "null source id"                "${llm_gateway_base[@]}" --set llmGateway.identity.sourceID=null
refuses "null agent generation"         "${llm_gateway_base[@]}" --set llmGateway.identity.agentGeneration=null

note "== llm gateway valid configurations still render =="
# The gateway is stateless and must not be a singleton: the explorer refuses
# replicas above 1 and the gateway must not, so scaling out is asserted to render
# rather than merely left unforbidden.
renders "scaled out to ten"                 "${llm_gateway_base[@]}" --set llmGateway.replicas=10
renders "scaled to zero"                    "${llm_gateway_base[@]}" --set llmGateway.replicas=0
renders "loopback upstream needs no credential" "${llm_gateway_base[@]}" --set llmGateway.upstream.credentialSecretName=,llmGateway.upstream.baseURL=http://localhost:11434/v1
renders "plaintext admission acknowledged"  "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://shoal-explorer:8098,llmGateway.admission.allowPlaintext=true
renders "loopback admission needs no acknowledgement" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://127.0.0.1:8098
# The fleet's claim ceiling is the upper bound, and the report window is the
# lower one. A lease equal to the timeout is refused, not accepted: the binary
# refuses that pair at startup, so rendering it would produce a pod that never
# serves — which is the failure these guards exist to move to render time.
# The grace period moves with the lease, because the gateway drains for the lease:
# at the 5m ceiling the shipped 75s grace period is refused, and that refusal is
# asserted below rather than worked around here.
renders "lease at the ceiling with room to report" "${llm_gateway_base[@]}" --set llmGateway.admission.lease=5m,llmGateway.upstream.requestTimeout=4m,llmGateway.terminationGracePeriodSeconds=305
refuses "lease equal to the timeout"        "${llm_gateway_base[@]}" --set llmGateway.admission.lease=5m,llmGateway.upstream.requestTimeout=5m
refuses "lease inside the report window"    "${llm_gateway_base[@]}" --set llmGateway.admission.lease=35s,llmGateway.upstream.requestTimeout=31s
renders "compound durations"                "${llm_gateway_base[@]}" --set llmGateway.admission.lease=2m30s,llmGateway.upstream.requestTimeout=1m30s,llmGateway.terminationGracePeriodSeconds=155
refuses "timeout equal to the lease"        "${llm_gateway_base[@]}" --set llmGateway.upstream.requestTimeout=60s
renders "millisecond timeout"               "${llm_gateway_base[@]}" --set llmGateway.upstream.requestTimeout=500ms
renders "disruption budget disabled"        "${llm_gateway_base[@]}" --set llmGateway.podDisruptionBudget.enabled=false,llmGateway.podDisruptionBudget.maxUnavailable=0
renders "operator-supplied affinity"        "${llm_gateway_base[@]}" --set 'llmGateway.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].key=kubernetes.io/os' --set 'llmGateway.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].operator=In' --set 'llmGateway.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values={linux}'
renders "several allowed hosts, trimmed"    "${llm_gateway_base[@]}" --set 'llmGateway.allowedHosts={ llm.example.test , llm-internal.example.test }'

note "== the grace period covers the drain =="
# The gateway drains for the lease on SIGTERM, because the lease is the bound on
# an admitted call's whole lifetime including its report. terminationGracePeriod
# is the kubelet's budget for that window, so a grace period inside it is a
# SIGKILL mid-drain: the calls killed are exactly the ones whose egress has
# already happened and whose grant has already been spent, and every rolling
# update produces them on a schedule while the pod terminates cleanly.
#
# The shipped values did this — 45 against a 60s lease — which is why the
# default moved and why both sides of the boundary are pinned here.
refuses "grace period below the lease"      "${llm_gateway_base[@]}" --set llmGateway.terminationGracePeriodSeconds=30
refuses "grace period equal to the lease"   "${llm_gateway_base[@]}" --set llmGateway.terminationGracePeriodSeconds=60
# One second apart, on either side of the required 5s margin. A guard that only
# refused a grace period *below* the lease would pass the first of these, and
# the pod would be killed while the drain still had five seconds of work.
refuses "grace period one second inside the margin" "${llm_gateway_base[@]}" --set llmGateway.terminationGracePeriodSeconds=64
renders "grace period exactly the margin above the lease" "${llm_gateway_base[@]}" --set llmGateway.terminationGracePeriodSeconds=65
refuses "the shipped grace period against a ceiling lease" "${llm_gateway_base[@]}" --set llmGateway.admission.lease=5m,llmGateway.upstream.requestTimeout=4m
renders "a ceiling lease with the grace period raised" "${llm_gateway_base[@]}" --set llmGateway.admission.lease=5m,llmGateway.upstream.requestTimeout=4m,llmGateway.terminationGracePeriodSeconds=305
refuses "zero grace period"                 "${llm_gateway_base[@]}" --set llmGateway.terminationGracePeriodSeconds=0
# Cited, because a value that is not a number casts to 0 and is then refused by
# the drain guard above as well.
refuses_citing "must be a whole number of seconds" "null grace period" "${llm_gateway_base[@]}" --set llmGateway.terminationGracePeriodSeconds=null
refuses_citing "must be a whole number of seconds" "negative grace period" "${llm_gateway_base[@]}" --set llmGateway.terminationGracePeriodSeconds=-1
# A float that is comfortably above the lease, via --set-json for the reason the
# token lifetime needs it: this is the only shape the whole-number guard alone
# catches, because it casts to 75 and clears the drain guard.
refuses_citing "must be a whole number of seconds" "fractional grace period" "${llm_gateway_base[@]}" --set-json 'llmGateway.terminationGracePeriodSeconds=75.5'

note "== the admission token file form =="
# The file form exists because a rotating credential is a file: the kubelet
# rewrites a projected ServiceAccount token in place and never updates an
# environment variable. Every refusal here is either a pod the binary rejects at
# startup, a pod spec the API server rejects, or — the common case and the
# dangerous one — a pod that comes up, passes every probe, and fails every
# admission request on a credential it cannot read or that nothing accepts.
renders "a projected ServiceAccount token"  "${token_file_base[@]}"
# The binary compares -admission-token-env against its own flag default rather
# than against emptiness, so the default being present is not a second choice.
# If it were, the file form would be unreachable without also blanking the env
# key, and this case is what proves the chart kept that property.
renders "the file form with tokenEnv at its default" "${token_file_base[@]}" --set llmGateway.admission.tokenEnv=SHOAL_ADMISSION_TOKEN
renders "the file form with tokenEnv blanked" "${token_file_base[@]}" --set llmGateway.admission.tokenEnv=
refuses "both token forms chosen explicitly" "${token_file_base[@]}" --set llmGateway.admission.tokenEnv=OTHER_VAR
# Blank and absent are the env form, not a broken file form — so these render
# against the base's Secret rather than being refused. Without them, reading
# `tokenFile` as configured-when-whitespace would be invisible.
renders "a blank token file is the env form" "${llm_gateway_base[@]}" --set 'llmGateway.admission.tokenFile= '
renders "a null token file is the env form"  "${llm_gateway_base[@]}" --set llmGateway.admission.tokenFile=null
refuses "a relative token file"             "${token_file_base[@]}" --set llmGateway.admission.tokenFile=secrets/token
refuses "a token file at the filesystem root" "${token_file_base[@]}" --set llmGateway.admission.tokenFile=/token
# Cited, not merely refused: an unrecognised source leaves the audience set with
# no projection to bind it to, so the audience guard refuses these too and a
# bare `refuses` would pass with the source guard deleted.
refuses_citing "tokenFileSource must be projected" "an unknown token file source" "${token_file_base[@]}" --set llmGateway.admission.tokenFileSource=serviceaccount
refuses_citing "tokenFileSource must be projected" "a null token file source" "${token_file_base[@]}" --set llmGateway.admission.tokenFileSource=null
refuses_citing "tokenFileSource must be projected" "a blank token file source" "${token_file_base[@]}" --set 'llmGateway.admission.tokenFileSource= '
# A Secret named where nothing reads it is the invisible credential the binary
# refuses two token flags over: the operator believes one thing is presented and
# another is, and the symptom names neither.
refuses "a projected token with an admission Secret" "${token_file_base[@]}" --set llmGateway.admission.credentialSecretName=shoal-admission-token
refuses "an operator volume with an admission Secret" "${token_file_base[@]}" --set llmGateway.admission.tokenFileSource=volume,llmGateway.admission.tokenAudience=,llmGateway.admission.credentialSecretName=shoal-admission-token --set 'llmGateway.admission.tokenVolume.csi.driver=csi.spiffe.io'
# Without an audience the projected token is issued for the cluster's own API
# server, which the explorer is not: a 401 on every admission request.
refuses "a projected token with no audience" "${token_file_base[@]}" --set llmGateway.admission.tokenAudience=
refuses "a projected token with a blank audience" "${token_file_base[@]}" --set 'llmGateway.admission.tokenAudience= '
refuses "a projected token with a placeholder audience" "${token_file_base[@]}" --set llmGateway.admission.tokenAudience=REPLACE_ME
refuses "a placeholder token file"          "${token_file_base[@]}" --set llmGateway.admission.tokenFile=/var/run/REPLACE_ME/token
# A projected token's subject is the pod's ServiceAccount. Left implicit it is
# the namespace's `default`, so authorizing the gateway authorizes every pod in
# the namespace.
refuses "a projected token with no ServiceAccount" "${token_file_base[@]}" --set llmGateway.serviceAccountName=
refuses "a projected token with a blank ServiceAccount" "${token_file_base[@]}" --set 'llmGateway.serviceAccountName= '
renders "default named explicitly as the ServiceAccount" "${token_file_base[@]}" --set llmGateway.serviceAccountName=default
# 600 is the API server's floor for a token projection. Below it the Deployment
# is accepted and no pod is ever created from it, which is a rollout that never
# completes and no pod log to read at all.
refuses "a token lifetime below the API server floor" "${token_file_base[@]}" --set llmGateway.admission.tokenExpirationSeconds=599
refuses "a zero token lifetime"             "${token_file_base[@]}" --set llmGateway.admission.tokenExpirationSeconds=0
refuses "a negative token lifetime"         "${token_file_base[@]}" --set llmGateway.admission.tokenExpirationSeconds=-1
# Cited for the reason above, from the other side: anything non-numeric casts to
# 0, so the floor guard refuses all of these as well.
refuses_citing "must be a whole number of seconds" "a null token lifetime" "${token_file_base[@]}" --set llmGateway.admission.tokenExpirationSeconds=null
refuses_citing "must be a whole number of seconds" "an unparseable token lifetime" "${token_file_base[@]}" --set llmGateway.admission.tokenExpirationSeconds=1h
# --set-json, because --set cannot express a float: it parses what it can as an
# integer and keeps the rest as a string, and a string casts to 0 and lands on
# the floor guard. A real fractional value is the one shape only the
# whole-number guard can catch — it casts to a perfectly acceptable 600 — and
# a values file can hold one, so the fixture has to be able to say it.
refuses_citing "must be a whole number of seconds" "a fractional token lifetime" "${token_file_base[@]}" --set-json 'llmGateway.admission.tokenExpirationSeconds=600.5'
renders "a token lifetime at the floor"     "${token_file_base[@]}" --set llmGateway.admission.tokenExpirationSeconds=600
renders "a day-long token lifetime"         "${token_file_base[@]}" --set llmGateway.admission.tokenExpirationSeconds=86400
# The file form with a loopback upstream is the one configuration where nothing
# at all goes into the environment. `env:` with no entries under it is YAML
# null, which the API server rejects as not a list, so the key has to be omitted
# rather than emptied — and this is the only case that renders it.
loopback_token_file=(
  "${token_file_base[@]}"
  --set llmGateway.upstream.credentialSecretName=
  --set llmGateway.upstream.baseURL=http://localhost:11434/v1
)
renders "a token file with nothing in the environment" "${loopback_token_file[@]}"

# The Secret-as-a-file source. Worth having rather than redundant with the env
# form: the kubelet updates a mounted Secret's contents in place and the gateway
# re-reads per request, so rotating the value takes effect without rolling the
# pod — which an environment variable cannot do at all.
secret_file=(
  "${llm_gateway_base[@]}"
  --set llmGateway.admission.tokenFile=/etc/shoal/admission/token
  --set llmGateway.admission.tokenFileSource=secret
)
renders "the admission Secret mounted as a file" "${secret_file[@]}"
refuses "a Secret file source with no Secret" "${secret_file[@]}" --set llmGateway.admission.credentialSecretName=
refuses "a Secret file source with no key"  "${secret_file[@]}" --set llmGateway.admission.credentialSecretKey=
# An audience names the verifier a projected token is minted for. With this
# source nothing issues a token, so a value here records a binding that exists
# nowhere in the deployment.
refuses "a Secret file source with an audience" "${secret_file[@]}" --set llmGateway.admission.tokenAudience=shoal
# The lifetime is only consulted for a projection, so a bad one here must not
# refuse: a guard that fired anyway would be refusing a configuration that works.
renders "a Secret file source ignores the token lifetime" "${secret_file[@]}" --set llmGateway.admission.tokenExpirationSeconds=1

# The operator-supplied volume: the seam for a CSI driver the chart does not
# model. It can check only the two things that make the volume unusable rather
# than merely unknown.
operator_volume=(
  "${llm_gateway_base[@]}"
  --set llmGateway.admission.credentialSecretName=
  --set llmGateway.admission.tokenFile=/var/run/spiffe/token
  --set llmGateway.admission.tokenFileSource=volume
)
renders "an operator-supplied CSI token volume" "${operator_volume[@]}" --set 'llmGateway.admission.tokenVolume.csi.driver=csi.spiffe.io' --set 'llmGateway.admission.tokenVolume.csi.readOnly=true'
# Cited: with the guard gone the chart renders a volume whose source is an empty
# flow mapping, which Helm refuses while parsing its own output — so the case
# would pass for a reason that has nothing to do with the guard.
refuses_citing "tokenVolume is required" "a volume source with no volume" "${operator_volume[@]}"
refuses "a volume source that names itself" "${operator_volume[@]}" --set 'llmGateway.admission.tokenVolume.name=my-token' --set 'llmGateway.admission.tokenVolume.csi.driver=csi.spiffe.io'
refuses "a volume source with an audience"  "${operator_volume[@]}" --set llmGateway.admission.tokenAudience=shoal --set 'llmGateway.admission.tokenVolume.csi.driver=csi.spiffe.io'

# The env form's own guards still own the env form, which is what makes
# relaxing them for the file form safe. Each of these is already asserted above
# against llm_gateway_base; the point here is that the file form does not silently
# relax them for everyone.
renders "the env form still needs no volume" "${llm_gateway_base[@]}"
refuses "the env form still needs a Secret"  "${llm_gateway_base[@]}" --set llmGateway.admission.credentialSecretName=
refuses "the env form still needs a variable" "${llm_gateway_base[@]}" --set llmGateway.admission.tokenEnv=

note "== the transport acknowledgement, and the one hop that has none =="
# The listener (#424). The binary applies its outbound rule to the hop it
# receives: TLS, or plaintext on loopback only. The pod binds 0.0.0.0, so the
# chart renders either the key pair or the acknowledgement, never both and
# never neither; the wiring check below holds it to that.
plaintext_listener=(--set llmGateway.tls.enabled=false,llmGateway.tls.secretName=)
assert_renders "TLS by default: the certificate"     '"-tls-cert-file=/etc/shoal-llm-gateway/tls/tls.crt"' "${llm_gateway_base[@]}"
assert_renders "TLS by default: the key"             '"-tls-key-file=/etc/shoal-llm-gateway/tls/tls.key"' "${llm_gateway_base[@]}"
assert_renders "TLS by default: no acknowledgement"  '"-allow-plaintext-listener=false"' "${llm_gateway_base[@]}"
assert_renders "TLS by default: the key pair's Secret" '^ +secretName: "shoal-llm-gateway-tls"$' "${llm_gateway_base[@]}"
assert_renders "TLS by default: the port says https" '^ +- name: https$' "${llm_gateway_base[@]}"
refuses_citing "llmGateway.tls.secretName is required" "TLS with no Secret" "${llm_gateway_base[@]}" --set llmGateway.tls.secretName=
refuses_citing "llmGateway.tls.secretName is required" "TLS with a blank Secret" "${llm_gateway_base[@]}" --set-string 'llmGateway.tls.secretName=  '
refuses_citing "llmGateway.tls.secretName is required" "the shipped profile with everything but the Secret" -f "$chart/values-llm-gateway.yaml" "${valid_llm_gateway[@]}" --set llmGateway.tls.secretName=
for value in true false; do
  refuses_citing "llmGateway.tls.enabled must be a boolean" "a string tls.enabled ($value)" "${llm_gateway_base[@]}" --set-string llmGateway.tls.enabled="$value"
done
refuses_citing "llmGateway.tls.enabled must be a boolean" "a null tls.enabled" "${llm_gateway_base[@]}" --set llmGateway.tls.enabled=null
refuses_citing "llmGateway.tls.enabled must be a boolean" "a null tls block" "${llm_gateway_base[@]}" --set llmGateway.tls=null
refuses_citing "holds a control character" "a newline in the TLS Secret" "${llm_gateway_base[@]}" --set-string 'llmGateway.tls.secretName=gw-tls
injected'
# A TLS listener may be exposed: the transport is the process's own.
for service_type in LoadBalancer NodePort; do
  assert_renders "a TLS $service_type gateway" "type: \"$service_type\"" "${llm_gateway_base[@]}" --set llmGateway.service.type="$service_type"
done
refuses_citing "the acknowledgement covers nothing" "the exposure acknowledgement beside TLS" "${llm_gateway_base[@]}" --set llmGateway.service.allowPlaintext=true
# The credential files cannot land on the key pair's mount, under it or above
# it. The near miss renders: a directory that only shares a name prefix.
refuses_citing "where the chart mounts the listener's TLS Secret" "a token file on the TLS mount" "${token_file_base[@]}" --set llmGateway.admission.tokenFile=/etc/shoal-llm-gateway/tls/token
refuses_citing "where the chart mounts the listener's TLS Secret" "a token file above the TLS mount" "${token_file_base[@]}" --set llmGateway.admission.tokenFile=/etc/shoal-llm-gateway/token
refuses_citing "where the chart mounts the listener's TLS Secret" "a key file under the TLS mount" "${llm_gateway_base[@]}" --set llmGateway.upstream.apiKeyFile=/etc/shoal-llm-gateway/tls/upstream/api-key
renders "a token file beside the TLS mount" "${token_file_base[@]}" --set llmGateway.admission.tokenFile=/etc/shoal-llm-gateway-token/token
renders "a token file in a sibling that extends the TLS directory's name" "${token_file_base[@]}" --set llmGateway.admission.tokenFile=/etc/shoal-llm-gateway/tlsx/token
renders "a token file in a sibling whose name the TLS directory extends" "${token_file_base[@]}" --set llmGateway.admission.tokenFile=/etc/shoal-llm-gateway/t/token
renders "a plaintext listener's token file may use that directory" "${token_file_base[@]}" "${plaintext_listener[@]}" --set llmGateway.admission.tokenFile=/etc/shoal-llm-gateway/tls/token

# Plaintext is a choice the values file makes, and the acknowledgement reaches
# the process: without it the binary refuses the 0.0.0.0 bind and the pod
# crash-loops, which is the mismatch -allow-plaintext-admission was added for.
assert_renders "plaintext renders the acknowledgement" '"-allow-plaintext-listener=true"' "${llm_gateway_base[@]}" "${plaintext_listener[@]}"
assert_absent "plaintext mounts no key pair" 'listener-tls|-tls-cert-file|-tls-key-file' "${llm_gateway_base[@]}" "${plaintext_listener[@]}"
assert_renders "plaintext: the port says http" '^ +- name: http$' "${llm_gateway_base[@]}" "${plaintext_listener[@]}"
refuses_citing "nothing mounts or reads it" "a TLS Secret with TLS off" "${llm_gateway_base[@]}" --set llmGateway.tls.enabled=false

# And a plaintext listener stays inside the cluster unless a terminating load
# balancer is acknowledged. That acknowledgement is separate from the admission
# hop and must be a boolean: a quoted "false" is truthy in a Helm condition.
assert_renders "the default internal gateway Service" 'type: "ClusterIP"' "${llm_gateway_base[@]}" "${plaintext_listener[@]}"
for service_type in LoadBalancer NodePort ExternalName; do
  refuses_citing "exposes a plaintext prompt endpoint" "an unacknowledged plaintext $service_type gateway" "${llm_gateway_base[@]}" "${plaintext_listener[@]}" --set llmGateway.service.type="$service_type"
done
for service_type in LoadBalancer NodePort; do
  assert_renders "an acknowledged plaintext $service_type gateway" "type: \"$service_type\"" "${llm_gateway_base[@]}" "${plaintext_listener[@]}" --set llmGateway.service.type="$service_type",llmGateway.service.allowPlaintext=true
done
refuses_citing "exposes a plaintext prompt endpoint" "an explicitly unacknowledged public gateway" "${llm_gateway_base[@]}" "${plaintext_listener[@]}" --set llmGateway.service.type=LoadBalancer,llmGateway.service.allowPlaintext=false
refuses_citing "exposes a plaintext prompt endpoint" "admission acknowledgement does not cover the listener" "${llm_gateway_base[@]}" "${plaintext_listener[@]}" --set llmGateway.service.type=LoadBalancer,llmGateway.admission.allowPlaintext=true
for acknowledgement in true false yes; do
  refuses_citing "llmGateway.service.allowPlaintext must be a boolean" "a string exposure acknowledgement ($acknowledgement)" "${llm_gateway_base[@]}" "${plaintext_listener[@]}" --set llmGateway.service.type=LoadBalancer --set-string llmGateway.service.allowPlaintext="$acknowledgement"
done
refuses_citing "llmGateway.service.allowPlaintext must be a boolean" "a null exposure acknowledgement" "${llm_gateway_base[@]}" --set llmGateway.service.allowPlaintext=null
renders "a disabled gateway does not guard listener exposure" -f "$chart/values.yaml" --set llmGateway.enabled=false,llmGateway.service.type=LoadBalancer
# The acknowledgement is a flag now, because it was a values key that reached
# nothing: the binary refuses a remote http:// admission URL without it, so the
# documented mesh deployment rendered cleanly and produced CrashLoopBackOff.
renders "plaintext admission acknowledged, as a flag" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://shoal-explorer:8098,llmGateway.admission.allowPlaintext=true
# It is rendered verbatim into a boolean flag, so a YAML word that is not a
# boolean is a pod that exits on a flag error.
refuses "a non-boolean plaintext acknowledgement" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://shoal-explorer:8098 --set-string llmGateway.admission.allowPlaintext=yes
# And the upstream hop has no acknowledgement at all: it carries the prompt and
# the provider credential, so a mesh authenticating the hop to the explorer says
# nothing about it. allowPlaintext must not open this one.
refuses "a plaintext remote upstream" "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=http://api.example.test/v1
refuses_citing "is plaintext to a remote provider" "a plaintext remote upstream even with the admission acknowledgement" "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=http://api.example.test/v1,llmGateway.admission.allowPlaintext=true
refuses_citing "is plaintext to a remote provider" "a plaintext remote upstream even with the listener acknowledgements" "${llm_gateway_base[@]}" "${plaintext_listener[@]}" --set llmGateway.upstream.baseURL=http://api.example.test/v1,llmGateway.service.type=LoadBalancer,llmGateway.service.allowPlaintext=true
renders "a loopback provider over http"     "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=http://localhost:11434/v1,llmGateway.upstream.credentialSecretName=

note "== the declared model list =="
# Empty is valid: every model is then reported as "other", which is what keeps
# caller text out of the declaration without configuration.
renders "no models named"        "${llm_gateway_base[@]}"
renders "one model named"        "${llm_gateway_base[@]}" --set 'llmGateway.models={gpt-4o}'
renders "several models named"   "${llm_gateway_base[@]}" --set 'llmGateway.models={gpt-4o,claude-opus-5}'
# Blank entries are dropped rather than rendered, since the list is joined into
# one argument and a comma pair is an empty model name to the binary.
assert_renders "a blank model entry is dropped" "\-model=gpt-4o\"$" "${llm_gateway_base[@]}" --set 'llmGateway.models={gpt-4o, }'
assert_renders "models render as one joined argument" "\-model=gpt-4o,claude-opus-5\"$" "${llm_gateway_base[@]}" --set 'llmGateway.models={gpt-4o,claude-opus-5}'
assert_renders "no models renders an empty flag" "\-model=\"$" "${llm_gateway_base[@]}"

note "== rollout and disruption values are validated as written =="
# These reach the API server verbatim, and their only guard cast to int first —
# which is where the value escapes. int 1.5 is 1 and int -1 is -1, so a guard
# asking "did it cast to zero" approves both: the chart reports success and the
# API server rejects the object at install or upgrade time.
refuses_citing "non-negative whole number" "a fractional rollout surge" "${llm_gateway_base[@]}" --set-json llmGateway.strategy.maxSurge=1.5
refuses_citing "non-negative whole number" "a negative rollout surge" "${llm_gateway_base[@]}" --set llmGateway.strategy.maxSurge=-1
refuses_citing "non-negative whole number" "a fractional rollout maxUnavailable" "${llm_gateway_base[@]}" --set-json llmGateway.strategy.maxUnavailable=1.5
refuses_citing "non-negative whole number" "a negative rollout maxUnavailable" "${llm_gateway_base[@]}" --set llmGateway.strategy.maxUnavailable=-1
refuses_citing "non-negative whole number" "a fractional disruption budget" "${llm_gateway_base[@]}" --set-json llmGateway.podDisruptionBudget.maxUnavailable=1.5
refuses_citing "non-negative whole number" "a negative disruption budget" "${llm_gateway_base[@]}" --set llmGateway.podDisruptionBudget.maxUnavailable=-1
refuses_citing "non-negative whole number" "a nonsense rollout value" "${llm_gateway_base[@]}" --set-string llmGateway.strategy.maxSurge=lots
# Both at zero is accepted by the API server and then never progresses: nothing
# may be added and nothing taken down, so every upgrade hangs with no event
# saying why. A values file that zeroes the surge "to be careful" produces it.
refuses_citing "cannot both be 0" "a rollout that cannot start" "${llm_gateway_base[@]}" --set llmGateway.strategy.maxSurge=0,llmGateway.strategy.maxUnavailable=0
# Percentages are an IntOrString and must still render, as must the shipped
# pair — a guard that refuses the chart's own defaults is the first thing to
# get wrong here, and did: Helm's "default" treats 0 as empty.
renders "percentage rollout values"   "${llm_gateway_base[@]}" --set-string llmGateway.strategy.maxSurge=25%,llmGateway.strategy.maxUnavailable=0%
renders "a percentage disruption budget" "${llm_gateway_base[@]}" --set-string llmGateway.podDisruptionBudget.maxUnavailable=50%
renders "the shipped rollout pair"    "${llm_gateway_base[@]}" --set llmGateway.strategy.maxSurge=1,llmGateway.strategy.maxUnavailable=0
renders "a larger surge"              "${llm_gateway_base[@]}" --set llmGateway.strategy.maxSurge=3,llmGateway.strategy.maxUnavailable=1
# Zero is judged on what was written, so "0%" is zero and "50%" is not. The
# pre-existing guard cast to int first, and int of "50%" is 0 — so a valid
# percentage budget was refused as if it blocked every eviction, which is the
# same cast-before-validating flaw one line further on.
refuses_citing "must not be 0" "a disruption budget of 0%" "${llm_gateway_base[@]}" --set-string llmGateway.podDisruptionBudget.maxUnavailable=0%
refuses_citing "cannot both be 0" "a rollout zeroed as percentages" "${llm_gateway_base[@]}" --set-string llmGateway.strategy.maxSurge=0%,llmGateway.strategy.maxUnavailable=0%

note "== a credential path names a file, not a directory =="
# A trailing slash passes an absolute-path test and is not a file. The chart
# derives the mount from the directory and the projected item from the base, so
# "/var/run/secrets/shoal/" renders that exact flag while the credential lands
# at "/var/run/secrets/shoal/shoal" — the gateway then opens a directory on every
# request, and the pod passes both probes while denying every call.
refuses_citing "must name a file" "a token path with a trailing slash" "${token_file_base[@]}" --set 'llmGateway.admission.tokenFile=/var/run/secrets/shoal/'
# The matching upstream case lives with the upstream key fixture further down:
# these arrays are ordinary shell arrays, so using one above its definition
# expands to nothing and renders a chart with the gateway disabled — which is a
# check that cannot fail, in a script rather than in Go this time.
#
# The same path without the slash still renders, or the guard refuses the
# feature.
renders "a token path naming a file"  "${token_file_base[@]}" --set 'llmGateway.admission.tokenFile=/var/run/secrets/shoal/token'
assert_renders "and the flag matches the mount" "admission-token-file=/var/run/secrets/shoal/token\"$" "${token_file_base[@]}"

note "== no Kubernetes API credential in the prompt-processing pod =="
# This pod needs no API access: it speaks HTTP to the workspace and HTTP to the
# provider, and touches the API server nowhere. The automatic mount put a token
# for its ServiceAccount on the filesystem anyway, so a compromise of the one
# process in this chart that parses arbitrary caller input inherited whatever
# RBAC the account carries — the opposite of the reason the gateway is a separate
# process from the workspace at all.
assert_renders "the automatic token mount is off in the env form" "automountServiceAccountToken: false" "${llm_gateway_base[@]}"
assert_renders "the automatic token mount is off in the file form" "automountServiceAccountToken: false" "${token_file_base[@]}"
assert_renders "the automatic token mount is off with an operator volume" "automountServiceAccountToken: false" "${operator_volume[@]}" --set 'llmGateway.admission.tokenVolume.secret.secretName=shoal-admission-token' --set 'llmGateway.admission.tokenVolume.secret.defaultMode=288' --set 'llmGateway.admission.tokenVolume.secret.items[0].key=token' --set 'llmGateway.admission.tokenVolume.secret.items[0].path=token'
# The pairing that looks like it should conflict and does not: suppressing the
# automatic mount leaves an explicitly declared serviceAccountToken projection
# alone, and the kubelet still mints it. Asserted rather than assumed, because
# if it were wrong the whole projected-token form would be dead on arrival and
# every other check here would still pass.
assert_renders "an explicit projection survives it" "serviceAccountToken:" "${token_file_base[@]}"
assert_renders "and keeps its audience" "audience: \"shoal\"" "${token_file_base[@]}"

note "== both URLs are parsed, not prefix-matched =="
# These guards tested hasPrefix "http://" and hasPrefix "http://localhost",
# which disagreed with the binary in both directions. Every case below is one
# the prefix form got wrong, and each produced a pod the chart had approved.
#
# Scheme present, host absent. The prefix matched, the binary refused it for
# having no host, and the pod went into CrashLoopBackOff.
refuses_citing "with a host" "an admission URL with a scheme and no host" "${llm_gateway_base[@]}" --set llmGateway.admission.url=https://
refuses_citing "with a host" "an upstream URL with a scheme and no host" "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=https://
# A host that merely begins with "localhost". The prefix form read this as
# loopback and exempted a remote plaintext hop from the rule entirely — the
# direction that matters, since it waved through what the rule exists to stop.
refuses_citing "is plaintext to a non-loopback decision plane" "an admission host that only starts with localhost" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://localhost.example:8098
refuses_citing "is plaintext to a remote provider" "an upstream host that only starts with localhost" "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=http://localhost.example:11434/v1,llmGateway.upstream.credentialSecretName=
# A scheme-relative URL has a host and no scheme, and is not absolute.
refuses_citing "with a host" "a scheme-relative admission URL" "${llm_gateway_base[@]}" --set llmGateway.admission.url=//shoal-explorer:8098
refuses_citing "with a host" "a non-HTTP scheme" "${llm_gateway_base[@]}" --set llmGateway.admission.url=ftp://shoal-explorer:8098
# And the loopback forms the binary accepts must all still render, or parsing
# has traded one disagreement for another. isLoopback there is EqualFold on
# "localhost" plus net.IP.IsLoopback, so the whole 127.0.0.0/8 block counts and
# case does not.
renders "a loopback admission URL in upper case" "${llm_gateway_base[@]}" --set llmGateway.admission.url=HTTP://LOCALHOST:8098
renders "an admission URL elsewhere in 127.0.0.0/8" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://127.5.5.5:8098
renders "an IPv6 loopback admission URL" "${llm_gateway_base[@]}" --set 'llmGateway.admission.url=http://[::1]:8098'
renders "an IPv6 loopback upstream" "${llm_gateway_base[@]}" --set 'llmGateway.upstream.baseURL=http://[::1]:11434/v1' --set llmGateway.upstream.credentialSecretName=
renders "an https URL with a port and a path" "${llm_gateway_base[@]}" --set llmGateway.admission.url=https://shoal.example.test:8443/base
# Loopback is matched as an address, not as a "127." prefix. The prefix form
# classified the DNS name 127.example.com as loopback and exempted it from the
# plaintext rule, which the binary then refuses at startup — the same
# chart-approves-what-the-binary-refuses failure the parsing was meant to end.
refuses_citing "is plaintext to a non-loopback decision plane" "a DNS name beginning with 127." "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://127.example.com:8098
refuses_citing "is plaintext to a remote provider" "an upstream DNS name beginning with 127." "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=http://127.example.com:11434/v1,llmGateway.upstream.credentialSecretName=
# Each octet is bounded, because net.ParseIP refuses this and a loose \d{1,3}
# would approve it.
refuses_citing "is plaintext to a non-loopback decision plane" "an octet above 255" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://127.0.0.256:8098
# Go refuses the short form too, so a full dotted quad is required.
refuses_citing "is plaintext to a non-loopback decision plane" "a short-form loopback address" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://127.1:8098
# And the whole block still counts, as net.IP.IsLoopback has it.
renders "the lowest address in 127.0.0.0/8" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://127.0.0.0:8098
renders "the expanded IPv6 loopback spelling" "${llm_gateway_base[@]}" --set 'llmGateway.admission.url=http://[0:0:0:0:0:0:0:1]:8098'

note "== values rendered verbatim are validated as written =="
# Each of these is written into an argument or a port declaration unchanged, so
# a guard that converts before testing passes a value the flag parser or the API
# server then rejects. int64 of 1.5 is a positive 1; int of 8100.5 is 8100.
refuses "a fractional agent generation"     "${llm_gateway_base[@]}" --set llmGateway.identity.agentGeneration=1.5
# --set-string, because --set normalises 010 to 10 and the fixture would then be
# unable to express the condition it is named for. Written as a string it
# renders as 010, which Go parses as octal 8 — a generation nothing registered.
refuses "an agent generation with a leading zero" "${llm_gateway_base[@]}" --set-string llmGateway.identity.agentGeneration=010
refuses "an agent generation past the int64 maximum" "${llm_gateway_base[@]}" --set llmGateway.identity.agentGeneration=9223372036854775808
refuses "a twenty-digit agent generation"   "${llm_gateway_base[@]}" --set llmGateway.identity.agentGeneration=99999999999999999999
renders "the largest int64 agent generation" "${llm_gateway_base[@]}" --set llmGateway.identity.agentGeneration=9223372036854775807
# Cited: a non-numeric port casts to 0 and the range guard refuses it too, so
# only a float can attribute to the whole-number guard — and only --set-json can
# express one.
refuses_citing "llmGateway.containerPort must be a whole number" "a fractional container port" "${llm_gateway_base[@]}" --set-json 'llmGateway.containerPort=8100.5'
refuses_citing "llmGateway.servicePort must be a whole number" "a fractional service port" "${llm_gateway_base[@]}" --set-json 'llmGateway.servicePort=8100.5'

note "== the agent id is an encoding, not a name =="
# The workspace decodes this field and the binary refuses an undecodable value
# at startup. The guard reaches encoding mistakes and not the class: a readable
# name that happens to decode is indistinguishable from a registered ID here.
refuses "an agent id that is not base64url" "${llm_gateway_base[@]}" --set 'llmGateway.identity.agentID=governed/gateway'
refuses "a padded agent id"                 "${llm_gateway_base[@]}" --set 'llmGateway.identity.agentID=Z292ZXJuZWQtcHJveHk='
refuses "a truncated agent id"              "${llm_gateway_base[@]}" --set llmGateway.identity.agentID=Z292ZXJuZWQtcHJveHkBC
# The documented limit, pinned so nobody later claims the guard catches it: a
# display name of exactly the right shape decodes to garbage and renders.
renders "a readable agent id that happens to decode" "${llm_gateway_base[@]}" --set llmGateway.identity.agentID=gateway

note "== the upstream credential file form =="
# The same reasoning as the admission token, applied to the provider key: an
# environment variable cannot be rotated under a running pod, because a
# container's environment is fixed after start. A Secret mounted as a volume is
# updated in place and the per-request read picks it up.
upstream_key_file=(
  "${llm_gateway_base[@]}"
  --set llmGateway.upstream.apiKeyFile=/etc/shoal/upstream/api-key
  --set llmGateway.upstream.apiKeyFileSource=secret
)
renders "the provider key mounted as a file" "${upstream_key_file[@]}"
renders "the upstream file form with apiKeyEnv at its default" "${upstream_key_file[@]}" --set llmGateway.upstream.apiKeyEnv=SHOAL_UPSTREAM_API_KEY
# Blanking the variable is the other way to say "use the file", and it has to
# render: the binary only refuses a *changed* variable name beside a file.
renders "the upstream file form with apiKeyEnv blanked" "${upstream_key_file[@]}" --set llmGateway.upstream.apiKeyEnv=
refuses "both upstream key forms chosen explicitly" "${upstream_key_file[@]}" --set llmGateway.upstream.apiKeyEnv=OTHER_VAR
# A trailing slash is the token path's problem too; see that section. The chart
# derives the mount from the directory and the item from the base, so the flag
# would name a directory while the credential landed inside it.
refuses_citing "must name a file" "a key path with a trailing slash" "${upstream_key_file[@]}" --set 'llmGateway.upstream.apiKeyFile=/var/run/secrets/upstream/'
assert_renders "the key flag matches its mount" "upstream-api-key-file=/etc/shoal/upstream/api-key\"$" "${upstream_key_file[@]}"
refuses "a relative upstream key file"      "${upstream_key_file[@]}" --set llmGateway.upstream.apiKeyFile=upstream/api-key
refuses "an upstream key file at the filesystem root" "${upstream_key_file[@]}" --set llmGateway.upstream.apiKeyFile=/api-key
# Cited: with the source guard gone the chart renders an empty volume source,
# which Helm refuses while parsing its own output.
refuses_citing "apiKeyFileSource must be secret or volume" "an unknown upstream key file source" "${upstream_key_file[@]}" --set llmGateway.upstream.apiKeyFileSource=projected
# Against a loopback provider, so the remote-credential rule above cannot be
# what refuses it: the Secret source with no Secret is its own failure.
refuses "a Secret source for the provider key with no Secret" "${upstream_key_file[@]}" --set llmGateway.upstream.baseURL=http://localhost:11434/v1,llmGateway.upstream.credentialSecretName=
refuses "a Secret source for the provider key with no key" "${upstream_key_file[@]}" --set llmGateway.upstream.credentialSecretKey=
upstream_key_volume=(
  "${llm_gateway_base[@]}"
  --set llmGateway.upstream.credentialSecretName=
  --set llmGateway.upstream.apiKeyFile=/etc/shoal/upstream/api-key
  --set llmGateway.upstream.apiKeyFileSource=volume
)
# The relaxation that makes the volume source worth having: a remote provider
# with no Secret anywhere must render, or a CSI-delivered credential is
# unexpressible and the guard is a guard against the feature.
renders "a remote provider whose key comes from a CSI volume" "${upstream_key_volume[@]}" --set 'llmGateway.upstream.apiKeyVolume.csi.driver=secrets-store.csi.k8s.io' --set 'llmGateway.upstream.apiKeyVolume.csi.readOnly=true'
refuses_citing "apiKeyVolume is required" "an upstream volume source with no volume" "${upstream_key_volume[@]}"
refuses "an upstream volume source that names itself" "${upstream_key_volume[@]}" --set 'llmGateway.upstream.apiKeyVolume.name=my-key' --set 'llmGateway.upstream.apiKeyVolume.csi.driver=secrets-store.csi.k8s.io'
# Both credentials from files is the configuration the mount paths collide in.
both_files=(
  "${token_file_base[@]}"
  --set llmGateway.upstream.apiKeyFile=/etc/shoal/upstream/api-key
  --set llmGateway.upstream.apiKeyFileSource=secret
)
renders "both credentials from files in separate directories" "${both_files[@]}"
refuses "both credentials from files in one directory" "${both_files[@]}" --set llmGateway.upstream.apiKeyFile=/var/run/secrets/shoal/api-key
# Including the case where they are the same file, which is the same refusal
# and the one an operator reaches by copying the path.
refuses "both credentials from the same file" "${both_files[@]}" --set llmGateway.upstream.apiKeyFile=/var/run/secrets/shoal/token

note "== the probe surface is wired end to end =="
# Three separate things have to agree, and the pod never becomes ready if any
# one of them drifts:
#
#   the probes address the health port, not the workspace port, which refuses
#   any Host an allow-list cannot name and so answers 421 to a kubelet;
#   -health-address is actually passed, or nothing listens there;
#   the port it names is the port the probes and the container port declare.
#
# Checking only the first would pass a chart whose probes point at a port no
# process is bound to.
if ! helm template shoal "$chart" "${explorer_base[@]}" | python3 -c '
import sys, yaml

problems = []
for document in yaml.safe_load_all(sys.stdin):
    if not document or document.get("kind") != "StatefulSet":
        continue
    container = document["spec"]["template"]["spec"]["containers"][0]

    probes = ("readinessProbe", "livenessProbe", "startupProbe")
    for probe in probes:
        if probe not in container:
            problems.append(f"{probe} is missing")
            continue
        port = container[probe]["httpGet"]["port"]
        if port != "health":
            problems.append(f"{probe} addresses {port} rather than the health port")

    declared = {port["name"]: port["containerPort"] for port in container["ports"]}
    if "health" not in declared:
        problems.append("the container declares no health port")

    served = [
        argument.split(":")[-1]
        for argument in container["args"]
        if argument.startswith("-health-address=")
    ]
    if not served:
        problems.append(
            "-health-address is not passed, so nothing listens on the port the "
            "probes address and the pod never becomes ready"
        )
    elif "health" in declared and int(served[0]) != int(declared["health"]):
        problems.append(
            f"-health-address serves {served[0]} but the health port is "
            f"{declared['health']}"
        )

for problem in problems:
    print(problem)
raise SystemExit(1 if problems else 0)
'; then
  fail "the explorer probe surface is not wired end to end (see above)"
fi

note "== the llm gateway is wired end to end =="
# Same end-to-end rule as the explorer's check above, plus the second listener
# the gateway has: the one carrying traffic.
#
# The trap this is written against is real and was hit on the explorer: a check
# that asserted only that the probes *name* the health port passed a chart whose
# probes addressed a port nothing was listening on. So every link is asserted
# rather than any one of them:
#
#   the probes address the health port, not the OpenAI-compatible port, which
#   answers 421 to a kubelet addressing the pod by IP;
#   -health-address is passed, and names the port the probes address and the
#   container declares;
#   -listen names the port the container declares as http and the Service
#   targets, or the Service routes to a port nothing serves;
#   the Service does not publish the health port, which is unauthenticated;
#   the scrape annotations name /metrics on the port -health-address serves,
#   which is where the gateway's counters are (#425) — an annotation naming
#   the traffic port is scraped with a pod-IP Host and answered 421 forever;
#   the Service's selector actually matches the pod template's labels, or the
#   Service has no endpoints at all and nothing above matters.
#
# It renders with padding around the configured values on purpose. The host
# gate matches an authority exactly, so " llm.example.test" matches nothing —
# a 421 to every request, produced by a space in a YAML list. Trimming in the
# guard but not in the argument would let the chart pass its own check and
# render exactly that, so the arguments are asserted to come out trimmed.
if ! helm template shoal "$chart" "${llm_gateway_base[@]}" \
  --set 'llmGateway.allowedHosts={ llm.example.test ,, llm-internal.example.test }' \
  --set 'llmGateway.admission.url= https://shoal.example.test ' \
  --set 'llmGateway.identity.capability= chat.completions ' | python3 -c '
import sys, yaml

problems = []
deployment = None
services = []
for document in yaml.safe_load_all(sys.stdin):
    if not document:
        continue
    component = document.get("metadata", {}).get("labels", {}).get(
        "app.kubernetes.io/component")
    if component != "llm-gateway":
        continue
    if document["kind"] == "Deployment":
        deployment = document
    elif document["kind"] == "Service":
        services.append(document)

if deployment is None:
    print("no llm-gateway Deployment was rendered")
    raise SystemExit(1)

# A Deployment, not a StatefulSet: the gateway is stateless, and this is the one
# structural difference from the explorer.
template = deployment["spec"]["template"]
container = template["spec"]["containers"][0]
declared = {port["name"]: port["containerPort"] for port in container["ports"]}
# The listener port is named for what it speaks, so a mesh does not parse a
# TLS handshake as an HTTP request.
listener = "https" if any(
    argument.startswith("-tls-cert-file=") for argument in container["args"]) else "http"

for name in (listener, "health"):
    if name not in declared:
        problems.append(f"the container declares no {name} port")

if "startupProbe" in container:
    problems.append(
        "a startupProbe only delays the first readiness check: the gateway opens "
        "no corpus"
    )
for probe in ("readinessProbe", "livenessProbe"):
    if probe not in container:
        problems.append(f"{probe} is missing")
        continue
    port = container[probe]["httpGet"]["port"]
    if port != "health":
        problems.append(
            f"{probe} addresses {port} rather than the health port, which "
            "answers 421 to a kubelet addressing the pod by its IP"
        )


expected_hosts = "llm.example.test,llm-internal.example.test"
for argument in container["args"]:
    flag, _, value = argument.partition("=")
    if value != value.strip():
        problems.append(
            f"{flag} is rendered as {argument!r}, with whitespace the host gate "
            "and the flag parser both match literally"
        )
    if flag == "-allowed-host" and value != expected_hosts:
        problems.append(
            f"-allowed-host is {value!r}, not {expected_hosts!r}: blank entries "
            "and padding must be dropped, or an authority matched exactly can "
            "never match"
        )

for name in ("env",):
    for entry in container.get(name, []):
        if entry["name"] != entry["name"].strip():
            problems.append(f"env name {entry['name']!r} carries whitespace")


def served(flag):
    values = [
        argument.split("=", 1)[1].rsplit(":", 1)[-1]
        for argument in container["args"]
        if argument.startswith(flag + "=")
    ]
    return values[0] if values else None


for flag, name in (("-health-address", "health"), ("-listen", listener)):
    port = served(flag)
    if port is None:
        problems.append(
            f"{flag} is not passed, so nothing listens on the {name} port"
        )
    elif name in declared and int(port) != int(declared[name]):
        problems.append(
            f"{flag} serves {port} but the {name} port is {declared[name]}"
        )

annotations = template["metadata"].get("annotations") or {}
health_port = served("-health-address")
if (
    annotations.get("prometheus.io/scrape") != "true"
    or annotations.get("prometheus.io/path") != "/metrics"
    or health_port is None
    or annotations.get("prometheus.io/port") != health_port
):
    problems.append(
        "the scrape annotations %r do not name /metrics on the health port %s"
        % (
            {k: v for k, v in annotations.items() if k.startswith("prometheus.io/")},
            health_port,
        )
    )

if not services:
    problems.append("no llm-gateway Service was rendered")
for service in services:
    for port in service["spec"]["ports"]:
        target = port.get("targetPort")
        if target != listener or port.get("name") != listener:
            problems.append(
                "Service port %s (%s) targets %s rather than the %s port"
                % (port["port"], port.get("name"), target, listener)
            )
        if "health" in declared and port["port"] == declared["health"]:
            problems.append(
                "the Service publishes the health port, which is "
                "unauthenticated and exists only for the kubelet"
            )
    selector = service["spec"]["selector"]
    labels = template["metadata"]["labels"]
    missing = {
        key: value for key, value in selector.items() if labels.get(key) != value
    }
    if missing:
        problems.append(
            f"the Service selector {missing} does not match the pod labels, so "
            "it has no endpoints"
        )

for problem in problems:
    print(problem)
raise SystemExit(1 if problems else 0)
'; then
  fail "the llm gateway is not wired end to end (see above)"
fi

note "== each credential reaches the process in exactly one form =="
# No `refuses` case can see any of this. A guard can only refuse a values file;
# it cannot tell whether the pod that *did* render presents the credential the
# operator configured, and every failure below produces a pod that starts,
# passes both probes, and fails on the credential at request time:
#
#   both forms of one credential rendered at once, which the binary refuses at
#   startup, or neither rendered at all;
#   a file form rendered with the Secret still projected into the environment,
#   so which credential is in use depends on the binary precedence rather than
#   on the manifest;
#   a mount path that is not the directory of the path in the flag, or a
#   projected file name that is not its base name — either way the process
#   opens a path nothing put a credential at;
#   two credentials mounted at one path, which the API server refuses;
#   a file mode the container cannot read. This is the one that cost the most
#   thought: the kubelet writes projected and Secret volumes owned by root, the
#   container runs as uid 65532 with every capability dropped, and 0400 with no
#   fsGroup is a credential the only process that needs it cannot open. The
#   symptom is a 401 on every call, which looks exactly like a policy problem;
#   an `env:` key with nothing under it, which is YAML null and not a list;
#   -allow-plaintext-admission not reaching the process, which is the defect
#   that made the documented mesh deployment CrashLoopBackOff.
#
# Every form is checked, because "exactly one" is a claim about all of them.
credential_wiring='
import os, sys, yaml

problems = []
deployment = None
for document in yaml.safe_load_all(sys.stdin):
    if not document:
        continue
    labels = document.get("metadata", {}).get("labels", {})
    if document.get("kind") == "Deployment" and labels.get(
            "app.kubernetes.io/component") == "llm-gateway":
        deployment = document

if deployment is None:
    print("no llm-gateway Deployment was rendered")
    raise SystemExit(1)

pod = deployment["spec"]["template"]["spec"]
container = pod["containers"][0]
security = pod.get("securityContext", {})
fsgroup = security.get("fsGroup")
run_as_group = security.get("runAsGroup")
arguments = dict(
    argument.split("=", 1) for argument in container["args"] if "=" in argument)
if "env" in container and not container["env"]:
    problems.append(
        "env: is rendered with nothing under it, which is YAML null rather than "
        "an empty list, and the API server rejects the pod spec"
    )
environment = {entry["name"]: entry for entry in container.get("env") or []}
mounts = {mount["name"]: mount for mount in container.get("volumeMounts") or []}
volumes = {volume["name"]: volume for volume in pod.get("volumes") or []}

# The acknowledgement has to reach the process, or it acknowledges nothing: the
# binary refuses a remote plaintext admission URL without it, which is the
# CrashLoopBackOff a values key that reached nothing used to produce.
acknowledged = arguments.get("-allow-plaintext-admission")
if acknowledged not in ("true", "false"):
    problems.append(
        f"-allow-plaintext-admission is {acknowledged!r}: it is a boolean flag, "
        "and anything else is a flag error at startup"
    )
plane = arguments.get("-admission-url", "")
host = plane.split("//", 1)[-1].split("/", 1)[0].split(":")[0]
if plane.startswith("http://") and host not in ("127.0.0.1", "localhost", "[") and (
        acknowledged != "true"):
    problems.append(
        f"-admission-url is {plane} and -allow-plaintext-admission is "
        f"{acknowledged}: the binary refuses a remote plaintext decision plane "
        "without the acknowledgement, so this pod exits at startup"
    )

# volume name, file flag, variable flag, what the pod does without the
# credential, and whether the variable form must be projected from a Secret.
# The admission token must be: the workspace has no unauthenticated mode. The
# provider credential need not: a loopback model server takes none.
credentials = [
    ("admission-token", "-admission-token-file", "-admission-token-env",
     "denies every call", True),
    ("upstream-api-key", "-upstream-api-key-file", "-upstream-api-key-env",
     "fails every admitted call after the admission is spent", False),
]
expected_volumes = set()
expected_variables = set()
for name, file_flag, env_flag, consequence, must_project in credentials:
    from_file = arguments.get(file_flag)
    from_env = arguments.get(env_flag)
    if from_file and from_env:
        problems.append(
            f"{file_flag} and {env_flag} are both rendered: the binary refuses "
            "that pair at startup rather than ranking them, so this pod never "
            "serves"
        )
    if not from_file and not from_env:
        problems.append(f"neither {file_flag} nor {env_flag} is rendered")
    if not from_file:
        if from_env:
            expected_variables.add(from_env)
            if must_project and from_env not in environment:
                problems.append(
                    f"{from_env} is named by {env_flag} and projected from "
                    f"nothing, so the gateway {consequence}"
                )
        if name in volumes or name in mounts:
            problems.append(
                f"the {env_flag} form rendered a {name} volume nothing reads")
        continue

    expected_volumes.add(name)
    mount = mounts.get(name)
    if mount is None:
        problems.append(
            f"{file_flag}={from_file} is rendered with no volumeMount, so the "
            f"path does not exist in the container and the gateway {consequence}"
        )
    else:
        mount_path = mount["mountPath"]
        if mount_path != os.path.dirname(from_file):
            problems.append(
                f"{name} is mounted at {mount_path} but {file_flag} names "
                f"{from_file}, so the process opens a path nothing mounts"
            )
        if not mount.get("readOnly"):
            problems.append(f"the {name} mount is writable")

    volume = volumes.get(name)
    if volume is None:
        problems.append(f"the {name} mount references a volume that is not declared")
        continue
    source = {key: value for key, value in volume.items() if key != "name"}
    if len(source) != 1:
        problems.append(f"the {name} volume has {len(source)} sources, not one")
    kind = next(iter(source), None)
    body = source.get(kind, {})

    # The mode belongs to the chart for the sources it renders. An
    # operator-supplied volume is theirs, and the chart can see nothing inside
    # a CSI driver, so it is not checked here.
    mode = body.get("defaultMode")
    if kind in ("projected", "secret") and mode is None:
        problems.append(
            f"the {name} volume sets no defaultMode, so it takes 0644 and says "
            "nothing about who can read the credential"
        )
    elif mode is not None:
        if mode & 0o222:
            problems.append(f"the {name} file is writable (mode {mode:04o})")
        if not mode & 0o044:
            problems.append(
                f"the {name} file is mode {mode:04o}, which only its owner can "
                "read. The kubelet writes it owned by root and the container "
                "runs as uid 65532, so the one process that needs the "
                f"credential cannot open it: the gateway starts, passes every "
                f"probe, and {consequence}"
            )
        elif not mode & 0o004 and fsgroup != run_as_group:
            problems.append(
                f"the {name} file is group-readable (mode {mode:04o}) but "
                f"fsGroup is {fsgroup} and the container runs as group "
                f"{run_as_group}, so the group that can read it is not the "
                "group the process is in"
            )

    # The file name inside the volume has to be the base name in the flag, from
    # whichever side the volume builds it. Checking only the projected form left
    # the Secret form free to mount the credential under another name — the
    # process then opens a path the volume never creates.
    if kind == "secret":
        items = body.get("items") or []
        names = [item.get("path") for item in items]
        if names != [os.path.basename(from_file)]:
            problems.append(
                f"the {name} Secret is mounted as {names} but the flag names "
                f"{os.path.basename(from_file)!r}, so the process opens a path "
                "the volume does not create"
            )
        for item in items:
            if not str(item.get("key", "")).strip():
                problems.append(f"the {name} Secret item names no key")
    if kind == "projected":
        projections = [
            entry["serviceAccountToken"]
            for entry in body.get("sources", [])
            if "serviceAccountToken" in entry
        ]
        if len(projections) != 1:
            problems.append(
                f"{len(projections)} token projections in {name}, not one")
        for projection in projections:
            projected_name = projection.get("path")
            if projected_name != os.path.basename(from_file):
                problems.append(
                    f"the token is projected as {projected_name!r} but the flag "
                    f"names {os.path.basename(from_file)!r}"
                )
            if not str(projection.get("audience", "")).strip():
                problems.append(
                    "the token is projected with no audience, so it is issued "
                    "for the cluster API server and the explorer rejects every "
                    "request"
                )
            expiry = projection.get("expirationSeconds")
            if not isinstance(expiry, int) or expiry < 600:
                problems.append(
                    f"expirationSeconds is {expiry!r}: below the API server "
                    "floor of 600 the pod spec is invalid and no pod is ever "
                    "created"
                )
        if not str(pod.get("serviceAccountName", "")).strip():
            problems.append(
                "a projected token is rendered with no serviceAccountName, so "
                "its subject is the namespace default account, which every "
                "other pod in the namespace can also obtain a token for"
            )

# A Secret projected into a variable no flag names is the invisible credential
# the mutual exclusion exists to prevent: the operator sees two sources in the
# manifest and the binary picks one.
from_secrets = {
    name for name, entry in environment.items()
    if "secretKeyRef" in entry.get("valueFrom", {})
}
unread = sorted(from_secrets - expected_variables)
if unread:
    problems.append(
        f"a Secret is projected into {unread} that no flag names, so the "
        "credential the process uses is not the one the manifest shows"
    )
paths = [mount["mountPath"] for mount in container.get("volumeMounts") or []]
if len(paths) != len(set(paths)):
    problems.append(
        f"two volumes are mounted at one path ({paths}), which the API server "
        "refuses: the Deployment is created and no pod ever is"
    )
# The listener key pair. The chart binds 0.0.0.0, which the binary serves in
# plaintext only with -allow-plaintext-listener=true and refuses beside TLS,
# so exactly one of the two must be rendered or the pod exits at startup.
cert_file = arguments.get("-tls-cert-file")
key_file = arguments.get("-tls-key-file")
plaintext = arguments.get("-allow-plaintext-listener")
if plaintext not in ("true", "false"):
    problems.append(
        f"-allow-plaintext-listener is {plaintext!r}: it is a boolean flag, "
        "rendered explicitly either way")
if bool(cert_file) != bool(key_file):
    problems.append("only one of -tls-cert-file and -tls-key-file is rendered")
if bool(cert_file) == (plaintext == "true"):
    problems.append(
        f"-tls-cert-file is {cert_file!r} and -allow-plaintext-listener is "
        f"{plaintext!r}: the binary refuses a plaintext 0.0.0.0 listener "
        "without the acknowledgement and the acknowledgement beside TLS")
if cert_file:
    expected_volumes.add("listener-tls")
    mount = mounts.get("listener-tls")
    volume = volumes.get("listener-tls", {})
    secret = volume.get("secret") or {}
    if mount is None:
        problems.append("-tls-cert-file is rendered with no listener-tls mount")
    else:
        mount_path = mount["mountPath"]
        for path in (cert_file, key_file):
            if os.path.dirname(path) != mount_path:
                problems.append(
                    f"{path} is not in {mount_path}, where the key pair is mounted")
        if not mount.get("readOnly"):
            problems.append("the listener-tls mount is writable")
    if not secret.get("secretName"):
        problems.append("the listener-tls volume names no Secret")
    names = sorted(item.get("path") for item in secret.get("items") or [])
    if names != sorted(os.path.basename(p) for p in (cert_file, key_file or "")):
        problems.append(
            f"the key pair is mounted as {names}, not as the files the flags name")
    mode = secret.get("defaultMode")
    if mode is None or mode & 0o222 or not mode & 0o040 or fsgroup != run_as_group:
        problems.append(
            f"the key pair is mode {mode!r} with fsGroup {fsgroup}: it must be "
            "group-readable, by the group the process runs as, and not writable")
unused = sorted(set(volumes) - expected_volumes)
if unused:
    problems.append(f"volumes nothing reads: {unused}")
if not expected_volumes and "fsGroup" in security:
    problems.append("an fsGroup is set with no volume to own")
if not container["securityContext"].get("readOnlyRootFilesystem"):
    problems.append("a credential mount came at the cost of a writable root filesystem")

for problem in problems:
    print(problem)
raise SystemExit(1 if problems else 0)
'
wired() {
  local description="$1"; shift
  if ! helm template shoal "$chart" "$@" | python3 -c "$credential_wiring"; then
    fail "the credentials are not wired end to end: $description (see above)"
  fi
}
wired "a projected admission token"           "${token_file_base[@]}"
wired "the admission Secret as a file"        "${secret_file[@]}"
wired "a token file with nothing in the environment" "${loopback_token_file[@]}"
# The operator volume is checked against the same rules as the chart's own
# sources, with an explicit item and mode, because that is what makes the path
# the flag names deterministic. A volume source that leaves either out puts the
# credential wherever the Secret's keys happen to fall, which is the operator's
# business and not something the chart can see — so the fixture says it the way
# the guide tells an operator to.
wired "an operator-supplied admission volume" "${operator_volume[@]}" --set 'llmGateway.admission.tokenVolume.secret.secretName=shoal-admission-token' --set 'llmGateway.admission.tokenVolume.secret.defaultMode=288' --set 'llmGateway.admission.tokenVolume.secret.items[0].key=token' --set 'llmGateway.admission.tokenVolume.secret.items[0].path=token'
wired "the provider key as a file"            "${upstream_key_file[@]}"
wired "both credentials from files"           "${both_files[@]}"
wired "both credentials from the environment" "${llm_gateway_base[@]}"
wired "a loopback provider that needs no credential" "${llm_gateway_base[@]}" --set llmGateway.upstream.baseURL=http://localhost:11434/v1,llmGateway.upstream.credentialSecretName=
wired "an acknowledged plaintext decision plane" "${llm_gateway_base[@]}" --set llmGateway.admission.url=http://shoal-explorer:8098,llmGateway.admission.allowPlaintext=true
wired "a plaintext listener behind a mesh"      "${llm_gateway_base[@]}" "${plaintext_listener[@]}"
wired "a plaintext listener with a token file" "${token_file_base[@]}" "${plaintext_listener[@]}"

note "== a map rendered through toYaml cannot carry YAML of its own (#468) =="
# toYaml was assumed to make whatever it renders safe, and it does not, in two
# ways. A map key holding a newline is emitted as a block that nindent indents
# line by line, so the key ends early: nodeSelector key
# "k<newline>w<newline>    hostNetwork: true<newline>    junk: |" put
# hostNetwork: true in the pod spec. And a value holding LF is emitted as a
# `|-` block with U+2028 and U+2029 written raw inside it; YAML reads those as
# line breaks and nindent does not indent after them, so annotation
# "v<LF>w<U+2028>namespace: kube-system" moved the Service into kube-system.
#
# So every toYaml'd map, for both components, gets each line break as a key and
# as a value, in a configuration that renders that map. Keys are refused every
# character of the class. Values are refused it too, except that the
# annotations and the operator volumes, which legitimately span lines, keep LF,
# CRLF and tab: those stay inside the block, and a multi-line value with them
# must render and parse back to exactly itself with nothing injected. Any
# render, refused or not, is parsed and must contain no injected key.
join_args() { printf '%s\x1f' "$@"; }
if ! SHOAL_EXPLORER="$(join_args "${explorer_base[@]}")" \
  SHOAL_GATEWAY="$(join_args "${llm_gateway_base[@]}")" \
  SHOAL_TOKEN_VOLUME="$(join_args "${operator_volume[@]}" --set llmGateway.admission.tokenVolume.csi.driver=csi.spiffe.io)" \
  SHOAL_STORAGE="$(join_args -f "$chart/values.yaml")" \
  SHOAL_DISTRIBUTED="$(join_args -f "$chart/values-distributed.yaml")" \
  SHOAL_KEY_VOLUME="$(join_args "${upstream_key_volume[@]}" --set llmGateway.upstream.apiKeyVolume.csi.driver=secrets-store.csi.k8s.io)" \
  python3 - "$chart" <<'TOYAML'
import concurrent.futures, json, os, subprocess, sys, tempfile, yaml

chart = sys.argv[1]
bases = {name: [a for a in os.environ["SHOAL_" + name].split("\x1f") if a]
         for name in ("EXPLORER", "GATEWAY", "TOKEN_VOLUME", "KEY_VOLUME", "STORAGE", "DISTRIBUTED")}

# (base, path of the map, whether its values may span lines). A path ending in
# [0] is a one-element list holding the map.
targets = [
    ("EXPLORER", "explorer.service.annotations", True),
    ("EXPLORER", "explorer.nodeSelector", False),
    ("EXPLORER", "explorer.tolerations[0]", False),
    ("EXPLORER", "explorer.affinity", False),
    ("EXPLORER", "explorer.resources.limits", False),
    ("GATEWAY", "llmGateway.service.annotations", True),
    ("GATEWAY", "llmGateway.nodeSelector", False),
    ("GATEWAY", "llmGateway.tolerations[0]", False),
    ("GATEWAY", "llmGateway.affinity", False),
    ("GATEWAY", "llmGateway.resources.limits", False),
    ("STORAGE", "writeTier.resources.limits", False),
    ("DISTRIBUTED", "readFleet.resources.limits", False),
    ("TOKEN_VOLUME", "llmGateway.admission.tokenVolume.csi.volumeAttributes", True),
    ("KEY_VOLUME", "llmGateway.upstream.apiKeyVolume.csi.volumeAttributes", True),
]
breaks = {"LF": "\n", "CR": "\r", "NEL": "\u0085", "LS": " ", "PS": " "}
injected = {"hostNetwork", "namespace", "injected"}

def overlay(path, mapping):
    root = {}
    node = root
    parts = path.split(".")
    for part in parts[:-1]:
        node = node.setdefault(part, {})
    last = parts[-1]
    if last.endswith("[0]"):
        node[last[:-3]] = [mapping]
    else:
        node[last] = mapping
    return root

def render(base, values):
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as handle:
        json.dump(values, handle)
    try:
        return subprocess.run(["helm", "template", "shoal", chart, *bases[base], "-f", handle.name],
                              capture_output=True, text=True)
    finally:
        os.unlink(handle.name)

def injected_keys(text):
    found = set()
    def walk(node):
        if isinstance(node, dict):
            for key, child in node.items():
                if key in injected:
                    found.add(key)
                walk(child)
        elif isinstance(node, list):
            for child in node:
                walk(child)
    for document in yaml.safe_load_all(text):
        walk(document)
    return found

def strings(text):
    found = set()
    def walk(node):
        if isinstance(node, dict):
            for child in node.values():
                walk(child)
        elif isinstance(node, list):
            for child in node:
                walk(child)
        elif isinstance(node, str):
            found.add(node)
    for document in yaml.safe_load_all(text):
        walk(document)
    return found

def cases():
    for base, path, multiline in targets:
        for name, char in breaks.items():
            key = f"k{char}hostNetwork: true"
            yield (base, path, f"{name} in a key", {key: "x"}, f"{path} has a key")
            value = f"v\nw{char}namespace: kube-system" if char != "\n" else "v\n  injected: true"
            allowed = multiline and char == "\n"
            yield (base, path, f"{name} in a value", {"a": value}, None if allowed else f"{path}.a holds")
        yield (base, path, "the issue's nodeSelector key",
               {"k\nw\n    hostNetwork: true\n    junk: |": "x"}, f"{path} has a key")
        if multiline:
            yield (base, path, "LF, CRLF and tab in a value",
                   {"a": "{\r\n\t\"line\": 1\n}\n"}, None)

def check(case):
    base, path, label, mapping, refusal = case
    result = render(base, overlay(path, mapping))
    problems = []
    if result.returncode == 0:
        found = injected_keys(result.stdout)
        if found:
            problems.append(f"{label} under {path} injects {sorted(found)}")
        if refusal:
            problems.append(f"{label} under {path} renders and must be refused")
        else:
            # The value must reach the object as exactly itself.
            if mapping["a"] not in strings(result.stdout):
                problems.append(f"{label} under {path} renders, but not as the value given")
    elif refusal is None:
        problems.append(f"{label} under {path} is refused and must render: "
                        + result.stderr.strip().splitlines()[0][:160])
    elif refusal not in result.stderr:
        problems.append(f"{label} under {path} is refused, but not by the walk: "
                        + result.stderr.strip().splitlines()[0][:160])
    return problems

all_cases = list(cases())
with concurrent.futures.ThreadPoolExecutor(8) as pool:
    problems = [p for found in pool.map(check, all_cases) for p in found]
for problem in problems:
    print("      " + problem)
raise SystemExit(1 if problems or len(all_cases) < 100 else 0)
TOYAML
then
  fail "a toYaml'd map can carry a line break into the manifests, or a valid multi-line value is refused (see above)"
fi
# The list above is only as good as its coverage of the templates, so every
# toYaml in them is found, resolved to the values path it renders, and given a
# line break in a key under that path — on the default values, where the walk
# alone stands between the payload and the manifest. A site this cannot
# resolve fails, so a new toYaml cannot be added without being covered here.
# (The templates' other loops over caller-supplied values are the gateway's
# model and allowed-host lists, which hold strings and are walked as values.)
#
# The scan reads template actions, not lines: each {{ ... }} is joined across
# the lines it spans before it is matched, so a toYaml whose argument is on the
# next line, or inside a multi-line include (dict ...), is still found. `.` is
# resolved through a stack of with/range/if/define ... end blocks, so a `with`
# that has already closed does not lend its value to a later `toYaml .`;
# variables ($explorer := .Values.explorer) are followed; and a helper's `.x`
# is resolved through every include of that helper that passes "x". Anything
# else it cannot resolve is a failure, not a skip.
toyaml_scanner=$(cat <<'SCAN'
import json, os, re, sys

def scan(chart):
    templates = os.path.join(chart, "templates")
    comment = re.compile(r"\{\{-?\s*/\*.*?\*/\s*-?\}\}", re.S)
    action = re.compile(r"\{\{-?(.*?)-?\}\}", re.S)
    sites, includes = [], []
    for name in sorted(os.listdir(templates)):
        text = open(os.path.join(templates, name)).read()
        # Comments are blanked to the same length so line numbers survive.
        text = comment.sub(lambda m: re.sub(r"[^\n]", " ", m.group(0)), text)
        stack, variables = [], {}
        def resolve(expression):
            expression = expression.strip("()")
            if expression.startswith(".Values."):
                return expression[len(".Values."):]
            match = re.match(r"\$(\w+)((?:\.\w+)*)$", expression)
            # A variable bound to a helper's argument ($ssh := .ssh) is not a
            # values path; a toYaml of it is then unresolved, which fails.
            if match and isinstance(variables.get(match.group(1)), str):
                return variables[match.group(1)] + match.group(2)
            if expression == ".":
                # The innermost block that rebinds dot; at the top level, or
                # under a range, dot is nothing a values path names.
                for kind, value in reversed(stack):
                    if kind == "with":
                        return value
                    if kind in ("range", "define"):
                        return None
                return None
            match = re.match(r"\.(\w+)$", expression)
            if match:
                for kind, value in reversed(stack):
                    if kind == "define":
                        return ("helper", value, match.group(1))
            return None
        for found in action.finditer(text):
            body = " ".join(found.group(1).split())
            line = text.count("\n", 0, found.start()) + 1
            site = f"{name}:{line}"
            head = body.split(" ", 1)[0] if body else ""
            if head in ("with", "range", "if", "define", "block"):
                argument = body.split(" ", 1)[1] if " " in body else ""
                if head == "with":
                    stack.append(("with", resolve(argument.split(" ")[0])))
                elif head == "define":
                    stack.append(("define", argument.strip('"')))
                else:
                    stack.append((head, None))
            elif head == "end":
                if stack:
                    stack.pop()
            assignment = re.match(r"\$(\w+) :?= (\S+)$", body)
            if assignment:
                variables[assignment.group(1)] = resolve(assignment.group(2))
            for call in re.finditer(r'include "([\w.]+)" \(dict (.*?)\)(?: \||$)', body):
                for key, value in re.findall(r'"(\w+)" (\S+)', call.group(2)):
                    includes.append((call.group(1), key, resolve(value), site))
            arguments = re.findall(r"toYaml (\S+)", body) + re.findall(r"(\S+) \| toYaml\b", body)
            for argument in arguments:
                sites.append((site, argument, resolve(argument)))
    resolved, unresolved = {}, []
    for site, argument, target in sites:
        if isinstance(target, tuple):
            _, helper, key = target
            paths = [path for name, k, path, _ in includes if name == helper and k == key]
            if not paths or None in paths:
                unresolved.append(f"{site}: toYaml {argument}")
                continue
        elif target is None:
            unresolved.append(f"{site}: toYaml {argument}")
            continue
        else:
            paths = [target]
        for path in paths:
            resolved.setdefault(path, []).append(site)
    return resolved, unresolved

if __name__ == "__main__":
    resolved, unresolved = scan(sys.argv[1])
    print(json.dumps({"resolved": resolved, "unresolved": unresolved}))
SCAN
)
# The scanner is tested before it is trusted: a copy of the chart gains a
# toYaml split across lines, one inside a multi-line include (dict ...), one
# under a `with` that is still open, and a `toYaml .` after a `with` has
# closed. The first three must resolve to their paths and the last must be
# reported as unresolvable rather than borrowing the closed block's value.
scanner_probe="$(mktemp -d)"
cp -R "$chart" "$scanner_probe/shoal"
cat > "$scanner_probe/shoal/templates/zz-scanner-probe.yaml" <<'PROBE'
{{- if false }}
metadata:
  labels:
    {{- toYaml
          .Values.zzSplit | nindent 4 }}
  {{- include "shoal.zzProbe" (dict
        "rendered" (toYaml .Values.zzInclude)
        "other" 1) }}
  {{- with .Values.zzOpen }}
  annotations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.zzClosed }}{{ end }}
  more:
    {{- toYaml . | nindent 4 }}
{{- end }}
PROBE
if ! python3 -c "$toyaml_scanner" "$scanner_probe/shoal" | python3 -c '
import json, sys
report = json.load(sys.stdin)
resolved, unresolved = report["resolved"], report["unresolved"]
problems = []
for path in ("zzSplit", "zzInclude", "zzOpen"):
    if not any(site.startswith("zz-scanner-probe.yaml:") for site in resolved.get(path, [])):
        problems.append(f"the scanner did not resolve the probe site for {path}")
if "zzClosed" in resolved:
    problems.append("the scanner resolved `toYaml .` to a with block that had already closed")
if not any(entry.startswith("zz-scanner-probe.yaml:15:") for entry in unresolved):
    problems.append("the scanner did not report `toYaml .` outside any with block as unresolvable: " + repr(unresolved))
for problem in problems:
    print("      " + problem)
raise SystemExit(1 if problems else 0)
'; then
  fail "the toYaml site scanner misses or misresolves a site (see above)"
fi
rm -rf "$scanner_probe"

# The scan's report goes through a file: the check below is read from stdin.
scanner_report="$(mktemp)"
python3 -c "$toyaml_scanner" "$chart" > "$scanner_report" || printf '{"resolved": {}, "unresolved": ["the scanner itself failed"]}' > "$scanner_report"
if ! python3 - "$chart" "$scanner_report" <<'SITES'
import json, os, subprocess, sys, tempfile, yaml

chart = sys.argv[1]
report = json.load(open(sys.argv[2]))
sources, problems = report["resolved"], []
for entry in report["unresolved"]:
    problems.append(f"{entry}: cannot tell which value this renders; teach the scanner, or the walk cannot be shown to cover it")
if len(sources) < 12:
    problems.append(f"only {len(sources)} toYaml sources found: the scan is broken")
defaults = yaml.safe_load(open(os.path.join(chart, "values.yaml")))

def default_at(path):
    node = defaults
    for part in path.split("."):
        node = node.get(part) if isinstance(node, dict) else None
    return node

for path, sites in sorted(sources.items()):
    if path.split(".")[0] in ("global", "llmProxy"):
        problems.append(f"{path} ({', '.join(sites)}) is rendered but not walked")
        continue
    key = "k\nw\n    hostNetwork: true\n    junk: |"
    parts = path.split(".")
    overlay = node = {}
    for part in parts[:-1]:
        node = node.setdefault(part, {})
    if isinstance(default_at(path), list):
        node[parts[-1]] = [{key: "x"}]
        expected = f"{path}[0] has a key"
    else:
        node[parts[-1]] = {key: "x"}
        expected = f"{path} has a key"
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as handle:
        json.dump(overlay, handle)
    result = subprocess.run(["helm", "template", "shoal", chart, "-f", os.path.join(chart, "values.yaml"),
                             "-f", handle.name], capture_output=True, text=True)
    os.unlink(handle.name)
    if result.returncode == 0 or expected not in result.stderr:
        problems.append(f"a line break in a key under {path} ({', '.join(sites)}) is not refused by the walk")

for problem in problems:
    print("      " + problem)
raise SystemExit(1 if problems else 0)
SITES
then
  fail "a toYaml site in the templates renders a values path the walk does not guard (see above)"
fi
rm -f "$scanner_report"

note "== the guide's worked example still installs =="
# The example in docs/llm-gateway-deploy.md is copied by operators verbatim, and a
# values file in prose is the first thing to rot when a key is renamed or a new
# refusal lands. So it is extracted from the document and rendered, rather than
# trusted.
#
# It is the projected-token example specifically, because that is the one with
# keys the chart refuses to render without — an audience, a ServiceAccount, no
# admission Secret — and the one where a stale document produces a pod that
# passes every probe and denies every call.
guide="$chart/../../../docs/llm-gateway-deploy.md"
if [ -f "$guide" ]; then
  example="$(mktemp)"
  python3 - "$guide" > "$example" <<'EXTRACT'
import re, sys

document = open(sys.argv[1]).read()
heading = "### The values file, complete"
if heading not in document:
    raise SystemExit("the guide no longer has a complete values example")
block = re.search(r"```yaml\n(.*?)```", document[document.index(heading):], re.S)
if not block:
    raise SystemExit("the example after that heading is not a yaml block")
sys.stdout.write(block.group(1))
EXTRACT
  if [ -s "$example" ]; then
    renders "the worked example from docs/llm-gateway-deploy.md" -f "$chart/values-llm-gateway.yaml" -f "$example"
    wired "the worked example from docs/llm-gateway-deploy.md" -f "$chart/values-llm-gateway.yaml" -f "$example"
  else
    fail "could not extract the worked example from docs/llm-gateway-deploy.md"
  fi
  rm -f "$example"
else
  fail "docs/llm-gateway-deploy.md is missing: the worked example cannot be checked"
fi

note "== every rendered name fits the 63-character limit =="
# Kubernetes rejects a name longer than 63 characters, and the explorer's
# headless Service name is the longest the chart derives. A long release name is
# the case that finds it: truncating the finished name is not enough, because
# the suffix is appended after the truncation. Helm caps a release name at 53,
# so this is the worst case an install can actually present.
#
# Both planes are rendered together so every name the chart derives — including
# the gateway's, which hangs off its own bounded stem — is measured in one pass.
long_release="shoal-production-authorized-plane-euw1-cluster-prime"
if ! helm template "$long_release" "$chart" "${explorer_base[@]}" "${valid_llm_gateway[@]}" | python3 -c '
import sys, yaml

problems = []
for document in yaml.safe_load_all(sys.stdin):
    if not document:
        continue
    kind = document.get("kind", "object")
    name = document.get("metadata", {}).get("name", "")
    if len(name) > 63:
        problems.append("%s name is %d characters: %s" % (kind, len(name), name))
    if kind == "StatefulSet":
        service = document["spec"]["serviceName"]
        if len(service) > 63:
            problems.append("serviceName is %d characters: %s" % (len(service), service))

for problem in problems:
    print(problem)
raise SystemExit(1 if problems else 0)
'; then
  fail "a long release name produces a name Kubernetes will reject (see above)"
fi

note "== effects gateway (#391): renders, and nothing without an enabled entry =="
helm lint "$chart" "${effects_gateway_base[@]}" >/dev/null || fail "helm lint with an effects gateway"
renders "an effects gateway" "${effects_gateway_base[@]}"
renders "an effects gateway beside the storage tier" -f "$chart/values.yaml" "${effects_gateway_base[@]}"
assert_absent "a disabled entry renders nothing" 'shoal-gw-' "${effects_gateway_base[@]}" --set 'effectsGateways[0].enabled=false'
assert_absent "the default values render no gateway" 'effects-gateway' -f "$chart/values.yaml"

# The checks below read the rendered objects. One program, told which check to
# run, so each property is a separately named failure.
effects_objects=$(cat <<'OBJECTS'
import json, os, sys, yaml

check = sys.argv[1]
documents = [d for d in yaml.safe_load_all(sys.stdin) if d]
mine = lambda kind: [d for d in documents if d["kind"] == kind and "-gw-" in d["metadata"]["name"]]
deployments = mine("Deployment")
problems = []
if len(deployments) != 1:
    problems.append(f"expected one gateway Deployment, found {len(deployments)}")
    print("\n".join(problems)); raise SystemExit(1)
deployment = deployments[0]
spec = deployment["spec"]
pod = spec["template"]["spec"]
container = pod["containers"][0]
volumes = {v["name"]: v for v in pod.get("volumes", [])}

if check == "singleton":
    if spec.get("replicas") != 1:
        problems.append(f"replicas is {spec.get('replicas')!r}, not 1")
    if spec.get("strategy") != {"type": "Recreate"}:
        problems.append(f"strategy is {spec.get('strategy')!r}, not exactly {{type: Recreate}}")
elif check == "claim":
    claims = mine("PersistentVolumeClaim")
    if len(claims) != 1:
        problems.append(f"expected one unrecorded claim, found {len(claims)}")
    else:
        claim = claims[0]
        if claim["spec"].get("accessModes") != ["ReadWriteOnce"]:
            problems.append(f"the claim's access modes are {claim['spec'].get('accessModes')!r}")
        if claim["metadata"].get("annotations", {}).get("helm.sh/resource-policy") != "keep":
            problems.append("the claim is not kept on uninstall")
        source = volumes.get("unrecorded", {}).get("persistentVolumeClaim", {})
        if source.get("claimName") != claim["metadata"]["name"]:
            problems.append(f"the unrecorded volume is {volumes.get('unrecorded')!r}, not the claim")
    if any("emptyDir" in v for v in volumes.values()):
        problems.append("an emptyDir volume is rendered by default")
    mounts = {m["name"]: m["mountPath"] for m in container.get("volumeMounts", [])}
    directory = next((a.split("=", 1)[1] for a in container["args"] if a.startswith("-unrecorded-dir=")), "")
    if not directory.startswith(mounts.get("unrecorded", "/nowhere") + "/"):
        problems.append(f"-unrecorded-dir={directory} is not on the unrecorded volume ({mounts.get('unrecorded')})")
elif check == "emptydir":
    if "emptyDir" not in volumes.get("unrecorded", {}):
        problems.append("the acknowledged emptyDir is not the unrecorded volume")
    if mine("PersistentVolumeClaim"):
        problems.append("a claim is rendered beside the emptyDir")
elif check == "grace":
    expected = int(os.environ["SHOAL_EXPECTED_GRACE"])
    if pod.get("terminationGracePeriodSeconds") != expected:
        problems.append(f"terminationGracePeriodSeconds is {pod.get('terminationGracePeriodSeconds')!r}, the binary says {expected}")
elif check == "security":
    security = pod.get("securityContext", {})
    for key, want in (("runAsNonRoot", True), ("runAsUser", 65532), ("runAsGroup", 65532)):
        if security.get(key) != want:
            problems.append(f"pod securityContext.{key} is {security.get(key)!r}, not {want!r}")
    if security.get("seccompProfile") != {"type": "RuntimeDefault"}:
        problems.append("the pod's seccomp profile is not RuntimeDefault")
    own = container.get("securityContext", {})
    for key, want in (("runAsNonRoot", True), ("readOnlyRootFilesystem", True), ("allowPrivilegeEscalation", False)):
        if own.get(key) is not want:
            problems.append(f"container securityContext.{key} is {own.get(key)!r}, not {want!r}")
    if own.get("capabilities") != {"drop": ["ALL"]}:
        problems.append(f"capabilities are {own.get('capabilities')!r}, not exactly drop: [ALL]")
    if own.get("privileged"):
        problems.append("the container is privileged")
    if pod.get("automountServiceAccountToken") is not False:
        problems.append("the pod automounts a ServiceAccount token")
    for account in mine("ServiceAccount"):
        if account.get("automountServiceAccountToken") is not False:
            problems.append("the ServiceAccount automounts its token")
    for flag in ("hostNetwork", "hostPID", "hostIPC"):
        if pod.get(flag):
            problems.append(f"{flag} is set")
elif check == "wiring":
    mounts = {m["name"]: m for m in container.get("volumeMounts", [])}
    args = container["args"]
    if args[0] != "run":
        problems.append(f"the first argument is {args[0]!r}, not run")
    for name, flag in (("dispatch-token", "-dispatch-token-file"), ("target-credential", "-target-credential-file"),
                       ("attestation", "-attestation-statement-file"), ("attestation", "-attestation-key-file")):
        values = [a.split("=", 1)[1] for a in args if a.startswith(flag + "=")]
        if name not in volumes:
            if values:
                problems.append(f"{flag} is passed with no {name} volume")
            continue
        if len(values) != 1:
            problems.append(f"{flag} is passed {len(values)} times with a {name} volume")
            continue
        mount = mounts.get(name)
        if not mount or not mount.get("readOnly") or os.path.dirname(values[0]) != mount["mountPath"]:
            problems.append(f"{flag}={values[0]} is not in the read-only {name} mount {mount!r}")
            continue
        volume = volumes[name]
        files = [s["serviceAccountToken"]["path"] for s in volume.get("projected", {}).get("sources", [])] + \
                [item["path"] for item in volume.get("secret", {}).get("items", [])]
        if os.path.basename(values[0]) not in files:
            problems.append(f"{flag} names {os.path.basename(values[0])}, which the {name} volume does not hold ({files})")
    for forbidden in ("-registrar-token", "-heartbeat", "-dispatch-token-env", "-target-credential-env"):
        if any(a.startswith(forbidden) for a in args):
            problems.append(f"{forbidden} is rendered")
    names = {e["name"] for e in container.get("env", [])}
    if any("REGISTRAR" in n or "TOKEN" in n or "CREDENTIAL" in n for n in names):
        problems.append(f"a credential is in the environment: {sorted(names)}")
    routes = [a for a in args if a.startswith("-routes=")]
    configmaps = {d["metadata"]["name"]: d for d in mine("ConfigMap")}
    reference = next((e["valueFrom"]["configMapKeyRef"] for e in container.get("env", []) if e["name"] == "SHOAL_GATEWAY_ROUTES"), None)
    if routes != ["-routes=$(SHOAL_GATEWAY_ROUTES)"] or not reference or reference["name"] not in configmaps:
        problems.append("the route table does not reach -routes from the ConfigMap")
    else:
        table = configmaps[reference["name"]]["data"][reference["key"]]
        import hashlib
        if spec["template"]["metadata"]["annotations"].get("checksum/routes") != hashlib.sha256(table.encode()).hexdigest():
            problems.append("the pod template's checksum is not the route table's")
    ports = {p["name"]: p["containerPort"] for p in container.get("ports", [])}
    health = next((a.rsplit(":", 1)[1] for a in args if a.startswith("-health-address=")), None)
    if str(ports.get("health")) != health:
        problems.append(f"the health port {ports.get('health')} is not -health-address's {health}")
    for probe, path in (("livenessProbe", "/healthz"), ("readinessProbe", "/readyz")):
        get = container.get(probe, {}).get("httpGet", {})
        if get.get("path") != path or get.get("port") != "health":
            problems.append(f"{probe} is {get!r}, not {path} on the health port")
    annotations = spec["template"]["metadata"]["annotations"]
    if annotations.get("prometheus.io/port") != health or annotations.get("prometheus.io/path") != "/metrics":
        problems.append("the metrics annotations do not name /metrics on the health port")
    if mine("Service"):
        problems.append("a Service is rendered for the gateway, which serves nothing but its probes")
elif check == "policy":
    policies = mine("NetworkPolicy")
    if len(policies) != 1:
        problems.append(f"expected one NetworkPolicy, found {len(policies)}")
    else:
        policy = policies[0]["spec"]
        if policy.get("policyTypes") != ["Ingress", "Egress"]:
            problems.append(f"policy types are {policy.get('policyTypes')!r}")
        if policy.get("podSelector", {}).get("matchLabels") != spec["selector"]["matchLabels"]:
            problems.append("the policy does not select the gateway's pod")
        cidrs = [peer["ipBlock"]["cidr"] for rule in policy["egress"] for peer in rule.get("to", []) if "ipBlock" in peer]
        if cidrs != json.loads(os.environ["SHOAL_EXPECTED_CIDRS"]):
            problems.append(f"the target CIDRs are {cidrs}")
        if any(rule.get("to") == [] or ("to" not in rule and any(p.get("port") not in (53,) for p in rule.get("ports", []))) for rule in policy["egress"]):
            problems.append("an egress rule allows every destination on a port other than DNS")
        if policy.get("ingress") != [{"ports": [{"port": "health", "protocol": "TCP"}]}]:
            problems.append(f"ingress is {policy.get('ingress')!r}, not the health port alone")
else:
    problems.append(f"unknown check {check}")
for problem in problems:
    print("      " + problem)
raise SystemExit(1 if problems else 0)
OBJECTS
)
effects_check() {
  local check="$1" description="$2"; shift 2
  local rendered
  if ! rendered=$(helm template shoal "$chart" "$@" 2>&1); then
    fail "should render but was refused: $description"
    return
  fi
  if ! printf '%s\n' "$rendered" | python3 -c "$effects_objects" "$check"; then
    fail "$description (see above)"
  fi
}

note "== effects gateway: one replica, stopped before it is started =="
# The design's one-replica-per-surface rule (#391). A second replica shares the
# executor principal and the unrecorded log: the log's lock refuses it, and two
# workers on one surface can evict each other's claims (#514). Recreate is the
# half of it a rollout needs: a surge pod would mount the same ReadWriteOnce
# log while the old pod drains.
effects_check singleton "the Deployment is one replica, Recreate" "${effects_gateway_base[@]}"
refuses_citing "replicas must be 0 or 1" "two replicas" "${effects_gateway_base[@]}" --set 'effectsGateways[0].replicas=2'
refuses_citing "replicas must be 0 or 1" "three replicas" "${effects_gateway_base[@]}" --set 'effectsGateways[0].replicas=3'
# Zero is how the gateway is stopped: to acknowledge its log, or before a
# rename. kubectl scale would be reverted by the next upgrade.
assert_renders "zero replicas stops the gateway" '^  replicas: 0$' "${effects_gateway_base[@]}" --set 'effectsGateways[0].replicas=0'
refuses_citing "replicas must be 0 or 1" "a fractional replica count" "${effects_gateway_base[@]}" --set-json 'effectsGateways[0].replicas=1.5'
refuses_citing "replicas must be 0 or 1" "a replica count written as a word" "${effects_gateway_base[@]}" --set-string 'effectsGateways[0].replicas=one'

note "== effects gateway: the unrecorded log is on a claim by default =="
effects_check claim "a ReadWriteOnce claim, kept on uninstall, holds -unrecorded-dir" "${effects_gateway_base[@]}"
refuses_citing "which deletes the unrecorded log with the pod" "an emptyDir without the acknowledgement" "${effects_gateway_base[@]}" --set 'effectsGateways[0].unrecorded.storage=emptyDir'
effects_check emptydir "an acknowledged emptyDir" "${effects_gateway_base[@]}" --set 'effectsGateways[0].unrecorded.storage=emptyDir,effectsGateways[0].unrecorded.acceptLossOfUnrecordedReports=true'
assert_renders "an existing claim is mounted" '^ +claimName: "stripe-log"$' "${effects_gateway_base[@]}" --set 'effectsGateways[0].unrecorded.storage=existingClaim,effectsGateways[0].unrecorded.existingClaim=stripe-log'
refuses_citing "existingClaim is required" "an existing claim with no name" "${effects_gateway_base[@]}" --set 'effectsGateways[0].unrecorded.storage=existingClaim'
refuses_citing "unrecorded.storage must be" "an unknown storage kind" "${effects_gateway_base[@]}" --set 'effectsGateways[0].unrecorded.storage=hostPath'
# The warning an operator sees at install. NOTES.txt is rendered only by
# install and upgrade, and Helm contacts the cluster for both even with
# --dry-run=client, so a CI runner with no cluster cannot render it. Its text
# is the named template shoal.effectsGatewayNotes, which NOTES.txt includes and
# nothing else does; a copy of the chart gains one manifest that renders it as
# a quoted scalar, which `helm template` can show with no cluster at all. The
# NOTES.txt half is pinned as well: it must still be that one include.
notes_probe="$(mktemp -d)"
cp -R "$chart" "$notes_probe/shoal"
cat > "$notes_probe/shoal/templates/zz-notes-probe.yaml" <<'PROBE'
notes: {{ include "shoal.effectsGatewayNotes" . | quote }}
PROBE
notes_with() {
  helm template shoal "$notes_probe/shoal" "${effects_gateway_base[@]}" -s templates/zz-notes-probe.yaml "$@" 2>&1
}
if notes=$(notes_with --set 'effectsGateways[0].unrecorded.storage=emptyDir,effectsGateways[0].unrecorded.acceptLossOfUnrecordedReports=true'); then
  grep -qF "WARNING: effects gateway stripe keeps its unrecorded log in an" <<<"$notes" ||
    fail "an acknowledged emptyDir renders no warning in the install notes"
else
  fail "should render but was refused: the install notes for an acknowledged emptyDir"
fi
if notes=$(notes_with); then
  grep -qF "WARNING" <<<"$notes" && fail "the install notes warn about a log that is on a claim"
else
  fail "should render but was refused: the install notes for the fixture"
fi
grep -qF '{{- include "shoal.effectsGatewayNotes" . -}}' "$chart/templates/NOTES.txt" ||
  fail "NOTES.txt no longer includes shoal.effectsGatewayNotes, so the checked text is not what an install prints"
rm -rf "$notes_probe"

note "== effects gateway: the grace period is the binary's =="
# The chart computes terminationGracePeriodSeconds from the timeouts rather
# than asking for it, so changing a timeout cannot leave the grace period
# behind. That is a second copy of GracePeriodSeconds, and this is what keeps
# it one: each pair is rendered and compared with what the real binary prints
# for the same pair. The pairs cross every branch of the formula — three plane
# timeouts below and above the report window, a sub-second remainder that must
# round up, the largest plane timeout a 5m lease admits, and a long renewing
# operation. A change to the formula on either side, the Go constants
# included, fails here.
gateway_binary="$effects_fixtures/shoal-gateway"
repository_root="$(cd "$chart/../../.." && pwd)"
if ! command -v go >/dev/null 2>&1; then
  fail "go is required: the effects gateway's grace period is compared with the binary's"
elif ! (cd "$repository_root" && GOWORK=off go build -o "$gateway_binary" ./cmd/shoal-gateway); then
  fail "cmd/shoal-gateway does not build, so the grace period cannot be compared"
else
  for pair in "3m 10s" "10m 15s" "1m 1s" "2m30s 1500ms" "90500ms 2s" "1h 75s" "1s 100ms" "4m 1666ms" "4m 1667ms"; do
    read -r operation plane <<<"$pair"
    if ! expected=$("$gateway_binary" grace-period -operation-timeout "$operation" -plane-timeout "$plane"); then
      fail "shoal-gateway grace-period refused T=$operation P=$plane"
      continue
    fi
    SHOAL_EXPECTED_GRACE="$expected" effects_check grace "the grace period for T=$operation, P=$plane" \
      "${effects_gateway_base[@]}" --set "effectsGateways[0].timing.renew=true,effectsGateways[0].timing.claimLease=5m,effectsGateways[0].timing.operationTimeout=$operation,effectsGateways[0].timing.planeTimeout=$plane"
  done
  # An operator's value can only raise it.
  SHOAL_EXPECTED_GRACE=400 effects_check grace "a larger grace period is kept" "${effects_gateway_base[@]}" --set 'effectsGateways[0].terminationGracePeriodSeconds=400'
  SHOAL_EXPECTED_GRACE=225 effects_check grace "the minimum itself is accepted" "${effects_gateway_base[@]}" --set 'effectsGateways[0].terminationGracePeriodSeconds=225'
  refuses_citing "is below 225" "a grace period one second short" "${effects_gateway_base[@]}" --set 'effectsGateways[0].terminationGracePeriodSeconds=224'
  refuses_citing "must be a whole, non-negative number" "a fractional grace period" "${effects_gateway_base[@]}" --set-json 'effectsGateways[0].terminationGracePeriodSeconds=300.5'
fi

note "== effects gateway: non-root, read-only, no capabilities =="
effects_check security "the pod and container security contexts" "${effects_gateway_base[@]}"
effects_check security "the security contexts with every optional volume" "${effects_gateway_base[@]}" --set 'effectsGateways[0].attestation.secretName=stripe-attestation,effectsGateways[0].unrecorded.storage=emptyDir,effectsGateways[0].unrecorded.acceptLossOfUnrecordedReports=true'

note "== effects gateway: wiring, probes, and no registrar credential =="
effects_check wiring "a projected executor token and a target Secret" "${effects_gateway_base[@]}"
effects_check wiring "a Secret executor token" "${effects_gateway_base[@]}" --set 'effectsGateways[0].dispatch.tokenSource=secret,effectsGateways[0].dispatch.tokenAudience=,effectsGateways[0].dispatch.credentialSecretName=stripe-dispatch'
effects_check wiring "with attestation" "${effects_gateway_base[@]}" --set 'effectsGateways[0].attestation.secretName=stripe-attestation'
effects_check wiring "a loopback target with no credential" "${effects_gateway_base[@]}" "${loopback_target[@]}"
assert_absent "no gateway argument is a bare scalar" '^ +- -' "${effects_gateway_base[@]}" -s templates/effects-gateway.yaml

note "== effects gateway: the binary accepts what the chart renders =="
# The guards above are the binary's refusals restated; this is the check that
# they and the binary agree. Each rendered Deployment's arguments are run
# against the real binary, with the pod's mounts and environment stood in for
# by local files. Every flag must parse and the unrecorded log must open: the
# run has to reach the descriptor resolve (which fails, against a closed
# loopback port) and not stop at exit 2, a flag the binary refuses.
binary_accepts=$(cat <<'ACCEPTS'
import os, re, subprocess, sys, tempfile, yaml

binary = sys.argv[1]
documents = [d for d in yaml.safe_load_all(sys.stdin) if d]
configmaps = {d["metadata"]["name"]: d["data"] for d in documents if d["kind"] == "ConfigMap"}
deployment = next(d for d in documents if d["kind"] == "Deployment" and "-gw-" in d["metadata"]["name"])
container = deployment["spec"]["template"]["spec"]["containers"][0]
environment = {}
for entry in container.get("env", []):
    source = entry.get("valueFrom", {})
    if "configMapKeyRef" in source:
        reference = source["configMapKeyRef"]
        environment[entry["name"]] = configmaps[reference["name"]][reference["key"]]
    elif source.get("fieldRef", {}).get("fieldPath") == "metadata.name":
        environment[entry["name"]] = "shoal-gw-stripe-0"
scratch = tempfile.mkdtemp()
args = []
for argument in container["args"]:
    argument = re.sub(r"\$\((\w+)\)", lambda m: environment[m.group(1)], argument)
    flag, _, value = argument.partition("=")
    if value.startswith(("/var/run/shoal/", "/var/lib/shoal-gateway/")):
        local = os.path.join(scratch, value.lstrip("/"))
        os.makedirs(os.path.dirname(local), exist_ok=True)
        if flag != "-unrecorded-dir":
            if flag in ("-target-ca-file", "-target-client-cert-file", "-target-client-key-file"):
                # The TLS files are loaded at startup (#662), so they are a
                # real certificate and key: one self-signed pair serves as
                # the CA and as the client certificate.
                pair = os.path.join(scratch, "pair")
                if not os.path.exists(pair + ".crt"):
                    subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256",
                                    "-nodes", "-days", "1", "-subj", "/CN=gateway", "-keyout", pair + ".key",
                                    "-out", pair + ".crt"], check=True, capture_output=True)
                with open(pair + (".key" if flag == "-target-client-key-file" else ".crt")) as source:
                    content = source.read()
                with open(local, "w") as handle:
                    handle.write(content)
            else:
                with open(local, "w") as handle:
                    handle.write("credential")
        argument = f"{flag}={local}"
    if flag == "-health-address":
        argument = "-health-address=127.0.0.1:0"
    args.append(argument)
result = subprocess.run([binary, *args], capture_output=True, text=True, timeout=60)
if result.returncode != 1 or "resolving descriptor" not in result.stderr:
    print(f"      exit {result.returncode}: {result.stderr.strip()[:300]}")
    raise SystemExit(1)
if not os.path.isdir(os.path.join(scratch, "var/lib/shoal-gateway/unrecorded")):
    print("      the unrecorded log's directory was not created")
    raise SystemExit(1)
ACCEPTS
)
accepted() {
  local description="$1"; shift
  if [ ! -x "$gateway_binary" ]; then
    fail "the binary is not built, so the rendered arguments cannot be run: $description"
    return
  fi
  if ! helm template shoal "$chart" "$@" | python3 -c "$binary_accepts" "$gateway_binary"; then
    fail "the binary refuses the arguments the chart renders: $description (see above)"
  fi
}
accepted "the fixture" "${effects_gateway_base[@]}"
accepted "a Secret executor token" "${effects_gateway_base[@]}" --set 'effectsGateways[0].dispatch.tokenSource=secret,effectsGateways[0].dispatch.tokenAudience=,effectsGateways[0].dispatch.credentialSecretName=stripe-dispatch'
accepted "a loopback target with no credential" "${effects_gateway_base[@]}" "${loopback_target[@]}"
accepted "renewal with a long operation" "${effects_gateway_base[@]}" --set 'effectsGateways[0].timing.renew=true,effectsGateways[0].timing.claimLease=60s,effectsGateways[0].timing.operationTimeout=10m,effectsGateways[0].timing.planeTimeout=15s,effectsGateways[0].target.idempotencyRetention=48h'
accepted "the largest response bound" "${effects_gateway_base[@]}" --set 'effectsGateways[0].maxResponseBytes=1048576,effectsGateways[0].maxInFlight=64,effectsGateways[0].pullLimit=256'
accepted "an acknowledged plaintext explorer" "${effects_gateway_base[@]}" --set 'effectsGateways[0].dispatch.url=http://127.0.0.2:1,effectsGateways[0].dispatch.allowPlaintext=true'

note "== effects gateway guards refuse =="
refuses_citing "is plaintext to a remote explorer" "a remote http:// explorer" "${effects_gateway_base[@]}" --set 'effectsGateways[0].dispatch.url=http://shoal-explorer:8098'
refuses_citing "is plaintext to a remote surface" "a remote http:// target" "${effects_gateway_base[@]}" --set 'effectsGateways[0].target.baseURL=http://api.example.test'
refuses_citing "addresses loopback" "a loopback target without allowPrivate" "${effects_gateway_base[@]}" --set 'effectsGateways[0].target.baseURL=http://127.0.0.1:9000,effectsGateways[0].target.credentialSecretName='
refuses_citing "no userinfo, query or fragment" "a target URL with a query" "${effects_gateway_base[@]}" --set 'effectsGateways[0].target.baseURL=https://api.example.test/?k=v'
refuses_citing "credentialSecretName is required for a remote target" "a remote target with no credential" "${effects_gateway_base[@]}" --set 'effectsGateways[0].target.credentialSecretName='
refuses_citing "tokenAudience is required" "a projected token with no audience" "${effects_gateway_base[@]}" --set 'effectsGateways[0].dispatch.tokenAudience='
refuses_citing "at least 600" "a projected token below the API server's floor" "${effects_gateway_base[@]}" --set 'effectsGateways[0].dispatch.tokenExpirationSeconds=599'
refuses_citing "reads no Secret" "a dispatch Secret beside a projected token" "${effects_gateway_base[@]}" --set 'effectsGateways[0].dispatch.credentialSecretName=stale'
refuses_citing "tokenSource must be" "an unknown token source" "${effects_gateway_base[@]}" --set 'effectsGateways[0].dispatch.tokenSource=env'
refuses_citing "must be unpadded base64url" "a display name as the agent ID" "${effects_gateway_base[@]}" --set 'effectsGateways[0].agentID=stripe gateway'
refuses_citing "executorRef must match" "an executor reference outside the charset" "${effects_gateway_base[@]}" --set 'effectsGateways[0].executorRef=stripe ref'
refuses_citing "routes needs at least one route" "no routes" "${effects_gateway_base[@]}" --set 'effectsGateways[0].routes=null'
refuses_citing "idempotencyHeader is required" "a key route with no idempotency header" "${effects_gateway_base[@]}" --set 'effectsGateways[0].target.idempotencyHeader='
refuses_citing "refused when no route uses idempotency" "an idempotency header with no key route" "${effects_gateway_base[@]}" --set 'effectsGateways[0].routes[0].idempotency=unprotected'
refuses_citing "must exceed timing.operationTimeout + 5s" "a retention inside the operation" "${effects_gateway_base[@]}" --set 'effectsGateways[0].target.idempotencyRetention=3m5s'
refuses_citing "must differ from target.authHeader" "one header for the key and the credential" "${effects_gateway_base[@]}" --set 'effectsGateways[0].target.idempotencyHeader=authorization'
refuses_citing "at most 5m" "a lease over MaxActionClaimTTL" "${effects_gateway_base[@]}" --set 'effectsGateways[0].timing.claimLease=6m'
refuses_citing "at most a quarter" "a plane timeout over L/4" "${effects_gateway_base[@]}" --set 'effectsGateways[0].timing.planeTimeout=61s'
refuses_citing "must exceed operationTimeout + 5s + planeTimeout" "a lease inside the operation without renewal" "${effects_gateway_base[@]}" --set 'effectsGateways[0].timing.operationTimeout=4m'
renders "the same lease with renewal" "${effects_gateway_base[@]}" --set 'effectsGateways[0].timing.operationTimeout=4m,effectsGateways[0].timing.renew=true'
refuses_citing "must be a Go duration" "a duration Go would not parse" "${effects_gateway_base[@]}" --set 'effectsGateways[0].timing.operationTimeout=3 minutes'
refuses_citing "maxInFlight must be 1 to 64" "too many claims in flight" "${effects_gateway_base[@]}" --set 'effectsGateways[0].maxInFlight=65'
# The pod runs as uid 65532 with every capability dropped (#423).
for port in 80 443 1023; do
  refuses_citing "healthPort must be 1024 to 65535" "a privileged health port $port" "${effects_gateway_base[@]}" --set "effectsGateways[0].healthPort=$port"
done
renders "a health port at the unprivileged boundary" "${effects_gateway_base[@]}" --set 'effectsGateways[0].healthPort=1024'
refuses_citing "must be a boolean" "renew written as a word" "${effects_gateway_base[@]}" --set-string 'effectsGateways[0].timing.renew=yes'
refuses_citing "networkPolicy.targetCIDRs is required" "a NetworkPolicy with no target CIDRs" "${effects_gateway_base[@]}" --set 'effectsGateways[0].networkPolicy.enabled=true'
refuses_citing "over the 24 the stem budget allows" "a name over the stem budget" "${effects_gateway_base[@]}" --set 'effectsGateways[0].name=abcdefghijklmnopqrstuvwxy'
refuses_citing "must be a DNS label" "an upper-case name" "${effects_gateway_base[@]}" --set 'effectsGateways[0].name=Stripe'
refuses_citing "is also the name of effectsGateways[0]" "two entries with one name" "${effects_gateway_base[@]}" --set 'effectsGateways[1].name=stripe'
refuses_citing "do not match the derived" "a remote target's route declaring {external} alone" "${effects_gateway_base[@]}" --set 'effectsGateways[0].routes[0].effects={external}'
refuses_citing "do not match the derived" "a loopback target's route declaring egress" "${effects_gateway_base[@]}" "${loopback_target[@]}" --set 'effectsGateways[0].routes[0].effects={external,egresses-content}'
refuses_citing "is required" "no executor reference" "${effects_gateway_base[@]}" --set 'effectsGateways[0].executorRef='
# The explorer in the same release must bind the reference with the ceiling the
# target's derived effects need.
refuses_citing "is not in explorer.fleet.externalEgressExecutorRefs" "an explorer that does not bind the gateway's reference" "${explorer_base[@]}" "${effects_gateway_base[@]}"
renders "an explorer that binds it and maps its subject" "${explorer_base[@]}" "${effects_gateway_base[@]}" "${explorer_mapping[@]}"
refuses_citing "is not in explorer.fleet.externalExecutorRefs" "a loopback target bound only for egress" "${explorer_base[@]}" "${effects_gateway_base[@]}" --set 'explorer.fleet.executorRefs={stripe}' --set 'explorer.fleet.externalEgressExecutorRefs={stripe}' "${loopback_target[@]}"
# A line break anywhere in an entry is refused by the walk (#468), and the
# arguments are quoted besides.
refuses_citing "holds" "a newline in the surface name" "${effects_gateway_base[@]}" --set-string 'effectsGateways[0].surfaceName=stripe
            - -allow-plaintext-dispatch=true'
refuses_citing "holds" "a newline in a route" "${effects_gateway_base[@]}" --set-string 'effectsGateways[0].routes[0].path=/v1/x
y'

note "== effects gateway: one principal per worker =="
# Two enabled entries on one executor reference, descriptor, ServiceAccount or
# operator-owned claim are two workers on one principal (#514). With separate
# claims the unrecorded log's lock never sees the second, so the chart is the
# only place it can be refused.
renders "two distinct surfaces" "${two_gateways[@]}"
refuses_citing "share the executorRef" "two entries on one executor reference" "${two_gateways[@]}" --set 'effectsGateways[1].executorRef=stripe'
refuses_citing "share the agentID" "two entries on one descriptor" "${two_gateways[@]}" --set 'effectsGateways[1].agentID=c3RyaXBlLWdhdGV3YXk'
refuses_citing "share the ServiceAccount" "an entry naming another's chart-created account" "${two_gateways[@]}" --set 'effectsGateways[1].serviceAccountName=shoal-gw-stripe'
refuses_citing "share the ServiceAccount" "two entries naming one existing account" "${two_gateways[@]}" --set 'effectsGateways[0].serviceAccountName=gateways,effectsGateways[1].serviceAccountName=gateways'
refuses_citing "share the unrecorded claim" "an existing claim naming another entry's created claim" "${two_gateways[@]}" --set 'effectsGateways[1].unrecorded.storage=existingClaim,effectsGateways[1].unrecorded.existingClaim=shoal-gw-stripe-unrecorded'
refuses_citing "share the unrecorded claim" "two entries on one existing claim" "${two_gateways[@]}" --set 'effectsGateways[0].unrecorded.storage=existingClaim,effectsGateways[0].unrecorded.existingClaim=log,effectsGateways[1].unrecorded.storage=existingClaim,effectsGateways[1].unrecorded.existingClaim=log'
renders "a disabled entry may keep a running entry's identity" "${two_gateways[@]}" --set 'effectsGateways[1].executorRef=stripe,effectsGateways[1].enabled=false'

note "== effects gateway: the explorer in this release mints its credential =="
# The token's audience must be the mapping's, and the projected subject —
# system:serviceaccount:<namespace>:<account> — must map to this entry's
# reference. Otherwise the gateway passes its probes and is refused on every
# pull.
refuses_citing "is not the executor mapping's audience" "a token audience the mapping does not accept" "${explorer_base[@]}" "${effects_gateway_base[@]}" "${explorer_mapping[@]}" --set 'effectsGateways[0].dispatch.tokenAudience=shoal'
refuses_citing "does not name the gateway's subject" "a ServiceAccount the mapping does not name" "${explorer_base[@]}" "${effects_gateway_base[@]}" "${explorer_mapping[@]}" --set 'effectsGateways[0].serviceAccountName=other'
refuses_citing "does not name the gateway's subject" "the subject in another namespace" "${explorer_base[@]}" "${effects_gateway_base[@]}" "${explorer_mapping[@]}" --namespace payments
refuses_citing 'maps "system:serviceaccount:default:shoal-gw-stripe" to "ledger"' "the subject mapped to another reference" "${explorer_base[@]}" "${effects_gateway_base[@]}" "${explorer_mapping[@]}" --set 'explorer.fleet.executorRefs={stripe,ledger}' --set 'explorer.auth.oidc.executorMapping.executors[0].executor_ref=ledger'
refuses_citing "needs explorer.auth.oidc.executorMapping" "no executor mapping at all" "${explorer_base[@]}" "${effects_gateway_base[@]}" --set 'explorer.fleet.executorRefs={stripe}' --set 'explorer.fleet.externalEgressExecutorRefs={stripe}'
renders "a Secret token is not cross-checked" "${explorer_base[@]}" "${effects_gateway_base[@]}" "${explorer_mapping[@]}" --set 'effectsGateways[0].dispatch.tokenSource=secret,effectsGateways[0].dispatch.tokenAudience=,effectsGateways[0].dispatch.credentialSecretName=stripe-dispatch,effectsGateways[0].serviceAccountName=other'

note "== effects gateway: a running entry is not renamed under itself =="
# The lookup guard reads the release's live Deployments, and lookup returns
# nothing under helm template, a client dry run and helm lint. So the guard is
# silent everywhere above, which these assert; it can only be exercised
# against an API server. Given SHOAL_LOOKUP_KUBECONFIG (a throwaway cluster,
# e.g. `kind create cluster --kubeconfig ...`), a Deployment shaped like a
# running gateway is created and `--dry-run=server` must refuse a rename and
# accept the same release once that Deployment is at zero replicas.
renders "a rename renders under helm template, where lookup is empty" "${effects_gateway_base[@]}" --set 'effectsGateways[0].name=stripe2'
if [ -n "${SHOAL_LOOKUP_KUBECONFIG:-}" ]; then
  lookup_namespace="shoal-lookup-$$"
  lookup_kubectl=(kubectl --kubeconfig "$SHOAL_LOOKUP_KUBECONFIG")
  lookup_helm=(helm --kubeconfig "$SHOAL_LOOKUP_KUBECONFIG")
  "${lookup_kubectl[@]}" create namespace "$lookup_namespace" >/dev/null
  lookup_deployment() {
    "${lookup_kubectl[@]}" -n "$lookup_namespace" apply -f - >/dev/null <<LIVE
apiVersion: apps/v1
kind: Deployment
metadata:
  name: shoal-gw-stripe
  # Owned by the release, as an installed gateway's Deployment is, so Helm
  # would adopt it rather than refuse it for its ownership.
  labels: {app.kubernetes.io/managed-by: Helm}
  annotations: {meta.helm.sh/release-name: shoal, meta.helm.sh/release-namespace: $lookup_namespace}
spec:
  replicas: $1
  selector:
    matchLabels: {app.kubernetes.io/name: shoal-gateway, app.kubernetes.io/instance: shoal, app.kubernetes.io/component: effects-gateway, shoal.effects-gateway/name: stripe}
  template:
    metadata:
      labels: {app.kubernetes.io/name: shoal-gateway, app.kubernetes.io/instance: shoal, app.kubernetes.io/component: effects-gateway, shoal.effects-gateway/name: stripe}
    spec:
      containers: [{name: pause, image: registry.k8s.io/pause:3.9}]
LIVE
  }
  lookup_install() {
    "${lookup_helm[@]}" install shoal "$chart" -n "$lookup_namespace" --dry-run=server "${effects_gateway_base[@]}" "$@" 2>&1
  }
  lookup_deployment 1
  if output=$(lookup_install --set 'effectsGateways[0].name=stripe2'); then
    fail "a rename with the old gateway running is accepted by a server dry run"
  elif ! grep -qF "no enabled entry in effectsGateways names it" <<<"$output"; then
    fail "a rename with the old gateway running is refused, but not by the lookup guard"
    printf '%s\n' "$output" | grep -oE 'execution error.*|Error: .*' | head -1 | cut -c1-200 | sed 's/^/      /' || true
  fi
  if output=$(lookup_install --set 'effectsGateways[0].enabled=false'); then
    fail "disabling a running gateway is accepted by a server dry run"
  fi
  lookup_install >/dev/null || fail "the running entry itself is refused by a server dry run"
  lookup_deployment 0
  "${lookup_kubectl[@]}" -n "$lookup_namespace" rollout status deployment/shoal-gw-stripe --timeout=60s >/dev/null 2>&1 || true
  lookup_install --set 'effectsGateways[0].name=stripe2' >/dev/null ||
    fail "a rename after replicas: 0 is refused by a server dry run"
  "${lookup_kubectl[@]}" delete namespace "$lookup_namespace" --wait=false >/dev/null
else
  note "  skipped: the lookup guard needs an API server (set SHOAL_LOOKUP_KUBECONFIG to a throwaway cluster)"
fi

note "== effects gateway: the NetworkPolicy =="
SHOAL_EXPECTED_CIDRS='["203.0.113.0/24"]' effects_check policy "ingress to the health port, egress to DNS, the explorer and the target" "${effects_gateway_base[@]}" --set 'effectsGateways[0].networkPolicy.enabled=true' --set 'effectsGateways[0].networkPolicy.targetCIDRs={203.0.113.0/24}'
assert_renders "the target's port is derived from its URL" '^ +- port: 8443$' "${effects_gateway_base[@]}" --set 'effectsGateways[0].networkPolicy.enabled=true,effectsGateways[0].target.baseURL=https://api.example.test:8443' --set 'effectsGateways[0].networkPolicy.targetCIDRs={203.0.113.0/24}'
assert_absent "no NetworkPolicy unless asked for" 'kind: NetworkPolicy' "${effects_gateway_base[@]}"

note "== effects gateway: every name fits under a long release =="
if ! helm template "$long_release" "$chart" "${effects_gateway_base[@]}" --set 'effectsGateways[0].name=abcdefghijklmnopqrstuvwx' | python3 -c '
import sys, yaml
problems = [f"{d['"'"'kind'"'"']} {d['"'"'metadata'"'"']['"'"'name'"'"']}" for d in yaml.safe_load_all(sys.stdin)
            if d and "-gw-" in d["metadata"]["name"] and len(d["metadata"]["name"]) > 63]
print("\n".join("      " + p for p in problems))
raise SystemExit(1 if problems else 0)
'; then
  fail "a 24-character gateway name under a long release produces a name Kubernetes will reject"
fi

note "== effects-ssh (#662): renders, and nothing without an enabled entry =="
# helm lint renders as release "test-release", whose Service name the gateway
# must then target.
helm lint "$chart" "${ssh_base[@]}" --set 'effectsGateways[0].target.baseURL=https://test-release-shoal-ssh-web.default.svc:8443' >/dev/null ||
  fail "helm lint with effects-ssh"
renders "an effects-ssh entry and its gateway" "${ssh_base[@]}"
assert_absent "a disabled entry renders nothing" 'name: "shoal-ssh-' "${ssh_base[@]}" --set 'effectsSsh[0].enabled=false,effectsGateways[0].networkPolicy.enabled=false'
assert_absent "the default values render no effects-ssh" 'effects-ssh' -f "$chart/values.yaml"

ssh_objects=$(cat <<'OBJECTS'
import hashlib, json, os, sys, yaml

check = sys.argv[1]
documents = [d for d in yaml.safe_load_all(sys.stdin) if d]
mine = lambda kind: [d for d in documents if d["kind"] == kind and "-ssh-" in d["metadata"]["name"]]
problems = []
deployments, jobs, claims = mine("Deployment"), mine("Job"), mine("PersistentVolumeClaim")
policies = {d["metadata"]["name"]: d["spec"] for d in mine("NetworkPolicy")}

def pod_of(workload):
    return workload["spec"]["template"]["spec"]

def flags(container):
    return {a.split("=", 1)[0]: a.split("=", 1)[1] for a in container["args"] if a.startswith("-") and "=" in a}

if check in ("serving", "singleton", "journal", "security", "wiring", "grace", "retention"):
    if len(deployments) != 1 or jobs:
        print(f"      expected one Deployment and no Job, found {len(deployments)} and {len(jobs)}")
        raise SystemExit(1)
    deployment = deployments[0]
    spec, pod = deployment["spec"], pod_of(deployment)
    container = pod["containers"][0]
    args = flags(container)
    volumes = {v["name"]: v for v in pod.get("volumes", [])}
    mounts = {m["name"]: m for m in container.get("volumeMounts", [])}

if check == "serving":
    # One Deployment and no Job, asserted above.
    pass
elif check == "singleton":
    if spec.get("replicas") != 1:
        problems.append(f"replicas is {spec.get('replicas')!r}, not 1")
    if spec.get("strategy") != {"type": "Recreate"}:
        problems.append(f"strategy is {spec.get('strategy')!r}, not exactly {{type: Recreate}}")
    if container["args"][0] != "serve":
        problems.append(f"the container runs {container['args'][0]!r}, not serve")
    if any("init" in a for a in container["args"]) or container.get("command") != ["/usr/local/bin/shoal-effects-ssh"]:
        problems.append("the pod's start runs something other than serve: journal init must never be in it")
    if pod.get("initContainers"):
        problems.append("the pod has init containers")
elif check == "journal":
    if len(claims) != 1:
        problems.append(f"expected one journal claim, found {len(claims)}")
    else:
        claim = claims[0]
        if claim["spec"].get("accessModes") != ["ReadWriteOnce"]:
            problems.append(f"the claim's access modes are {claim['spec'].get('accessModes')!r}")
        if claim["metadata"].get("annotations", {}).get("helm.sh/resource-policy") != "keep":
            problems.append("the claim is not kept on uninstall")
        if volumes.get("journal", {}).get("persistentVolumeClaim", {}).get("claimName") != claim["metadata"]["name"]:
            problems.append("the journal volume is not the claim")
    if any("emptyDir" in v for v in volumes.values()):
        problems.append("an emptyDir volume is rendered")
    if mounts.get("journal", {}).get("mountPath") != args.get("-journal-dir"):
        problems.append(f"-journal-dir={args.get('-journal-dir')} is not the journal mount itself ({mounts.get('journal')})")
    if args.get("-journal-id") != os.environ.get("SHOAL_EXPECTED_ID", "0123456789abcdef0123456789abcdef"):
        problems.append(f"-journal-id is {args.get('-journal-id')!r}")
elif check == "init":
    if deployments:
        problems.append("a Deployment is rendered while journal.id is empty")
    if len(jobs) != 1:
        problems.append(f"expected one journal-init Job, found {len(jobs)}")
    else:
        job = jobs[0]
        annotations = job["metadata"].get("annotations", {})
        if any(k.startswith("helm.sh/hook") for k in annotations):
            problems.append(f"the Job is a hook ({annotations!r}): it must be the claim's ordinary first consumer")
        if not job["metadata"]["name"].startswith("shoal-ssh-web-init-") or len(job["metadata"]["name"]) > 63:
            problems.append(f"the Job is named {job['metadata']['name']!r}")
        pod = pod_of(job)
        container = pod["containers"][0]
        if container["args"][:3] != ["journal", "init", "-print-existing"] or len(container["args"]) != 4:
            problems.append(f"the Job runs {container['args']!r}, not journal init -print-existing -journal-dir")
        directory = container["args"][-1].split("=", 1)[-1]
        mounts = {m["name"]: m["mountPath"] for m in container.get("volumeMounts", [])}
        volumes = {v["name"]: v for v in pod.get("volumes", [])}
        if mounts != {"journal": directory}:
            problems.append(f"the Job mounts {mounts}, not exactly the journal at {directory}")
        if volumes.get("journal", {}).get("persistentVolumeClaim", {}).get("claimName") != (claims[0]["metadata"]["name"] if claims else os.environ.get("SHOAL_EXPECTED_CLAIM")):
            problems.append("the Job's journal is not the entry's claim")
        if pod.get("restartPolicy") != "Never":
            problems.append("the Job's pod restarts in place: a retry is a new pod, counted by backoffLimit")
        if pod.get("automountServiceAccountToken") is not False:
            problems.append("the Job's pod automounts a token")
        own = container.get("securityContext", {})
        if own.get("readOnlyRootFilesystem") is not True or own.get("capabilities") != {"drop": ["ALL"]}:
            problems.append("the Job's container is not read-only with every capability dropped")
        init_policy = next((p for n, p in policies.items() if n.endswith("-init")), None)
        job_labels = job["spec"]["template"]["metadata"]["labels"]
        if init_policy and any(job_labels.get(k) != v for k, v in init_policy.get("podSelector", {}).get("matchLabels", {}).items()):
            problems.append("the init policy does not select the Job's pod")
        if init_policy and not init_policy.get("podSelector", {}).get("matchLabels"):
            problems.append("the init policy's selector is empty")
        if (not init_policy or init_policy.get("policyTypes") != ["Ingress", "Egress"] or init_policy.get("ingress") or init_policy.get("egress")):
            problems.append(f"the Job's pod is not denied all network: {init_policy!r}")
elif check == "security":
    security = pod.get("securityContext", {})
    for key, want in (("runAsNonRoot", True), ("runAsUser", 65532), ("runAsGroup", 65532), ("fsGroup", 65532)):
        if security.get(key) != want:
            problems.append(f"pod securityContext.{key} is {security.get(key)!r}, not {want!r}")
    if security.get("seccompProfile") != {"type": "RuntimeDefault"}:
        problems.append("the pod's seccomp profile is not RuntimeDefault")
    own = container.get("securityContext", {})
    for key, want in (("runAsNonRoot", True), ("readOnlyRootFilesystem", True), ("allowPrivilegeEscalation", False)):
        if own.get(key) is not want:
            problems.append(f"container securityContext.{key} is {own.get(key)!r}, not {want!r}")
    if own.get("capabilities") != {"drop": ["ALL"]}:
        problems.append(f"capabilities are {own.get('capabilities')!r}")
    if pod.get("automountServiceAccountToken") is not False:
        problems.append("the pod automounts a ServiceAccount token")
    for flag in ("hostNetwork", "hostPID", "hostIPC"):
        if pod.get(flag):
            problems.append(f"{flag} is set")
elif check == "wiring":
    expected = json.loads(os.environ["SHOAL_EXPECTED_FILES"])
    for flag, volume in (("-tls-cert-file", "tls"), ("-tls-key-file", "tls"), ("-caller-token-file", "caller-token"),
                         ("-client-ca-file", "client-ca")):
        value = args.get(flag)
        if (flag in expected) != (value is not None):
            problems.append(f"{flag} is {'missing' if value is None else 'rendered'}, expected {'present' if flag in expected else 'absent'}")
            continue
        if value is None:
            if volume in volumes and volume != "tls":
                problems.append(f"the {volume} volume is mounted with no flag reading it")
            continue
        mount = mounts.get(volume)
        if not mount or not mount.get("readOnly") or os.path.dirname(value) != mount["mountPath"]:
            problems.append(f"{flag}={value} is not in the read-only {volume} mount")
            continue
        items = [item["path"] for item in volumes[volume]["secret"].get("items", [])]
        if os.path.basename(value) not in items:
            problems.append(f"{flag} names {os.path.basename(value)}, which the {volume} volume does not hold ({items})")
    identities = [a.split("=", 1)[1] for a in container["args"] if a.startswith("-client-identity=")]
    if identities != expected.get("-client-identity", []):
        problems.append(f"the pinned identities are {identities}")
    configmaps = {d["metadata"]["name"]: d["data"] for d in mine("ConfigMap")}
    config_volume = volumes.get("config", {}).get("configMap", {}).get("name")
    if args.get("-config") != mounts.get("config", {}).get("mountPath", "") + "/commands.json" or config_volume not in configmaps:
        problems.append("-config is not the ConfigMap's commands.json")
    else:
        registry = json.loads(configmaps[config_volume]["commands.json"])
        if spec["template"]["metadata"]["annotations"].get("checksum/commands") != hashlib.sha256(configmaps[config_volume]["commands.json"].encode()).hexdigest():
            problems.append("the pod's checksum is not the registry's")
        keys = {i["path"] for i in volumes["ssh-keys"]["secret"]["items"]}
        for name, command in registry["commands"].items():
            if os.path.dirname(command["private_key_file"]) != mounts["ssh-keys"]["mountPath"] or os.path.basename(command["private_key_file"]) not in keys:
                problems.append(f"command {name}'s key file is not in the ssh-keys volume")
    if container.get("env") or container.get("envFrom"):
        problems.append("the container has environment variables: every credential is a file")
    if args.get("-idempotency-header") != "Idempotency-Key":
        problems.append(f"-idempotency-header is {args.get('-idempotency-header')!r}, not the gateway's")
    services = mine("Service")
    if len(services) != 1 or services[0]["spec"]["selector"] != spec["selector"]["matchLabels"] or services[0]["spec"].get("type") != "ClusterIP":
        problems.append("the Service does not select the pod as a ClusterIP")
    for probe in ("readinessProbe", "livenessProbe"):
        if container.get(probe, {}).get("tcpSocket", {}).get("port") != "https":
            problems.append(f"{probe} is not a TCP probe of the listener")
elif check == "grace":
    if pod.get("terminationGracePeriodSeconds") != int(os.environ["SHOAL_EXPECTED_GRACE"]):
        problems.append(f"terminationGracePeriodSeconds is {pod.get('terminationGracePeriodSeconds')!r}, want {os.environ['SHOAL_EXPECTED_GRACE']}")
    if args.get("-shutdown-grace") != os.environ["SHOAL_EXPECTED_SHUTDOWN"]:
        problems.append(f"-shutdown-grace is {args.get('-shutdown-grace')!r}, want {os.environ['SHOAL_EXPECTED_SHUTDOWN']}")
elif check == "retention":
    if args.get("-journal-retention") != os.environ["SHOAL_EXPECTED_RETENTION"]:
        problems.append(f"-journal-retention is {args.get('-journal-retention')!r}, want {os.environ['SHOAL_EXPECTED_RETENTION']}")
elif check == "policy":
    ssh = policies.get("shoal-ssh-web")
    if not ssh:
        problems.append("no NetworkPolicy for the effects-ssh pod")
    else:
        if ssh.get("policyTypes") != ["Ingress", "Egress"]:
            problems.append(f"policy types are {ssh.get('policyTypes')!r}")
        serving = [d for d in deployments]
        if len(serving) != 1:
            problems.append("no effects-ssh Deployment to hold the policy against")
        else:
            labels = serving[0]["spec"]["template"]["metadata"]["labels"]
            selected = ssh.get("podSelector", {}).get("matchLabels", {})
            if not selected or any(labels.get(k) != v for k, v in selected.items()):
                problems.append(f"the policy's selector {selected} does not select the effects-ssh pod {labels}")
        gateway_labels = {"app.kubernetes.io/name": "shoal-gateway", "app.kubernetes.io/instance": "shoal",
                          "app.kubernetes.io/component": "effects-gateway", "shoal.effects-gateway/name": "web-fleet"}
        if ssh.get("ingress") != [{"from": [{"podSelector": {"matchLabels": gateway_labels}}], "ports": [{"port": "https", "protocol": "TCP"}]}]:
            problems.append(f"ingress is {ssh.get('ingress')!r}, not the gateway's pods on the listener alone")
        want = json.loads(os.environ["SHOAL_EXPECTED_EGRESS"])
        got = []
        for rule in ssh.get("egress", []):
            if "to" not in rule or not rule["to"]:
                problems.append(f"an egress rule has no destination: {rule!r}")
                continue
            if any("namespaceSelector" in peer for peer in rule["to"]):
                peers = rule["to"]
                if [p.get("port") for p in rule.get("ports", [])] != [53, 53] or len(peers) != 1 or \
                        not peers[0]["namespaceSelector"].get("matchLabels") or not peers[0].get("podSelector", {}).get("matchLabels"):
                    problems.append(f"the DNS rule is not port 53 to the DNS pods alone: {rule!r}")
                got.append("dns")
                continue
            if not rule["to"] or any("ipBlock" not in peer for peer in rule["to"]):
                problems.append(f"an egress rule reaches more than address ranges: {rule!r}")
            got.append([[peer["ipBlock"]["cidr"] for peer in rule["to"] if "ipBlock" in peer], [p["port"] for p in rule.get("ports", [])]])
        if got != want:
            problems.append(f"egress is {got}, want {want}")
    gateway = next((p for n, p in {d["metadata"]["name"]: d["spec"] for d in documents if d["kind"] == "NetworkPolicy"}.items() if "-gw-" in n), None)
    if gateway is None:
        problems.append("the gateway's NetworkPolicy is not rendered")
    else:
        targets = [rule for rule in gateway["egress"] if any("podSelector" in peer and peer["podSelector"]["matchLabels"].get("shoal.effects-ssh/name") == "web" for peer in rule.get("to", []))]
        if deployments and targets and targets[0]["to"][0]["podSelector"]["matchLabels"] != deployments[0]["spec"]["selector"]["matchLabels"]:
            problems.append("the gateway's egress selector is not the effects-ssh Deployment's selector")
        if len(targets) != 1 or targets[0]["ports"] != [{"port": 8443, "protocol": "TCP"}]:
            problems.append(f"the gateway's policy does not reach effects-ssh's pods on 8443 alone: {targets!r}")
        if any("ipBlock" in peer for rule in gateway["egress"] for peer in rule.get("to", [])):
            problems.append("the gateway's policy also opens address ranges")
else:
    problems.append(f"unknown check {check}")
for problem in problems:
    print("      " + problem)
raise SystemExit(1 if problems else 0)
OBJECTS
)
ssh_check() {
  local check="$1" description="$2"; shift 2
  local rendered
  if ! rendered=$(helm template shoal "$chart" "$@" 2>&1); then
    fail "should render but was refused: $description"
    printf '%s' "$rendered" | grep -oE 'execution error.*|Error: .*' | head -1 | sed 's/^/      /' || true
    return
  fi
  if ! printf '%s\n' "$rendered" | python3 -c "$ssh_objects" "$check"; then
    fail "$description (see above)"
  fi
}

note "== effects-ssh: one replica, Recreate, serve only =="
ssh_check singleton "the Deployment is one replica, Recreate, and runs serve alone" "${ssh_base[@]}"
refuses_citing "replicas must be 0 or 1" "two replicas" "${ssh_base[@]}" --set 'effectsSsh[0].replicas=2'
refuses_citing "replicas must be 0 or 1" "a fractional replica count" "${ssh_base[@]}" --set-json 'effectsSsh[0].replicas=1.5'
assert_renders "zero replicas stops it" '^  replicas: 0$' "${ssh_base[@]}" --set 'effectsSsh[0].replicas=0'
assert_absent "never RollingUpdate" 'type: RollingUpdate|maxSurge' "${ssh_base[@]}" -s templates/effects-ssh.yaml

note "== effects-ssh: the journal is a pinned, kept, ReadWriteOnce claim =="
ssh_check journal "a kept ReadWriteOnce claim mounted at -journal-dir, with the pinned ID" "${ssh_base[@]}"
SHOAL_EXPECTED_ID=fedcba9876543210fedcba9876543210 ssh_check journal "the pinned ID is the value's" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id=fedcba9876543210fedcba9876543210'
refuses_citing "there is no emptyDir" "an emptyDir journal" "${ssh_base[@]}" --set 'effectsSsh[0].journal.storage=emptyDir'
refuses_citing "journal.storage must be" "an unknown storage kind" "${ssh_base[@]}" --set 'effectsSsh[0].journal.storage=hostPath'
refuses_citing "existingClaim is required" "an existing claim with no name" "${ssh_base[@]}" --set 'effectsSsh[0].journal.storage=existingClaim'
assert_renders "an existing claim is mounted" '^ +claimName: "ssh-journal"$' "${ssh_base[@]}" --set 'effectsSsh[0].journal.storage=existingClaim,effectsSsh[0].journal.existingClaim=ssh-journal'
refuses_citing "32 lower-case hex digits" "a malformed journal ID" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id=0123456789ABCDEF0123456789ABCDEF'
refuses_citing "32 lower-case hex digits" "a journal ID one digit short" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id=0123456789abcdef0123456789abcde'
refuses_citing "share the journal claim" "two entries on one existing claim" "${ssh_base[@]}" \
  --set 'effectsSsh[0].journal.storage=existingClaim,effectsSsh[0].journal.existingClaim=j' \
  --set-json 'effectsSsh[1]={"name":"web2","enabled":true,"gateway":"other","journal":{"storage":"existingClaim","existingClaim":"j"}}' \
  --set-json 'effectsGateways[1]={"name":"other","enabled":true,"agentID":"b3RoZXI","surfaceName":"other","executorRef":"other","dispatch":{"url":"http://127.0.0.1:1","tokenAudience":"a"},"target":{"baseURL":"https://shoal-ssh-web2.default.svc:8443","allowPrivate":true,"credentialSecretName":"c","caSecretName":"ca","idempotencyHeader":"Idempotency-Key","idempotencyRetention":"24h"},"routes":[{"action":"restart-web","method":"POST","path":"/v1/commands/restart-web","effects":["external","egresses-content"],"idempotency":"key"}]}' \
  --set 'effectsSsh[1].tls.secretName=t,effectsSsh[1].callerToken.secretName=t,effectsSsh[1].sshKeys.secretName=k,effectsSsh[1].journal.id=0123456789abcdef0123456789abcdef' \
  --set-json 'effectsSsh[1].commands={"restart-web":{"address":"10.0.4.17:22","hostKeys":["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIRh5PXk+0rm7CbKyH/ViV5hPII8CP+XNiZ58vWMgteW"],"user":"u","privateKeyKey":"k","command":"c","runTimeout":"1m"}}'
refuses_citing "is effectsGateways[0]'s unrecorded log claim" "the journal on the gateway's unrecorded claim" "${ssh_base[@]}" --set 'effectsSsh[0].journal.storage=existingClaim,effectsSsh[0].journal.existingClaim=log,effectsGateways[0].unrecorded.storage=existingClaim,effectsGateways[0].unrecorded.existingClaim=log'

note "== effects-ssh: journal init is a one-off Job, never beside serve =="
# The install's first half: no journal.id renders the claim and the init Job,
# and no Deployment, so nothing serves a journal the chart pinned by itself;
# the operator copies the ID the Job printed into values. With the ID set the
# Job is gone. journal init refuses a claim that already holds a journal.
ssh_check init "journal.id empty renders the Job and no Deployment" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id='
SHOAL_EXPECTED_CLAIM=ssh-journal ssh_check init "the Job initializes an existing claim" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id=,effectsSsh[0].journal.storage=existingClaim,effectsSsh[0].journal.existingClaim=ssh-journal'
assert_absent "a pinned journal renders no Job" 'kind: Job' "${ssh_base[@]}" -s templates/effects-ssh.yaml
# A Job's pod template is immutable, so a changed template must be a new Job.
init_job_name() { helm template shoal "$chart" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id=' "$@" -s templates/effects-ssh.yaml | grep -A2 '^kind: Job$' | sed -n 's/^  name: "\(.*\)"$/\1/p'; }
first_job="$(init_job_name)"
if [ -z "$first_job" ] || [ "$first_job" = "$(init_job_name --set 'effectsSsh[0].image.tag=other')" ] ||
  [ "$first_job" = "$(init_job_name --set 'effectsSsh[0].resources.limits.memory=512Mi')" ] ||
  [ "$first_job" != "$(init_job_name)" ]; then
  fail "the journal-init Job's name does not follow its pod template ($first_job)"
fi
ssh_check serving "a pinned journal renders the Deployment alone" "${ssh_base[@]}"

note "== effects-ssh: non-root, read-only, no capabilities =="
ssh_check security "the pod and container security contexts" "${ssh_base[@]}"

note "== effects-ssh: wiring =="
SHOAL_EXPECTED_FILES='{"-tls-cert-file":1,"-tls-key-file":1,"-caller-token-file":1,"-client-ca-file":1,"-client-identity":["uri:spiffe://cluster.local/ns/default/sa/shoal-gw-web-fleet"]}' \
  ssh_check wiring "token and pinned client certificate" "${ssh_base[@]}"
SHOAL_EXPECTED_FILES='{"-tls-cert-file":1,"-tls-key-file":1,"-caller-token-file":1}' \
  ssh_check wiring "token alone" "${ssh_base[@]}" --set 'effectsSsh[0].clientAuth.caSecretName=,effectsSsh[0].clientAuth.identities=null'
SHOAL_EXPECTED_FILES='{"-tls-cert-file":1,"-tls-key-file":1,"-client-ca-file":1,"-client-identity":["uri:spiffe://cluster.local/ns/default/sa/shoal-gw-web-fleet"]}' \
  ssh_check wiring "pinned client certificate alone" "${ssh_base[@]}" --set 'effectsSsh[0].callerToken.secretName=,effectsGateways[0].target.credentialSecretName='
assert_renders "the gateway verifies effects-ssh with the CA" '"-target-ca-file=/var/run/shoal/target-ca/ca.crt"' "${ssh_base[@]}"
assert_renders "the gateway presents its client certificate" '"-target-client-key-file=/var/run/shoal/target-client/tls.key"' "${ssh_base[@]}"
assert_absent "no effects-ssh argument is a bare scalar" '^ +- -' "${ssh_base[@]}" -s templates/effects-ssh.yaml

note "== effects-ssh: retention is computed against the gateway's =="
SHOAL_EXPECTED_RETENTION=86700000ms ssh_check retention "gateway 24h + 5m skew" "${ssh_base[@]}"
SHOAL_EXPECTED_RETENTION=172860000ms ssh_check retention "gateway 48h + 1m skew" "${ssh_base[@]}" --set 'effectsGateways[0].target.idempotencyRetention=48h,effectsSsh[0].journal.clockSkew=1m'
SHOAL_EXPECTED_RETENTION=259200000ms ssh_check retention "a larger retention is kept" "${ssh_base[@]}" --set 'effectsSsh[0].journal.retention=72h'
SHOAL_EXPECTED_RETENTION=86700000ms ssh_check retention "the minimum itself is accepted" "${ssh_base[@]}" --set 'effectsSsh[0].journal.retention=24h5m'
refuses_citing "is below effectsGateways[0].target.idempotencyRetention" "a retention one second short" "${ssh_base[@]}" --set 'effectsSsh[0].journal.retention=24h4m59s'
refuses_citing "is below effectsGateways[0].target.idempotencyRetention" "a retention equal to the gateway's" "${ssh_base[@]}" --set 'effectsSsh[0].journal.retention=24h'
refuses_citing "clockSkew must be positive" "no clock skew" "${ssh_base[@]}" --set 'effectsSsh[0].journal.clockSkew=0s'

note "== effects-ssh: timing against the gateway =="
SHOAL_EXPECTED_GRACE=160 SHOAL_EXPECTED_SHUTDOWN=150000ms ssh_check grace "grace is the longest command's budget + 10s" "${ssh_base[@]}"
SHOAL_EXPECTED_GRACE=310 SHOAL_EXPECTED_SHUTDOWN=300000ms ssh_check grace "a larger shutdown grace is kept" "${ssh_base[@]}" --set 'effectsSsh[0].shutdownGrace=5m'
SHOAL_EXPECTED_GRACE=400 SHOAL_EXPECTED_SHUTDOWN=150000ms ssh_check grace "a larger termination grace is kept" "${ssh_base[@]}" --set 'effectsSsh[0].terminationGracePeriodSeconds=400'
refuses_citing "is below 150000ms" "a shutdown grace shorter than a command" "${ssh_base[@]}" --set 'effectsSsh[0].shutdownGrace=2m'
refuses_citing "is below 160" "a termination grace below shutdownGrace + 10s" "${ssh_base[@]}" --set 'effectsSsh[0].terminationGracePeriodSeconds=159'
refuses_citing "must be below effectsGateways[0].timing.operationTimeout" "a command that outlives the gateway's attempt" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.runTimeout=2m40s'
refuses_citing "attachWait" "an attach wait as long as the operation" "${ssh_base[@]}" --set 'effectsSsh[0].attachWait=3m'
refuses_citing "runTimeout must be positive and at most 1h" "a run timeout over 1h" "${ssh_base[@]}" --set 'effectsGateways[0].timing.renew=true,effectsGateways[0].timing.operationTimeout=2h,effectsGateways[0].target.idempotencyRetention=48h,effectsSsh[0].commands.restart-web.runTimeout=61m'

note "== effects-ssh: the gateway is wired to this entry =="
refuses_citing "names no entry of effectsGateways" "an unknown gateway" "${ssh_base[@]}" --set 'effectsSsh[0].gateway=nope,effectsGateways[0].networkPolicy.enabled=false'
refuses_citing "is not enabled" "a disabled gateway" "${ssh_base[@]}" --set 'effectsGateways[0].enabled=false'
refuses_citing "is served at" "the gateway pointed elsewhere" "${ssh_base[@]}" --set 'effectsGateways[0].target.baseURL=https://effects-ssh.example.test:8443,effectsGateways[0].networkPolicy.targetCIDRs={203.0.113.0/24}'
refuses_citing "is served at" "the gateway pointed at another namespace" "${ssh_base[@]}" --namespace payments
refuses_citing "target.caSecretName is required with effectsSsh" "a gateway with only the system roots" "${ssh_base[@]}" --set 'effectsGateways[0].target.caSecretName='
refuses_citing "must be /v1/commands/<name>" "a templated route" "${ssh_base[@]}" --set 'effectsGateways[0].routes[0].path=/v1/commands/{name}'
refuses_citing "must be /v1/commands/<name>" "a route to an unregistered command" "${ssh_base[@]}" --set 'effectsGateways[0].routes[0].path=/v1/commands/reboot'
refuses_citing "method must be POST" "a PUT route" "${ssh_base[@]}" --set 'effectsGateways[0].routes[0].method=PUT'
refuses_citing "both serve effects gateway" "two entries for one gateway" "${ssh_base[@]}" --set-json 'effectsSsh[1]={"name":"web2","enabled":true,"gateway":"web-fleet"}' --set 'effectsSsh[1].tls.secretName=t,effectsSsh[1].callerToken.secretName=t,effectsSsh[1].sshKeys.secretName=k' --set-json 'effectsSsh[1].commands={"restart-web":{"address":"10.0.4.17:22","hostKeys":["x"],"user":"u","privateKeyKey":"k","command":"c","runTimeout":"1m"}}' --set 'effectsSsh[1].journal.id=0123456789abcdef0123456789abcdef'
refuses_citing "is also the name of effectsSsh[0]" "two entries with one name" "${ssh_base[@]}" --set-json 'effectsSsh[1]={"name":"web"}'

note "== effects-ssh: caller authentication has both halves =="
refuses_citing "there is no unauthenticated mode" "neither a token nor a client CA" "${ssh_base[@]}" --set 'effectsSsh[0].callerToken.secretName=,effectsSsh[0].clientAuth.caSecretName=,effectsSsh[0].clientAuth.identities=null'
refuses_citing "a CA alone admits every certificate" "a client CA with no pin" "${ssh_base[@]}" --set 'effectsSsh[0].clientAuth.identities=null'
refuses_citing "a pin with nothing to verify" "a pin with no CA" "${ssh_base[@]}" --set 'effectsSsh[0].clientAuth.caSecretName='
refuses_citing "target.clientCertificate.secretName is required" "a pinned identity and a gateway with no certificate" "${ssh_base[@]}" --set 'effectsGateways[0].target.clientCertificate.secretName='
refuses_citing "target.credentialSecretName is required: effectsSsh" "a token and a gateway with no credential" "${ssh_base[@]}" --set 'effectsGateways[0].target.credentialSecretName='
refuses_citing "must be uri:<URI SAN>" "a common-name pin" "${ssh_base[@]}" --set 'effectsSsh[0].clientAuth.identities={cn:gateway}'
refuses_citing "must be uri:<URI SAN>" "a wildcard DNS pin" "${ssh_base[@]}" --set 'effectsSsh[0].clientAuth.identities={dns:*.shoal.svc}'
refuses_citing "must be uri:<URI SAN>" "a short SPKI digest" "${ssh_base[@]}" --set 'effectsSsh[0].clientAuth.identities={spki-sha256:abcd}'
renders "an SPKI pin" "${ssh_base[@]}" --set 'effectsSsh[0].clientAuth.identities={spki-sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef}'
refuses_citing "need an https target.baseURL" "a gateway client certificate over loopback plaintext" "${effects_gateway_base[@]}" "${loopback_target[@]}" --set 'effectsGateways[0].target.clientCertificate.secretName=c'
renders "a remote gateway with a client certificate and no credential" "${effects_gateway_base[@]}" --set 'effectsGateways[0].target.credentialSecretName=,effectsGateways[0].target.clientCertificate.secretName=c'

note "== effects-ssh: the commands =="
refuses_citing "needs at least one command" "no commands" "${ssh_base[@]}" --set 'effectsSsh[0].commands=null'
refuses_citing "is not a command setting" "a misspelled field" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.run_timeout=1m'
refuses_citing "hostKeys must list 1 to 8" "no pinned host key" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.hostKeys=null'
refuses_citing "address must be host:port" "an address with no port" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.address=10.0.4.17'
refuses_citing "privateKeyKey must be a Secret key" "a key path that climbs" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.privateKeyKey=../x'
refuses_citing "holds a control, format or line-break character" "a command with a bidi override" "${ssh_base[@]}" --set-string "effectsSsh[0].commands.restart-web.command=sudo systemctl restart web$(printf '‮')"
refuses_citing "must be one authorized_keys line" "a malformed host key" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.hostKeys={ssh-ed25519}'
refuses_citing "must be one authorized_keys line" "a host key with options" "${ssh_base[@]}" --set-json 'effectsSsh[0].commands.restart-web.hostKeys=["from=\"10.0.0.1\" ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIRh5PXk+0rm7CbKyH/ViV5hPII8CP+XNiZ58vWMgteW"]'
refuses_citing "must be 1 to 64 printable bytes" "a user with a space" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.user=de ploy'
refuses_citing "not starting with .." "a key name Kubernetes reserves" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.privateKeyKey=..data'
for name in web-init web-init-0a1b web-journal web-commands; do
  refuses_citing "ends in a suffix the chart appends" "an entry named $name" "${ssh_base[@]}" --set "effectsSsh[0].name=$name"
done
for bad in :/64 ::::/64 1:2/64 fd00::1:/64 1::2::3/64; do
  refuses_citing "not an IPv4 or IPv6 CIDR" "a malformed IPv6 CIDR $bad" "${ssh_base[@]}" --set "effectsSsh[0].commands.restart-web.cidrs={$bad}"
done
# IPv6 ranges that carry IPv4 (#672 review): mapped, compatible and NAT64,
# in every spelling, are refused rather than rendered into the egress rule.
for embedded in '::ffff:0.0.0.0/96' '::ffff:0:0/96' '::ffff:0.0.0.0/97' '::FFFF:0:0/96' \
    '0:0:0:0:0:ffff:0:0/96' '0000:0000:0000:0000:0000:FFFF:0000:0000/96' '::ffff:10.0.4.17/128' \
    '::0.0.0.0/96' '::/96' '0::1/128' '0:0:1::/32' '64:ff9b::/96' '64:FF9B::0.0.0.0/96' \
    '0064:ff9b:0000::/96' '64:ff9b:1::/48' '64:ff9b::a00:411/128'; do
  refuses_citing "overlaps the IPv4-mapped" "an IPv6 range carrying IPv4: $embedded" "${ssh_base[@]}" --set "effectsSsh[0].commands.restart-web.cidrs={$embedded}"
done
refuses_citing "overlaps the IPv4-mapped" "an IPv4-mapped address literal's derived /128" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.address=[::ffff:10.0.4.17]:22'
renders "an IPv6 /48 outside the embedding ranges" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.cidrs={2001:db8:1::/48,64:ff9c::/32,1::/32}'
renders "an IPv6 address" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.address=[fd00:1::17]:22'
assert_renders "a startup probe covers the journal's replay" '^ +startupProbe:$' "${ssh_base[@]}" -s templates/effects-ssh.yaml
refuses_citing "commands has a command named" "a command name with a slash" "${ssh_base[@]}" --set-json 'effectsSsh[0].commands={"a/b":{}}'
refuses_citing "bytes of live records" "a compaction bound the binary refuses" "${ssh_base[@]}" --set 'effectsSsh[0].journal.maxEntries=1048576,effectsSsh[0].commands.restart-web.maxOutputBytes=262144'
renders "the largest compaction bound the binary allows" "${ssh_base[@]}" --set 'effectsSsh[0].journal.maxEntries=700,effectsSsh[0].commands.restart-web.maxOutputBytes=262144,effectsSsh[0].journal.size=3Gi'
refuses_citing "is below the" "a claim smaller than the journal can grow" "${ssh_base[@]}" --set 'effectsSsh[0].journal.maxEntries=700,effectsSsh[0].commands.restart-web.maxOutputBytes=262144'
refuses_citing "is below the" "the old 1Gi default at the default bounds" "${ssh_base[@]}" --set 'effectsSsh[0].journal.size=1Gi'
renders "a fractional size that is large enough" "${ssh_base[@]}" --set 'effectsSsh[0].journal.size=1.5Gi'
refuses_citing "must be a Kubernetes quantity" "a size with an unknown unit" "${ssh_base[@]}" --set 'effectsSsh[0].journal.size=2GB'

note "== effects-ssh: the NetworkPolicies =="
SHOAL_EXPECTED_EGRESS='[[["10.0.4.17/32"],[22]]]' ssh_check policy "ingress from the gateway only; egress to the registered host only, no DNS for an IP literal" "${ssh_base[@]}"
SHOAL_EXPECTED_EGRESS='["dns",[["10.0.8.0/24"],[2222]]]' ssh_check policy "a host name address: its CIDRs on its port, and DNS" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.address=web-1.internal:2222' --set 'effectsSsh[0].commands.restart-web.cidrs={10.0.8.0/24}'
refuses_citing "cidrs is required with networkPolicy.enabled" "a host name address with no CIDRs" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.address=web-1.internal:22'
for wide in 0.0.0.0/0 1.2.3.4/0 0.0.0.0/00 0.0.0.0/1 10.0.0.0/7 ::0/0 0::/0 ::/0 2001:db8::/31 10.0.0.0/33 fd00::/129; do
  refuses_citing "cidrs holds" "a CIDR the policy must not open: $wide" "${ssh_base[@]}" --set "effectsSsh[0].commands.restart-web.cidrs={$wide}"
done
refuses_citing "cidrs holds" "two half-spaces that open everything" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.cidrs={0.0.0.0/1,128.0.0.0/1}'
refuses_citing "not an IPv4 or IPv6 CIDR" "an octet over 255" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.cidrs={256.0.0.1/32}'
renders "a /8 and an IPv6 /64" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.cidrs={10.0.0.0/8,fd00:1::/64}'
refuses_citing "must both name the cluster's DNS pods" "a host name with an empty DNS selector" "${ssh_base[@]}" --set 'effectsSsh[0].commands.restart-web.address=web-1.internal:22' --set 'effectsSsh[0].commands.restart-web.cidrs={10.0.8.0/24}' --set 'effectsSsh[0].networkPolicy.dns.podLabels=null'
refuses_citing "target.allowPrivate must be true" "a gateway that would refuse the ClusterIP" "${ssh_base[@]}" --set 'effectsGateways[0].target.allowPrivate=false'
refuses_citing "is effectsGateways[0]'s unrecorded log claim" "the journal on the gateway's chart-created unrecorded claim" "${ssh_base[@]}" --set 'effectsSsh[0].journal.storage=existingClaim,effectsSsh[0].journal.existingClaim=shoal-gw-web-fleet-unrecorded'
ssh_check init "the init Job's pod is denied all network" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id='
renders "the gateway's policy needs no target CIDRs for effects-ssh" "${ssh_base[@]}"

note "== effects-ssh: notes =="
ssh_notes_probe="$(mktemp -d)"
cp -R "$chart" "$ssh_notes_probe/shoal"
printf '%s\n' 'notes: {{ include "shoal.effectsSshNotes" . | quote }}' > "$ssh_notes_probe/shoal/templates/zz-ssh-notes-probe.yaml"
if notes=$(helm template shoal "$ssh_notes_probe/shoal" "${ssh_base[@]}" -s templates/zz-ssh-notes-probe.yaml --set 'effectsSsh[0].journal.id=' 2>&1); then
  grep -qE "job/shoal-ssh-web-init-[0-9a-f]{12}([^0-9a-f]|$)" <<<"$notes" || fail "the notes do not tell the operator where the journal ID is printed"
  grep -qF "is NOT serving yet" <<<"$notes" || fail "the notes do not say the entry is not serving"
else
  fail "the notes do not render with an uninitialized journal"
fi
rm -rf "$ssh_notes_probe"
grep -qF '{{- include "shoal.effectsSshNotes" . -}}' "$chart/templates/NOTES.txt" ||
  fail "NOTES.txt no longer includes shoal.effectsSshNotes"

note "== effects-ssh: the binary accepts what the chart renders =="
# Every rendered flag must parse and every startup check pass: the Deployment's
# arguments are run against the real binary with the mounts stood in for by
# local files (a CA and certificate from openssl, an SSH key from ssh-keygen,
# a journal from `journal init`, whose ID replaces the fixture's), and the
# run must reach its start line.
ssh_binary="$effects_fixtures/shoal-effects-ssh"
ssh_accepts=$(cat <<'ACCEPTS'
import json, os, re, subprocess, sys, tempfile, time, yaml

binary = sys.argv[1]
documents = [d for d in yaml.safe_load_all(sys.stdin) if d]
configmaps = {d["metadata"]["name"]: d["data"] for d in documents if d["kind"] == "ConfigMap"}
deployment = next(d for d in documents if d["kind"] == "Deployment" and "-ssh-" in d["metadata"]["name"])
pod = deployment["spec"]["template"]["spec"]
container = pod["containers"][0]
scratch = tempfile.mkdtemp()
local = lambda path: os.path.join(scratch, path.lstrip("/"))
for mount in container["volumeMounts"]:
    os.makedirs(local(mount["mountPath"]), exist_ok=True)
config_map = next(v["configMap"]["name"] for v in pod["volumes"] if v["name"] == "config")
registry = configmaps[config_map]["commands.json"].replace('"/etc/effects-ssh/', '"' + scratch + "/etc/effects-ssh/")
open(local("/etc/effects-ssh/config/commands.json"), "w").write(registry)
for command in json.loads(registry)["commands"].values():
    if not os.path.exists(command["private_key_file"]):
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", command["private_key_file"]], check=True)
tls = local("/etc/effects-ssh/tls")
subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes", "-days", "1",
                "-subj", "/CN=effects-ssh", "-keyout", tls + "/tls.key", "-out", tls + "/tls.crt"], check=True, capture_output=True)
if os.path.isdir(local("/etc/effects-ssh/client-ca")):
    open(local("/etc/effects-ssh/client-ca/ca.crt"), "w").write(open(tls + "/tls.crt").read())
if os.path.isdir(local("/var/run/effects-ssh/token")):
    open(local("/var/run/effects-ssh/token/token"), "w").write("t" * 40)
journal = local("/var/lib/effects-ssh")
identity = subprocess.run([binary, "journal", "init", "-journal-dir", journal], capture_output=True, text=True, check=True).stdout.strip()
args = []
for argument in container["args"]:
    flag, _, value = argument.partition("=")
    if value.startswith(("/etc/effects-ssh/", "/var/run/effects-ssh/", "/var/lib/effects-ssh")):
        argument = f"{flag}={local(value)}"
    if flag == "-listen":
        argument = "-listen=127.0.0.1:0"
    if flag == "-journal-id":
        argument = f"-journal-id={identity}"
    args.append(argument)
process = subprocess.Popen([binary, *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
started = False
deadline = time.time() + 20
line = ""
while time.time() < deadline:
    line = process.stderr.readline()
    if '"event":"start"' in line:
        started = True
        break
    if not line and process.poll() is not None:
        break
process.terminate()
try:
    process.wait(timeout=20)
except subprocess.TimeoutExpired:
    process.kill()
if not started:
    print(f"      exit {process.returncode}: {line.strip()[:300]} {process.stderr.read().strip()[:300]}")
    raise SystemExit(1)
if process.returncode != 0:
    print(f"      the binary did not stop cleanly on SIGTERM: exit {process.returncode}")
    raise SystemExit(1)
ACCEPTS
)
ssh_accepted() {
  local description="$1"; shift
  if [ ! -x "$ssh_binary" ]; then
    fail "shoal-effects-ssh is not built, so the rendered arguments cannot be run: $description"
    return
  fi
  if ! helm template shoal "$chart" "$@" | python3 -c "$ssh_accepts" "$ssh_binary"; then
    fail "shoal-effects-ssh refuses the arguments the chart renders: $description (see above)"
  fi
}
if ! command -v openssl >/dev/null 2>&1 || ! command -v ssh-keygen >/dev/null 2>&1; then
  fail "openssl and ssh-keygen are required to run the rendered effects-ssh arguments"
elif ! (cd "$repository_root/extensions/effects-ssh" && GOWORK=off go build -o "$ssh_binary" ./cmd/shoal-effects-ssh); then
  fail "extensions/effects-ssh does not build, so the rendered arguments cannot be run"
else
  ssh_accepted "token and pinned client certificate" "${ssh_base[@]}"
  ssh_accepted "token alone" "${ssh_base[@]}" --set 'effectsSsh[0].clientAuth.caSecretName=,effectsSsh[0].clientAuth.identities=null'
  ssh_accepted "pinned client certificate alone" "${ssh_base[@]}" --set 'effectsSsh[0].callerToken.secretName=,effectsGateways[0].target.credentialSecretName='
  ssh_accepted "longer bounds" "${ssh_base[@]}" --set 'effectsSsh[0].journal.retention=72h,effectsSsh[0].shutdownGrace=5m,effectsSsh[0].maxConcurrent=64,effectsSsh[0].journal.maxEntries=700,effectsSsh[0].commands.restart-web.maxOutputBytes=262144,effectsSsh[0].journal.size=3Gi,effectsSsh[0].commands.restart-web.connectTimeout=20s'
  accepted "the gateway with a CA and a client certificate" "${ssh_base[@]}"
  # The init Job's arguments, run twice on one directory: the first run
  # creates the journal and prints its identity, the second prints the same
  # identity and writes nothing.
  if ! helm template shoal "$chart" "${ssh_base[@]}" --set 'effectsSsh[0].journal.id=' | python3 -c '
import os, subprocess, sys, tempfile, yaml
binary = sys.argv[1]
job = next(d for d in yaml.safe_load_all(sys.stdin) if d and d["kind"] == "Job")
container = job["spec"]["template"]["spec"]["containers"][0]
scratch = tempfile.mkdtemp()
args = []
for argument in container["args"]:
    flag, _, value = argument.partition("=")
    if flag == "-journal-dir":
        argument = f"{flag}={scratch}"
    args.append(argument)
first = subprocess.run([binary, *args], capture_output=True, text=True)
before = open(os.path.join(scratch, "journal.log"), "rb").read() if first.returncode == 0 else b""
second = subprocess.run([binary, *args], capture_output=True, text=True)
after = open(os.path.join(scratch, "journal.log"), "rb").read() if second.returncode == 0 else b"?"
if first.returncode or second.returncode or not first.stdout.strip() or first.stdout != second.stdout or before != after:
    print(f"      first: {first.returncode} {first.stdout!r} {first.stderr.strip()[:200]}; second: {second.returncode} {second.stdout!r} {second.stderr.strip()[:200]}")
    raise SystemExit(1)
' "$ssh_binary"; then
    fail "the journal-init Job's arguments do not initialize once and then print the same identity"
  fi
fi

note "== the effects gateway guide's worked example still installs =="
effects_guide="$chart/../../../docs/effects-gateway-deploy.md"
if [ -f "$effects_guide" ]; then
  python3 - "$effects_guide" > "$effects_fixtures/example.yaml" <<'EXTRACT'
import re, sys

document = open(sys.argv[1]).read()
heading = "### A complete entry"
if heading not in document:
    raise SystemExit("the guide no longer has a complete entry")
block = re.search(r"```yaml\n(.*?)```", document[document.index(heading):], re.S)
if not block:
    raise SystemExit("the example after that heading is not a yaml block")
sys.stdout.write(block.group(1))
EXTRACT
  if [ -s "$effects_fixtures/example.yaml" ]; then
    renders "the worked example from docs/effects-gateway-deploy.md" -f "$effects_fixtures/example.yaml"
    effects_check claim "the worked example keeps its log on a claim" -f "$effects_fixtures/example.yaml"
    effects_check security "the worked example's security contexts" -f "$effects_fixtures/example.yaml"
  else
    fail "could not extract the worked example from docs/effects-gateway-deploy.md"
  fi
else
  fail "docs/effects-gateway-deploy.md is missing: the worked example cannot be checked"
fi

if [ "$failures" -ne 0 ]; then
  note ""
  note "$failures check(s) failed"
  exit 1
fi
note ""
note "all chart checks passed"
