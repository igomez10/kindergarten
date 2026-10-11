{{- define "pgcloud.labels" -}}
app.kubernetes.io/part-of: pgcloud
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "pgcloud.podSecurity" -}}
securityContext:
  runAsNonRoot: true
  runAsUser: 65532
  runAsGroup: 65532
  seccompProfile:
    type: RuntimeDefault
{{- end -}}

{{- define "pgcloud.containerSecurity" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
{{- end -}}

{{- define "pgcloud.databaseURL" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef: {name: {{ .Values.metadataDBSecret }}, key: uri}
{{- end -}}
