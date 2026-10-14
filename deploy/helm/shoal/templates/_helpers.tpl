{{- define "shoal.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "shoal.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "shoal.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "shoal.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version | replace "+" "_" }}
{{- /* Quoted only when overridden: an override such as "1.5" or "True" is
       otherwise read as a number or boolean, which a label cannot be, and the
       chart's own name renders exactly as it always has. */}}
app.kubernetes.io/name: {{ if .Values.nameOverride }}{{ include "shoal.name" . | quote }}{{ else }}{{ include "shoal.name" . }}{{ end }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: shoal-platform
{{- end -}}

{{- define "shoal.writeTierEnabled" -}}
{{- if kindIs "bool" .Values.writeTier.enabled -}}
{{- .Values.writeTier.enabled -}}
{{- else -}}
{{- or (eq .Values.mode "single") (eq .Values.mode "distributed") -}}
{{- end -}}
{{- end -}}

{{- define "shoal.readFleetEnabled" -}}
{{- if kindIs "bool" .Values.readFleet.enabled -}}
{{- .Values.readFleet.enabled -}}
{{- else -}}
{{- or (eq .Values.mode "distributed") (eq .Values.mode "accumulo") -}}
{{- end -}}
{{- end -}}

{{- define "shoal.tserverEnabled" -}}
{{- if kindIs "bool" .Values.tserver.enabled -}}
{{- .Values.tserver.enabled -}}
{{- else -}}
{{- eq .Values.mode "accumulo" -}}
{{- end -}}
{{- end -}}

{{- define "shoal.compactorEnabled" -}}
{{- if kindIs "bool" .Values.compactor.enabled -}}
{{- .Values.compactor.enabled -}}
{{- else -}}
{{- eq .Values.mode "accumulo" -}}
{{- end -}}
{{- end -}}

{{- define "shoal.explorerEnabled" -}}
{{- .Values.explorer.enabled -}}
{{- end -}}

{{- /*
Explorer names are built from one bounded stem so that every name derived from
it fits Kubernetes' 63-character limit.

Truncating the finished name is not enough: the headless Service name is the
longest of them, and appending "-headless" to an already-63-character name
produces 72. The stem reserves room for the longest suffix instead, so a long
release name shortens the stem rather than overflowing the name.

  stem            45
  -explorer        9  -> 54
  -explorer-headless 18 -> 63
*/ -}}
{{- define "shoal.explorerStem" -}}
{{- include "shoal.fullname" . | trunc 45 | trimSuffix "-" -}}
{{- end -}}

{{- define "shoal.explorerName" -}}
{{- printf "%s-explorer" (include "shoal.explorerStem" .) -}}
{{- end -}}

{{- define "shoal.explorerHeadlessName" -}}
{{- printf "%s-explorer-headless" (include "shoal.explorerStem" .) -}}
{{- end -}}

{{- /* The approver mapping ConfigMap (#451, #526): stem plus -approvers, 55
       characters at most. */ -}}
{{- define "shoal.explorerApproversName" -}}
{{- printf "%s-approvers" (include "shoal.explorerStem" .) -}}
{{- end -}}

{{- /* The label grant ConfigMap (#570): stem plus -label-grants, 58
       characters at most. */ -}}
{{- define "shoal.explorerLabelGrantsName" -}}
{{- printf "%s-label-grants" (include "shoal.explorerStem" .) -}}
{{- end -}}

{{- /* The executor mapping ConfigMap (#391): stem plus -executors, 55
       characters at most. */ -}}
{{- define "shoal.explorerExecutorsName" -}}
{{- printf "%s-executors" (include "shoal.explorerStem" .) -}}
{{- end -}}

{{- define "shoal.explorerSelectorLabels" -}}
app.kubernetes.io/name: shoal-explore-web
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: explorer
{{- end -}}

{{- define "shoal.llmGatewayEnabled" -}}
{{- .Values.llmGateway.enabled -}}
{{- end -}}

{{- /*
The gateway's names come from a bounded stem for the same reason the explorer's
do: Kubernetes rejects a name over 63 characters, and truncating the finished
name does not help, because the suffix is appended after the truncation. The
stem reserves room for the longest suffix instead, so a long release name
shortens the stem rather than overflowing the name.

  stem         51
  -llm-gateway 12  -> 63
*/ -}}
{{- define "shoal.llmGatewayStem" -}}
{{- include "shoal.fullname" . | trunc 51 | trimSuffix "-" -}}
{{- end -}}

{{- define "shoal.llmGatewayName" -}}
{{- printf "%s-llm-gateway" (include "shoal.llmGatewayStem" .) -}}
{{- end -}}

{{- /*
A selector distinct from the explorer's and from the storage tier's. The two
planes are separate processes deliberately, and a selector that could match
either would let the Service carrying arbitrary prompt traffic land on the pod
holding the policy store.
*/ -}}
{{- define "shoal.llmGatewaySelectorLabels" -}}
app.kubernetes.io/name: shoal-llm-gateway
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: llm-gateway
{{- end -}}

{{- /*
The gateway's host allow-list, normalized and comma-joined for -allowed-host.

One definition rather than two, because the guard in validate.yaml and the
argument in the Deployment have to agree about what an entry is. A blank entry
is not a configured authority — the gate drops it and then reports the setting
as missing — so if the guard trimmed and the argument did not, the chart would
pass its own check and then render `-allowed-host=` with a stray comma or a
space inside an authority that is matched exactly. An authority with a space in
it matches nothing, which is the 421 the guard exists to prevent, reached
through the one path the guard was not looking at.

Emits the empty string when nothing survives, which is what the guard tests.
*/ -}}
{{- /*
The models this deployment names, trimmed the way the allow-list is and for the
same reason: the value is joined into one argument, so a blank element would
render a comma pair the binary reads as an empty model name.

Empty is a working configuration, not a missing one. With no models named the
declaration reports every model as "other", which keeps caller-controlled text
out of what the workspace receives — the field is free-form, so a prompt fits
in it as readily as a model name. Naming models here buys policy granularity
and nothing else; it does not restrict which models may be called.
*/ -}}
{{- define "shoal.llmGatewayModels" -}}
{{- $models := list -}}
{{- range (default (list) .Values.llmGateway.models) -}}
{{- if trim (default "" .) -}}
{{- $models = append $models (trim .) -}}
{{- end -}}
{{- end -}}
{{- join "," $models -}}
{{- end -}}

{{- /*
The explorer's host allow-list, trimmed and comma-joined for -allowed-host (#423).

One definition, used by the guard in validate.yaml and by the argument in the
StatefulSet, so the two cannot disagree about what an entry is. The gate matches
an authority exactly, so every entry the chart passes must be one it can match:

- A blank-after-trim entry is refused rather than dropped. The operator meant a
  host there, and rendering a shorter list than they wrote is the partial
  failure where one ingress name answers 421 while its neighbours work.
- A comma inside an entry is refused. The entries are joined into one argument
  that the binary splits on commas, so "a.test, " would arrive as "a.test" plus
  a blank the binary drops quietly — the same shortened list, reached around
  the blank check — and "a.test,b.test" is two entries written as one.
- Whitespace inside an entry is refused. The binary trims each piece but keeps
  interior space, and no Host header carries a space, so the entry matches
  nothing.
- An entry must be an authority as normalizeAuthority
  (pkg/explorer/webapi/hostauthority.go) reads one: host, host:port,
  [ipv6] or [ipv6]:port. A "/" is refused, because a URL such as
  "https://shoal.example.test" splits as host "https" with port
  "//shoal.example.test" and matches nothing. An unbracketed host with more
  than one colon ("::1") is refused, because the workspace refuses it at
  startup and the pod exits. An empty host (":8443", or "." once the
  trailing dot is folded) is refused for the same reason.

  This is slightly stricter than the binary in two places, both of which
  are false refusals of an authority no client sends: a bracketed host must
  be an IP literal even when a port follows (net.SplitHostPort does not
  check it), and a port must be digits (the binary compares it as a string).
  IPv6 literals with an embedded IPv4 tail (::ffff:192.0.2.1) are refused
  too, since the pattern below covers the hex forms only.

Emits the empty string for an empty list, which is what the guard tests.
*/ -}}
{{- define "shoal.explorerAllowedHosts" -}}
{{- $hosts := list -}}
{{- $octet := "(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])" -}}
{{- $v4 := printf "^%s(\\.%s){3}$" $octet $octet -}}
{{- $v6 := replace "H" "[0-9a-fA-F]{1,4}" "^((H:){7}H|(H:){1,7}:|(H:){1,6}:H|(H:){1,5}(:H){1,2}|(H:){1,4}(:H){1,3}|(H:){1,3}(:H){1,4}|(H:){1,2}(:H){1,5}|H:(:H){1,6}|:((:H){1,7}|:))$" -}}
{{- range (default (list) .Values.explorer.allowedHosts) -}}
{{- $host := trim (toString (default "" .)) -}}
{{- if not $host -}}
{{- fail "explorer.allowedHosts contains a blank-after-trim element: give every intended host a non-blank authority rather than silently shortening the allow-list" -}}
{{- end -}}
{{- if contains "," $host -}}
{{- fail (printf "explorer.allowedHosts contains %q, which holds a comma: entries are comma-joined into one -allowed-host argument that the workspace splits on commas, so this one entry becomes several and a blank piece is dropped silently. Give each authority its own list entry" $host) -}}
{{- end -}}
{{- if regexMatch `\p{Zs}` $host -}}
{{- fail (printf "explorer.allowedHosts contains %q, which holds whitespace inside the authority: the workspace matches the Host header exactly and no Host header carries a space, so every request for it is refused with 421" $host) -}}
{{- end -}}
{{- if contains "/" $host -}}
{{- fail (printf "explorer.allowedHosts contains %q, which holds a \"/\": an entry is an authority (host or host:port, as the Host header carries it), not a URL. The workspace would split it at the last colon and match nothing, so every request for it is refused with 421" $host) -}}
{{- end -}}
{{- $bracketed := `^\[([^\[\]]+)\](:[0-9]*)?$` -}}
{{- $plain := `^([^\[\]:]*)(:[0-9]*)?$` -}}
{{- if regexMatch $bracketed $host -}}
{{- $inner := regexReplaceAll $bracketed $host "${1}" -}}
{{- if not (or (regexMatch $v6 $inner) (regexMatch $v4 $inner)) -}}
{{- fail (printf "explorer.allowedHosts contains %q, whose bracketed host is not an IP address: brackets hold an IPv6 literal such as [::1], optionally followed by :port" $host) -}}
{{- end -}}
{{- else if regexMatch $plain $host -}}
{{- if not (trimSuffix "." (regexReplaceAll $plain $host "${1}")) -}}
{{- fail (printf "explorer.allowedHosts contains %q, which has no host: the workspace refuses an authority with an empty host at startup" $host) -}}
{{- end -}}
{{- else if regexMatch `^[^\[\]]*:[^\[\]]*:` $host -}}
{{- fail (printf "explorer.allowedHosts contains %q, an unbracketed host with more than one colon: the workspace refuses it at startup and the pod exits. Write an IPv6 address in brackets, as [::1] or [::1]:8443" $host) -}}
{{- else -}}
{{- fail (printf "explorer.allowedHosts contains %q, which is not an authority the workspace accepts: write host, host:port, [ipv6] or [ipv6]:port, with a numeric port" $host) -}}
{{- end -}}
{{- $hosts = append $hosts $host -}}
{{- end -}}
{{- join "," $hosts -}}
{{- end -}}

{{- define "shoal.llmGatewayAllowedHosts" -}}
{{- $hosts := list -}}
{{- range (default (list) .Values.llmGateway.allowedHosts) -}}
{{- if trim (default "" .) -}}
{{- $hosts = append $hosts (trim .) -}}
{{- end -}}
{{- end -}}
{{- join "," $hosts -}}
{{- end -}}

{{- /*
Where the listener's TLS Secret is mounted. One value for the volumeMount, both
-tls-*-file flags and the guard that keeps the credential mounts off it, so the
three cannot name different directories.
*/ -}}
{{- define "shoal.llmGatewayTLSDir" -}}
/etc/shoal-llm-gateway/tls
{{- end -}}

{{- /*
Whether the gateway listener serves TLS. validate.yaml refuses anything but a
boolean, so this only has to survive a tls block that was nulled out.
*/ -}}
{{- define "shoal.llmGatewayTLS" -}}
{{- $tls := default (dict) .Values.llmGateway.tls -}}
{{- if eq $tls.enabled true }}true{{ end -}}
{{- end -}}

{{- /*
One credential volume, for either of the gateway's two -file credentials.

The two break differently — an unreadable admission token denies every call,
an unreadable upstream key fails calls policy already allowed — but the
mechanism that delivers them is identical, and two copies of it would be two
places for the mode, the path and the source to drift apart.

Takes a dict: name, path, source, audience, expirationSeconds, secretName,
secretKey, volume.

The file name inside the volume is derived from the path the flag carries, so
the projection and the flag cannot name different files. The directory half of
that same path is the mountPath at the call site.

defaultMode is 0440 and never 0400, which is the mistake worth spelling out.
The kubelet writes projected and Secret volumes owned by root; the gateway
container runs as uid 65532 with every capability dropped. At 0400 the one
process that needs the credential cannot open it, and the failure is not a
crash — the pod starts, passes both probes, and fails on the credential at
every request. 0440 is readable exactly because the pod declares fsGroup 65532
alongside it; neither half works without the other.
*/ -}}
{{- define "shoal.llmGatewayCredentialVolume" -}}
- name: {{ .name | quote }}
  {{- if eq .source "projected" }}
  projected:
    defaultMode: 0440
    sources:
      - serviceAccountToken:
          path: {{ base .path | quote }}
          audience: {{ .audience | quote }}
          expirationSeconds: {{ .expirationSeconds }}
  {{- else if eq .source "secret" }}
  secret:
    secretName: {{ .secretName | quote }}
    defaultMode: 0440
    items:
      - key: {{ .secretKey | quote }}
        path: {{ base .path | quote }}
  {{- else }}
  {{- toYaml .volume | nindent 2 }}
  {{- end }}
{{- end -}}

{{- /*
Converts a Go duration literal to milliseconds so the chart can compare two of
them, failing on anything Go itself would not parse.

Helm has no duration type and no duration arithmetic, and the two durations the
gateway takes are not independent: an upstream request timeout above the
admission lease produces a call that outlives the permission it was granted
under. Comparing them needs a number, and reading one out of "90s" is the only
way to get it.

Compound literals are accepted ("1m30s") because Go accepts them, and a guard
that refused a duration the binary would take is a false refusal. The
concatenation check is what makes that safe: it rejects anything with
characters the unit scan did not consume, so "30sec", "5 s" and "1e3s" fail
rather than silently parsing as their leading prefix. Fractional and unitless
forms ("1.5m", "0") are refused deliberately — a bare 0 means "no timeout" to
Go, which for this component is an upstream call that cannot outlive the lease
because it cannot end.

Takes a dict of name and value; emits the total in milliseconds.
*/ -}}
{{- define "shoal.durationMillis" -}}
{{- $name := .name -}}
{{- $value := trim (toString .value) -}}
{{- $parts := regexFindAll "[0-9]+(ms|h|m|s)" $value -1 -}}
{{- if or (not $parts) (ne (join "" $parts) $value) -}}
{{- fail (printf "%s must be a Go duration built from whole numbers and the units ms, s, m or h — for example 30s, 2m or 1m30s (got %q). Anything else is not a duration the binary can parse, so the pod exits at startup with a flag error" $name $value) -}}
{{- end -}}
{{- $factors := dict "ms" 1 "s" 1000 "m" 60000 "h" 3600000 -}}
{{- $total := int64 0 -}}
{{- range $parts -}}
{{- $unit := regexFind "(ms|h|m|s)$" . -}}
{{- $total = add $total (mul (int64 (regexFind "^[0-9]+" .)) (index $factors $unit)) -}}
{{- end -}}
{{- $total -}}
{{- end -}}

{{- /*
The gateway's transport rule, as the binary spells it (absoluteURL and isLoopback
in cmd/shoal-llm-gateway/admission.go): an absolute http(s) URL with a host, and
http permitted only to loopback.

These were scheme-prefix tests, which disagreed with the binary in both
directions. "https://" has the prefix and no host, so the chart passed it and
the binary refused it for having no host — a pod in CrashLoopBackOff from a
values file the chart approved. And "http://localhost.example" has the
"http://localhost" prefix without being loopback at all, so the chart treated a
remote plaintext host as exempt while the binary refused it. Parsing is the only
way to get both right, and urlParse gives the two fields the rule needs.

The hostname is lowered because the binary compares it with EqualFold, so
"HTTP://LOCALHOST" is loopback there and must be here. Go's url.Parse already
lowers the scheme but not the host.
*/ -}}
{{- define "shoal.urlIsAbsolute" -}}
{{- $parsed := urlParse . -}}
{{- if and (has $parsed.scheme (list "http" "https")) $parsed.hostname -}}
true
{{- end -}}
{{- end -}}

{{- define "shoal.urlIsPlaintext" -}}
{{- if eq (urlParse .).scheme "http" -}}
true
{{- end -}}
{{- end -}}

{{- /* 127.0.0.0/8, matched as an address rather than as a prefix.
       net.IP.IsLoopback accepts any address in that block, so "127.0.0.1"
       alone is too narrow — but "127." as a prefix is too broad in the
       direction that matters: "127.example.com" is a DNS name the binary
       refuses, and a prefix test approved it, which is the same
       chart-renders-what-the-binary-refuses failure the parsing was meant to
       end.

       Each octet is bounded, because net.ParseIP refuses 127.0.0.256 and a
       looser \d{1,3} would approve it. Go also refuses a short form like
       "127.1", so a full dotted quad is required here too.

       IPv6 loopback is matched as "::1" and its fully expanded spelling. Other
       spellings (0::1, ::0001) are loopback to net.ParseIP and are refused
       here, which is a false refusal rather than an approved-but-broken pod —
       the safe direction for a guard to be wrong in, and stated so nobody
       reads it as an oversight. */ -}}
{{- define "shoal.urlIsLoopback" -}}
{{- $host := lower (urlParse .).hostname -}}
{{- $octet := "(25[0-5]|2[0-4][0-9]|1[0-9][0-9]|[1-9]?[0-9])" -}}
{{- $v4 := printf "^127\\.%s\\.%s\\.%s$" $octet $octet $octet -}}
{{- if or (eq $host "localhost") (eq $host "::1")
          (eq $host "0:0:0:0:0:0:0:1") (regexMatch $v4 $host) -}}
true
{{- end -}}
{{- end -}}

{{- /*
An explorer model-provider base URL (explorer.chat.baseURL,
explorer.embedding.baseURL), refused here exactly where the workspace refuses
it at startup (#423). Takes a dict of name (the values key) and value.

pkg/model validates every provider the explorer offers (Ollama, the
OpenAI-compatible client, Voyage) the same way, after trimming: an absolute
http(s) URL with a host; no userinfo, query or fragment; a path of nothing or
"/"; and plaintext only to a loopback host. The provider appends its own API
path, so a base such as https://api.example.test/v1 is refused by the binary,
and a chart that approved it would render a pod that exits at startup.

A bare "?" or "#" with nothing after it is refused here as well. The
OpenAI-compatible and Voyage clients refuse both; Ollama tolerates them. That
is a false refusal of a URL nobody means to write, which is the safe
direction for a guard to be wrong in.
*/ -}}
{{- define "shoal.explorerProviderURL" -}}
{{- $name := .name -}}
{{- $value := trim (toString (default "" .value)) -}}
{{- if not (include "shoal.urlIsAbsolute" $value) -}}
{{- fail (printf "%s must be an absolute http(s) URL with a host (got %q)" $name $value) -}}
{{- end -}}
{{- $parsed := urlParse $value -}}
{{- if $parsed.userinfo -}}
{{- fail (printf "%s must not carry userinfo (got %q): the workspace refuses a provider URL with credentials in it at startup. Put the credential in a Secret and name it in credentialSecretName" $name $value) -}}
{{- end -}}
{{- if or $parsed.query $parsed.fragment (contains "?" $value) (contains "#" $value) -}}
{{- fail (printf "%s must not carry a query or fragment (got %q): the workspace refuses either at startup" $name $value) -}}
{{- end -}}
{{- if not (has $parsed.path (list "" "/")) -}}
{{- fail (printf "%s must be the provider's root, with no path (got %q): the workspace appends the provider's own API path and refuses a base URL that already has one at startup" $name $value) -}}
{{- end -}}
{{- if and (include "shoal.urlIsPlaintext" $value) (not (include "shoal.urlIsLoopback" $value)) -}}
{{- fail (printf "%s must use HTTPS unless the host is loopback (got %q): the workspace refuses plaintext HTTP to any other host at startup" $name $value) -}}
{{- end -}}
{{- end -}}

{{- /*
A rollout or disruption value, validated as written.

These reach the API server verbatim, and the only guard they had cast to int
first — which is exactly where the value escapes. int 1.5 is 1 and int -1 is
-1, so a guard testing "did it cast to zero" approves both, the chart reports
success, and the API server rejects the object at install or upgrade time.
Casting before validating cannot see the thing it is validating.

Kubernetes takes an IntOrString here: a non-negative whole number, or a
percentage. Both forms are accepted and nothing else is, which is narrower than
int and wider than a bare integer.
*/ -}}
{{- define "shoal.requireIntOrPercent" -}}
{{- $name := .name -}}
{{- /* No "default" here: Helm's default treats 0 as empty, so
       `default "" 0` is "" and this guard refused the chart's own shipped
       maxUnavailable: 0. The value is stringified directly, and a missing key
       fails the match like any other non-value. */ -}}
{{- $raw := trim (toString .value) -}}
{{- if not (or (regexMatch "^(0|[1-9][0-9]*)$" $raw) (regexMatch "^(0|[1-9][0-9]*)%$" $raw)) -}}
{{- fail (printf "%s must be a non-negative whole number or a percentage (got %q): it is rendered into the object verbatim, so the API server rejects the %s at install or upgrade time while the chart reports success. A fractional or negative value is not an IntOrString, and a guard that casts to int before testing cannot tell — int of 1.5 is 1 and int of -1 is -1" $name $raw (default "object" .kind)) -}}
{{- end -}}
{{- end -}}

{{- /*
Whether an IntOrString means zero, judged on what was written.

"0" and "0%" both mean zero; "50%" does not. The guard that needed this cast
with int first, and int of "50%" is 0 — so a perfectly valid percentage
disruption budget was refused as if it were zero, with a message about
blocking evictions that had nothing to do with it. Same flaw as the one
requireIntOrPercent exists for, in the guard that was already there.
*/ -}}
{{- define "shoal.isZeroIntOrPercent" -}}
{{- $raw := trim (toString .) -}}
{{- if or (eq $raw "0") (eq $raw "0%") -}}
true
{{- end -}}
{{- end -}}

{{- /*
Refuse a line break anywhere in a values subtree, naming the key that holds it.

Takes a dict of path (the dotted key, for the message), value, and two lists of
paths: multiline, whose string values may hold LF, CRLF and tab (and nothing
else of the class below), and trimmed, whose strings are checked after trimming
because the templates trim them before use. Recurses through maps and lists;
every map key and every string value is checked. Every other scalar kind
(number, boolean, null) renders through Go formatting and cannot carry one.

The class is [\p{Cc}\p{Zl}\p{Zp}], in a raw string so the template does not
unescape it: the ASCII and C1 controls, which take in LF, CR and NEL, plus the
Unicode line and paragraph separators. go-yaml breaks lines on all of those,
and RE2's [[:cntrl:]] is ASCII only, so a narrower class lets NEL through.

For anything toYaml emits, this refusal is load-bearing, not defence in depth.
Quoting protects a value interpolated into a single scalar; toYaml output has
no quoting to add, because the block it emits is itself the structure, and
nindent indents that block line by line. Refusing the characters is the only
defence there:

  Map keys are held to the whole class everywhere, multi-line subtrees
  included. A key is only ever rendered through toYaml (nodeSelector,
  tolerations, affinity, resources, annotations, the operator volumes), and a
  key holding a newline is emitted as a block that nindent indents like any
  other, so the key can end early and the remainder land in the pod spec —
  hostNetwork: true among it. No Kubernetes key a values file reaches here has
  a line break in it.

  The multi-line values are the ones that legitimately span lines: the
  service-account key file, the body of a `|` block scalar through `indent 4`,
  and the annotations and operator volumes, which toYaml emits as a `|-` block
  when they hold LF. Both indent after LF only, so LF, CRLF and tab stay inside
  the block, and nothing else of the class does: a lone CR, a NEL or
  U+2028/U+2029 is a line break to YAML that neither indent nor nindent indents
  after — go-yaml writes the separators raw inside a block scalar — so the text
  after it lands at column zero, outside the block and outside the object.
  Annotation "v<LF>w<U+2028>namespace: kube-system" moved the gateway's
  Service into kube-system.
*/ -}}
{{- define "shoal.refuseLineBreaks" -}}
{{- $context := . -}}
{{- $multiline := or .inMultiline (has .path .multiline) -}}
{{- if kindIs "map" .value -}}
{{- range $key, $child := .value -}}
{{- /* The key is named, quoted: keys are not secrets, and without it the
       operator has no way to find which one. */ -}}
{{- $found := regexFind `[\p{Cc}\p{Zl}\p{Zp}]` $key -}}
{{- if $found -}}
{{- fail (printf "%s has a key %q, which holds %q, a line break or other control character (NEL, U+2028 and U+2029 included): map keys reach the manifests through toYaml, which renders one holding a line break as a block that nindent indents line by line, so the key ends early and the rest of it becomes fields of the pod spec or object. No key may carry one" $context.path $key $found) -}}
{{- end -}}
{{- include "shoal.refuseLineBreaks" (dict "path" (printf "%s.%s" $context.path $key) "value" $child "multiline" $context.multiline "inMultiline" $multiline "trimmed" $context.trimmed) -}}
{{- end -}}
{{- else if kindIs "slice" .value -}}
{{- range $index, $child := .value -}}
{{- include "shoal.refuseLineBreaks" (dict "path" (printf "%s[%d]" $context.path $index) "value" $child "multiline" $context.multiline "inMultiline" $multiline "trimmed" $context.trimmed) -}}
{{- end -}}
{{- else if kindIs "string" .value -}}
{{- $text := .value -}}
{{- range $prefix := .trimmed -}}
{{- if or (eq $context.path $prefix) (hasPrefix (printf "%s." $prefix) $context.path) (hasPrefix (printf "%s[" $prefix) $context.path) -}}
{{- $text = trim $text -}}
{{- end -}}
{{- end -}}
{{- if $multiline -}}
{{- $text = $text | replace "\r\n" "" | replace "\n" "" | replace "\t" "" -}}
{{- end -}}
{{- /* The message names the character and not the value: the walk reaches
       passwords and key files, and a refusal is printed to CI logs. */ -}}
{{- $found := regexFind `[\p{Cc}\p{Zl}\p{Zp}]` $text -}}
{{- if $found -}}
{{- fail (printf "%s holds %q, a line break or other control character (NEL, U+2028 and U+2029 included): the chart renders it into the manifests as a YAML scalar, and a line break there ends the scalar, so the rest of the value becomes YAML of its own — another field, another container argument, another key. %s A value read with --set-file keeps the file's trailing newline, so strip it" .path $found (ternary "This value may span lines with LF, CRLF and tab, and nothing else: a lone CR, a NEL or a Unicode separator is a line break the indentation does not follow." "No value this chart renders may carry one." $multiline)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
A value with a fixed textual shape, validated as written: a Kubernetes quantity,
a Go duration, a Secret name, an enumerated field. Takes a dict of name, value,
pattern, what (the shape, for the message) and why. An empty value is left to
whatever already owns that case.

Each of these is rendered unquoted by a template the storage profiles also
render, and those profiles must stay byte-identical, so quoting is not
available; the value is held to a shape that contains no YAML syntax instead.
That matters beyond line breaks in the accumulo templates, where the value sits
inside a flow mapping and a comma or a brace ends it on the same line: a Secret
name of "creds, key: other" names a different key.
*/ -}}
{{- define "shoal.requireShape" -}}
{{- $raw := toString (default "" .value) -}}
{{- if and $raw (not (regexMatch .pattern $raw)) -}}
{{- fail (printf "%s must be %s (got %q): %s" .name .what $raw .why) -}}
{{- end -}}
{{- end -}}
