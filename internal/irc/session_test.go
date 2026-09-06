package irc

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestObserverReceivesAuthenticatedAccountsWithoutSendingChannelMessages(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client, server := net.Pipe()
	if err := server.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	events := make(chan Event, 16)
	manager := &Manager{ctx: ctx, cfg: Config{Rooms: []string{"#i2p"}}, rooms: map[string]string{"#i2p": "#i2p"}, onEvent: func(event Event) { events <- event }}
	state := &accountConnection{ctx: ctx, account: Account{ID: 0, Nick: "observer"}, joined: make(map[string]bool)}
	done := make(chan error, 1)
	go func() { done <- manager.serveConnection(state, &wireConnection{Conn: client}) }()
	t.Cleanup(func() {
		cancel()
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("IRC session did not stop after cancellation")
		}
	})
	reader := bufio.NewReader(server)
	for _, expected := range []string{"NICK observer\r\n", "USER observer 0 * :HexChat\r\n"} {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line != expected {
			t.Fatalf("IRC registration = %q, want %q", line, expected)
		}
	}
	if _, err := io.WriteString(server, ":irc.example.i2p 001 observer :Welcome\r\n"); err != nil {
		t.Fatal(err)
	}
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "JOIN #i2p\r\n" {
		t.Fatalf("welcome response = %q, want room join", line)
	}
	if _, err := io.WriteString(server, ":observer!observer@observer.b32.i2p JOIN :#i2p\r\n:alice!alice@alice.b32.i2p PRIVMSG #i2p :안녕하세요\r\n"); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case event := <-events:
			if event.Kind != "message" {
				continue
			}
			if event.AccountID != 0 || event.Nick != "alice" || event.Room != "#i2p" || event.Text != "안녕하세요" || event.Service {
				t.Fatalf("observer received incorrect channel message: %+v", event)
			}
			return
		case <-time.After(5 * time.Second):
			t.Fatal("observer did not receive account's channel message")
		}
	}
}

func TestServiceOriginalRedactsCredentialsAcrossAccounts(t *testing.T) {
	observer := &accountConnection{account: Account{ID: 0}}
	manager := &Manager{accounts: map[int64]*accountConnection{
		4: {account: Account{ID: 4, Password: "private-nickserv-password"}},
	}}
	got := manager.redactService(observer, "Password accepted: private-nickserv-password. Welcome back.")
	if got != "Password accepted: [redacted]. Welcome back." {
		t.Fatalf("service original or credential redaction lost: %q", got)
	}
}

