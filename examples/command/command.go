package main

import (
	"encoding/binary"
	"errors"
	"github.com/netflix/iomux"
	"io"
	"os"
	"os/exec"
)

type OutputType int

const (
	StdOut OutputType = iota
	StdErr
)

func main() {
	mux, _ := iomux.NewMux[OutputType]()
	defer mux.Close()
	cmd := exec.Command("sh", "-c", "echo out1 && echo err1 1>&2 && echo out2")
	stdout, _ := mux.Tag(StdOut)
	stderr, _ := mux.Tag(StdErr)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	taggedData, err := mux.ReadWhile(func() error {
		err := cmd.Run()
		return err
	})
	for _, td := range taggedData {
		var w io.Writer
		switch td.Tag {
		case StdOut:
			w = os.Stdout
		case StdErr:
			w = os.Stderr
		}
		binary.Write(w, binary.BigEndian, td.Data)
	}
	if err != nil {
		err = errors.Unwrap(err)
		if exitError, ok := err.(*exec.ExitError); ok {
			os.Exit(exitError.ProcessState.ExitCode())
		}
	}
}
