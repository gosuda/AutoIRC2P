package irc

import (
	"strings"
)

// Service credentials are routed to the server learned from WHOIS, never merely
// to a nickname. Numeric 313 is required: ordinary clients cannot assert it.
type nickService struct {
	account       Account
	server        string
	mask          string
	serviceServer string
	operator      bool
	verified      bool
	queried       bool
	registerSent  bool
	identifySent  bool
	done          bool
}

type serviceAction struct {
	line       string
	status     string
	registered bool
}

func (s *nickService) accept(msg frame) serviceAction {
	if s.done || s.account.Password == "" {
		return serviceAction{}
	}
	if msg.prefix == s.server && len(msg.params) >= 2 && fold(msg.params[0]) == fold(s.account.Nick) && fold(msg.params[1]) == "nickserv" {
		switch msg.command {
		case "311":
			if len(msg.params) >= 5 && safeAtom(msg.params[2]) && safeAtom(msg.params[3]) {
				s.mask = "NickServ!" + msg.params[2] + "@" + msg.params[3]
			}
		case "312":
			if len(msg.params) >= 3 && safeAtom(msg.params[2]) && strings.Contains(msg.params[2], ".") {
				s.serviceServer = msg.params[2]
			}
		case "313":
			s.operator = true
		case "318":
			if s.mask == "" || s.serviceServer == "" || !s.operator {
				s.done = true
				return serviceAction{status: "NickServ could not be verified as a network service; no credentials sent"}
			}
			s.verified = true
			if !s.queried {
				s.queried = true
				return serviceAction{line: s.command("INFO " + s.account.Nick)}
			}
		}
	}
	if !s.verified || msg.command != "NOTICE" || len(msg.params) != 2 || fold(msg.prefix) != fold(s.mask) || fold(msg.params[0]) != fold(s.account.Nick) {
		return serviceAction{}
	}
	text := strings.ToLower(stripFormatting(msg.params[1]))
	switch {
	case strings.Contains(text, "password accepted"), strings.Contains(text, "you are now identified"), strings.Contains(text, "you are now recognized"), strings.Contains(text, "you are already identified"), strings.Contains(text, "you are now logged in"):
		if s.identifySent || s.registerSent {
			s.done = true
			return serviceAction{status: "NickServ authentication succeeded", registered: true}
		}
	case strings.Contains(text, "nickname registered"), strings.Contains(text, "nickname "+strings.ToLower(s.account.Nick)+" registered"), strings.Contains(text, "has been registered"), strings.Contains(text, "successfully registered"), strings.Contains(text, "is now registered"):
		if s.registerSent {
			s.done = true
			return serviceAction{status: "NickServ registration succeeded", registered: true}
		}
	case strings.Contains(text, "not registered"), strings.Contains(text, "isn't registered"):
		if s.registerSent {
			s.done = true
			return serviceAction{status: "NickServ registration state differs from the saved account; manual recovery required"}
		}
		s.registerSent = true
		// The application's email address is never sent to IRC.
		email := strings.TrimSuffix(s.account.Identity.Address, ".b32.i2p") + "@irc.invalid"
		return serviceAction{line: s.command("REGISTER " + s.account.Password + " " + email), status: "Registering nickname with NickServ"}
	case strings.Contains(text, "invalid email"), strings.Contains(text, "invalid e-mail"), strings.Contains(text, "valid email"), strings.Contains(text, "valid e-mail"):
		s.done = true
		return serviceAction{status: "NickServ registration failed: network requires another email; the application will not disclose your account email"}
	case strings.Contains(text, "password incorrect"), strings.Contains(text, "incorrect password"), strings.Contains(text, "invalid password"), strings.Contains(text, "access denied"), strings.Contains(text, "authentication failed"), strings.Contains(text, "registration is disabled"), strings.Contains(text, "syntax:"), strings.Contains(text, "verification email"), strings.Contains(text, "confirmation code"):
		s.done = true
		return serviceAction{status: "NickServ authentication failed or requires manual verification; no further credentials sent"}
	case strings.Contains(text, "is registered"), strings.Contains(text, "registered:"), strings.Contains(text, "registered :"), strings.Contains(text, "time registered"), strings.Contains(text, "registered on"), strings.Contains(text, "registered at"), strings.Contains(text, "identify yourself"), strings.Contains(text, "identify via"), strings.Contains(text, "/msg nickserv identify"), strings.Contains(text, "already registered"):
		if !s.identifySent {
			s.identifySent = true
			return serviceAction{line: s.command("IDENTIFY " + s.account.Password), status: "Identifying nickname with NickServ"}
		}
	}
	return serviceAction{}
}

func (s *nickService) command(text string) string {
	return "PRIVMSG NickServ@" + s.serviceServer + " :" + text
}

func safeAtom(text string) bool {
	if text == "" || len(text) > 128 {
		return false
	}
	return !strings.ContainsAny(text, " \t\r\n\x00!@:") && validText(text)
}

func stripFormatting(text string) string {
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c == 3 {
			for count := 0; count < 2 && i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9'; count++ {
				i++
			}
			if i+1 < len(text) && text[i+1] == ',' {
				i++
				for count := 0; count < 2 && i+1 < len(text) && text[i+1] >= '0' && text[i+1] <= '9'; count++ {
					i++
				}
			}
			continue
		}
		if c >= 32 && c != 127 {
			out.WriteByte(c)
		}
	}
	return out.String()
}