func TestAuthenticatedObserverCannotSendChat(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	client, server := net.Pipe()
	if err := server.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	account := nickServiceFixture().account
	account.ID = 0
	account.Password = "local-only:!@"
	events := make(chan Event, 32)
	manager := &Manager{ctx: ctx, cfg: Config{Rooms: []string{"#test"}}, rooms: map[string]string{"#test": "#test"}, onEvent: func(event Event) { events <- event }}
	state := &accountConnection{ctx: ctx, account: account, joined: make(map[string]bool)}
	done := make(chan error, 1)
	go func() { done <- manager.serveConnection(state, &wireConnection{Conn: client}) }()
	defer func() {
		cancel()
		if err := server.Close(); err != nil {
			t.Error(err)
		}
		<-done
	}()
	reader := bufio.NewReader(server)
	readLine := func() string {
		t.Helper()
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	readLine()
	readLine()
	if _, err := io.WriteString(server, ":irc.example.i2p 001 alice :Welcome\r\n"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"WHOIS NickServ\r\n", "JOIN #test\r\n"} {
		if got := readLine(); got != want {
			t.Fatalf("observer startup = %q, want %q", got, want)
		}
	}
	verified := strings.Join([]string{
		":irc.example.i2p 311 alice NickServ services services.example.i2p * :Nickname service",
		":irc.example.i2p 312 alice NickServ services.example.i2p :Services",
		":irc.example.i2p 313 alice NickServ :is a Network Service",
		":irc.example.i2p 318 alice NickServ :End of WHOIS",
	}, "\r\n") + "\r\n"
	if _, err := io.WriteString(server, verified); err != nil {
		t.Fatal(err)
	}
	if got := readLine(); got != "PRIVMSG NickServ@services.example.i2p :INFO alice\r\n" {
		t.Fatalf("observer lookup = %q", got)
	}
	if _, err := io.WriteString(server, ":NickServ!services@services.example.i2p NOTICE alice :This nickname is registered. Identify yourself.\r\n"); err != nil {
		t.Fatal(err)
	}
	if got := readLine(); got != "PRIVMSG NickServ@services.example.i2p :IDENTIFY local-only:!@\r\n" {
		t.Fatalf("observer identification = %q", got)
	}
	if _, err := io.WriteString(server, ":NickServ!services@services.example.i2p NOTICE alice :Password accepted.\r\n"); err != nil {
		t.Fatal(err)
	}
	for {
		event := <-events
		if event.Kind == "registered" {
			if event.AccountID != 0 {
				t.Fatal("observer authentication changed account ownership")
			}
			break
		}
	}
	if err := manager.Send(ctx, 0, "#test", "must not send"); !errors.Is(err, ErrObserverReadOnly) {
		t.Fatalf("authenticated observer send = %v", err)
	}
	if _, err := io.WriteString(server, "PING :after-auth\r\n"); err != nil {
		t.Fatal(err)
	}
	if got := readLine(); got != "PONG :after-auth\r\n" {
		t.Fatalf("unexpected traffic after authentication: %q", got)
	}
}

func TestJoinPacingKeepsPongResponsiveAndCancelsPendingRooms(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		client, server := net.Pipe()
		if err := server.SetDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		manager := &Manager{ctx: ctx, cfg: Config{Rooms: []string{"#first", "#second", "#third"}}, onEvent: func(Event) {}}
		state := &accountConnection{ctx: ctx, account: Account{Nick: "reader"}, joined: make(map[string]bool)}
		done := make(chan error, 1)
		go func() { done <- manager.serveConnection(state, &wireConnection{Conn: client}) }()
		defer func() {
			cancel()
			if err := server.Close(); err != nil {
				t.Error(err)
			}
			<-done
		}()
		reader := bufio.NewReader(server)
		readLine := func() string {
			t.Helper()
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			return line
		}
		readLine()
		readLine()
		if _, err := io.WriteString(server, ":irc.example.i2p 001 reader :Welcome\r\n"); err != nil {
			t.Fatal(err)
		}
		if got := readLine(); got != "JOIN #first\r\n" {
			t.Fatalf("first join = %q", got)
		}
		first := time.Now()
		if _, err := io.WriteString(server, "PING :keepalive\r\n"); err != nil {
			t.Fatal(err)
		}
		if got := readLine(); got != "PONG :keepalive\r\n" {
			t.Fatalf("response during join pacing = %q", got)
		}
		if elapsed := time.Since(first); elapsed >= 2*time.Second {
			t.Fatalf("PONG blocked behind join timer: %s", elapsed)
		}
		if got := readLine(); got != "JOIN #second\r\n" {
			t.Fatalf("second join = %q", got)
		}
		if elapsed := time.Since(first); elapsed < 2*time.Second {
			t.Fatalf("JOIN burst after %s", elapsed)
		}
		cancel()
		remaining, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if len(remaining) != 0 {
			t.Fatalf("sent after cancellation: %q", remaining)
		}
	})
}

func TestI2PKeepaliveToleratesDelayedPingAndPong(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		idle, pong, silence, writeDelay time.Duration
	}{
		{"defaults", 0, 0, 6 * time.Minute, time.Minute},
		{"configured", 30 * time.Minute, 4 * time.Minute, 25 * time.Minute, 3 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				client, server := net.Pipe()
				manager := &Manager{ctx: ctx, cfg: Config{IdleTimeout: tc.idle, PongTimeout: tc.pong}, onEvent: func(Event) {}}
				state := &accountConnection{ctx: ctx, account: Account{Nick: "reader"}, joined: make(map[string]bool)}
				done := make(chan error, 1)
				go func() { done <- manager.serveConnection(state, &wireConnection{Conn: client}) }()
				defer func() {
					cancel()
					if err := server.Close(); err != nil {
						t.Error(err)
					}
					<-done
				}()
				reader := bufio.NewReader(server)
				for range 2 {
					if _, err := reader.ReadString('\n'); err != nil {
						t.Fatal(err)
					}
				}
				<-time.After(2 * time.Minute)
				if _, err := io.WriteString(server, "PING :before-welcome\r\n"); err != nil {
					t.Fatal(err)
				}
				<-time.After(tc.writeDelay)
				if line, err := reader.ReadString('\n'); err != nil || line != "PONG :before-welcome\r\n" {
					t.Fatalf("registration PONG = %q, %v", line, err)
				}
				if _, err := io.WriteString(server, ":irc.example.i2p 001 reader :Welcome\r\n"); err != nil {
					t.Fatal(err)
				}
				for range int(tc.silence / (30 * time.Second)) {
					if line, err := reader.ReadString('\n'); err != nil || line != "PING :autoirc2p\r\n" {
						t.Fatalf("client keepalive = %q, %v", line, err)
					}
				}
				if _, err := io.WriteString(server, "PING :slow-route\r\n"); err != nil {
					t.Fatal(err)
				}
				if line, err := reader.ReadString('\n'); err != nil || line != "PONG :slow-route\r\n" {
					t.Fatalf("delayed PONG = %q, %v", line, err)
				}
			})
		})
	}
}
