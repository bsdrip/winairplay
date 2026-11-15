//go:build !windows || !(amd64 || arm64)

package airplay

import (
	"context"
	"fmt"
)

func runLoopbackCapture(_ context.Context, _ *pcmQueue, ready chan<- error) error {
	err := fmt.Errorf("sound capture is available only on 64-bit Windows")
	ready <- err
	return err
}
