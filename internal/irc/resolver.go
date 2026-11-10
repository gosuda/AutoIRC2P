package irc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"gosuda.org/ivnp"
	"gosuda.org/ivnp/client"
	"gosuda.org/ivnp/foundation"
)

type addressBookResolver struct {
	addressBook *client.AddressBookService
}

var bootstrapIRCDestinations = mustParseBootstrapIRCDestinations(map[string]string{
	"irc.postman.i2p": "d38SuD-ayF9gprdrl2uJM-FfFBor8qmBVQDxZt8SL7VZPUukrkzvxeaCE8riF5~2g1EgOA1nSfrQ4mt5NhuyL8MxGf6xByDng2lVCZ5Ks2InFVdaCEVg0BCfzdpztUGfO81oT7902zoE260P-qDuiSih5KMEouSWr7RPfP75ivoKfnRhVFcZiI36mRUfgOjKSGloeVLdkGIYh3AVYZ4MqQSWP-MXcY0xr1cD~G1ars2vfTNUjwErPirsyktbES0xXDtgr-Q7EF1vYo3cbPttFTyG6LUSEHvcHErfov5hAy~3lYkhgqMqVFyeKDEyb4ZkoSse1q2TpKxzmDAC95H4DQAGMjLIRldTSu6k0tq1kmOpmP~5ky1yRwwaQq6DOwxWcHUv9Z0Z4Z4pzXy83Z65XTBz5jLPmSdjg4dbfrOyrt9f4CtaYaoKCJl-cGFxPjTV4WHf0xtqQD~eQlsrKAT-tE3rCkU3QEvJnnjVQ6U1rG7p-W7wkT35wA0AqSouhYIEBQAEAAcAAA==",
	"irc.echelon.i2p": "ooAkOd014ooZXQ4rpvFsqAZ108YU9knllXDocY5fD84IrRVXDbRyUFtlboDmiKDPNRTY2JIXvb-l5FahAB9SUAZ8voAdH4ozHrigHVg6scVtwU7GCfjq5byuwnyemupgk6saBQKfuyc7k3bYl5Iro85Lnx6EnaYYbyWjMwoUWksI6o3Hp5bNyj2B5wHX7HLzeg1ByxQZ4Q8BZbALg8yIcvKn3rHN4tO9FhQ-e5u6hRxINbMtBL1Hasxl9I8XSI0yalKSsmpWDNxRcx-2VKlVh3MPK~9VZlBrQzXwObLmpTuTukeNt8nheFQ7mZVAHt~1gCs2TmjQPVm2g9BQ32zqCFDhePNfBcxcG-Xl56mE9n6kUvaJWsy5~VhiZZmCUGKWMx0uH7odSiC3ohNgv7dhDpANVA~gvt0IhhwlnifHBa~HHfjFQz75kl0nzv3htkcBfLt4M3AZ5bKLP8ymeGP5NYINf6uKNJA9XrSoBjmv0GqvBvDP0Zl5OO2tkIxOBEWfAAAA",
	"irc.dg.i2p":      "bunRitjuWmSGkf5UV7pnjMRIxmP7fuHy9SgDfAA0M~4TWoBr4Ji4m5AyMGzhAdNaQW6c2-0CIe~RCDZ~vcN-BSNpaqzd80gKhXYxqUQQB83XRWDdLz-z0H~Y15k90p~n0GUSzsjlZctkYglNMyQ8MAUIpUEiLz6MVwArZqUI-CDOE664ZazcGtSKfBLZKycHsSj6WfLbwl2-R5Zv4f5Xisv9Hd0b8BsqJEWn1AmBMhM7p8l7okM2ZcRnC5ypzBdLfLSdkGJ5dEZAJIxz-GhtB8rJ3e0jJkbFjknGrJfxbwt~5n00nVuiUNMro2JRxQ7w~VzMw~lYRq~1B5TMtaKRDhxk7pnv5MxmVBFNhcT~hImcyxfT7GfIPzYU9s~uiFReoSjAOJZv-rKq4Oyeyz-Pa6lcg-c3MtuTXjJ2BMX2dA5Jw0FFVDDMOdDL2b1lwtyseQQkGtc15i4EtNy6iJrkbqAnIhO86E2C3jDf~yt2FlDlcbx6dkflJ3y3j2Wu9DAgAAAA",
	"irc.00.i2p":      "YE~zhkIoJuo3mfQUEY3XVLK9Quf2mSB~auQj13-pzWydzMbW0ZfN~tIoUTKJ-6YXX1YwuU-rB3k4v5a-WjRGxue2H0jhReGnvoY3eI~5RSvquM0rA9zpkaqYXSTlHMweSQCaAZdQdQ3pOZTq0xzFuuHBKwL~0F~-khPjXTxUrixvsXJsIeNuO2UPTSj5Jwm1J3uO4OCyoBhKn0~oIWwDnOQwm0TkVoZUT1QVwSzcW0H9O99XqtO6M4yLRMrX-Dvg7iGszoigzbdngQEil3ZibKYegv3tPnz8y80UzmI4VW5sqN35qZpkhz~IV5jvSQGs~LEQZoZ0qjFaYmYcUewsW9KYlRmvVhQ32YB-X4ZbZUNSz354uK65AhHW6khBb4koJZ3ZLuPKguH66JkH6SIbB67R3CH2JKyCTLyM100sJmUgdGy69nUg8ZOuGuvETNCus4t5pWaTCkjbixCletBorMEz5vSSjCVgk2MuyxaBqicxMZ-hj7UoXHFK9CnNEeadBQAEAAcAAA==",
})

