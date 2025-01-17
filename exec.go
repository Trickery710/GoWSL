package gowsl

// This file contains utilities to launch commands into WSL distros.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
)

// Cmd is a wrapper around the Windows process spawned by WslLaunch.
// Its interface is the same as the standard library's exec (except
// for func Command) and its implementation is very similar.
//
// A Cmd cannot be reused after calling its Run method.
type Cmd struct {
	// Public parameters
	Stdin  io.Reader // Reader to read stdin from
	Stdout io.Writer // Writer to write stdout into
	Stderr io.Writer // Writer to write stdout into
	UseCWD bool      // Whether WSL is launched in the current working directory (true) or the home directory (false)

	// Immutable parameters
	distro  *Distro // The distro that the command will be launched into.
	command string  // The command to be launched

	// Pipes
	closeAfterStart []io.Closer    // IO closers to be invoked after Launching the command
	closeAfterWait  []io.Closer    // IO closers to be invoked after Waiting for the command to end
	goroutine       []func() error // Goroutines that monitor Stdout/Stderr/Stdin and copy them asyncrounously
	errch           chan error     // The gouroutines will send any error down this chanel

	// File descriptors for pipes. These are analogous to (*exec.Cmd).childFiles[:3]
	stdinR  *os.File // File that acts as a reader for WSL to read stdin from
	stdoutW *os.File // File that acts as a writer for WSL to write stdout into
	stderrW *os.File // File that acts as a writer for WSL to write stderr into

	// Book-keeping
	Process      *os.Process      // The windows handle to the WSL process
	finished     bool             // Flag to fail nicely when Wait is invoked twice
	ProcessState *os.ProcessState // Status of the process. Cached because it cannot be read after the process is closed.

	// Context management
	ctx      context.Context // Context to kill the process before it finishes
	ctxErr   error           // We deviate from the stdlib: "context cancelled" is more useful than "exit code 1"
	waitDone chan struct{}   // This chanel prevents the context from attempting to kill the process when it is closed already
}

// Command returns the Cmd struct to execute the named program with
// the given arguments in the same string.
//
// It sets only the command and stdin/stdout/stderr in the returned structure.
//
// The provided context is used to kill the process (by calling
// CloseHandle) if the context becomes done before the command
// completes on its own.
func (d *Distro) Command(ctx context.Context, cmd string) *Cmd {
	if ctx == nil {
		panic("nil Context")
	}
	return &Cmd{
		distro:  d,
		command: cmd,
		ctx:     ctx,
	}
}

// Start starts the specified command but does not wait for it to complete.
//
// The Wait method will return the exit code and release associated resources
// once the command exits.
func (c *Cmd) Start() (err error) {
	// Based on exec/exec.go.
	r, err := c.distro.IsRegistered()
	if err != nil {
		return err
	}
	if !r {
		return errors.New("wsl: distro is not registered")
	}

	if c.Process != nil {
		return errors.New("wsl: already started")
	}

	if c.ctx != nil {
		select {
		case <-c.ctx.Done():
			c.closeDescriptors(c.closeAfterStart)
			c.closeDescriptors(c.closeAfterWait)
			return c.ctx.Err()
		default:
		}
	}

	type F func(*Cmd) error
	for _, setupFd := range []F{(*Cmd).stdin, (*Cmd).stdout, (*Cmd).stderr} {
		err := setupFd(c)
		if err != nil {
			c.closeDescriptors(c.closeAfterStart)
			c.closeDescriptors(c.closeAfterWait)
			return err
		}
	}

	c.Process, err = c.startProcess()
	if err != nil {
		c.closeDescriptors(c.closeAfterStart)
		c.closeDescriptors(c.closeAfterWait)
		return err
	}

	c.closeDescriptors(c.closeAfterStart)

	// Don't allocate the channel unless there are goroutines to fire.
	if len(c.goroutine) > 0 {
		c.errch = make(chan error, len(c.goroutine))
		for _, fn := range c.goroutine {
			go func(fn func() error) {
				c.errch <- fn()
			}(fn)
		}
	}

	if c.ctx != nil {
		c.waitDone = make(chan struct{})
		go func() {
			select {
			case <-c.ctx.Done():
				//nolint: errcheck // Mimicking behaviour from stdlib
				c.Process.Kill()
				// We deviate from the stdlib: "context cancelled" is more useful than "exit code 1"
				c.ctxErr = c.ctx.Err()
			case <-c.waitDone:
			}
		}()
	}

	return nil
}

