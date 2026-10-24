//go:build integration

package critical_test

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// storageLink is an opaque TCP boundary. AuthKit may clone its pgx pool, but
// every resulting connection still traverses this listener. Cutting it never
// substitutes a database/authenticator result and leaves billing's pool alive.
type storageLink struct {
	listener    net.Listener
	target      string
	mu          sync.Mutex
	closed      bool
	connections map[net.Conn]struct{}
}

func newStorageLink(t *testing.T, target string) *storageLink {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &storageLink{listener: listener, target: target, connections: map[net.Conn]struct{}{}}
	t.Cleanup(p.cut)
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go p.relay(client)
		}
	}()
	return p
}

func (p *storageLink) relay(client net.Conn) {
	server, err := net.DialTimeout("tcp", p.target, 2*time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = client.Close()
		_ = server.Close()
		return
	}
	p.connections[client] = struct{}{}
	p.connections[server] = struct{}{}
	p.mu.Unlock()
	defer func() {
		_ = client.Close()
		_ = server.Close()
		p.mu.Lock()
		delete(p.connections, client)
		delete(p.connections, server)
		p.mu.Unlock()
	}()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(server, client); _ = server.Close(); close(done) }()
	_, _ = io.Copy(client, server)
	_ = client.Close()
	<-done
}

func (p *storageLink) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	_ = p.listener.Close()
	for c := range p.connections {
		_ = c.Close()
	}
}
