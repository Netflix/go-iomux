package main

import (
	"encoding/binary"
	"errors"
	"github.com/netflix/iomux"
	"io"
	"os"
	"os/exec"
)

const colorRed = "\033[31m"
const colorReset = "\033[0m"

func main() {
	mux, err := iomux.NewMuxUnixGram[int]()
	if err != nil {
		panic(err)
	}
	cmd := exec.Command("sh", "-c", "echo out1 && echo err1 1>&2 && echo out2")
	stdout, _ := mux.Tag(0)
	stderr, _ := mux.Tag(1)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	done := make(chan bool)
	go func() {
		err := cmd.Run()
		if err != nil {
			panic(err)
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
				break
			}
		} else {
			switch t {
			case 0:
				binary.Write(os.Stdout, binary.BigEndian, b)
			case 1:
				io.WriteString(os.Stderr, colorRed)
				binary.Write(os.Stderr, binary.BigEndian, b)
				io.WriteString(os.Stderr, colorReset)
			}
		}
	}
}
