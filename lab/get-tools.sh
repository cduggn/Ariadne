#!/usr/bin/env bash
# Fetch the pinned lab binaries into .bin/ and verify their SHA-256 sums.
#   lab/get-tools.sh              kind, kubectl, promtool, golangci-lint and gitleaks
#   lab/get-tools.sh promtool     only the named tools
set -euo pipefail
KIND_VERSION=v0.33.0
KUBECTL_VERSION=v1.37.1
PROMETHEUS_VERSION=3.14.0       # the node's Prometheus (chart 29.33.0); promtool tests deploy/observability/alerts.yaml
GOLANGCI_LINT_VERSION=2.14.0    # gateway lint, gateway/.golangci.yml (D-47)
GITLEAKS_VERSION=8.30.1         # secret scan in the git hooks and CI (D-47)
os=$(uname -s | tr '[:upper:]' '[:lower:]'); arch=$(uname -m); [ "$arch" = x86_64 ] && arch=amd64; [ "$arch" = aarch64 ] && arch=arm64
cd "$(dirname "$0")/.." && mkdir -p .bin && cd .bin
want=${*:-kind kubectl promtool golangci-lint gitleaks}

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
    golangci-lint)
      case "$os-$arch" in
        darwin-arm64) sum=5ef5f36a7147e91dc58ef9ef4d11bb7bad5ead0c76eb6c01327a73c641d1dcc3 ;;
        darwin-amd64) sum=a5667c1c3536be1740133213e1e822bfb8f0d98ea12903174d6d5f635e4ed68d ;;
        linux-amd64)  sum=ab90aeb7b066f92a33415b638a50fe5344bbb75a0d32ad30cc248d88f81032ab ;;
        linux-arm64)  sum=ee7ec5f3453d15ddf106fae5a4d6c71737712348a979d1fe9cd52ec7ea299bae ;;
        *) echo "no pinned golangci-lint for $os-$arch" >&2; exit 1 ;;
      esac
      name="golangci-lint-${GOLANGCI_LINT_VERSION}-${os}-${arch}"
      tmp=$(mktemp -d)
      curl -sfLo "$tmp/$name.tar.gz" "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/$name.tar.gz"
      echo "$sum  $tmp/$name.tar.gz" | shasum -a 256 -c -
      tar -xzf "$tmp/$name.tar.gz" -C "$tmp" "$name/golangci-lint"
      install -m 755 "$tmp/$name/golangci-lint" golangci-lint
      rm -rf "$tmp" ;;
    gitleaks)
      case "$os-$arch" in
        darwin-arm64) sum=b40ab0ae55c505963e365f271a8d3846efbc170aa17f2607f13df610a9aeb6a5; garch=arm64 ;;
        darwin-amd64) sum=dfe101a4db2255fc85120ac7f3d25e4342c3c20cf749f2c20a18081af1952709; garch=x64 ;;
        linux-amd64)  sum=551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb; garch=x64 ;;
        linux-arm64)  sum=e4a487ee7ccd7d3a7f7ec08657610aa3606637dab924210b3aee62570fb4b080; garch=arm64 ;;
        *) echo "no pinned gitleaks for $os-$arch" >&2; exit 1 ;;
      esac
      name="gitleaks_${GITLEAKS_VERSION}_${os}_${garch}"
      tmp=$(mktemp -d)
      curl -sfLo "$tmp/$name.tar.gz" "https://github.com/gitleaks/gitleaks/releases/download/v${GITLEAKS_VERSION}/$name.tar.gz"
      echo "$sum  $tmp/$name.tar.gz" | shasum -a 256 -c -
      tar -xzf "$tmp/$name.tar.gz" -C "$tmp" gitleaks
      install -m 755 "$tmp/gitleaks" gitleaks
      rm -rf "$tmp" ;;
    *) echo "unknown tool $tool (kind, kubectl, promtool, golangci-lint, gitleaks)" >&2; exit 1 ;;
  esac
done
