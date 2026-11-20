{{/*
Expand the name of the chart.
*/}}
{{- define "velocor-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "velocor-csi.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
app.kubernetes.io/name: {{ include "velocor-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
CSI image reference
*/}}
{{- define "velocor-csi.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end }}

{{/*
Secret name for Velocor API credentials
*/}}
{{- define "velocor-csi.secretName" -}}
{{- if .Values.auth.existingSecret -}}
{{- .Values.auth.existingSecret -}}
{{- else -}}
{{- .Values.auth.secretName -}}
{{- end -}}
{{- end }}

{{/*
The VolumeGroupSnapshot API the cluster serves: v1 (GA; OpenShift 5 / external-snapshotter
v8.5+ CRDs) if present, else v1beta1, else "".
*/}}
{{- define "velocor-csi.groupSnapshotAPI" -}}
{{- if .Capabilities.APIVersions.Has "groupsnapshot.storage.k8s.io/v1" -}}
groupsnapshot.storage.k8s.io/v1
{{- else if .Capabilities.APIVersions.Has "groupsnapshot.storage.k8s.io/v1beta1" -}}
groupsnapshot.storage.k8s.io/v1beta1
{{- end -}}
{{- end }}

{{/*
Changed-block tracking (snapshotMetadata): the sidecar's TLS secret, and the CA (PEM) clients
verify it with. On OpenShift the service CA issues the certificate (annotation on the Service)
and its bundle is in every namespace's openshift-service-ca.crt ConfigMap.
*/}}
{{- define "velocor-csi.snapshotMetadataTLSSecret" -}}
{{- default "velocor-csi-snapshot-metadata-tls" .Values.snapshotMetadata.tlsSecretName -}}
{{- end -}}

{{- define "velocor-csi.snapshotMetadataCA" -}}
{{- if .Values.snapshotMetadata.caCert -}}
{{- .Values.snapshotMetadata.caCert -}}
{{- else -}}
{{- $cm := lookup "v1" "ConfigMap" .Release.Namespace "openshift-service-ca.crt" -}}
{{- if and $cm (index $cm.data "service-ca.crt") -}}
{{- index $cm.data "service-ca.crt" -}}
{{- end -}}
{{- end -}}
{{- end -}}
