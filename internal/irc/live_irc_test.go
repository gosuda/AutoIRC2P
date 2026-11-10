package irc

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestLiveGosunutsDeployment(t *testing.T) {
	if os.Getenv("AUTOIRC2P_LIVE_IRC") != "1" {
		t.Skip("set AUTOIRC2P_LIVE_IRC=1 to run the live deployment check")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	headers := http.Header{"Origin": []string{"https://irc2p.gosunuts.xyz"}}
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	conn, resp, err := dialer.DialContext(ctx, "wss://irc2p.gosunuts.xyz/api/ws?room=%23i2p-chat&lang=ko", headers)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		t.Logf("wss://irc2p.gosunuts.xyz/api/ws dial error: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()

	for i := range 6 {
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, raw, readErr := conn.ReadMessage()
		if readErr != nil {
			t.Logf("WS frame[%d] read ended: %v", i, readErr)
			break
		}
		if len(raw) > 1200 {
			raw = raw[:1200]
		}
		t.Logf("WS frame[%d]: %s", i, string(raw))
	}
}

func TestLiveIRCPostmanConnection(t *testing.T) {
	if os.Getenv("AUTOIRC2P_LIVE_IRC") != "1" {
		t.Skip("set AUTOIRC2P_LIVE_IRC=1 to run the live irc.postman.i2p connection test")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()

	primary, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("generate primary identity: %v", err)
	}
	alt0, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("generate alternate identity 0: %v", err)
	}
	alt1, err := GenerateIdentity()
	if err != nil {
		t.Fatalf("generate alternate identity 1: %v", err)
	}

	observer := Account{
		ID:         0,
		Nick:       "observer",
		Identity:   primary,
		Alternates: []Identity{alt0, alt1},
	}
	defer observer.ReleaseSensitive()

	start := time.Now()
	connected := make(chan struct{})
	var connectOnce sync.Once

	manager, err := New(Config{
		Server:   "irc.postman.i2p:6667",
		Rooms:    []string{"#i2p-chat"},
		StateDir: filepath.Join(t.TempDir(), "state"),
	}, func(event Event) {
		t.Logf("[+%s] event kind=%s state=%s room=%s nick=%s text=%q",
			time.Since(start).Round(time.Millisecond),
			event.Kind, event.State, event.Room, event.Nick, event.Text)
		if event.Kind == "status" && event.State == "connected" {
			connectOnce.Do(func() { close(connected) })
		}
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	defer func() {
		_ = manager.Close()
	}()

	if err := manager.Start(ctx); err != nil {
		t.Fatalf("start manager: %v", err)
	}
	if err := manager.ConnectObserver(ctx, observer); err != nil {
		t.Fatalf("connect observer: %v", err)
	}

	select {
	case <-connected:
		t.Logf("connected to irc.postman.i2p:6667 in %s", time.Since(start).Round(time.Millisecond))
	case <-ctx.Done():
		t.Fatalf("timed out waiting for IRC 001 welcome from irc.postman.i2p:6667 after %s: %v",
			time.Since(start).Round(time.Millisecond), ctx.Err())
	}

	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		if manager.RoomState(0, "#i2p-chat") == RoomReady {
			t.Logf("joined #i2p-chat on irc.postman.i2p:6667 in %s", time.Since(start).Round(time.Millisecond))
			return
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("connected to irc.postman.i2p:6667, but timed out waiting to join #i2p-chat: %v", ctx.Err())
		}
	}
}
