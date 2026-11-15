# Cross-compiles the Windows program from Linux or macOS, and runs the tests.
.PHONY: all windows test clean

all: windows

windows:
	GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-H windowsgui -s -w" -o bin/winairplay.exe ./cmd/winairplay

test:
	go vet ./...
	GOOS=windows go vet ./...
	go test ./...

clean:
	rm -rf bin dist
