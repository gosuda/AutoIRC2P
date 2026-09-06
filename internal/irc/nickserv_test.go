package irc

import (
	"strings"
	"testing"
)

func nickServiceFixture() nickService {
	return nickService{account: Account{ID: 12, Nick: "alice", Password: "test-password-not-real", Email: "private@example.org", Identity: Identity{Address: "destination.b32.i2p"}}, server: "irc.example.i2p"}
}

func serviceFrame(t *testing.T, line string) frame {
	t.Helper()
	msg, err := parseFrame(line)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func verifyService(t *testing.T, service *nickService) serviceAction {
	t.Helper()
	for _, line := range []string{
		":irc.example.i2p 311 alice NickServ services services.example.i2p * :Nickname service",
		":irc.example.i2p 312 alice NickServ services.example.i2p :Services",
		":irc.example.i2p 313 alice NickServ :is a Network Service",
	} {
		if action := service.accept(serviceFrame(t, line)); action.line != "" {
			t.Fatalf("credentials or commands before WHOIS completion: %q", action.line)
		}
	}
	return service.accept(serviceFrame(t, ":irc.example.i2p 318 alice NickServ :End of WHOIS"))
}

func TestNickServRejectsSpoofedServiceNotices(t *testing.T) {
	service := nickServiceFixture()
	if action := service.accept(serviceFrame(t, ":NickServ!services@services.example.i2p NOTICE alice :This nickname is registered. Identify yourself.")); action.line != "" {
		t.Fatal("unverified NickServ received credentials")
	}
	verifyService(t, &service)
	for _, line := range []string{
		":NickServ!attacker@services.example.i2p NOTICE alice :This nickname is registered. Identify yourself.",
		":NickServ!services@attacker.i2p NOTICE alice :This nickname is registered. Identify yourself.",
		":NickServ!services@services.example.i2p PRIVMSG alice :This nickname is registered. Identify yourself.",
		":NickServ!services@services.example.i2p NOTICE bob :This nickname is registered. Identify yourself.",
	} {
		if action := service.accept(serviceFrame(t, line)); action.line != "" {
			t.Fatalf("spoofed or wrong-target notice sent credentials: %q", line)
		}
	}
}

func TestNickServRequiresServerOperatorAttestation(t *testing.T) {
	service := nickServiceFixture()
	for _, line := range []string{
		":irc.example.i2p 311 alice NickServ services services.example.i2p * :Nickname service",
		":irc.example.i2p 312 alice NickServ services.example.i2p :Services",
		":attacker!user@host 313 alice NickServ :is a Network Service",
		":irc.example.i2p 318 alice NickServ :End of WHOIS",
		":NickServ!services@services.example.i2p NOTICE alice :This nickname is registered. Identify yourself.",
	} {
		if action := service.accept(serviceFrame(t, line)); action.line != "" {
			t.Fatal("unprivileged nickname received a service command")
		}
	}
}

func TestNickServRoutesCredentialsToVerifiedServerOnce(t *testing.T) {
	service := nickServiceFixture()
	info := verifyService(t, &service)
	if info.line != "PRIVMSG NickServ@services.example.i2p :INFO alice" {
		t.Fatalf("INFO target = %q", info.line)
	}
	notice := serviceFrame(t, ":NickServ!services@services.example.i2p NOTICE alice :This nickname is registered. Identify yourself.")
	action := service.accept(notice)
	if action.line != "PRIVMSG NickServ@services.example.i2p :IDENTIFY test-password-not-real" {
		t.Fatalf("IDENTIFY did not use verified server routing: %q", action.line)
	}
	if duplicate := service.accept(notice); duplicate.line != "" {
		t.Fatal("duplicate notice replayed credentials")
	}
	if success := service.accept(serviceFrame(t, ":NickServ!services@services.example.i2p NOTICE alice :Password accepted - you are now recognized.")); !success.registered {
		t.Fatal("successful identification did not mark saved nickname registered")
	}
}

func TestNickServRegistrationDoesNotDiscloseAccountEmail(t *testing.T) {
	service := nickServiceFixture()
	verifyService(t, &service)
	action := service.accept(serviceFrame(t, ":NickServ!services@services.example.i2p NOTICE alice :Nickname alice is not registered."))
	if action.line != "PRIVMSG NickServ@services.example.i2p :REGISTER test-password-not-real destination@irc.invalid" {
		t.Fatalf("unexpected registration command: %q", action.line)
	}
	if strings.Contains(action.line, service.account.Email) {
		t.Fatal("real account email disclosed to IRC")
	}
	refusal := service.accept(serviceFrame(t, ":NickServ!services@services.example.i2p NOTICE alice :You must use a valid email address."))
	if refusal.line != "" || refusal.status == "" || refusal.registered {
		t.Fatal("email refusal did not stop registration safely")
	}
	if retry := service.accept(serviceFrame(t, ":NickServ!services@services.example.i2p NOTICE alice :Nickname alice is not registered.")); retry.line != "" {
		t.Fatal("registration retried after email rejection")
	}
}

func TestNickServInfoRegistrationTimeTriggersIdentification(t *testing.T) {
	service := nickServiceFixture()
	verifyService(t, &service)
	action := service.accept(serviceFrame(t, ":NickServ!services@services.example.i2p NOTICE alice :Time registered : Sep 06 2026"))
	if action.line != "PRIVMSG NickServ@services.example.i2p :IDENTIFY test-password-not-real" {
		t.Fatalf("INFO did not identify registered nickname: %q", action.line)
	}
}
