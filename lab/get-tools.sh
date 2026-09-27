#!/usr/bin/env bash
# Fetch the pinned lab binaries into .bin/ and verify their published SHA-256 sums.
set -euo pipefail
KIND_VERSION=v0.33.0
KUBECTL_VERSION=v1.37.1
os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m); [ "$arch" = x86_64 ] && arch=amd64; [ "$arch" = aarch64 ] && arch=arm64
cd "$(dirname "$0")/.." && mkdir -p .bin && cd .bin
curl -sfLo kind "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${os}-${arch}"
curl -sfL "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${os}-${arch}.sha256sum" | awk '{print $1"  kind"}' | shasum -a 256 -c -
curl -sfLo kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${os}/${arch}/kubectl"
echo "$(curl -sfL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${os}/${arch}/kubectl.sha256")  kubectl" | shasum -a 256 -c -
chmod +x kind kubectl
