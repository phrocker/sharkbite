{{- /*
The effects gateways (#391), each entry deep-merged over effectsGatewayDefaults,
as a JSON list. One definition, so validate.yaml and every template that
renders an entry read the same merged values: a guard that checked the raw
entry while the Deployment rendered the merged one could pass a value it never
saw. Disabled entries are included (validate.yaml checks names across all of
them); the templates skip them.

Numbers come back from fromJsonArray as float64, so the templates print them
with %v and cast with int64 before arithmetic.
*/ -}}
{{- define "shoal.effectsGateways" -}}
{{- $merged := list -}}
{{- $entries := default (list) .Values.effectsGateways -}}
{{- range $index, $entry := (ternary $entries (list) (kindIs "slice" $entries)) -}}
{{- /* An entry that is not a map is refused at the end of validate.yaml,
       after the line-break walk has had its say about what it holds. */ -}}
{{- if kindIs "map" $entry -}}
{{- $gateway := mergeOverwrite (deepCopy $.Values.effectsGatewayDefaults) (deepCopy $entry) -}}
{{- $_ := set $gateway "_index" $index -}}
{{- $merged = append $merged $gateway -}}
{{- end -}}
{{- end -}}
{{- toJson $merged -}}
{{- end -}}

{{- /*
Names hang off a bounded stem for the same reason the explorer's and the LLM
gateway's do: Kubernetes rejects a name over 63 characters, and the suffix is
appended after any truncation. validate.yaml holds an entry name to 24
characters, so the longest name the chart derives fits:

  stem              24
  -gw-               4
  <name>            24  -> 52: the Deployment, ServiceAccount, ConfigMap
                           (-routes, 59) and NetworkPolicy
  -unrecorded       11  -> 63: the PersistentVolumeClaim
*/ -}}
{{- define "shoal.effectsGatewayStem" -}}
{{- include "shoal.fullname" . | trunc 24 | trimSuffix "-" -}}
{{- end -}}

{{- /* Takes a dict of root and gateway. */ -}}
{{- define "shoal.effectsGatewayName" -}}
{{- printf "%s-gw-%s" (include "shoal.effectsGatewayStem" .root) .gateway.name -}}
{{- end -}}

{{- /*
A selector distinct from every other component's, and per entry: two gateways
in one release are two surfaces with two credentials, and a selector that
matched both would let one Deployment adopt the other's pod.
*/ -}}
{{- define "shoal.effectsGatewaySelectorLabels" -}}
app.kubernetes.io/name: shoal-gateway
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: effects-gateway
shoal.effects-gateway/name: {{ .gateway.name | quote }}
{{- end -}}

{{- /*
The minimum terminationGracePeriodSeconds for an entry, in whole seconds:

  T + max(5s, 3 × planeTimeout) + 2 × 5s + 5s, rounded up

which is GracePeriodSeconds in internal/effectsgateway/timing.go: the request
in flight (T), its completion (CompletionBudget: the report window, or three
plane timeouts when that is longer), the two fallback ambiguity reports
(FallbackReportAttempts × ReportWindow), and the exit margin.

The chart computes it rather than asking for it, so changing a timeout changes
the grace period with it and the two cannot be set apart. It is a second copy
of a Go formula, and a copy can drift; validate-chart.sh renders it for a
table of inputs and compares each with what `shoal-gateway grace-period`
prints for the same pair, so a change to the formula on either side fails the
chart job. Rounding is up, as in Go: rounding down would shave the margin the
formula exists to provide.

Takes a dict of name (for messages), operationTimeout and planeTimeout.
*/ -}}
{{- define "shoal.effectsGatewayGraceSeconds" -}}
{{- $operation := int64 (include "shoal.durationMillis" (dict "name" (printf "%s.timing.operationTimeout" .name) "value" (default "" .operationTimeout))) -}}
{{- $plane := int64 (include "shoal.durationMillis" (dict "name" (printf "%s.timing.planeTimeout" .name) "value" (default "" .planeTimeout))) -}}
{{- $reportWindow := int64 5000 -}}
{{- $completion := max $reportWindow (mul $plane 3) -}}
{{- $grace := add $operation $completion (mul $reportWindow 2) $reportWindow -}}
{{- div (add $grace 999) 1000 -}}
{{- end -}}

