{{/*
Names. With the release name "quackmux" they are those of deploy/kubernetes:
Deployment and Service "quackmux", ServiceAccount, Role and RoleBinding
"quackmux-gateway", ConfigMap "quackmux-config-<hash>", and so on.
*/}}
{{- define "quackmux.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else if contains "quackmux" .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-quackmux" .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.labels: the labels of deploy/kubernetes (name, component, part-of)
plus those of Helm. Argument: dict "root" $ "component" COMPONENT.
*/}}
{{- define "quackmux.labels" -}}
app.kubernetes.io/name: quackmux
app.kubernetes.io/component: {{ .component }}
app.kubernetes.io/part-of: quackmux
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{/*
quackmux.selectorLabels: the labels of the gateway pods. The Deployment, the
Service, the NetworkPolicies, the PodDisruptionBudget and the anti-affinity
select them; they never change (a Deployment's selector is immutable).
*/}}
{{- define "quackmux.selectorLabels" -}}
app.kubernetes.io/name: quackmux
app.kubernetes.io/component: gateway
{{- end -}}

{{/* quackmux.serviceSelector: the Service selector; with ha.enabled the leader alone. */}}
{{- define "quackmux.serviceSelector" -}}
{{ include "quackmux.selectorLabels" . }}
{{- if .Values.ha.enabled }}
quackmux.io/leader: "true"
{{- end }}
{{- end -}}

