package iomux

import (
	"context"
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

// Mux provides a single receive and multiple send ends using unix domain networking.
type Mux[T comparable] struct {
	network   string
	dir       string
	recvonce  sync.Once
	recvaddr  *net.UnixAddr
	recvconns []*net.UnixConn
	recvbufs  [][]byte
	recvchan  chan *taggedData[T]
	recvmutex []sync.Mutex
	recvstate map[recvKey]*recvState
	acceptFn  func() error
	senders   map[T]*net.UnixConn
	closed    bool
	closers   []io.Closer
}

type TaggedData[T comparable] struct {
	Tag  T
	Data []byte
}

type taggedData[T comparable] struct {
	tag  T
	data []byte
	conn *net.UnixConn
	err  error
}

type recvKey struct {
	ctx  context.Context
	conn *net.UnixConn
}

type recvState struct {
	eof bool
}

var (
	MuxClosed        = errors.New("mux has been closed")
	MuxNoConnections = errors.New("no senders have been connected")
)

const deadlineDuration = 100 * time.Millisecond

// Option to override defaults settings
type Option[T comparable] func(*Mux[T])

func WithCustomDir[T comparable](dir string) Option[T] {
	return func(m *Mux[T]) {
		m.dir = dir
	}
}

// NewMuxUnix Create a new Mux using 'unix' network.
func NewMuxUnix[T comparable]() *Mux[T] {
	return &Mux[T]{network: "unix"}
}

// NewMuxUnixGram Create a new Mux using 'unixgram' network.
func NewMuxUnixGram[T comparable]() *Mux[T] {
	return &Mux[T]{network: "unixgram"}
}

// NewMuxUnixPacket Create a new Mux using 'unixpacket' network.
func NewMuxUnixPacket[T comparable]() *Mux[T] {
	return &Mux[T]{network: "unixpacket"}
}

// Tag Create a file to receive data tagged with tag T. Returns an *os.File ready for writing, or an error. If an error
// occurs when creating the receive end of the connection, the Mux will be closed.
func (mux *Mux[T]) Tag(tag T, opts ...Option[T]) (*os.File, error) {
	if mux.closed {
		return nil, MuxClosed
	}
	err := mux.createReceiver()
	if err != nil {
		mux.Close()
		return nil, err
	}

	for _, opt := range opts {
		opt(mux)
	}

	sender, err := mux.createSender(tag)
	if err != nil {
		return nil, err
	}
	return sender, nil
}

// Read perform a read, blocking until data is available or ctx.Done. For connection oriented networks, Read
// concurrently reads all connections buffering in order received for consecutive calls to Read. Returns io.EOF error
// when there is no data remaining to be read.
func (mux *Mux[T]) Read(ctx context.Context) ([]byte, T, error) {
	var zeroTag T
	if mux.closed {
		return nil, zeroTag, MuxClosed
	}
	if len(mux.recvconns) == 0 {
		return nil, zeroTag, MuxNoConnections
	}

	if len(mux.recvconns) == 1 {
		return mux.read(ctx, mux.recvconns[0], mux.recvbufs[0])
	}

	if mux.recvstate == nil {
		mux.recvstate = make(map[recvKey]*recvState)
	}
	for i, c := range mux.recvconns {
		key := recvKey{ctx: ctx, conn: c}
		if _, ok := mux.recvstate[key]; !ok {
			mux.recvstate[key] = &recvState{}
		} else if mux.recvstate[key].eof {
			// avoid spinning up another read, we're done
			continue
		}
		conn := c
		buf := mux.recvbufs[i]
		mutex := &mux.recvmutex[i]
		if mutex.TryLock() {
			go func() {
				data, tag, err := mux.read(ctx, conn, buf)
				mux.recvchan <- &taggedData[T]{
					data: data,
					tag:  tag,
					conn: conn,
					err:  err,
				}
				mutex.Unlock()
			}()
		}
	}

	sleepDuration := 1 * time.Millisecond
	for {
		select {
		case td := <-mux.recvchan:
			if td.err != nil {
				if td.err == io.EOF {
					key := recvKey{ctx: ctx, conn: td.conn}
					mux.recvstate[key].eof = true
					continue
				}
				return nil, zeroTag, td.err
			}
			return td.data, td.tag, td.err
		case <-ctx.Done():
			done := true
			for _, c := range mux.recvconns {
				key := recvKey{ctx: ctx, conn: c}
				if state, ok := mux.recvstate[key]; ok {
					if !state.eof {
						done = false
					}
				} else {
					panic("no state")
				}
			}
			if done {
				for _, c := range mux.recvconns {
					key := recvKey{ctx: ctx, conn: c}
					delete(mux.recvstate, key)
				}
				return nil, zeroTag, io.EOF
			}
		default:
		}

		time.Sleep(sleepDuration)
		sleepDuration += sleepDuration
		if sleepDuration > deadlineDuration {
			sleepDuration = deadlineDuration
		}
	}
}

func (mux *Mux[T]) read(ctx context.Context, conn *net.UnixConn, buf []byte) ([]byte, T, error) {
	var zeroTag T
	for {
		_ = conn.SetDeadline(time.Now().Add(deadlineDuration))
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Unwrap(err) != os.ErrDeadlineExceeded {
				return nil, zeroTag, err
			}
			select {
			case <-ctx.Done():
				return nil, zeroTag, io.EOF
			default:
				continue
			}
		}
		data := make([]byte, n)
		copy(data, buf[0:n])
		var tag T
		for t, c := range mux.senders {
			localAddr := c.LocalAddr().String()
			remoteAddr := conn.RemoteAddr()
			if addr != nil {
				if addr.String() == localAddr {
					tag = t
					break
				}
			} else if remoteAddr != nil && remoteAddr.String() == localAddr {
				tag = t
				break
			}
		}
		return data, tag, nil
	}
}