{{- /*
The grace period an entry renders: the formula, or the operator's larger value.
validate.yaml refuses a smaller one, so this never lowers it. Takes a dict of
name and gateway.
*/ -}}
{{- define "shoal.effectsGatewayGracePeriod" -}}
{{- $minimum := int64 (include "shoal.effectsGatewayGraceSeconds" (dict "name" .name "operationTimeout" .gateway.timing.operationTimeout "planeTimeout" .gateway.timing.planeTimeout)) -}}
{{- $override := include "shoal.effectsGatewayWhole" (dict "name" (printf "%s.terminationGracePeriodSeconds" .name) "value" .gateway.terminationGracePeriodSeconds) -}}
{{- if and $override (gt (int64 $override) $minimum) -}}
{{- $override -}}
{{- else -}}
{{- $minimum -}}
{{- end -}}
{{- end -}}

{{- /*
A whole, non-negative number as decimal digits, or "" when the value is unset.

Every number in an entry has been through toJson and fromJsonArray, so it is a
float64, and neither %v nor toString is safe to render: both print 1048576 as
1.048576e+06, which no flag parser and no pod spec accepts. A string is taken
as written. Anything fractional, negative or non-numeric is refused here,
naming the key. Takes a dict of name and value.
*/ -}}
{{- define "shoal.effectsGatewayWhole" -}}
{{- $value := .value -}}
{{- if kindIs "float64" $value -}}
{{- if or (lt $value 0.0) (ne $value (float64 (int64 $value))) -}}
{{- fail (printf "%s must be a whole, non-negative number (got %v)" .name $value) -}}
{{- end -}}
{{- printf "%d" (int64 $value) -}}
{{- else if kindIs "string" $value -}}
{{- if and $value (not (regexMatch "^(0|[1-9][0-9]*)$" $value)) -}}
{{- fail (printf "%s must be a whole, non-negative number written plainly (got %q)" .name $value) -}}
{{- end -}}
{{- $value -}}
{{- else if or (kindIs "int" $value) (kindIs "int64" $value) -}}
{{- if lt (int64 $value) 0 -}}
{{- fail (printf "%s must be a whole, non-negative number (got %v)" .name $value) -}}
{{- end -}}
{{- printf "%d" (int64 $value) -}}
{{- else if not (kindIs "invalid" $value) -}}
{{- fail (printf "%s must be a whole, non-negative number (got %v)" .name $value) -}}
{{- end -}}
{{- end -}}

{{- /* Whether any of an entry's routes uses idempotency "key". */ -}}
{{- define "shoal.effectsGatewayRequiresKey" -}}
{{- range (default (list) .routes) -}}
{{- if and (kindIs "map" .) (eq (toString (get . "idempotency")) "key") -}}
true
{{- end -}}
{{- end -}}
{{- end -}}

{{- /*
The install notes for the effects gateways, and the loud warning for an
emptyDir log. A named template, included by NOTES.txt, because NOTES.txt is
rendered only by install and upgrade, and Helm 3.16 contacts the cluster for
both even with --dry-run=client: a check of the warning that needs a cluster
cannot run in CI. validate-chart.sh renders this template through
`helm template` instead.
*/ -}}
{{- define "shoal.effectsGatewayNotes" -}}
{{- range $gateway := fromJsonArray (include "shoal.effectsGateways" .) }}
{{- if $gateway.enabled }}
{{- $name := include "shoal.effectsGatewayName" (dict "root" $ "gateway" $gateway) }}
Effects gateway {{ $gateway.name }} ({{ $name }}): terminationGracePeriodSeconds {{ include "shoal.effectsGatewayGracePeriod" (dict "name" (printf "effectsGateways[%v]" $gateway._index) "gateway" $gateway) }}, replicas {{ $gateway.replicas }}, Recreate.
{{- if eq $gateway.unrecorded.storage "emptyDir" }}

  ******************************************************************************
  WARNING: effects gateway {{ $gateway.name }} keeps its unrecorded log in an
  emptyDir (unrecorded.storage: emptyDir, acceptLossOfUnrecordedReports: true).

  The log holds the only record of effects the explorer refused, did not find
  or could not confirm (#514). It is DELETED with the pod: a reschedule, an
  eviction, a node loss or an upgrade loses every report in it, and nothing
  else in the system remembers those effects happened.

  Use unrecorded.storage: persistentVolumeClaim (the default) in any
  deployment whose effects matter.
  ******************************************************************************
{{- else if eq $gateway.unrecorded.storage "persistentVolumeClaim" }}
  The unrecorded log's claim {{ $name }}-unrecorded is kept on uninstall
  (helm.sh/resource-policy: keep). Delete it by hand once
  `shoal-gateway unrecorded list` shows it empty.
{{- end }}
{{- end }}
{{- end -}}
{{- end -}}
