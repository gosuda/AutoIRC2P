package irc

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gosuda.org/ivnp/client"
)

type resolverTestDialer struct {
	target   string
	listener net.Listener
}

func (d resolverTestDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "i2p" || address != d.target {
		return nil, fmt.Errorf("unexpected I2P destination: %s %s, want i2p %s", network, address, d.target)
	}
	return (&net.Dialer{}).DialContext(ctx, "tcp", d.listener.Addr().String())
}

func TestDialIRCResolvesHostOnTheAccountEndpoint(t *testing.T) {
	identity, err := GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(identity.Keys)
	local, err := restoreIdentity(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer local.ReleaseSensitive()
	hosts := filepath.Join(t.TempDir(), "hosts.txt")
	if err := os.WriteFile(hosts, []byte("irc.example.i2p="+string(local.Destination())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	book, err := client.AddressBookNewService(client.AddressBookConfig{HostsPath: hosts})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := errors.Join(book.Close(), book.Wait()); err != nil {
			t.Error(err)
		}
	}()
	for _, tc := range []struct {
		name, server, port string
		book               *client.AddressBookService
	}{
		{"addressbook hostname", "irc.example.i2p:6667", "6667", book},
		{"b32 without addressbook", net.JoinHostPort(identity.Address, "6697"), "6697", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			finished := make(chan error, 1)
			go func() {
				incoming, err := listener.Accept()
				if err != nil {
					finished <- err
					return
				}
				_ = incoming.SetWriteDeadline(time.Now().Add(time.Second))
				_, err = fmt.Fprint(incoming, ":irc.example.i2p 001 reader :Welcome\r\n")
				finished <- errors.Join(err, incoming.Close())
			}()
			defer func() {
				if err := listener.Close(); err != nil {
					t.Error(err)
				}
				if err := <-finished; err != nil && !errors.Is(err, net.ErrClosed) {
					t.Error(err)
				}
			}()
			manager := &Manager{cfg: Config{Server: tc.server}, router: &routerRuntime{addressBook: tc.book}}
			endpoint := resolverTestDialer{target: net.JoinHostPort(identity.Address, tc.port), listener: listener}
			conn, err := manager.dialIRC(t.Context(), endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			line, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if line != ":irc.example.i2p 001 reader :Welcome\r\n" {
				t.Fatalf("IRC welcome = %q", line)
			}
		})
	}
}
