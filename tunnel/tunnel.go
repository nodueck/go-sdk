package tunnel

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/gorilla/websocket"
)

// WithADBPath lets you supply a custom path to the adb executable if it's not in PATH.
func WithADBPath(p string) Option {
	return func(t *Tunnel) {
		t.ADBPath = p
	}
}

// WithAutoConnect allows enabling/disabling the internal `adb connect <Addr()>` call
// after the TCP listener is up. In Kubernetes/remote mode set this to false so that
// an external client can take the single available connection.
func WithAutoConnect(b bool) Option {
	return func(t *Tunnel) {
		t.AutoConnect = b
	}
}

// WithAdvertiseHost sets the host part used by Addr(). If empty, Addr() falls
// back to "127.0.0.1" (local dev). In Kubernetes set this to your Service FQDN,
// e.g. "deviceapi-hl.droidrun.svc.cluster.local".
func WithAdvertiseHost(h string) Option {
	return func(t *Tunnel) {
		t.AdvertiseHost = h
	}
}

type Option func(*Tunnel)

// New returns a new Tunnel that will listen on an available port and converts Tunnel traffic into WebSocket.
func New(remoteURL, token string, opts ...Option) (*Tunnel, error) {
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		return nil, fmt.Errorf("creating a tcp listener failed: %w", err)
	}
	t := &Tunnel{
		RemoteURL:     remoteURL,
		Token:         token,
		ADBPath:       "adb",
		AutoConnect:   true,
		AdvertiseHost: "",
		listener:      listener,
	}

	// TUNNEL_AUTOCONNECT=false  → AutoConnect off
	// TUNNEL_ADVERTISE_HOST=... → set AdvertiseHost
	if v := os.Getenv("TUNNEL_AUTOCONNECT"); v == "0" || v == "false" {
		t.AutoConnect = false
	}
	if v := os.Getenv("TUNNEL_ADVERTISE_HOST"); v != "" {
		t.AdvertiseHost = v
	}

	for _, f := range opts {
		f(t)
	}
	return t, nil
}

// Tunnel connects to a remote WebSocket endpoint and forwards Tunnel packets from and to the address it listens on locally.
type Tunnel struct {
	// RemoteURL is the URL of the remote server.
	RemoteURL string

	// Token is used to authenticate the user. The server may still reject it
	// if it's marked as revoked.
	Token string

	// ADBPath is the path to adb executable. Defaults to just "adb".
	ADBPath string

	// AdvertiseHost is the host part used by Addr(). If empty, Addr() uses 127.0.0.1.
	AdvertiseHost string

	// AutoConnect controls whether Start() runs a local `adb connect Addr()`.
	AutoConnect bool

	listener net.Listener
	cancel   context.CancelCauseFunc
}

// Start starts a tunnel to the Android instance through the given URL and optionally
// runs a local `adb connect Addr()` if AutoConnect is true.
// It is non-blocking and continues to run in the background.
// Call Close() method of the returned Tunnel to make sure it's properly cleaned up.
func (t *Tunnel) Start() error {
	go func() {
		if err := t.startTunnel(); err != nil {
			log.Printf("failed to start TCP tunnel: %s", err)
		}
	}()

	if t.AutoConnect {
		out, err := exec.CommandContext(context.Background(), t.ADBPath, "connect", t.Addr()).CombinedOutput()
		if err != nil {
			return fmt.Errorf("failed to connect adb: %w %s", err, string(out))
		}
	}
	return nil
}

// Addr returns the advertised host:port for clients.
// In cluster set AdvertiseHost to a Service FQDN; locally it falls back to 127.0.0.1.
func (t *Tunnel) Addr() string {
	host := t.AdvertiseHost
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, t.listener.Addr().(*net.TCPAddr).Port)
}

// Close closes the underlying Tunnel listener.
func (t *Tunnel) Close() {
	if t.cancel != nil {
		t.cancel(nil)
	}
}

// startTunnel starts the local Tunnel server to forward to WebSocket.
// Blocks until connection is closed.
// Single-client semantics: we accept exactly one TCP connection, then pump bytes TCP<->WS until close.
func (t *Tunnel) startTunnel() error {
	tCtx, cancel := context.WithCancelCause(context.Background())
	t.cancel = cancel
	defer cancel(nil)

	defer func() { _ = t.listener.Close() }()

	tcpConn, err := t.listener.Accept()
	if err != nil {
		return fmt.Errorf("failed to accept connection: %w", err)
	}
	defer func() { _ = tcpConn.Close() }()

	ws, _, err := websocket.DefaultDialer.Dial(t.RemoteURL, http.Header{
		"Authorization": []string{fmt.Sprintf("Bearer %s", t.Token)},
	})
	if err != nil {
		return fmt.Errorf("failed to dial remote websocket server: %w", err)
	}
	defer func() { _ = ws.Close() }()

	// keepalive ping
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-tCtx.Done():
				return
			case <-ticker.C:
				if err := ws.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(10*time.Second)); err != nil {
					cancel(fmt.Errorf("ping failed: %v", err))
					return
				}
			}
		}
	}()

	// TCP → WS
	go func() {
		// 32Kb is default frame size.
		buffer := make([]byte, 32*1024)
		for {
			select {
			case <-tCtx.Done():
				return
			default:
			}

			n, err := tcpConn.Read(buffer)
			if err != nil {
				if err != io.EOF {
					cancel(fmt.Errorf("failed to read from tcp: %w", err))
				} else {
					log.Printf("tcp->ws: TCP connection closed by client")
				}
				return
			}

			if n > 0 {
				err = ws.WriteMessage(websocket.BinaryMessage, buffer[:n])
				if err != nil {
					cancel(fmt.Errorf("failed to write to websocket: %w", err))
					return
				}
			}
		}
	}()

	// WS → TCP
	go func() {
		for {
			select {
			case <-tCtx.Done():
				return
			default:
			}
			_, message, err := ws.ReadMessage()
			if err != nil {
				cancel(fmt.Errorf("websocket read error: %w", err))
				return
			}
			if len(message) > 0 {
				_, err = tcpConn.Write(message)
				if err != nil {
					cancel(fmt.Errorf("failed to write to tcp: %w", err))
					return
				}
			}
		}
	}()
	<-tCtx.Done()
	return context.Cause(tCtx)
}
