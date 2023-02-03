package main

import (
	"encoding/binary"
	"errors"
	"github.com/netflix/go-iomux"
	"io"
	"os"
	"os/exec"
)

type OutputType int

const (
	StdOut OutputType = iota
	StdErr
)

const colorRed = "\033[31m"
const colorReset = "\033[0m"

// ignore errs for brevity in this example, you should handle these appropriately
func main() {
	mux, _ := iomux.NewMuxUnixGram[OutputType]()
	defer mux.Close()
	cmd := exec.Command("sh", "-c", "echo out1 && echo err1 1>&2 && echo out2")
	stdout, _ := mux.Tag(StdOut)
	stderr, _ := mux.Tag(StdErr)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	done := make(chan bool)
	var runErr error
	go func() {
		err := cmd.Run()
		if err != nil {
			runErr = err
		}
		done <- true
	}()

	cmdDone := false
	for {
		select {
		case cmdDone = <-done:
		default:
		}
		b, t, err := mux.Read()
		if err != nil {
			if errors.Unwrap(err) != os.ErrDeadlineExceeded {
				panic(err)
			} else if cmdDone {
				// If this wasn't an unixgram, you'd need to read until you saw deadline exceeded n times,
				// where n is the number of tags registered
				break
			}
		} else {
			switch t {
			case StdOut:
				binary.Write(os.Stdout, binary.BigEndian, b)
			case StdErr:
				io.WriteString(os.Stderr, colorRed)
				binary.Write(os.Stderr, binary.BigEndian, b)
				io.WriteString(os.Stderr, colorReset)
			}
		}
		if runErr != nil {
			runErr = errors.Unwrap(runErr)
			if exitError, ok := runErr.(*exec.ExitError); ok {
				os.Exit(exitError.ProcessState.ExitCode())
			}
		}
	}
}
