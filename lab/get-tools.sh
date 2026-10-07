#!/usr/bin/env bash
# Fetch the pinned lab binaries into .bin/ and verify their SHA-256 sums.
#   lab/get-tools.sh              kind, kubectl and promtool
#   lab/get-tools.sh promtool     only the named tools
set -euo pipefail
KIND_VERSION=v0.33.0
KUBECTL_VERSION=v1.37.1
PROMETHEUS_VERSION=3.14.0       # the node's Prometheus (chart 29.33.0); promtool tests deploy/observability/alerts.yaml
os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m); [ "$arch" = x86_64 ] && arch=amd64; [ "$arch" = aarch64 ] && arch=arm64
cd "$(dirname "$0")/.." && mkdir -p .bin && cd .bin
want=${*:-kind kubectl promtool}

for tool in $want; do
  case "$tool" in
    kind)
      curl -sfLo kind "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${os}-${arch}"
      curl -sfL "https://github.com/kubernetes-sigs/kind/releases/download/${KIND_VERSION}/kind-${os}-${arch}.sha256sum" | awk '{print $1"  kind"}' | shasum -a 256 -c -
      chmod +x kind ;;
    kubectl)
      curl -sfLo kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${os}/${arch}/kubectl"
      echo "$(curl -sfL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/${os}/${arch}/kubectl.sha256")  kubectl" | shasum -a 256 -c -
      chmod +x kubectl ;;
    promtool)
      # Sums pinned here from the release's sha256sums.txt, not fetched alongside the archive.
      case "$os-$arch" in
        darwin-arm64) sum=a9623f7f4fe65b1b171b423c1a72bbf23dfdf41a171dcb33e7dd302af80dc01c ;;
        darwin-amd64) sum=a14307b9726e66cadb81be9a544732623af26dabeb7702c987aa9c3c062ada34 ;;
        linux-amd64)  sum=f665c6da19eb7ba399c915d30c7d9793c9b417bf8a749b504bc470678631478d ;;
        linux-arm64)  sum=077f3781ab7245dc04c9a3c9b78ba120fc8e41aa0dc97489b0af67247e50ba83 ;;
        *) echo "no pinned promtool for $os-$arch" >&2; exit 1 ;;
      esac
      name="prometheus-${PROMETHEUS_VERSION}.${os}-${arch}"
      tmp=$(mktemp -d)
      curl -sfLo "$tmp/$name.tar.gz" "https://github.com/prometheus/prometheus/releases/download/v${PROMETHEUS_VERSION}/$name.tar.gz"
      echo "$sum  $tmp/$name.tar.gz" | shasum -a 256 -c -
      tar -xzf "$tmp/$name.tar.gz" -C "$tmp" "$name/promtool"
      install -m 755 "$tmp/$name/promtool" promtool
      rm -rf "$tmp" ;;
    *) echo "unknown tool $tool (kind, kubectl, promtool)" >&2; exit 1 ;;
  esac
done
