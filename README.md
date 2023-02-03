# iomux

iomux allows multiplexing of file descriptors using any Go supported unix domain network. When using `unixgram`, ordering is guaranteed.

The primary use case is for multiplexing `exec.Cmd` stdout/stderr keeping the original output order:
```
	mux, _ := NewMux[int]()
	cmd := exec.Command("sh", "-c", "echo out1 && echo err1 1>&2 && echo out2")
	stdout, _ := mux.Tag(0)
	stderr, _ := mux.Tag(1)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	td, err := mux.ReadWhile(func() error {
		err := cmd.Run()
		return err
	})
```

This module was inspired by Josh Triplett's Rust crate https://github.com/joshtriplett/io-mux/.
