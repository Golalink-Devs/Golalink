// Package process runs media helpers with cancellation and bounded output.
package process

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

type Buffer struct {
	bytes.Buffer
	Limit int
}

func (b *Buffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.Limit {
		return 0, errors.New("helper output exceeded limit")
	}
	return b.Buffer.Write(p)
}
func Run(ctx context.Context, executable string, stdin io.Reader, limit int, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stdin = stdin
	out := &Buffer{Limit: limit}
	stderr := &Buffer{Limit: 32 << 10}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s failed: %w: %.1000s", executable, err, stderr.String())
	}
	return out.Bytes(), nil
}
