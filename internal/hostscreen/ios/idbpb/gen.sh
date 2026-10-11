#!/bin/sh
# Regenerates idb.pb.go and idb_grpc.pb.go from idb.proto (run by
# `go generate`). protoc comes from grpcio-tools through uv; the versions
# match go.mod's google.golang.org/grpc and google.golang.org/protobuf.
set -eu
cd "$(dirname "$0")"
bin=$(mktemp -d)
trap 'rm -rf "$bin"' EXIT
GOBIN=$bin go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12
GOBIN=$bin go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.6.2
uv run --no-project --with grpcio-tools==1.84.0 python -m grpc_tools.protoc -I. \
	--plugin=protoc-gen-go="$bin/protoc-gen-go" --plugin=protoc-gen-go-grpc="$bin/protoc-gen-go-grpc" \
	--go_out=. --go_opt=paths=source_relative \
	--go-grpc_out=. --go-grpc_opt=paths=source_relative \
	idb.proto
