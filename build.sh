#!/bin/bash
rm  -rf build
mkdir -p build/
GO111MODULE=on CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -v -ldflags="-s -w -X main.version=${VERSION}" -o ./build/khostdns

