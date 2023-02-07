package iomux

import (
	"context"
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

func TestMuxRead(t *testing.T) {
	for _, network := range networks {
		t.Run(network, func(t *testing.T) {
			mux := &Mux[string]{network: network}
			t.Cleanup(func() {
				mux.Close()
			})
			taga, err := mux.Tag("a")
			if err != nil {
				skipIfProtocolNotSupported(t, err, network)
				assert.Nil(t, err)
			}
			assert.Nil(t, err)
			tagb, _ := mux.Tag("b")
			assert.Nil(t, err)

			ctx, cancelFn := context.WithCancel(context.Background())
			io.WriteString(taga, "hello taga")
			bytes, tag, err := mux.Read(ctx)
			assert.Nil(t, err)
			assert.Equal(t, "a", tag)
			assert.Equal(t, "hello taga", string(bytes))

			io.WriteString(tagb, "hello tagb")
			bytes, tag, err = mux.Read(ctx)
			assert.Nil(t, err)
			assert.Equal(t, "b", tag)
			assert.Equal(t, "hello tagb", string(bytes))

			cancelFn()
			bytes, tag, err = mux.Read(ctx)
			assert.Equal(t, io.EOF, err)
		})
	}
}

func TestMuxReadNoSenders(t *testing.T) {
	mux := &Mux[string]{}
	t.Cleanup(func() {
		mux.Close()
	})
	ctx := context.Background()
	ctx.Done()
	data, tag, err := mux.Read(ctx)

	assert.Nil(t, data)
	assert.Equal(t, "", tag)
	assert.ErrorIs(t, err, MuxNoConnections)
}

func TestMuxReadClosed(t *testing.T) {
	mux := &Mux[string]{}
	mux.Close()
	ctx := context.Background()
	ctx.Done()
	_, _, err := mux.Read(ctx)

	assert.ErrorIs(t, err, MuxClosed)
}

func TestMuxReadNoData(t *testing.T) {
	for _, network := range networks {
		t.Run(network, func(t *testing.T) {
			mux := &Mux[string]{network: network}
			t.Cleanup(func() {
				mux.Close()
			})
			_, err := mux.Tag("a")
			if err != nil {
				skipIfProtocolNotSupported(t, err, network)
				assert.Nil(t, err)
			}

			ctx, cancelFn := context.WithCancel(context.Background())
			cancelFn()
			bytes, tag, err := mux.Read(ctx)
			assert.Nil(t, bytes)
			assert.Equal(t, "", tag)
			assert.Equal(t, io.EOF, err)
		})
	}
}

func TestMuxMultipleContexts(t *testing.T) {
	mux := &Mux[string]{}
	t.Cleanup(func() {
		mux.Close()
	})
	taga, err := mux.Tag("a")
	assert.Nil(t, err)
	tagb, _ := mux.Tag("b")

	ctx1, cancelFn1 := context.WithCancel(context.Background())
	io.WriteString(taga, "hello taga")
	bytes, tag, err := mux.Read(ctx1)
	assert.Nil(t, err)
	assert.Equal(t, "a", tag)
	assert.Equal(t, "hello taga", string(bytes))

	ctx2, cancelFn2 := context.WithCancel(context.Background())
	io.WriteString(tagb, "hello tagb")
	bytes, tag, err = mux.Read(ctx2)
	assert.Nil(t, err)
	assert.Equal(t, "b", tag)
	assert.Equal(t, "hello tagb", string(bytes))

	cancelFn2()
	bytes, tag, err = mux.Read(ctx2)
	assert.Equal(t, io.EOF, err)

	cancelFn1()
	bytes, tag, err = mux.Read(ctx1)
	assert.Equal(t, io.EOF, err)

	assert.Empty(t, mux.recvstate)
}

func TestMuxReadWhileErr(t *testing.T) {
	mux := &Mux[string]{}
	t.Cleanup(func() {
		mux.Close()
	})
	_, err := mux.Tag("a")
	assert.Nil(t, err)

	expected := errors.New("this is an error")
	_, err = mux.ReadWhile(func() error {
		return expected
	})

	assert.ErrorIs(t, expected, err)
}

func TestMuxTruncatedRead(t *testing.T) {
	mux := NewMuxUnix[string]()
	t.Cleanup(func() {
		mux.Close()
	})
	taga, err := mux.Tag("a")
	assert.Nil(t, err)
	tagb, _ := mux.Tag("b")

	td, err := mux.ReadWhile(func() error {
		binary.Write(taga, binary.BigEndian, make([]byte, 256)) // Double the receive buffer size
		time.Sleep(1 * time.Millisecond)                        // Some slack for connection oriented networks
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
			mux := &Mux[string]{network: network}
			t.Cleanup(func() {
				mux.Close()
			})
			taga, err := mux.Tag("a")
			if err != nil {
				skipIfProtocolNotSupported(t, err, network)
				assert.Nil(t, err)
			}
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
			mux := &Mux[int]{network: network}
			t.Cleanup(func() {
				mux.Close()
			})
			// sleep to avoid racing on connection oriented networks
			cmd := exec.Command("sh", "-c", "echo out1 && sleep 0.1 && echo err1 1>&2 && sleep 0.1 && echo out2")
			stdout, err := mux.Tag(0)
			if err != nil {
				skipIfProtocolNotSupported(t, err, network)
				assert.Nil(t, err)
			}
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
