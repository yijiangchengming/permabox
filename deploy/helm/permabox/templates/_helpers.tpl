{{- define "permabox.installerName" -}}
{{- printf "%s-installer" .Release.Name -}}
{{- end -}}

{{- define "permabox.workloadGCName" -}}
{{- printf "%s-gc-%s" .Release.Name (.Release.Namespace | sha256sum | trunc 10) -}}
{{- end -}}

{{- define "permabox.selectorLabels" -}}
app.kubernetes.io/name: permabox-installer
app.kubernetes.io/instance: {{ .Release.Name | quote }}
{{- end -}}

{{- define "permabox.scheduling" -}}
{{- if not (omit .Values.nodeSelector "kubernetes.io/os") -}}
{{- fail "set nodeSelector to explicitly target K3s nodes (e.g. kubernetes.io/hostname)" -}}
{{- end -}}
{{- if and (not .Values.allowParallelInstall) (not (get .Values.nodeSelector "kubernetes.io/hostname")) -}}
{{- fail "select a single node using kubernetes.io/hostname, or explicitly set allowParallelInstall=true" -}}
{{- end -}}
nodeSelector:
  {{- dict "kubernetes.io/os" "linux" | mergeOverwrite (deepCopy .Values.nodeSelector) | toYaml | nindent 2 }}
{{- with .Values.tolerations }}
tolerations:
  {{- toYaml . | nindent 2 }}
{{- end }}
{{- end -}}