// ReadWhile Read until waitFn returns, returning the read data.
func (mux *Mux[T]) ReadWhile(waitFn func() error) ([]*TaggedData[T], error) {
	if mux.closed {
		return nil, MuxClosed
	}
	ctx, cancelFn := context.WithCancel(context.Background())
	var waitErr error
	go func() {
		waitErr = waitFn()
		cancelFn()
	}()
	td, err := mux.ReadUntil(ctx)
	if err != nil {
		return nil, err
	}
	return td, waitErr
}

// ReadUntil Read the receiver until done receives true
func (mux *Mux[T]) ReadUntil(ctx context.Context) ([]*TaggedData[T], error) {
	if mux.closed {
		return nil, MuxClosed
	}
	var result []*TaggedData[T]
	for {
		data, tag, err := mux.Read(ctx)
		if err != nil {
			if err == io.EOF {
				return result, nil
			}
			return nil, err
		}
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

// Close closes the Mux, closing connections and removing temporary files. Prevents reuse.
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

func (mux *Mux[T]) createReceiver() (e error) {
	mux.recvonce.Do(func() {
		if mux.network == "" {
			switch runtime.GOOS {
			case "darwin":
				mux.network = "unix"
			default:
				mux.network = "unixgram"
			}
		}

		if mux.dir == "" {
			mux.dir, e = os.MkdirTemp("", "mux")
			if e != nil {
				return
			}
		}

		file := filepath.Join(mux.dir, "recv.sock")
		mux.recvaddr, e = net.ResolveUnixAddr(mux.network, file)
		if e != nil {
			return
		}
		mux.recvchan = make(chan *taggedData[T], 10)
		e = mux.startListener()
		if e != nil {
			return
		}
	})
	return
}

func (mux *Mux[T]) startListener() error {
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
			mux.acceptFn = func() error {
				return nil
			}
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

func (mux *Mux[T]) createSender(tag T) (*os.File, error) {
	if mux.senders == nil {
		mux.senders = make(map[T]*net.UnixConn)
	}
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
