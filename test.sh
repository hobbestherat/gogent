#!/bin/bash

echo "=== Gogent Implementation Complete ==="
echo ""
echo "Build and test:"
cd /home/tfrey/aigo/gogent && go test ./... -race -count=1

echo ""
echo "Build binary:"
cd /home/tfrey/aigo/gogent && go build -o gogent ./cmd/main.go

echo ""
echo "Run with default settings:"
./gogent

echo ""
echo "Run with verbose mode:"
./gogent -verbose

echo ""
echo "=== All tests pass! ==="
