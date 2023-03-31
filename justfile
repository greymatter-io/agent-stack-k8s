default:
  just --list

run *FLAGS:
  go run ./... {{FLAGS}}

test *FLAGS:
  #!/usr/bin/env bash

  set -exufo pipefail

  GIT_BRANCH=$(git rev-parse --abbrev-ref HEAD)

  go test \
    -ldflags="-X github.com/buildkite/agent-stack-k8s/v2/internal/integration_test.branch=${GIT_BRANCH}" \
    {{FLAGS}} \
    ./...

lint *FLAGS: gomod
  golangci-lint run {{FLAGS}}

generate:
  go run github.com/Khan/genqlient api/genqlient.yaml
  go generate ./...

gomod:
  #!/usr/bin/env sh
  set -euf

  go mod tidy
  git diff -G. --no-ext-diff --exit-code go.mod go.sum

agent target os=("linux") arch=("amd64"):
  #!/usr/bin/env bash
  set -euxo pipefail
  pushd agent
  version=$(git describe --tags)
  platforms=()
  for os in {{os}}; do
    for arch in {{arch}}; do
      platforms+=("${os}/${arch}")
      ./scripts/build-binary.sh $os $arch $version
    done
  done
  commaified=$(IFS=, ; echo "${platforms[*]}")
  mkdir -p {{justfile_directory()}}/dist
  mv pkg/buildkite-agent-* packaging/docker/alpine/
  docker buildx build --tag {{target}} --platform "$commaified" --push --metadata-file {{justfile_directory()}}/dist/metadata.json packaging/docker/alpine
  rm packaging/docker/alpine/buildkite-agent-*

controller *FLAGS:
  #!/usr/bin/env bash
  set -eufo pipefail

  export VERSION=$(git describe)
  ko build --preserve-import-paths {{FLAGS}}

deploy *FLAGS:
  #!/usr/bin/env bash
  set -euxo pipefail

  helm upgrade agent-stack-k8s charts/agent-stack-k8s \
    --namespace buildkite \
    --install \
    --create-namespace \
    --wait \
    {{FLAGS}}


# version should be a semver version like `0.1.0`
release version:
  ./scripts/release.sh {{version}}

cleanup-orphans:
  go test -v -run TestCleanupOrphanedPipelines ./integration --delete-orphaned-pipelines
