#!/usr/bin/env bash
# sync-crds.sh: wrap the generated CRDs for the Helm chart.
# Usage: make chart-crds  (which runs this script after make manifests)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../" && pwd)"

CRD_SOURCE_DIR="$REPO_ROOT/config/crd/bases"
CRD_TARGET_DIR="$REPO_ROOT/charts/meitheal/templates/crds"

# Validate source CRDs exist
if ! compgen -G "$CRD_SOURCE_DIR/meitheal.dev_*.yaml" > /dev/null; then
  echo "No meitheal.dev CRDs in $CRD_SOURCE_DIR (did make manifests run?)"
  exit 1
fi

# Ensure target directory exists
mkdir -p "$CRD_TARGET_DIR"

synced=0
for src in "$CRD_SOURCE_DIR"/meitheal.dev_*.yaml; do
  # Strip the group prefix: meitheal.dev_agents.yaml -> agents.yaml
  base="$(basename "$src")"
  short="${base#meitheal.dev_}"
  dst="$CRD_TARGET_DIR/$short"

  echo "Syncing $base -> $short"

  # Wrap with the Helm install conditional and inject the resource-policy
  # keep annotation, so `helm uninstall` leaves the CRDs (and every custom
  # resource) in place unless crds.keep is turned off.
  {
    echo '{{- if .Values.crds.install }}'
    awk '/controller-gen.kubebuilder.io\/version:/{print; print "    {{- if .Values.crds.keep }}"; print "    helm.sh/resource-policy: keep"; print "    {{- end }}"; next}1' "$src"
    echo '{{- end }}'
  } > "$dst"

  synced=$((synced + 1))
done

echo "Synced $synced CRD(s)"
