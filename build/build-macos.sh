#!/bin/bash

APP_NAME="app"

echo "[Info] start building app"

export GOOS=darwin
export CGO_ENABLED=0

go version
if [ $? -ne 0 ]; then
    echo "[Error] failed to find go. Please install Go or check PATH."
    exit 1
fi

if [ ! -d "../bin" ]; then
    mkdir -p "../bin"
    echo "[Info] create ../bin"
fi

# Build for Intel Mac (amd64)
echo "[Info] building for macOS amd64"
export GOARCH=amd64
go build -o "../bin/${APP_NAME}_darwin_amd64" ../cmd/golalink

if [ $? -ne 0 ]; then
    echo "[Error] failed to build amd64"
    exit 1
fi

# Build for Apple Silicon (arm64)
echo "[Info] building for macOS arm64"
export GOARCH=arm64
go build -o "../bin/${APP_NAME}_darwin_arm64" ../cmd/golalink

if [ $? -ne 0 ]; then
    echo "[Error] failed to build arm64"
    exit 1
fi

echo "[Success] output files:"
echo "../bin/${APP_NAME}_darwin_amd64"
echo "../bin/${APP_NAME}_darwin_arm64"