# Cross-compile single executables into dist/
.PHONY: build test dist
build:
	go build -o oncallcalc .
test:
	go test ./...
dist:
	GOOS=darwin  GOARCH=arm64 go build -o dist/oncallcalc-mac-apple-silicon .
	GOOS=darwin  GOARCH=amd64 go build -o dist/oncallcalc-mac-intel .
	GOOS=windows GOARCH=amd64 go build -o dist/oncallcalc-windows-amd64.exe .
	GOOS=linux   GOARCH=amd64 go build -o dist/oncallcalc-linux-amd64 .
