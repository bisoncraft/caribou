#!/usr/bin/env bash
# Regenerates the Go protobuf and gRPC code for the lightwalletd protocol from
# the .proto files in this directory.
#
# The .proto files are unmodified copies from
# https://github.com/zcash/lightwallet-protocol at the tag named in doc.go. To
# move to a newer protocol version, replace them, update doc.go, and run this
# script. The version must be the one that the pinned zcash_client_backend
# crate was built against (see its lightwallet-protocol/CHANGELOG.md), because
# tree states and compact blocks cross from this package into the Rust backend
# as encoded bytes.
#
# Requires protoc and Go. The two protoc plugins are built at pinned versions
# into a temporary directory.
set -euo pipefail

PROTOC_GEN_GO_VERSION=v1.36.11
PROTOC_GEN_GO_GRPC_VERSION=v1.6.1
PKG=github.com/bisoncraft/caribou/wallet/asset/zec/zeclib/walletrpc

cd "$(dirname "$0")"

command -v protoc >/dev/null || { echo "protoc not found in PATH" >&2; exit 1; }

tools=$(mktemp -d)
trap 'rm -rf "$tools"' EXIT
GOBIN="$tools" go install "google.golang.org/protobuf/cmd/protoc-gen-go@${PROTOC_GEN_GO_VERSION}"
GOBIN="$tools" go install "google.golang.org/grpc/cmd/protoc-gen-go-grpc@${PROTOC_GEN_GO_GRPC_VERSION}"

# The files declare go_package "lightwalletd/walletrpc". The M options map
# them to this package without editing the upstream files.
map="Mcompact_formats.proto=${PKG},Mservice.proto=${PKG}"
PATH="$tools:$PATH" protoc --proto_path=. \
	--go_out=. --go_opt=paths=source_relative --go_opt="$map" \
	--go-grpc_out=. --go-grpc_opt=paths=source_relative --go-grpc_opt="$map" \
	compact_formats.proto service.proto