func mustParseBootstrapIRCDestinations(raw map[string]string) map[string]ivnp.Hash {
	parsed := make(map[string]ivnp.Hash, len(raw))
	for host, dest := range raw {
		identity, err := foundation.ParseDestination([]byte(dest))
		if err != nil {
			panic(fmt.Sprintf("parse bootstrap IRC destination %s: %v", host, err))
		}
		parsed[host] = identity.Hash()
	}
	return parsed
}

func newAddressBookResolver(book *client.AddressBookService) ivnp.NameResolver {
	return &addressBookResolver{addressBook: book}
}

func (r *addressBookResolver) LookupDestination(ctx context.Context, name string) (ivnp.Hash, error) {
	if r == nil {
		return ivnp.Hash{}, ivnp.ErrNameResolutionUnavailable
	}
	if err := ctx.Err(); err != nil {
		return ivnp.Hash{}, err
	}
	var lookupErr error
	if r.addressBook != nil {
		destination, err := r.addressBook.ResolveDestination(ctx, name)
		if err == nil {
			if strings.HasSuffix(strings.ToLower(destination), ".b32.i2p") {
				addr, err := ivnp.ParseAddr(net.JoinHostPort(destination, "0"))
				if err != nil {
					return ivnp.Hash{}, err
				}
				return addr.Hash, nil
			}
			identity, err := foundation.ParseDestination([]byte(destination))
			if err != nil {
				return ivnp.Hash{}, fmt.Errorf("parse resolved IRC destination: %w", err)
			}
			return identity.Hash(), nil
		}
		lookupErr = err
	}
	normalized := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if hash, ok := bootstrapIRCDestinations[normalized]; ok {
		return hash, nil
	}
	if lookupErr != nil {
		return ivnp.Hash{}, lookupErr
	}
	return ivnp.Hash{}, ivnp.ErrNameResolutionUnavailable
}

func newAddressBook(stateDir string) (*client.AddressBookService, error) {
	if stateDir == "" {
		return nil, nil
	}
	return client.AddressBookNewService(client.AddressBookConfig{
		StatePath: filepath.Join(stateDir, "addressbook.json"),
		Subscriptions: []string{
			"https://raw.githubusercontent.com/i2p/i2p.i2p/master/installer/resources/hosts.txt",
		},
	})
}

const maxSameSlotDialTimeouts = 3

func (m *Manager) dialIRC(ctx context.Context, endpoint destinationEndpoint) (net.Conn, error) {
	var err error
	for attempt := 0; attempt < maxSameSlotDialTimeouts; attempt++ {
		var conn net.Conn
		conn, err = endpoint.DialContext(ctx, "i2p", m.cfg.Server)
		if err == nil {
			return conn, nil
		}
		if ctx.Err() != nil || !errors.Is(err, os.ErrDeadlineExceeded) {
			break
		}
	}
	return nil, fmt.Errorf("dial IRC stream: %w", err)
}