{{- define "quackmux.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (printf "%s-gateway" (include "quackmux.fullname" .)) .Values.serviceAccount.name -}}
{{- else -}}
{{- required "serviceAccount.name is required with serviceAccount.create false" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "quackmux.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}

{{/* quackmux.replicas: replicas, default 1 (2 with ha.enabled), checked. */}}
{{- define "quackmux.replicas" -}}
{{- $n := ternary 2 1 (eq .Values.ha.enabled true) -}}
{{- if not (kindIs "invalid" .Values.replicas) -}}
{{- $n = int .Values.replicas -}}
{{- end -}}
{{- if and (gt $n 1) (not .Values.ha.enabled) -}}
{{- fail (printf "replicas: %d gateways without leader election would all manage the engines; set ha.enabled true (or replicas 1)" $n) -}}
{{- end -}}
{{- if and .Values.ha.enabled (eq $n 1) -}}
{{- fail "replicas: ha.enabled with one replica has no standby, and its PodDisruptionBudget would block every eviction; use 2 or more" -}}
{{- end -}}
{{- $n -}}
{{- end -}}

{{/* Secret names: an existing Secret or the chart's own. */}}
{{- define "quackmux.sessionKeySecret" -}}
{{- default (printf "%s-session-key" (include "quackmux.fullname" .)) .Values.secrets.sessionKey.existingSecret -}}
{{- end -}}

{{- define "quackmux.adminTokenSecret" -}}
{{- default (printf "%s-admin-token" (include "quackmux.fullname" .)) .Values.secrets.adminToken.existingSecret -}}
{{- end -}}

{{- define "quackmux.usersSecret" -}}
{{- default (printf "%s-users" (include "quackmux.fullname" .)) .Values.secrets.users.existingSecret -}}
{{- end -}}

{{/*
quackmux.checkExistingSecrets: an existing Secret must not be one that the
chart generates. The chart stops rendering it, so Helm takes it out of the
release: it deletes the users Secret and leaves the others unmanaged. Prints
nothing.
*/}}
{{- define "quackmux.checkExistingSecrets" -}}
{{- $fullname := include "quackmux.fullname" . -}}
{{- range $value, $suffix := dict "sessionKey" "session-key" "adminToken" "admin-token" "users" "users" -}}
{{- $own := printf "%s-%s" $fullname $suffix -}}
{{- if eq (dig $value "existingSecret" "" $.Values.secrets) $own -}}
{{- fail (printf "secrets.%s.existingSecret: %s is the Secret that the chart generates; copy its value into a Secret of your own and name that one (README.md, \"Secrets\")" $value $own) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.secretData prints the base64 value of the key "key" of the chart's
Secret "name": the value the Secret in the cluster already has, else a new
random one of 43 letters and digits (about 256 bits, like quackmux token).
Without a cluster (helm template, --dry-run=client) lookup finds nothing, and
every rendering generates new values. Argument: dict "root" $ "name" NAME
"key" KEY.
*/}}
{{- define "quackmux.secretData" -}}
{{- $old := lookup "v1" "Secret" .root.Release.Namespace .name -}}
{{- $value := "" -}}
{{- if $old -}}
{{- $value = index (default dict $old.data) .key | default "" -}}
{{- end -}}
{{- if $value -}}
{{- $value -}}
{{- else -}}
{{- randAlphaNum 43 | b64enc -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.usersFile: the users file built from the values "users". It fails
for a file the gateway would refuse (the rules of examples/users.yaml beyond
those of values.schema.json): the gateway keeps its previous users when a
changed file is invalid and does not start with one, so an upgrade would
report success while the change never applies.
*/}}
{{- define "quackmux.usersFile" -}}
{{- $users := default list .Values.users -}}
{{- $names := dict -}}
{{- $tokens := dict -}}
{{- range $i, $u := $users -}}
{{- $where := printf "users[%d]" $i -}}
{{- include "quackmux.checkName" (dict "where" (printf "%s.name" $where) "name" $u.name) -}}
{{- if hasKey $names $u.name -}}
{{- fail (printf "%s: user %q is defined twice (also %s)" $where $u.name (get $names $u.name)) -}}
{{- end -}}
{{- $_ := set $names $u.name $where -}}
{{- range $j, $g := default list $u.groups -}}
{{- include "quackmux.checkName" (dict "where" (printf "%s.groups[%d]" $where $j) "name" $g) -}}
{{- end -}}
{{- range $j, $t := default list $u.tokens -}}
{{- $twhere := printf "%s.tokens[%d]" $where $j -}}
{{- $hash := "" -}}
{{- if and $t.sha256 $t.plain -}}
{{- fail (printf "%s: set either sha256 or plain, not both" $twhere) -}}
{{- else if $t.sha256 -}}
{{- $hash = lower (trim $t.sha256) -}}
{{- else if $t.plain -}}
{{- $hash = sha256sum $t.plain -}}
{{- else -}}
{{- fail (printf "%s: needs sha256 (quackmux token, quackmux hash-token) or plain" $twhere) -}}
{{- end -}}
{{- if hasKey $tokens $hash -}}
{{- fail (printf "%s: duplicate token, already used by %s" $twhere (get $tokens $hash)) -}}
{{- end -}}
{{- $_ := set $tokens $hash $twhere -}}
{{- with trim (default "" $t.expires) -}}
{{- if (toDate "2006-01-02T15:04:05Z07:00" .).IsZero -}}
{{- fail (printf "%s: invalid expires %q (want RFC 3339 like 2027-01-01T00:00:00Z)" $twhere .) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- toYaml (dict "users" $users) }}
{{- end -}}

{{/*
quackmux.checkName fails for a user or group name that the gateway refuses:
longer than 256 bytes, with leading or trailing white space, or with control
characters (empty ones values.schema.json refuses). Argument: dict "where"
WHERE "name" NAME.
*/}}
{{- define "quackmux.checkName" -}}
{{- if gt (len .name) 256 -}}
{{- fail (printf "%s: %q is longer than 256 bytes" .where .name) -}}
{{- else if ne (trim .name) .name -}}
{{- fail (printf "%s: %q has leading or trailing white space" .where .name) -}}
{{- else if regexMatch "\\p{Cc}" .name -}}
{{- fail (printf "%s: %q contains control characters" .where .name) -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.merge merges the map "src" into the map "dst" (in place): maps are
merged, null removes the key, every other value replaces the one in dst.
Prints nothing. Argument: dict "dst" DST "src" SRC.
*/}}
{{- define "quackmux.merge" -}}
{{- $dst := .dst -}}
{{- range $key, $value := .src -}}
{{- if kindIs "invalid" $value -}}
{{- $_ := unset $dst $key -}}
{{- else if and (kindIs "map" $value) (kindIs "map" (get $dst $key)) -}}
{{- $_ := include "quackmux.merge" (dict "dst" (get $dst $key) "src" $value) -}}
{{- else -}}
{{- $_ := set $dst $key (deepCopy $value) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.config prints the gateway configuration (config.yaml): configYaml,
else the shipped one (files/config.yaml, with ha.enabled files/config-ha.yaml:
copies of deploy/kubernetes/base/config.yaml and overlays/ha/config.yaml),
with the values "config" merged in. Unchanged it is the text as it is,
comments included; otherwise the merged configuration rendered anew.

It fails for a configuration that the chart cannot run: one whose ha.enabled
differs from the chart's, or one that launches engines outside the release
namespace (the chart's Role, NetworkPolicies and the engines' data volumes
live in the release namespace).
*/}}
{{- define "quackmux.config" -}}
{{- $text := .Values.configYaml -}}
{{- $shipped := not $text -}}
{{- if $shipped -}}
{{- $text = .Files.Get (ternary "files/config-ha.yaml" "files/config.yaml" (eq .Values.ha.enabled true)) -}}
{{- end -}}
{{- $cfg := fromYaml $text -}}
{{- if hasKey $cfg "Error" -}}
{{- fail (printf "configYaml: not a YAML map: %s" (get $cfg "Error")) -}}
{{- end -}}
{{- $changed := false -}}
{{- /* The shipped configuration names the namespace quackmux; elsewhere the engines follow the release. */ -}}
{{- if and $shipped (ne .Release.Namespace "quackmux") -}}
{{- $k8s := dig "provider" "kubernetes" dict $cfg -}}
{{- if eq (toString (get $k8s "namespace")) "quackmux" -}}
{{- $_ := set $k8s "namespace" .Release.Namespace -}}
{{- $changed = true -}}
{{- end -}}
{{- end -}}
{{- if .Values.config -}}
{{- $_ := include "quackmux.merge" (dict "dst" $cfg "src" .Values.config) -}}
{{- $changed = true -}}
{{- end -}}
{{- if ne (toString (dig "ha" "enabled" false $cfg)) (toString (eq .Values.ha.enabled true)) -}}
{{- fail (printf "the configuration sets ha.enabled %v, the chart value ha.enabled is %v: they must match (the chart value selects replicas, rollout strategy and Service selector)" (dig "ha" "enabled" false $cfg) (eq .Values.ha.enabled true)) -}}
{{- end -}}
{{- if eq (toString (dig "provider" "type" "kubernetes" $cfg)) "kubernetes" -}}
{{- $ns := toString (dig "provider" "kubernetes" "namespace" "" $cfg) -}}
{{- if and $ns (ne $ns .Release.Namespace) -}}
{{- fail (printf "the configuration launches engines in namespace %s (provider.kubernetes.namespace), the release is in %s: the chart supports engines in the release namespace only; set config.provider.kubernetes.namespace to %s or remove it" $ns .Release.Namespace .Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- if $changed -}}
{{- toYaml $cfg -}}
{{- else -}}
{{- $text -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.configMapName: <fullname>-config-<hash>, the hash the first 10 hex
digits of the SHA-256 of config.yaml (configmap.yaml).
*/}}
{{- define "quackmux.configMapName" -}}
{{- printf "%s-config-%s" (include "quackmux.fullname" .) (include "quackmux.config" . | sha256sum | trunc 10) -}}
{{- end -}}

{{/*
quackmux.affinity: the affinity of the values; with ha.enabled it has a
preferred podAntiAffinity (as overlays/ha) unless the values set
affinity.podAntiAffinity, so that a nodeAffinity alone keeps the replicas
spread over nodes.
*/}}
{{- define "quackmux.affinity" -}}
{{- $affinity := deepCopy (default dict .Values.affinity) -}}
{{- if and .Values.ha.enabled (not (hasKey $affinity "podAntiAffinity")) -}}
{{- $term := dict "topologyKey" "kubernetes.io/hostname" "labelSelector" (dict "matchLabels" (fromYaml (include "quackmux.selectorLabels" .))) -}}
{{- $_ := set $affinity "podAntiAffinity" (dict "preferredDuringSchedulingIgnoredDuringExecution" (list (dict "weight" 100 "podAffinityTerm" $term))) -}}
{{- end -}}
{{- with $affinity -}}
{{- toYaml . -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.seconds prints a duration of the gateway configuration in whole
seconds, rounded down, as config.Duration reads it: a Go duration such as
"30s" or "1m30s", a number of seconds (also as a string), "" or "0" for
none. Argument: the value; null prints nothing.
*/}}
{{- define "quackmux.seconds" -}}
{{- if kindIs "string" . -}}
{{- if or (eq . "") (eq . "0") -}}
0
{{- else if regexMatch "^[-+]?([0-9]+\\.?[0-9]*|\\.[0-9]+)([eE][-+]?[0-9]+)?$" . -}}
{{- int64 (floor (float64 .)) -}}
{{- else -}}
{{- $t0 := toDate "2006-01-02" "2000-01-01" -}}
{{- sub (int64 (unixEpoch (mustDateModify . $t0))) (int64 (unixEpoch $t0)) -}}
{{- end -}}
{{- else if not (kindIs "invalid" .) -}}
{{- int64 (floor (float64 .)) -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.checkGracePeriod: the kubelet kills the gateway
terminationGracePeriodSeconds after SIGTERM. Shortly before
server.shutdownTimeout the gateway answers the queries that still wait for
their engine; killed earlier, a request it has read ends without an answer,
and the client may send the query again. Prints nothing.
*/}}
{{- define "quackmux.checkGracePeriod" -}}
{{- $cfg := fromYaml (include "quackmux.config" .) -}}
{{- $shutdown := include "quackmux.seconds" (dig "server" "shutdownTimeout" nil $cfg) | default "30" | int64 -}}
{{- if le (int64 .Values.terminationGracePeriodSeconds) $shutdown -}}
{{- fail (printf "terminationGracePeriodSeconds %v must be greater than server.shutdownTimeout of the configuration (%ds), with a margin (45 for the default 30s): the kubelet would kill the gateway before it has answered every request it read" .Values.terminationGracePeriodSeconds $shutdown) -}}
{{- end -}}
{{- end -}}

{{/*
quackmux.checkHASwitch: switching an installed release to ha.enabled must not
start leaders next to the gateway without leader election that still runs
(both would manage the engines). The README asks to scale it to 0 first;
helm install and upgrade check that against the cluster (lookup). Prints
nothing.
*/}}
{{- define "quackmux.checkHASwitch" -}}
{{- if .Values.ha.enabled -}}
{{- $name := include "quackmux.fullname" . -}}
{{- $live := lookup "apps/v1" "Deployment" .Release.Namespace $name -}}
{{- if and $live (eq (toString (dig "spec" "strategy" "type" "" $live)) "Recreate") (gt (int (dig "spec" "replicas" 0 $live)) 0) -}}
{{- fail (printf "ha.enabled: deployment/%s runs without leader election; scale it to 0 first (kubectl -n %s scale deployment/%s --replicas=0), wait until its pod is gone, then upgrade" $name .Release.Namespace $name) -}}
{{- end -}}
{{- end -}}
{{- end -}}
