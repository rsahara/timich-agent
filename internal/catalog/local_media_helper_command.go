package catalog

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"time"
)

func runLocalMediaHelperCommandContext(ctx context.Context, timeout time.Duration, helperPath string, vipsPath string, ffmpegPath string, args ...string) ([]byte, error) {
	return runLocalMediaHelperCommandWithInputFileContext(ctx, timeout, helperPath, vipsPath, ffmpegPath, nil, args...)
}

func runLocalMediaHelperCommandWithInputFileContext(ctx context.Context, timeout time.Duration, helperPath string, vipsPath string, ffmpegPath string, inputFile *os.File, args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.Command(helperPath, args...)
	command.Env = localMediaHelperCommandEnv(os.Environ(), vipsPath, ffmpegPath)
	if inputFile != nil {
		if _, err := inputFile.Seek(0, io.SeekStart); err != nil {
			return nil, err
		}
		command.ExtraFiles = []*os.File{inputFile}
	}
	configureHelperProcessGroup(command)

	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output

	if err := command.Start(); err != nil {
		return output.Bytes(), err
	}
	done := make(chan error, 1)
	go func() {
		done <- command.Wait()
	}()

	select {
	case err := <-done:
		return output.Bytes(), err
	case <-runCtx.Done():
		_ = killHelperProcessGroup(command)
		err := <-done
		if err != nil {
			return output.Bytes(), runCtx.Err()
		}
		return output.Bytes(), runCtx.Err()
	}
}
