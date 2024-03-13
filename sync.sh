#!/bin/bash

set -e

cd cmd/internal2pkg
go build
cd ../..

TOOLS_DIR=$HOME/tools

rsync -av --delete-excluded --itemize-changes \
  --exclude='*/testdata' \
  --exclude='*/*_test.go' \
  --include-from=sync.list \
  --exclude='*' \
  "$TOOLS_DIR"/ ./staging/

find ./staging/ -type f -exec sed -i 's/"golang.org\/x\/tools\//\"github.com\/kralicky\/tools-lite\//g' {} \;

rm -rf ./gopls ./internal ./pkg ./go ./txtar
mv ./staging/gopls .
mv ./staging/internal .
mv ./staging/go .
mv ./staging/txtar .
rmdir ./staging

git apply -v .patches/*.patch
go generate ./gopls/internal/protocol

./cmd/internal2pkg/internal2pkg ./internal
./cmd/internal2pkg/internal2pkg ./gopls/internal

while IFS= read -r line; do
  package=$(echo "$line" | rev | cut -d. -f2- | rev)
  symbol=$(echo "$line" | rev | cut -d. -f1 | rev)
  exportedSymbol=$(echo "$symbol" | awk '{first=toupper(substr($0,1,1)); rest=substr($0,2); print first rest}')
  gorename -from $package.$symbol -to $exportedSymbol
done <exports.txt

go mod tidy
go vet ./...
