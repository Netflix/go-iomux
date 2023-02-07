# iomux

iomux allows multiplexing of file descriptors using any Go supported unix domain network. When using `unixgram`, ordering is guaranteed.

The primary use case is for multiplexing `exec.Cmd` stdout/stderr keeping the original output order:

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

For more, see the [examples](examples) directory.

This module was inspired by Josh Triplett's Rust crate https://github.com/joshtriplett/io-mux/.

## Limitations

On platforms other than Linux, `NewMux` defaults to `unix` rather than `unixgram` as it's the least likely to have issues (see 'macOS' below). `unix` is connection oriented and doesn't come with the ordering of `unixgram`, so can occasionally see writes of order, but in our testing the tolerance is under one millisecond, so for real world use cases, this is unlikely to be a concern. These limitations do not affect the read order of an individual connection, so output for an individual tag is always consistent.

### macOS

`unixgram` on macOS has a message limit of 2048 bytes, larger writes fail with:
```
write /dev/stdout: message too long
```

That makes it unsuitable when you can't control the write size of the sender. Another symptom of this is children of children processes failing with `write /dev/stdout: broken pipe`.
