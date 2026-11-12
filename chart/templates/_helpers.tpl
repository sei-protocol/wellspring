{{- define "wellspring.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "wellspring.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else if contains (include "wellspring.name" .) .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name (include "wellspring.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}

{{- define "wellspring.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "wellspring.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "wellspring.selectorLabels" -}}
app.kubernetes.io/name: {{ include "wellspring.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "wellspring.labels" -}}
{{ include "wellspring.selectorLabels" . }}
app.kubernetes.io/version: {{ include "wellspring.imageTag" . | trunc 63 | trimSuffix "-" | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "wellspring.imageTag" -}}
{{- .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{/*
The image at its tag. A published chart's own image, used when no tag is set,
is also pinned to the digest it was built with.
*/}}
{{- define "wellspring.image" -}}
{{- $digest := .Values.image.digest }}
{{- if and (not .Values.image.tag) .Chart.Annotations }}
{{- $digest = $digest | default (index .Chart.Annotations "sei.io/image-digest") }}
{{- end }}
{{- .Values.image.repository }}:{{ include "wellspring.imageTag" . }}{{ with $digest }}@{{ . }}{{ end }}
{{- end }}

{{- define "wellspring.captchaSecret" -}}
{{- if ne (index .Values.config "captcha-provider" | default "turnstile") "none" }}
{{- required "captcha.existingSecret is required unless config.captcha-provider is none" .Values.captcha.existingSecret }}
{{- end }}
{{- end }}
