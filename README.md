# iomux

iomux allows multiplexing of file descriptors using any Go supported unix domain network. It makes it possible to distinctly capture `exec.Cmd` stdout/stderr keeping the original output order:

```
	mux := &iomux.Mux[OutputType]{}
	defer mux.Close()
	cmd := exec.Command("sh", "-c", "echo out1 && echo err1 1>&2 && echo out2")
	stdout, err := mux.Tag(StdOut)
	if err != nil {
		panic(err)
	}
	stderr, _ := mux.Tag(StdErr)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	taggedData, err := mux.ReadWhile(func() error {
		return cmd.Run()
	})
```

When using `unixgram` networking, ordering is guaranteed (see Limitations below). More can be found in the [examples](examples) directory.

This module was inspired by Josh Triplett's Rust crate https://github.com/joshtriplett/io-mux/.

## Limitations

On all platforms except macOS the network defaults to `unixgram`. On Linux, `unixgram` behaves like a pipe and will behave exactly as you'd expect, and always see messages in order. On other UNIXes, there is a possibility of different behaviour when [buffers are full](https://docs.rs/io-mux/latest/io_mux/#portability), but it's unlikely a reader will be outpaced.

Using `unixgram` on macOS when you cannot control the write size of the sender, which rules out just about any usage with `exec.Cmd`, is impossible due a message size limit of 2048 bytes:
```
write /dev/stdout: message too long
```

So on macOs, the default network is `unix`. It is connection oriented, so it doesn't come with the ordering guarantees of `unixgram`. It's possible to see see writes out of order, but on a MacBook Pro M1 0.1ms is the threshold for writes being read out of order, so for real world use cases it's unlikely to be a problem.

These limitations do not affect the read order of an individual connection, so output for an individual tag is always consistent. If you prefer a different network type, the default can be overridden using the convenience constructors `NewMuxUnix`, `NewMuxUnixGram` and `NewMuxUnixPacket`.
