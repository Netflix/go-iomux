# iomux

iomux allows multiplexing of file descriptors using any Go supported unix domain network. When using `unixgram`, ordering is guaranteed.

The primary use case is for multiplexing `exec.Cmd` stdout/stderr keeping the original output order:

```
        mux, _ := iomux.NewMux[OutputType]() // ignore errors for brevity
        defer mux.Close()
        cmd := exec.Command("sh", "-c", "echo out1 && echo err1 1>&2 && echo out2")
        stdout, _ := mux.Tag(0)
        stderr, _ := mux.Tag(1)
        cmd.Stdout = stdout
        cmd.Stderr = stderr
        taggedData, _ := mux.ReadWhile(func() error {
                return cmd.Run()
        })
        for _, td := range taggedData {
                var w io.Writer
                switch td.Tag {
                case 0:
                        w = os.Stdout
                case 1:
                        w = os.Stderr
                }
                binary.Write(w, binary.BigEndian, td.Data)
        }
```

For more, see the [examples](examples) directory.

This module was inspired by Josh Triplett's Rust crate https://github.com/joshtriplett/io-mux/.

## Limitations

On platforms other than Linux, `NewMux` defaults to `unix` rather than `unixgram` as it's the least likely to have issues. `unix` is connection oriented, so doesn't come with the ordering of `unixgram`, so can occasionally see writes of order. To address this limitation, the `ReadWhile` and `ReadUntil` convenience functions check for new lines at the end of writes (assuming console output) to delimit writes to try and avoid the possibility of intermixed output. These limitations do not affect the read order of an individual connection, so output for an individual tag is always consistent.

### macOS

`unixgram` on macOS has a message limit of 2048 bytes, larger writes fail with:
```
write /dev/stdout: message too long
```

That makes it unsuitable when you can't control the write size of the sender. Another symptom of this is children of children processes failing with `write /dev/stdout: broken pipe`.
