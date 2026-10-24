{{- define "sandherd.metadata" -}}
{{- $root := .root -}}
{{- $object := .object -}}
{{- if ne $object.kind "ClusterRoleBinding" -}}
{{- $_ := set $object.metadata "namespace" $root.Release.Namespace -}}
{{- end -}}
{{- $labels := default (dict) $object.metadata.labels -}}
{{- $_ := set $labels "app.kubernetes.io/instance" $root.Release.Name -}}
{{- $_ := set $labels "app.kubernetes.io/managed-by" "Helm" -}}
{{- $_ := set $object.metadata "labels" $labels -}}
{{- end -}}

{{- define "sandherd.network" -}}
{{- $root := .root -}}
{{- $object := .object -}}
{{- /* Namespace selectors are reviewed constants in the source manifests. */ -}}
{{- $encoded := toJson $object | replace "sandherd-system" $root.Release.Namespace | replace "agent-sandbox-system" $root.Release.Namespace -}}
{{- $updated := fromJson $encoded -}}
{{- if or (eq $object.metadata.name "sandherd-control-plane") (eq $object.metadata.name "sandbox-router") -}}
{{- $egress := list -}}
{{- range $rule := $updated.spec.egress -}}
{{- $apiRule := false -}}
{{- range $destination := $rule.to -}}
{{- if eq (dig "namespaceSelector" "matchLabels" "kubernetes.io/metadata.name" "" $destination) "default" -}}
{{- $apiRule = true -}}
{{- end -}}
{{- end -}}
{{- if not $apiRule -}}{{- $egress = append $egress $rule -}}{{- end -}}
{{- end -}}
{{- range $cidr := $root.Values.network.apiServerCIDRs -}}
{{- $egress = append $egress (dict "to" (list (dict "ipBlock" (dict "cidr" $cidr))) "ports" (list (dict "protocol" "TCP" "port" 443) (dict "protocol" "TCP" "port" 6443))) -}}
{{- end -}}
{{- $_ := set $updated.spec "egress" $egress -}}
{{- end -}}
{{- include "sandherd.metadata" (dict "root" $root "object" $updated) -}}
{{ toYaml $updated }}
{{- end -}}
