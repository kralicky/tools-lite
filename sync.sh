#!/bin/bash

set -e

# sync all the files found in sync.list here. sync.list is a list of directories

cd cmd/internal2pkg
go build
cd ../..

TOOLS_FORK_DIR=$HOME/tools

rsync -av --delete-excluded --itemize-changes \
  --exclude='*/testdata' \
  --exclude='*/*_test.go' \
  --include-from=sync.list \
  --exclude='*' \
  "$TOOLS_FORK_DIR"/ ./staging/

# in staging, replace all instances of '"golang.org/x/tools/' with '"github.com/kralicky/tools-lite"
find ./staging/ -type f -exec sed -i 's/"golang.org\/x\/tools\//\"github.com\/kralicky\/tools-lite\//g' {} \;

rm -rf ./gopls ./internal ./pkg ./go ./txtar
mv ./staging/gopls .
mv ./staging/internal .
mv ./staging/go .
mv ./staging/txtar .
rmdir ./staging

git apply .patches/*.patch
go generate ./gopls/internal/protocol

./cmd/internal2pkg/internal2pkg ./internal
./cmd/internal2pkg/internal2pkg ./gopls/internal

go mod tidy
go vet ./...
