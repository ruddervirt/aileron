package build

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	v1alpha1 "github.com/ruddervirt/aileron/api/v1alpha1"
	"golang.org/x/crypto/ssh"
)

// SSHCommunicator manages SSH connections to a build VM via a port-forward tunnel.
type SSHCommunicator struct {
	// Dial is a function that establishes a raw TCP connection to the VM's SSH port.
	// In production this uses the KubeVirt API subresource port-forward.
	Dial func(ctx context.Context) (net.Conn, error)

	Username   string
	Password   string
	PrivateKey []byte
	Port       int32
	Shell      v1alpha1.ShellType
}

// RunCommand executes a command over SSH and returns combined stdout+stderr output.
// The caller's ctx controls the timeout — if no deadline is set, the command
// runs until it completes or the context is cancelled.
func (c *SSHCommunicator) RunCommand(ctx context.Context, command string, env map[string]string) (string, error) {
	client, err := c.connect(ctx)
	if err != nil {
		return "", fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close() //nolint:errcheck

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close() //nolint:errcheck

	// Set environment variables.
	for k, v := range env {
		if err := session.Setenv(k, v); err != nil {
			// Many SSH servers don't allow SetEnv; fall back to prefixing the command.
			switch c.Shell {
			case v1alpha1.ShellTypePowerShell:
				// SSH default shell on Windows is cmd.exe; use 'set' so the
				// env var is inherited by the child powershell.exe process.
				command = fmt.Sprintf("set %s=%s&& %s", k, v, command)
			default:
				command = fmt.Sprintf("export %s=%q; %s", k, v, command)
			}
		}
	}

	// Run in a goroutine so the context can interrupt it.
	type result struct {
		output []byte
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := session.CombinedOutput(command)
		ch <- result{out, err}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return string(r.output), fmt.Errorf("command %q failed: %w\noutput: %s", command, r.err, string(r.output))
		}
		return string(r.output), nil
	case <-ctx.Done():
		// Force-close the session to unblock CombinedOutput.
		_ = session.Close()
		_ = client.Close()
		return "", fmt.Errorf("command %q cancelled: %w", command, ctx.Err())
	}
}

// RunCommandStreaming executes a command over SSH and invokes onLine for each
// stdout/stderr line as it is produced. The full combined output is also
// returned for error context. onLine is called from reader goroutines and may
// be invoked concurrently for stdout vs stderr lines.
//
// Use this for long-running commands (provisioner scripts, Windows Update
// cycles) where buffered output via RunCommand would hide progress and
// failures until the command exits.
func (c *SSHCommunicator) RunCommandStreaming(
	ctx context.Context, command string, env map[string]string, onLine func(string),
) (string, error) {
	client, err := c.connect(ctx)
	if err != nil {
		return "", fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close() //nolint:errcheck

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("SSH session: %w", err)
	}
	defer session.Close() //nolint:errcheck

	for k, v := range env {
		if err := session.Setenv(k, v); err != nil {
			switch c.Shell {
			case v1alpha1.ShellTypePowerShell:
				command = fmt.Sprintf("set %s=%s&& %s", k, v, command)
			default:
				command = fmt.Sprintf("export %s=%q; %s", k, v, command)
			}
		}
	}

	stdout, err := session.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		return "", fmt.Errorf("stderr pipe: %w", err)
	}

	if err := session.Start(command); err != nil {
		return "", fmt.Errorf("session start: %w", err)
	}

	var (
		mu  sync.Mutex
		buf strings.Builder
	)
	consume := func(r io.Reader, wg *sync.WaitGroup) {
		defer wg.Done()
		scanner := bufio.NewScanner(r)
		// Match RunCommand's tolerance for long single lines (e.g. PowerShell
		// stack traces that exceed bufio's 64K default).
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if onLine != nil {
				onLine(line)
			}
			mu.Lock()
			buf.WriteString(line)
			buf.WriteByte('\n')
			mu.Unlock()
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go consume(stdout, &wg)
	go consume(stderr, &wg)

	waitDone := make(chan error, 1)
	go func() {
		wg.Wait()
		waitDone <- session.Wait()
	}()

	select {
	case waitErr := <-waitDone:
		out := buf.String()
		if waitErr != nil {
			return out, fmt.Errorf("command %q failed: %w", command, waitErr)
		}
		return out, nil
	case <-ctx.Done():
		_ = session.Close()
		_ = client.Close()
		// Drain reader goroutines so onLine isn't invoked after we return.
		<-waitDone
		return buf.String(), fmt.Errorf("command %q cancelled: %w", command, ctx.Err())
	}
}

