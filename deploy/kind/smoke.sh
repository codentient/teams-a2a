#!/usr/bin/env bash
# End-to-end smoke test of the container image and Helm chart in a throwaway kind cluster.
set -euo pipefail
cd "$(dirname "$0")/../.."
CLUSTER=${CLUSTER:-teams-a2a-smoke}

docker build -q -t teams-a2a-gateway:dev .
docker build -q -f deploy/kind/Dockerfile.tools -t teams-a2a-tools:dev .
kind get clusters | grep -qx "$CLUSTER" || kind create cluster --name "$CLUSTER" --wait 120s
kind load docker-image --name "$CLUSTER" teams-a2a-gateway:dev teams-a2a-tools:dev
CTX="kind-$CLUSTER"

kubectl --context "$CTX" delete job teams-sim --ignore-not-found
kubectl --context "$CTX" apply -f deploy/kind/smoke.yaml
helm --kube-context "$CTX" upgrade --install gw deploy/helm/teams-a2a-gateway -f deploy/kind/values.yaml --wait --timeout 180s
kubectl --context "$CTX" wait --for=condition=complete job/teams-sim --timeout=180s
kubectl --context "$CTX" logs job/teams-sim
