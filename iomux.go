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
type mux[T comparable] struct {
	network   string
	dir       string
	recvaddr  *net.UnixAddr
	recvconns []*net.UnixConn
	recvbufs  [][]byte
	recvchan  chan *TaggedData[T]
	recvmutex []sync.Mutex
	acceptFn  func() error
	senders   map[T]*net.UnixConn
	closed    bool
	closers   []io.Closer
}

type TaggedData[T comparable] struct {
	Tag  T
	Data []byte
	err  error
}

var (
	MuxClosed        = errors.New("mux has been closed")
	MuxNoConnections = errors.New("no senders have been connected")
)

const deadlineDuration = 100 * time.Millisecond

// NewMux Create a new Mux using the best connection type for the platform
func NewMux[T comparable]() (*mux[T], error) {
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
func NewMuxUnix[T comparable]() (*mux[T], error) {
	return newMux[T]("unix")
}

// NewMuxUnixGram Create a new Mux using 'unixgram' network.
func NewMuxUnixGram[T comparable]() (*mux[T], error) {
	return newMux[T]("unixgram")
}

// NewMuxUnixPacket Create a new Mux using 'unixpacket' network.
func NewMuxUnixPacket[T comparable]() (*mux[T], error) {
	return newMux[T]("unixpacket")
}

func newMux[T comparable](network string) (*mux[T], error) {
	dir, err := os.MkdirTemp("", "mux")
	if err != nil {
		return nil, err
	}
	file := filepath.Join(dir, "recv.sock")
	recvaddr, err := net.ResolveUnixAddr(network, file)
	if err != nil {
		return nil, err
	}
	mux := &mux[T]{
		network:  network,
		dir:      dir,
		recvaddr: recvaddr,
		recvchan: make(chan *TaggedData[T], 10),
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
func (mux *mux[T]) Tag(tag T) (*os.File, error) {
	if mux.closed {
		return nil, MuxClosed
	}
	sender, err := mux.createSender(tag)
	if err != nil {
		return nil, err
	}
	return sender, nil
}

// Read perform a read. Prefer the convenience functions ReadUtil and ReadWhile. For connection oriented networks, every
// call to Read concurrently reads all connections, buffering and returning in order received for consecutive calls
// to Read.
func (mux *mux[T]) Read() ([]byte, T, error) {
	var emptyTag T
	if mux.closed {
		return nil, emptyTag, MuxClosed
	}
	if len(mux.recvconns) == 0 {
		return nil, emptyTag, MuxNoConnections
	}

	for i, c := range mux.recvconns {
		conn := c
		buf := mux.recvbufs[i]
		mutex := &mux.recvmutex[i]
		if mutex.TryLock() {
			go func() {
				_ = conn.SetDeadline(time.Now().Add(deadlineDuration))
				n, addr, err := conn.ReadFrom(buf)
				td := &TaggedData[T]{err: err}
				for t, c := range mux.senders {
					localAddr := c.LocalAddr().String()
					remoteAddr := conn.RemoteAddr()
					if addr != nil {
						if addr.String() == localAddr {
							td.Tag = t
							break
						}
					} else if remoteAddr != nil && remoteAddr.String() == localAddr {
						td.Tag = t
						break
					}
				}
				if err == nil {
					td.Data = make([]byte, n)
					copy(td.Data, buf[0:n])
				}
				mux.recvchan <- td
				mutex.Unlock()
			}()
		}
	}
	td := <-mux.recvchan
	if td.err != nil {
		return nil, td.Tag, td.err
	}
	return td.Data, td.Tag, td.err
}

// ReadWhile Read until waitFn returns, returning the read data.
func (mux *mux[T]) ReadWhile(waitFn func() error) ([]*TaggedData[T], error) {
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
func (mux *mux[T]) ReadUntil(done <-chan bool) ([]*TaggedData[T], error) {
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
	return result, nil
}

func (mux *mux[T]) Close() error {
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

func (mux *mux[T]) startListener() error {
	// If we got at the underlying poll.FD it would be possible to call recvfrom with MSG_PEEK | MSG_TRUNC to size
	// the buffer to the current packet, but for now we just set the maximum message size for the OS for message
	// oriented unixgram, because the message truncates if it exceeds the buffer, and a modest read buffer otherwise.
	bufsize := 0
	switch mux.network {
	case "unixgram":
		{
			switch runtime.GOOS {
			case "darwin":
				bufsize = 2048
			default:
				bufsize = 65536
			}
			conn, err := net.ListenUnixgram(mux.network, mux.recvaddr)
			if err != nil {
				return err
			}
			mux.closers = append(mux.closers, conn)
			_ = conn.CloseWrite()
			mux.recvconns = append(mux.recvconns, conn)
			mux.recvbufs = append(mux.recvbufs, make([]byte, bufsize))
			mux.recvmutex = append(mux.recvmutex, sync.Mutex{})
		}
	case "unix", "unixpacket":
		{
			bufsize = 256
			listener, err := net.ListenUnix(mux.network, mux.recvaddr)
			if err != nil {
				return err
			}
			mux.closers = append(mux.closers, listener)
			mux.acceptFn = func() error {
				err := listener.SetDeadline(time.Now().Add(deadlineDuration))
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
				mux.recvbufs = append(mux.recvbufs, make([]byte, bufsize))
				mux.recvmutex = append(mux.recvmutex, sync.Mutex{})
				return nil
			}
		}
	}

	return nil
}

func (mux *mux[T]) createSender(tag T) (*os.File, error) {
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
