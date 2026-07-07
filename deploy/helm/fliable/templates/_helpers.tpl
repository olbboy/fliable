{{- define "fliable.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fliable.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "fliable.name" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "fliable.labels" -}}
app.kubernetes.io/name: {{ include "fliable.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}