// Output runs the command and returns its standard output.
// Any returned error will usually be of type *ExitError.
// If c.Stderr was nil, Output populates ExitError.Stderr.
func (c *Cmd) Output() ([]byte, error) {
	// Taken from exec/exec.go.
	if c.Stdout != nil {
		return nil, errors.New("wsl: Stdout already set")
	}
	var stdout bytes.Buffer
	c.Stdout = &stdout

	captureErr := c.Stderr == nil
	if captureErr {
		c.Stderr = &prefixSuffixSaver{N: 32 << 10}
	}

	err := c.Run()
	if err != nil && captureErr {
		//nolint: errorlint
		// copied from stdlib. (*Cmd).Wait returns an unwrapped *ExitError so there should be no issue
		if ee, ok := err.(*exec.ExitError); ok {
			//nolint: forcetypeassert
			// copied from stdlib. We know this to be true because it is set further up in this same function
			ee.Stderr = c.Stderr.(*prefixSuffixSaver).Bytes()
		}
	}
	return stdout.Bytes(), err
}

// CombinedOutput runs the command and returns its combined standard
// output and standard error.
func (c *Cmd) CombinedOutput() ([]byte, error) {
	// Taken from exec/exec.go.
	if c.Stdout != nil {
		return nil, errors.New("wsl: Stdout already set")
	}
	if c.Stderr != nil {
		return nil, errors.New("wsl: Stderr already set")
	}
	var b bytes.Buffer
	c.Stdout = &b
	c.Stderr = &b
	err := c.Run()
	return b.Bytes(), err
}

