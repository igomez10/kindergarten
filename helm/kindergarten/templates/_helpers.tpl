{{- define "kindergarten.labels" -}}
app.kubernetes.io/part-of: kindergarten
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "kindergarten.podSecurity" -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
{{- end -}}

{{- define "kindergarten.containerSecurity" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
{{- end -}}

{{- define "kindergarten.databaseURL" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef: {name: {{ .Values.metadataDBSecret }}, key: uri}
{{- end -}}
