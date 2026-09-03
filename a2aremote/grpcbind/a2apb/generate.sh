#!/usr/bin/env bash
# Regenerates the A2A protobuf bindings from the normative proto.
#
# a2a.proto is vendored verbatim from github.com/a2aproject/A2A at
# specification/a2a.proto. It is the canonical data model the three
# protocol bindings are derived from, so hand-editing either it or the
# generated files means cortex stops speaking the protocol it claims to.
#
# The googleapis imports are fetched rather than vendored, because they
# are large, stable, and not ours.
set -euo pipefail

cd "$(dirname "$0")"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cp a2a.proto "$work/"
mkdir -p "$work/google/api"
for f in annotations http client field_behavior launch_stage; do
    gh api "repos/googleapis/googleapis/contents/google/api/$f.proto" --jq '.content' \
        | base64 -d > "$work/google/api/$f.proto"
done

protoc -I"$work" \
    --go_out="$work" --go_opt=Ma2a.proto=github.com/xraph/cortex/a2aremote/grpcbind/a2apb \
    --go-grpc_out="$work" --go-grpc_opt=Ma2a.proto=github.com/xraph/cortex/a2aremote/grpcbind/a2apb \
    "$work/a2a.proto"

cp "$work"/github.com/xraph/cortex/a2aremote/grpcbind/a2apb/*.pb.go .

# protoc-gen-go names the package from the proto's own package (v1),
# which is not the directory it lands in. Renaming it is the one edit
# these generated files get.
sed -i '' 's/^package v1$/package a2apb/' *.pb.go
# protoc-gen-go's import grouping is not what gofmt produces in this Go
# version, so the generated files fail a format check straight out of the
# generator. Formatting here keeps regeneration from reintroducing it.
gofmt -w ./*.pb.go

echo "regenerated $(ls *.pb.go | tr '\n' ' ')"
