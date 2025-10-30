#!/bin/bash
# 构建多平台版本

mkdir -p dist

# Windows
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o dist/go-chat.exe main.go

# macOS Intel
GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build -o dist/go-chat-mac-intel main.go

# macOS Apple Silicon
GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build -o dist/go-chat-mac-arm main.go

# Linux
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o dist/go-chat-linux main.go

# 复制资源
# cp -r public dist/
# mkdir dist/uploads