// UploadFile uploads content to a remote path via SFTP.
// Uses the SSH subsystem — no shell escaping or piping needed.
func (c *SSHCommunicator) UploadFile(ctx context.Context, content []byte, remotePath string) error {
	client, err := c.connect(ctx)
	if err != nil {
		return fmt.Errorf("SSH connect: %w", err)
	}
	defer client.Close() //nolint:errcheck

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("SFTP client: %w", err)
	}
	defer sftpClient.Close() //nolint:errcheck

	// Normalize backslashes to forward slashes for SFTP.
	sftpPath := strings.ReplaceAll(remotePath, `\`, "/")

	// Ensure parent directory exists.
	dir := path.Dir(sftpPath)
	if dir != "" && dir != "." && dir != "/" {
		_ = sftpClient.MkdirAll(dir)
	}

	f, err := sftpClient.OpenFile(sftpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("SFTP create %s: %w", remotePath, err)
	}

	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("SFTP write %s: %w", remotePath, err)
	}
	_ = f.Close()

	// Verify the file was written.
	info, err := sftpClient.Stat(sftpPath)
	if err != nil {
		return fmt.Errorf("SFTP verify %s: file not found after write: %w", remotePath, err)
	}
	if info.Size() != int64(len(content)) {
		return fmt.Errorf("SFTP verify %s: size mismatch (wrote %d, got %d)", remotePath, len(content), info.Size())
	}

	return nil
}

// TrySSH attempts a single SSH connection. Returns nil if successful,
// or the connection error. Callers should requeue and retry rather than
// looping internally, so the controller can check DeletionTimestamp
// between attempts.
func (c *SSHCommunicator) TrySSH(ctx context.Context) error {
	client, err := c.connect(ctx)
	if err != nil {
		return err
	}
	_ = client.Close()
	return nil
}

// sshDialTimeout bounds the entire dial+handshake sequence so a stuck
// SPDY exec or unresponsive VM cannot block the controller goroutine.
const sshDialTimeout = 30 * time.Second

func (c *SSHCommunicator) connect(ctx context.Context) (*ssh.Client, error) {
	var authMethods []ssh.AuthMethod

	if c.Password != "" {
		authMethods = append(authMethods, ssh.Password(c.Password))
	}
	if len(c.PrivateKey) > 0 {
		signer, err := ssh.ParsePrivateKey(c.PrivateKey)
		if err != nil {
			return nil, fmt.Errorf("parsing SSH private key: %w", err)
		}
		authMethods = append(authMethods, ssh.PublicKeys(signer))
	}

	config := &ssh.ClientConfig{
		User:            c.Username,
		Auth:            authMethods,
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	// Use the parent ctx for the dial so the ExecConn's SPDY stream lives
	// as long as the caller's context. The timeout only bounds how long we
	// wait for dial+handshake — it must NOT cancel the connection on success.
	conn, err := c.Dial(ctx)
	if err != nil {
		return nil, fmt.Errorf("dialing VM: %w", err)
	}

	// Run the SSH handshake in a goroutine with a deadline so a stuck
	// handshake can't block forever.
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		sshConn, chans, reqs, err := ssh.NewClientConn(conn, fmt.Sprintf("localhost:%d", c.Port), config)
		if err != nil {
			_ = conn.Close()
			ch <- result{err: fmt.Errorf("SSH handshake: %w", err)}
			return
		}
		ch <- result{client: ssh.NewClient(sshConn, chans, reqs)}
	}()

	select {
	case r := <-ch:
		if r.err != nil {
			return nil, r.err
		}
		startSSHKeepalive(r.client, sshKeepaliveInterval)
		return r.client, nil
	case <-time.After(sshDialTimeout):
		_ = conn.Close()
		return nil, fmt.Errorf("SSH connect timed out after %s", sshDialTimeout)
	case <-ctx.Done():
		_ = conn.Close()
		return nil, fmt.Errorf("SSH connect cancelled: %w", ctx.Err())
	}
}

// sshKeepaliveInterval is how often we ping the SSH server with
// `keepalive@openssh.com` to detect half-dead connections. golang.org/x/
// crypto/ssh ships no keepalive of its own, so without this a TCP flow held
// alive by an intermediary (KubeOVN, the relay pod) can keep a session's
// Read blocked indefinitely after the VM-side socket has gone away — the
// canonical trigger being a script reconfiguring the VM's NIC mid-cmdlet.
const sshKeepaliveInterval = 15 * time.Second

// startSSHKeepalive runs a background ping loop on the SSH client. On the
// first failed ping it force-closes the client, which unblocks any pending
// session Reads with an error so callers can retry. The goroutine exits on
// its own when the client is closed (either by us here, by the caller's
// defer, or by the peer hanging up).
func startSSHKeepalive(client *ssh.Client, interval time.Duration) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		done := make(chan struct{})
		go func() {
			_ = client.Wait()
			close(done)
		}()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if _, _, err := client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
					_ = client.Close()
					return
				}
			}
		}
	}()
}
