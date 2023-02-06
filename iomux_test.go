package iomux

import (
	"encoding/binary"
	"errors"
	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"
)

var networks = []string{
	"unix", "unixgram", "unixpacket",
}

func TestMuxReadWhileErr(t *testing.T) {
	mux, err := NewMux[string]()
	assert.Nil(t, err)
	_, err = mux.Tag("a")
	assert.Nil(t, err)

	expected := errors.New("this is an error")
	_, err = mux.ReadWhile(func() error {
		return expected
	})

	assert.ErrorIs(t, expected, err)
}

func TestMuxReadNoSenders(t *testing.T) {
	mux, err := NewMux[string]()
	assert.Nil(t, err)

	data, tag, err := mux.Read()

	assert.Nil(t, data)
	assert.Equal(t, "", tag)
	assert.ErrorIs(t, err, MuxNoConnections)
}

func TestMuxReadClosed(t *testing.T) {
	mux, _ := NewMux[string]()
	mux.Close()
	_, _, err := mux.Read()

	assert.ErrorIs(t, err, MuxClosed)
}

func TestMuxRead(t *testing.T) {
	for _, network := range networks {
		t.Run(network, func(t *testing.T) {
			mux, err := newMux[string](network)
			if err != nil {
				skipIfProtocolNotSupported(t, err, network)
				assert.Nil(t, err)
			}

			taga, _ := mux.Tag("a")
			assert.Nil(t, err)
			tagb, _ := mux.Tag("b")
			assert.Nil(t, err)

			io.WriteString(taga, "hello taga")
			bytes, tag, err := mux.Read()
			assert.Nil(t, err)
			assert.Equal(t, "a", tag)
			assert.Equal(t, "hello taga", string(bytes))

			io.WriteString(tagb, "hello tagb")
			bytes, tag, err = mux.Read()
			assert.Nil(t, err)
			assert.Equal(t, "b", tag)
			assert.Equal(t, "hello tagb", string(bytes))

			bytes, tag, err = mux.Read()
			assert.ErrorIs(t, errors.Unwrap(err), os.ErrDeadlineExceeded)
		})
	}
}

func TestMuxReadNoData(t *testing.T) {
	for _, network := range networks {
		t.Run(network, func(t *testing.T) {
			mux, err := newMux[string](network)
			if err != nil {
				skipIfProtocolNotSupported(t, err, network)
				assert.Nil(t, err)
			}

			mux.Tag("a")

			bytes, tag, err := mux.Read()
			assert.Nil(t, bytes)
			if network == "unixgram" {
				assert.Equal(t, "", tag)
			} else {
				assert.Equal(t, "a", tag)
			}
			// assert error
		})
	}
}

func TestMuxTruncatedRead(t *testing.T) {
	mux, err := NewMuxUnix[string]()
	assert.Nil(t, err)
	taga, _ := mux.Tag("a")
	tagb, _ := mux.Tag("b")
	assert.Nil(t, err)

	td, err := mux.ReadWhile(func() error {
		binary.Write(taga, binary.BigEndian, make([]byte, 256)) // Double the receive buffer size
		time.Sleep(1 * time.Millisecond)
		binary.Write(tagb, binary.BigEndian, make([]byte, 10))
		time.Sleep(1 * time.Millisecond)
		binary.Write(taga, binary.BigEndian, make([]byte, 20))
		return nil
	})

	assert.Equal(t, 3, len(td))
	assert.Equal(t, 256, len(td[0].Data))
	assert.Equal(t, "a", td[0].Tag)
	assert.Equal(t, 10, len(td[1].Data))
	assert.Equal(t, "b", td[1].Tag)
	assert.Equal(t, 20, len(td[2].Data))
	assert.Equal(t, "a", td[2].Tag)
}

func skipIfProtocolNotSupported(t *testing.T, err error, network string) {
	err = errors.Unwrap(err)
	if sys, ok := err.(*os.SyscallError); ok {
		if sys.Syscall == "socket" {
			err = errors.Unwrap(err)
			if err == unix.EPROTONOSUPPORT {
				t.Skip("unsupported protocol")
			}
		}
	}
}

func TestMuxMultiple(t *testing.T) {
	for _, network := range networks {
		t.Run(network, func(t *testing.T) {
			mux, err := newMux[string](network)
			if err != nil {
				skipIfProtocolNotSupported(t, err, network)
			}
			taga, _ := mux.Tag("a")
			tagb, _ := mux.Tag("b")
			tagc, _ := mux.Tag("c")
			assert.Nil(t, err)

			td, err := mux.ReadWhile(func() error {
				io.WriteString(taga, "out1")
				time.Sleep(1 * time.Millisecond)
				io.WriteString(tagb, "err1")
				io.WriteString(tagb, "err2")
				time.Sleep(1 * time.Millisecond)
				io.WriteString(tagc, "other")
				return nil
			})

			assert.Equal(t, 3, len(td))
			out1 := td[0]
			assert.Equal(t, "a", out1.Tag)
			assert.Equal(t, "out1", string(out1.Data))
			err1 := td[1]
			assert.Equal(t, "b", err1.Tag)
			assert.Equal(t, "err1err2", string(err1.Data))
			out2 := td[2]
			assert.Equal(t, "c", out2.Tag)
			assert.Equal(t, "other", string(out2.Data))
		})
	}
}

func TestMuxCmd(t *testing.T) {
	for _, network := range networks {
		t.Run(network, func(t *testing.T) {
			mux, err := newMux[int](network)
			if err != nil {
				skipIfProtocolNotSupported(t, err, network)
			}
			// sleeps to avoid racing on connection oriented networks
			cmd := exec.Command("sh", "-c", "echo out1 && sleep 0.1 && echo err1 1>&2 && sleep 0.1 && echo out2")
			stdout, _ := mux.Tag(0)
			stderr, _ := mux.Tag(1)
			cmd.Stdout = stdout
			cmd.Stderr = stderr
			td, err := mux.ReadWhile(func() error {
				err := cmd.Run()
				return err
			})

			assert.Equal(t, 3, len(td))
			out1 := td[0]
			assert.Equal(t, 0, out1.Tag)
			assert.Equal(t, "out1\n", string(out1.Data))
			err1 := td[1]
			assert.Equal(t, 1, err1.Tag)
			assert.Equal(t, "err1\n", string(err1.Data))
			out2 := td[2]
			assert.Equal(t, 0, out2.Tag)
			assert.Equal(t, "out2\n", string(out2.Data))
		})
	}
}
