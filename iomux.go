package iomux

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// Mux provides a single receive and multiple send ends using unix sockets.
type Mux[T comparable] struct {
	network   string
	dir       string
	recvaddr  *net.UnixAddr
	recvconns []*net.UnixConn
	recvbuf   []byte
	recvidx   int
	acceptFn  func() error
	senders   map[T]*net.UnixConn
	closed    bool
	closers   []io.Closer
}

type TaggedData[T comparable] struct {
	Tag  T
	Data []byte
}

const deadline = 100 * time.Millisecond

var (
	MuxClosed        = errors.New("mux has been closed")
	MuxNoConnections = errors.New("no senders have been connected")
)

// NewMux Create a new Mux using the best connection type for the platform
func NewMux[T comparable]() (*Mux[T], error) {
	// Default to the most compatible/reliable network for non-Linux OSes. For instance, unixgram on macOS has a message
	// limit of 2048 bytes, larger writes fail with:
	//
	//	write /dev/stdout: message too long
	//
	// That makes it unsuitable when you can't control the write size of the sender. Another symptom of this is children
	// of children processes failing with `write /dev/stdout: broken pipe`.
	//
	// The non-message based networks don't come with the strong ordering guarantees as unixgram, but are suitable for
	// the kind of applications this will be used for.
	network := "unix"
	if runtime.GOOS == "linux" {
		network = "unixgram"
	}
	return newMux[T](network)
}

// NewMuxUnix Create a new Mux using 'unix' network.
func NewMuxUnix[T comparable]() (*Mux[T], error) {
	return newMux[T]("unix")
}

// NewMuxUnixGram Create a new Mux using 'unixgram' network.
func NewMuxUnixGram[T comparable]() (*Mux[T], error) {
	return newMux[T]("unixgram")
}

// NewMuxUnixPacket Create a new Mux using 'unixpacket' network.
func NewMuxUnixPacket[T comparable]() (*Mux[T], error) {
	return newMux[T]("unixpacket")
}

func newMux[T comparable](network string) (*Mux[T], error) {
	dir, err := os.MkdirTemp("", "mux")
	if err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "recv.sock")
	recvaddr, err := net.ResolveUnixAddr(network, file)
	if err != nil {
		return nil, err
	}

	// If we got at the underlying poll.FD it would be possible to call recvfrom with MSG_PEEK | MSG_TRUNC to size
	// the buffer to the current packet, but for now we just set the maximum message size for the OS for message
	// oriented unixgram, because the message truncates if it exceeds the buffer, and a modest read buffer otherwise.
	bufsize := 0
	switch network {
	case "unixgram":
		switch runtime.GOOS {
		case "darwin":
			bufsize = 2048
		default:
			bufsize = 65536
		}
	case "unix", "unixpacket":
		bufsize = 256
	default:
		return nil, fmt.Errorf("unknown network %s", network)
	}
	mux := &Mux[T]{
		network:  network,
		dir:      dir,
		recvaddr: recvaddr,
		recvbuf:  make([]byte, bufsize),
		acceptFn: func() error { return nil },
		senders:  make(map[T]*net.UnixConn),
	}
	err = mux.startListener()
	if err != nil {
		return nil, err
	}
	return mux, nil
}

// Tag Create a file to receive data tagged with tag T. Returns an *os.File ready for writing.
func (mux *Mux[T]) Tag(tag T) (*os.File, error) {
	if mux.closed {
		return nil, MuxClosed
	}
	sender, err := mux.createSender(tag)
	if err != nil {
		return nil, err
	}
	return sender, nil
}

// Read perform a read. Prefer the convenience functions ReadUtil and ReadWhile. For connection oriented networks, Read
// round robins the receiver side connections, so when senders have finished writing, call read until you receive an
// os.ErrDeadlineExceeded consecutive times for least the number of tags you have registered.
func (mux *Mux[T]) Read() ([]byte, T, error) {
	var emptyTag T
	if mux.closed {
		return nil, emptyTag, MuxClosed
	}
	numConns := len(mux.recvconns)
	if numConns == 0 {
		return nil, emptyTag, MuxNoConnections
	}
	conn := mux.recvconns[mux.recvidx]
	if mux.recvidx < numConns-1 {
		mux.recvidx++
	} else {
		mux.recvidx = 0
	}
	conn.SetReadDeadline(time.Now().Add(deadline))
	n, addr, err := conn.ReadFrom(mux.recvbuf)
	var tag T
	for t, c := range mux.senders {
		sender := c.LocalAddr().String()
		if addr != nil {
			if addr != nil && addr.String() == sender {
				tag = t
				break
			}
		} else if conn.RemoteAddr() != nil && conn.RemoteAddr().String() == sender {
			tag = t
			break
		}
	}
	if err != nil {
		return nil, tag, err
	} else {
		bytes := make([]byte, n)
		copy(bytes, mux.recvbuf[0:n])
		return bytes, tag, nil
	}
}

