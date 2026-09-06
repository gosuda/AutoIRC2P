package irc

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

func TestChannelMessageRejectsInjection(t *testing.T) {
	for _, text := range []string{"hello\r\nPRIVMSG #other :injected", "hello\x00world", "\x01ACTION waves\x01", "hello\tworld", string([]byte{0xff})} {
		if _, err := channelLine("#i2p", text); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("control or malformed UTF-8 message accepted: %v", err)
		}
	}
}

func TestChannelMessageLimitsWireBytesNotRunes(t *testing.T) {
	text := strings.Repeat("가", 165) + "a"
	line, err := channelLine("#i2p", text)
	if err != nil {
		t.Fatal(err)
	}
	if len(line)+2 != 512 {
		t.Fatalf("wire length = %d, want 512", len(line)+2)
	}
	if _, err := channelLine("#i2p", text+"a"); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("513-byte message error = %v", err)
	}
}

func TestReadFrameRejectsUnboundedInput(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", 2048)), 512)
	if _, err := readFrame(reader); !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("oversized frame error = %v", err)
	}
}

func TestFramePreservesTrailingMessage(t *testing.T) {
	msg, err := parseFrame(":alice!user@destination PRIVMSG #i2p :hello : 안녕하세요")
	if err != nil {
		t.Fatal(err)
	}
	if msg.command != "PRIVMSG" || len(msg.params) != 2 || msg.params[1] != "hello : 안녕하세요" {
		t.Fatalf("message lost IRC trailing content: %+v", msg)
	}
}

func TestObserverCannotSend(t *testing.T) {
	manager := &Manager{}
	if err := manager.Send(t.Context(), 0, "#i2p", "never sent"); !errors.Is(err, ErrObserverReadOnly) {
		t.Fatalf("observer send error = %v", err)
	}
}

func TestUnconfiguredRoomCannotSend(t *testing.T) {
	manager := &Manager{rooms: map[string]string{"#i2p": "#i2p"}}
	if err := manager.Send(t.Context(), 1, "#other", "never sent"); !errors.Is(err, ErrInvalidRoom) {
		t.Fatalf("unconfigured room error = %v", err)
	}
}
