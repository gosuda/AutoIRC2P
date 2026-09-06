package irc

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

func TestRoomReadinessRequiresJoinAndTracksRemoval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		client, peer := net.Pipe()
		if err := peer.SetDeadline(time.Now().Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		events := make(chan Event, 32)
		account := &accountConnection{ctx: ctx, account: Account{ID: 1, Nick: "alice"}, joined: make(map[string]bool)}
		manager := &Manager{ctx: ctx, started: true, cfg: Config{Rooms: []string{"#first", "#second"}}, rooms: map[string]string{"#first": "#first", "#second": "#second"}, accounts: map[int64]*accountConnection{1: account}, onEvent: func(event Event) { events <- event }}
		done := make(chan error, 1)
		go func() { done <- manager.serveConnection(account, &wireConnection{Conn: client}) }()
		defer func() {
			cancel()
			if err := peer.Close(); err != nil {
				t.Error(err)
			}
			<-done
		}()
		reader := bufio.NewReader(peer)
		for range 2 {
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := io.WriteString(peer, ":irc.example.i2p 001 alice :Welcome\r\n"); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := reader.ReadString('\n'); err != nil {
				t.Fatal(err)
			}
		}
		if got := manager.RoomState(1, "#first"); got != RoomPreparing {
			t.Fatalf("welcome alone enabled room: %s", got)
		}
		transition := func(line, room string, want MembershipState) {
			t.Helper()
			if _, err := io.WriteString(peer, line+"\r\n"); err != nil {
				t.Fatal(err)
			}
			for {
				event := <-events
				if event.Kind != "membership" {
					continue
				}
				if event.Room != room || event.State != string(want) {
					t.Fatalf("membership event = %+v", event)
				}
				break
			}
			if got := manager.RoomState(1, room); got != want {
				t.Fatalf("%s readiness = %s, want %s", room, got, want)
			}
		}
		transition(":alice!u@host JOIN :#first", "#first", RoomReady)
		if got := manager.RoomState(1, "#second"); got != RoomPreparing {
			t.Fatalf("joining first enabled second: %s", got)
		}
		transition(":irc.example.i2p 473 alice #second :Invite only", "#second", RoomUnavailable)
		if got := manager.RoomState(1, "#first"); got != RoomReady {
			t.Fatalf("other room rejection disabled first: %s", got)
		}
		transition(":operator!u@host KICK #first alice :Closed", "#first", RoomUnavailable)
		transition(":alice!u@host JOIN :#second", "#second", RoomReady)
		transition(":alice!u@host PART #second :Leaving", "#second", RoomUnavailable)
		cancel()
		if got := manager.RoomState(1, "#first"); got != RoomUnavailable {
			t.Fatalf("canceled connection readiness = %s", got)
		}
	})
}
