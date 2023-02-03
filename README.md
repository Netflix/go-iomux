# iomux

iomux allows multiplexing of file descriptors using any Go supported unix domain network. When using `unixgram`, ordering is guaranteed.

The primary use case is for multiplexing `exec.Cmd` stdout/stderr keeping the original output order. See the [examples](examples) directory.

This module was inspired by Josh Triplett's Rust crate https://github.com/joshtriplett/io-mux/.

## Limitations

On Linux, `unixgram` sockets work perfectly, and the received data will be read in exactly the order it was written. On other platforms, `NewMux` defaults to `unix` as it's the least likely to have issues. The non-message oriented networks don't come with the strong ordering guarantees as `unixgram`, but for most use cases, particularly console output from commands, output is rarely out of order. In cases where it matters, there are `ReadLine*` variants of the convenience functions to use newlines to estimate the original write boundaries.

For instance, `unixgram` on macOS has a message limit of 2048 bytes, larger writes fail with:
```
write /dev/stdout: message too long
```

That makes it unsuitable when you can't control the write size of the sender. Another symptom of this is children of children processes failing with `write /dev/stdout: broken pipe`. `*Line` versions of the `Read` functions are available to line buffer output to limit the impact of these limitations.
