# Cross-compile single executables into dist/
.PHONY: build test dist
build:
	go build -o oncallcalc .
test:
	go test ./...
dist:
	GOOS=darwin  GOARCH=arm64 go build -o dist/oncallcalc-macos-arm64 .
	GOOS=darwin  GOARCH=amd64 go build -o dist/oncallcalc-macos-amd64 .
	GOOS=windows GOARCH=amd64 go build -o dist/oncallcalc-windows-amd64.exe .
	GOOS=linux   GOARCH=amd64 go build -o dist/oncallcalc-linux-amd64 .
