# Cross-compiles the Windows programs from Linux or macOS, and runs the tests.
.PHONY: all windows test clean

all: windows

windows:
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui -s -w" -o bin/winairplay.exe ./cmd/winairplay
	GOOS=windows GOARCH=amd64 go build -trimpath -o bin/winairplay-cli.exe ./cmd/winairplay-cli
	GOOS=windows GOARCH=amd64 go build -trimpath -o bin/winairplay-test-receiver.exe ./cmd/winairplay-test-receiver

test:
	go vet ./...
	GOOS=windows go vet ./...
	go test ./...

clean:
	rm -rf bin
