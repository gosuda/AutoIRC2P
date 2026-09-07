package irc

import (
	"bufio"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

var (
	ErrInvalidMessage = errors.New("message must be nonempty UTF-8 without control characters")
	ErrLineTooLong    = errors.New("IRC message exceeds the 512-byte wire limit")
	ErrInvalidRoom    = errors.New("room is not configured")
	ErrInvalidNick    = errors.New("nickname must be 1–30 ASCII IRC nickname characters")
	errMalformedFrame = errors.New("malformed IRC frame")
)

type frame struct {
	prefix    string
	command   string
	params    []string
	rawParams string
}

type frameReader struct {
	reader  *bufio.Reader
	partial []byte
}

func (r *frameReader) read() (frame, error) {
	bytes, err := r.reader.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(r.partial)+len(bytes) > 512 {
		return frame{}, ErrLineTooLong
	}
	if err != nil && timeoutError(err) {
		// NickServ expiry interrupts reads without terminating the IRC stream.
		r.partial = append(r.partial, bytes...)
		return frame{}, err
	}
	if err != nil && len(r.partial)+len(bytes) != 0 {
		return frame{}, errMalformedFrame
	}
	if err != nil {
		return frame{}, err
	}
	if len(r.partial) != 0 {
		bytes = append(r.partial, bytes...)
		r.partial = bytes[:0]
	}
	line := string(bytes)
	if !strings.HasSuffix(line, "\r\n") {
		return frame{}, errMalformedFrame
	}
	return parseFrame(line[:len(line)-2])
}

func parseFrame(line string) (frame, error) {
	var msg frame
	if line == "" || strings.ContainsAny(line, "\r\n\x00") {
		return msg, errMalformedFrame
	}
	if strings.HasPrefix(line, "@") {
		_, rest, ok := strings.Cut(line, " ")
		if !ok {
			return msg, errMalformedFrame
		}
		line = strings.TrimLeft(rest, " ")
	}
	if strings.HasPrefix(line, ":") {
		prefix, rest, ok := strings.Cut(line[1:], " ")
		if !ok || prefix == "" {
			return msg, errMalformedFrame
		}
		msg.prefix = prefix
		line = strings.TrimLeft(rest, " ")
	}
	msg.command, line, _ = strings.Cut(line, " ")
	msg.command = strings.ToUpper(msg.command)
	if msg.command == "" {
		return frame{}, errMalformedFrame
	}
	msg.rawParams = line
	for line != "" {
		line = strings.TrimLeft(line, " ")
		if line == "" {
			break
		}
		if len(msg.params) == 15 {
			return frame{}, errMalformedFrame
		}
		if line[0] == ':' {
			msg.params = append(msg.params, line[1:])
			break
		}
		var param string
		param, line, _ = strings.Cut(line, " ")
		msg.params = append(msg.params, param)
	}
	return msg, nil
}

type ctcpResponder struct {
	lastVersion time.Time
	lastPing    time.Time
}

func (r *ctcpResponder) accept(msg frame, nick string, now time.Time) string {
	if msg.command != "PRIVMSG" || len(msg.params) != 2 || fold(msg.params[0]) != fold(nick) {
		return ""
	}
	sender := sourceNick(msg.prefix)
	if !validNick(sender) {
		return ""
	}
	payload, ok := strings.CutPrefix(msg.params[1], "\x01")
	if !ok {
		return ""
	}
	payload = strings.TrimSuffix(payload, "\x01")
	if strings.ContainsAny(payload, "\x00\x01\r\n") {
		return ""
	}
	command, params, hasParams := strings.Cut(payload, " ")
	var lastReply *time.Time
	switch {
	case strings.EqualFold(command, "VERSION") && !hasParams:
		payload = "VERSION HexChat 2.16.2"
		lastReply = &r.lastVersion
	case strings.EqualFold(command, "PING") && hasParams && params != "":
		lastReply = &r.lastPing
	default:
		return ""
	}
	if len("NOTICE ")+len(sender)+len(" :\x01")+len(payload)+len("\x01\r\n") > 512 {
		return ""
	}
	if !lastReply.IsZero() && now.Sub(*lastReply) < 10*time.Second {
		return ""
	}
	*lastReply = now
	return "NOTICE " + sender + " :\x01" + payload + "\x01"
}

func validNick(nick string) bool {
	if len(nick) == 0 || len(nick) > 30 {
		return false
	}
	for i, c := range []byte(nick) {
		lowercase := c >= 'a' && c <= 'z'
		uppercase := c >= 'A' && c <= 'Z'
		if lowercase || uppercase || strings.ContainsRune("[]\\`_^{|}", rune(c)) {
			continue
		}
		digit := c >= '0' && c <= '9'
		if i > 0 && (digit || c == '-') {
			continue
		}
		return false
	}
	return true
}

func validRoom(room string) bool {
	if len(room) < 2 || len(room) > 50 || room[0] != '#' || !utf8.ValidString(room) {
		return false
	}
	return !strings.ContainsFunc(room, func(c rune) bool { return unicode.IsControl(c) || unicode.IsSpace(c) || c == ',' || c == ':' })
}

func validText(text string) bool {
	return strings.TrimSpace(text) != "" && utf8.ValidString(text) && !strings.ContainsFunc(text, unicode.IsControl)
}

func validPassword(password string) bool {
	return len(password) <= 128 && validText(password) && !strings.ContainsFunc(password, unicode.IsSpace)
}

func channelLine(room, text string) (string, error) {
	if !validRoom(room) {
		return "", ErrInvalidRoom
	}
	if !validText(text) {
		return "", ErrInvalidMessage
	}
	if len("PRIVMSG ")+len(room)+len(" :")+len(text)+2 > 512 {
		return "", ErrLineTooLong
	}
	return "PRIVMSG " + room + " :" + text, nil
}

func fold(value string) string {
	return strings.Map(func(c rune) rune {
		switch c {
		case '[':
			return '{'
		case ']':
			return '}'
		case '\\':
			return '|'
		case '^':
			return '~'
		}
		if c >= 'A' && c <= 'Z' {
			return c + ('a' - 'A')
		}
		return c
	}, value)
}

func sourceNick(prefix string) string {
	nick, _, _ := strings.Cut(prefix, "!")
	return nick
}

func serverSource(prefix string) bool {
	return prefix != "" && !strings.ContainsAny(prefix, "!@ :\r\n\x00")
}
