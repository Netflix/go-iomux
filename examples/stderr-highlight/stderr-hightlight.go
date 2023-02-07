package main

import (
	"context"
	"encoding/binary"
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

func main() {
	mux := iomux.NewMuxUnixGram[OutputType]()
	defer mux.Close()
	cmd := exec.Command("sh", "-c", "echo out1 && echo err1 1>&2 && echo out2")
	stdout, err := mux.Tag(StdOut)
	if err != nil {
		panic(err)
	}
	stderr, _ := mux.Tag(StdErr)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	ctx, cancelFn := context.WithCancel(context.Background())
	cmd.Start()
	go func() {
		cmd.Wait()
		cancelFn()
	}()
	for {
		b, t, err := mux.Read(ctx)
		if err != nil {
			if err == io.EOF {
				break
			}
			panic(err)
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
	}
}
