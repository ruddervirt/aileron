package build

import (
	"context"
	"fmt"
	"net"
	"sync"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// ExecConn implements net.Conn over a SPDY exec stream running "nc <host> <port>"
// on a relay pod. This tunnels TCP traffic through kubectl exec.
//
// The underlying transport is a net.Pipe: the SSH client sees one end as a
// normal net.Conn (with working deadlines), and the other end is plumbed into
// the exec stream's Stdin/Stdout. Because net.Pipe implements SetDeadline,
// SetReadDeadline, and SetWriteDeadline correctly, callers that impose a
// per-call timeout can actually interrupt a blocked read — unlike a raw
// io.Pipe, which silently ignores deadline calls.
type ExecConn struct {
	net.Conn // local side of the net.Pipe — delegates all net.Conn methods (incl. deadlines)

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// NewExecConn creates a net.Conn by exec'ing "nc <host> <port>" on the given pod.
// Data written to the returned conn is streamed to the nc process's stdin, and
// data read from it comes from the nc process's stdout.
func NewExecConn(ctx context.Context, restConfig *rest.Config, namespace, podName, host string, port int32) (*ExecConn, error) {
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		Param("container", "relay").
		Param("command", "nc").
		Param("command", host).
		Param("command", fmt.Sprintf("%d", port)).
		Param("stdin", "true").
		Param("stdout", "true").
		Param("stderr", "false")

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("creating SPDY executor: %w", err)
	}

	// net.Pipe gives us a synchronous, in-memory, full-duplex net.Conn pair.
	// Both ends honor SetDeadline / SetReadDeadline / SetWriteDeadline.
	local, remote := net.Pipe()

	execCtx, cancel := context.WithCancel(ctx)
	conn := &ExecConn{
		Conn:   local,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	// Plumb the exec stream into the "remote" half: its Stdin reads data the
	// caller wrote to local, and its Stdout writes data the caller will read
	// from local. Passing the same net.Conn as both Stdin and Stdout works
	// because the API takes them as io.Reader / io.Writer separately.
	go func() {
		defer close(conn.done)
		defer remote.Close() //nolint:errcheck
		streamErr := exec.StreamWithContext(execCtx, remotecommand.StreamOptions{
			Stdin:  remote,
			Stdout: remote,
			Tty:    false,
		})
		// Record the stream error for debugging but don't surface it here —
		// the caller will observe it via the eventual EOF/ErrClosedPipe on
		// the local side once remote.Close() runs in the deferred call above.
		_ = streamErr
	}()

	return conn, nil
}

// Close tears down both the net.Pipe and the underlying SPDY exec stream.
// Safe to call multiple times.
func (c *ExecConn) Close() error {
	var err error
	c.once.Do(func() {
		// Cancel the exec ctx first so StreamWithContext unblocks and the
		// goroutine closes remote. Then close local to wake any in-flight
		// reads/writes the SSH client may be sitting on.
		c.cancel()
		err = c.Conn.Close()
	})
	return err
}