// ReadWhile Read until waitFn returns, returning the read data.
func (mux *Mux[T]) ReadWhile(waitFn func() error) ([]*TaggedData[T], error) {
	if mux.closed {
		return nil, MuxClosed
	}
	done := make(chan bool)
	var waitErr error
	go func() {
		waitErr = waitFn()
		done <- true
	}()
	td, err := mux.ReadUntil(done)
	if err != nil {
		return nil, err
	}
	return td, waitErr
}

// ReadUntil Read the receiver until done receives true
func (mux *Mux[T]) ReadUntil(done <-chan bool) ([]*TaggedData[T], error) {
	if mux.closed {
		return nil, MuxClosed
	}
	var result []*TaggedData[T]
	callerDone := false
	lastRead := 0
	for {
		select {
		case callerDone = <-done:
		default:
		}
		data, tag, err := mux.Read()
		if err != nil {
			if errors.Unwrap(err) != os.ErrDeadlineExceeded {
				return nil, err
			} else if callerDone {
				// lastRead isn't required for unixgram, but we have one connection per tag for other network types
				lastRead++
				if lastRead >= len(mux.recvconns) {
					break
				}
			}
		} else {
			lastRead = 0
			resultLen := len(result)
			if resultLen > 0 {
				previous := result[resultLen-1]
				if previous.Tag == tag {
					previous.Data = append(previous.Data, data...)
					continue
				}
			}
			result = append(result, &TaggedData[T]{
				Data: data,
				Tag:  tag,
			})
		}
	}
	if mux.network != "unixgram" {
		return mux.repairTruncatedReads(result), nil
	}
	return result, nil
}

func (mux *Mux[T]) Close() error {
	if mux.closed {
		return MuxClosed
	}
	mux.closed = true
	for _, closer := range mux.closers {
		closer.Close()
	}
	os.RemoveAll(mux.dir)
	return nil
}

func (mux *Mux[T]) startListener() error {
	switch mux.network {
	case "unixgram":
		{
			conn, err := net.ListenUnixgram(mux.network, mux.recvaddr)
			if err != nil {
				return err
			}
			mux.closers = append(mux.closers, conn)
			_ = conn.CloseWrite()
			mux.recvconns = append(mux.recvconns, conn)
		}
	case "unix", "unixpacket":
		{
			listener, err := net.ListenUnix(mux.network, mux.recvaddr)
			if err != nil {
				return err
			}
			mux.closers = append(mux.closers, listener)
			mux.acceptFn = func() error {
				err := listener.SetDeadline(time.Now().Add(deadline))
				if err != nil {
					return err
				}
				conn, err := listener.AcceptUnix()
				if err != nil {
					return err
				}
				mux.closers = append(mux.closers, conn)
				_ = conn.CloseWrite()
				mux.recvconns = append(mux.recvconns, conn)
				return nil
			}
		}
	}

	return nil
}

func (mux *Mux[T]) createSender(tag T) (*os.File, error) {
	num := len(mux.senders) + 1
	if _, ok := mux.senders[tag]; !ok {
		address := filepath.Join(mux.dir, fmt.Sprintf("send_%d.sock", num))
		addr, err := net.ResolveUnixAddr(mux.network, address)
		if err != nil {
			return nil, err
		}
		wg := sync.WaitGroup{}
		wg.Add(1)
		var acceptErr error
		go func() {
			acceptErr = mux.acceptFn()
			wg.Done()
		}()
		conn, dialErr := net.DialUnix(mux.network, addr, mux.recvaddr)
		wg.Wait()
		if acceptErr != nil {
			return nil, acceptErr
		}
		if dialErr != nil {
			return nil, dialErr
		}
		mux.closers = append(mux.closers, conn)
		_ = conn.CloseRead()
		mux.senders[tag] = conn
	}

	file, err := mux.senders[tag].File()
	if err != nil {
		return nil, err
	}
	return file, nil
}

func (mux *Mux[T]) repairTruncatedReads(taggedData []*TaggedData[T]) []*TaggedData[T] {
	tagLastIndex := make(map[T]int)
	for i := len(taggedData) - 1; i >= 0; i-- {
		tag := taggedData[i].Tag
		if _, ok := tagLastIndex[tag]; !ok {
			tagLastIndex[tag] = i
		}
		if len(tagLastIndex) == len(mux.senders) {
			break
		}
	}
	var bufferedData []*TaggedData[T]
	var result []*TaggedData[T]
	for i, td := range taggedData {
		tag := td.Tag
		data := td.Data
		bufferLen := len(bufferedData)
		var currentTag T
		if bufferLen > 0 {
			currentTag = bufferedData[0].Tag
		} else {
			currentTag = tag
		}
		lastByteNl := data[len(data)-1] == byte(10)
		if (lastByteNl && currentTag == tag) || tagLastIndex[tag] == i {
			for i, bd := range bufferedData {
				if bd.Tag == tag {
					td.Data = append(bd.Data, data...)
					bufferedData = append(bufferedData[:i], bufferedData[i+1:]...)
				}
			}
			result = append(result, td)
		} else {
			bufferedData = append(bufferedData, td)
		}
	}
	return result
}