// StdinPipe returns a pipe that will be connected to the command's
// standard input when the command starts.
// The pipe will be closed automatically after Wait sees the command exit.
// A caller need only call Close to force the pipe to close sooner.
// For example, if the command being run will not exit until standard input
// is closed, the caller must close the pipe.
func (c *Cmd) StdinPipe() (io.WriteCloser, error) {
	// Based on exec/exec.go.
	if c.Stdin != nil {
		return nil, errors.New("wsl: Stdin already set")
	}
	if c.Process != nil {
		return nil, errors.New("wsl: StdinPipe after process started")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c.Stdin = pr
	c.closeAfterStart = append(c.closeAfterStart, pr)
	wc := &closeOnce{File: pw}
	c.closeAfterWait = append(c.closeAfterWait, wc)
	return wc, nil
}

type closeOnce struct {
	// Taken from exec/exec.go.
	*os.File

	once sync.Once
	err  error
}

func (c *closeOnce) Close() error {
	// Taken from exec/exec.go.
	c.once.Do(c.close)
	return c.err
}

func (c *closeOnce) close() {
	// Taken from exec/exec.go.
	c.err = c.File.Close()
}

// StdoutPipe returns a pipe that will be connected to the command's
// standard output when the command starts.
//
// Wait will close the pipe after seeing the command exit, so most callers
// need not close the pipe themselves. It is thus incorrect to call Wait
// before all reads from the pipe have completed.
// For the same reason, it is incorrect to call Run when using StdoutPipe.
func (c *Cmd) StdoutPipe() (io.ReadCloser, error) {
	// Based on exec/exec.go.
	if c.Stdout != nil {
		return nil, errors.New("wsl: Stdout already set")
	}
	if c.Process != nil {
		return nil, errors.New("wsl: StdoutPipe after process started")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c.Stdout = pw
	c.closeAfterStart = append(c.closeAfterStart, pw)
	c.closeAfterWait = append(c.closeAfterWait, pr)
	return pr, nil
}

// StderrPipe returns a pipe that will be connected to the command's
// standard error when the command starts.
//
// Wait will close the pipe after seeing the command exit, so most callers
// need not close the pipe themselves. It is thus incorrect to call Wait
// before all reads from the pipe have completed.
// For the same reason, it is incorrect to use Run when using StderrPipe.
func (c *Cmd) StderrPipe() (io.ReadCloser, error) {
	// Based on exec/exec.go.
	if c.Stderr != nil {
		return nil, errors.New("wsl: Stderr already set")
	}
	if c.Process != nil {
		return nil, errors.New("wsl: StderrPipe after process started")
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	c.Stderr = pw
	c.closeAfterStart = append(c.closeAfterStart, pw)
	c.closeAfterWait = append(c.closeAfterWait, pr)
	return pr, nil
}

func (c *Cmd) stdin() error {
	r, e := c.readerDescriptor(c.Stdin)
	if e == nil {
		c.stdinR = r
	}
	return e
}

func (c *Cmd) stdout() error {
	// Based on exec/exec.go.
	w, e := c.writerDescriptor(c.Stdout)
	if e == nil {
		c.stdoutW = w
	}
	return e
}

func (c *Cmd) stderr() error {
	// Based on exec/exec.go.
	// Case where Stdout and Stderr are the same
	if c.Stderr != nil && interfaceEqual(c.Stdout, c.Stderr) {
		c.stderrW = c.stdoutW
		return nil
	}
	// Different stdout and stderr
	w, e := c.writerDescriptor(c.Stderr)
	if e == nil {
		c.stderrW = w
	}
	return e
}

// interfaceEqual protects against panics from doing equality tests on
// two interfaces with non-comparable underlying types.
func interfaceEqual(a, b any) bool {
	// Taken from exec/exec.go.
	defer func() {
		_ = recover()
	}()
	return a == b
}

func (c *Cmd) closeDescriptors(closers []io.Closer) {
	// Taken from exec/exec.go.
	for _, fd := range closers {
		fd.Close()
	}
}

// readerDescriptor connects an arbitrary reader to an os pipe's writer,
// and returns this pipe's reader as a file.
func (c *Cmd) readerDescriptor(r io.Reader) (f *os.File, err error) {
	// Based on exec/exec.go:stdin.
	if r == nil {
		f, err = os.Open(os.DevNull)
		if err != nil {
			return
		}
		c.closeAfterStart = append(c.closeAfterStart, f)
		return
	}

	if f, ok := r.(*os.File); ok {
		ft, err := fileType(f)
		if err == nil && ft == fileTypePipe {
			// It's a pipe: no need to create our own pipe.
			return f, nil
		}
		// General case: it is not a pipe (or we don't know for sure).
		// As such, we create a pipe to connect WslLaunch to the file.
		// This would seem unnecessary, but for some reason WslLaunch
		// fails silently if you try to redirect its streams
		// to something other than a pipe.
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return
	}

	c.closeAfterStart = append(c.closeAfterStart, pr)
	c.closeAfterWait = append(c.closeAfterWait, pw)
	c.goroutine = append(c.goroutine, func() error {
		_, err := io.Copy(pw, r)
		if err1 := pw.Close(); err == nil {
			err = err1
		}
		return err
	})
	return pr, nil
}

// writerDescriptor connects an arbitrary writer to an os pipe's reader,
// and returns this pipe's writer as a file.
func (c *Cmd) writerDescriptor(w io.Writer) (f *os.File, err error) {
	// Based on exec/exec.go.
	if w == nil {
		f, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return
		}
		c.closeAfterStart = append(c.closeAfterStart, f)
		return
	}

	if f, ok := w.(*os.File); ok {
		ft, err := fileType(f)
		if err == nil && ft == fileTypePipe {
			// It's a pipe: no need to create our own pipe.
			return f, nil
		}
		// General case: it is not a pipe (or we don't know for sure).
		// As such, we create a pipe to connect WslLaunch to the file.
		// This would seem unnecessary, but for some reason WslLaunch
		// fails silently if you try to redirect its streams
		// to something other than a pipe.
	}

	pr, pw, err := os.Pipe()
	if err != nil {
		return
	}

	c.closeAfterStart = append(c.closeAfterStart, pw)
	c.closeAfterWait = append(c.closeAfterWait, pr)
	c.goroutine = append(c.goroutine, func() error {
		_, err := io.Copy(w, pr)
		pr.Close() // in case io.Copy stopped due to write error
		return err
	})
	return pw, nil
}

// Wait waits for the command to exit and waits for any copying to
// stdin or copying from stdout or stderr to complete.
//
// The command must have been started by Start.
//
// The returned error is nil if the command runs, has no problems
// copying stdin, stdout, and stderr, and exits with a zero exit
// status.
//
// If the command fails to run or doesn't complete successfully, the
// error is of type ExitError. Other error types may be
// returned for I/O problems.
//
// If any of c.Stdin, c.Stdout or c.Stderr are not an *os.File, Wait also waits
// for the respective I/O loop copying to or from the process to complete.
//
// Wait releases any resources associated with the Cmd.
func (c *Cmd) Wait() error {
	// Based on exec/exec.go.
	if c.Process == nil {
		return errors.New("wsl: not started")
	}
	if c.finished {
		return errors.New("wsl: Wait was already called")
	}
	c.finished = true

	state, err := c.Process.Wait()
	if c.waitDone != nil {
		close(c.waitDone)
	}
	c.ProcessState = state

	var copyError error
	for range c.goroutine {
		if err := <-c.errch; err != nil && copyError == nil {
			copyError = err
		}
	}

	c.closeDescriptors(c.closeAfterWait)

	if c.ctxErr != nil {
		// This if block does not exist in the stdlib. We deviate because
		// printing "context cancelled" is more useful than "exit code 1".
		return c.ctxErr
	}

	if err != nil {
		return err
	} else if !state.Success() {
		return &exec.ExitError{ProcessState: state}
	}

	return copyError
}

// Run starts the specified WslProcess and waits for it to complete.
//
// The returned error is nil if the command runs and exits with a zero exit status.
//
// If the command fails to run or doesn't complete successfully, the error is of type *ExitError.
func (c *Cmd) Run() error {
	// Taken from exec/exec.go.
	if err := c.Start(); err != nil {
		return err
	}
	return c.Wait()
}

// prefixSuffixSaver is an io.Writer which retains the first N bytes
// and the last N bytes written to it. The Bytes() methods reconstructs
// it with a pretty error message.
type prefixSuffixSaver struct {
	// Taken from exec/exec.go.
	N         int // max size of prefix or suffix
	prefix    []byte
	suffix    []byte // ring buffer once len(suffix) == N
	suffixOff int    // offset to write into suffix
	skipped   int64

	// TODO(bradfitz): we could keep one large []byte and use part of it for
	// the prefix, reserve space for the '... Omitting N bytes ...' message,
	// then the ring buffer suffix, and just rearrange the ring buffer
	// suffix when Bytes() is called, but it doesn't seem worth it for
	// now just for error messages. It's only ~64KB anyway.
}

func (w *prefixSuffixSaver) Write(p []byte) (n int, err error) {
	// Taken from exec/exec.go.
	lenp := len(p)
	p = w.fill(&w.prefix, p)

	// Only keep the last w.N bytes of suffix data.
	if overage := len(p) - w.N; overage > 0 {
		p = p[overage:]
		w.skipped += int64(overage)
	}
	p = w.fill(&w.suffix, p)

	// w.suffix is full now if p is non-empty. Overwrite it in a circle.
	for len(p) > 0 { // 0, 1, or 2 iterations.
		n := copy(w.suffix[w.suffixOff:], p)
		p = p[n:]
		w.skipped += int64(n)
		w.suffixOff += n
		if w.suffixOff == w.N {
			w.suffixOff = 0
		}
	}
	return lenp, nil
}

// fill appends up to len(p) bytes of p to *dst, such that *dst does not
// grow larger than w.N. It returns the un-appended suffix of p.
func (w *prefixSuffixSaver) fill(dst *[]byte, p []byte) (pRemain []byte) {
	// Taken from exec/exec.go.
	if remain := w.N - len(*dst); remain > 0 {
		add := minInt(len(p), remain)
		*dst = append(*dst, p[:add]...)
		p = p[add:]
	}
	return p
}

// Bytes returns the contents of the buffer.
func (w *prefixSuffixSaver) Bytes() []byte {
	// Taken from exec/exec.go.
	if w.suffix == nil {
		return w.prefix
	}
	if w.skipped == 0 {
		return append(w.prefix, w.suffix...)
	}
	var buf bytes.Buffer
	buf.Grow(len(w.prefix) + len(w.suffix) + 50)
	buf.Write(w.prefix)
	buf.WriteString("\n... omitting ")
	buf.WriteString(strconv.FormatInt(w.skipped, 10))
	buf.WriteString(" bytes ...\n")
	buf.Write(w.suffix[w.suffixOff:])
	buf.Write(w.suffix[:w.suffixOff])
	return buf.Bytes()
}

func minInt(a, b int) int {
	// Taken from exec/exec.go.
	if a < b {
		return a
	}
	return b
}
