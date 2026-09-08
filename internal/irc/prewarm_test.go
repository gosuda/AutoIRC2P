package irc

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"gosuda.org/ivnp"
)

type warmupEndpoint struct {
	*recoveryEndpoint
	ready     chan struct{}
	readyOnce sync.Once
}

func (e *warmupEndpoint) WaitReady(ctx context.Context) error {
	err := e.recoveryEndpoint.WaitReady(ctx)
	if err == nil {
		e.readyOnce.Do(func() { close(e.ready) })
	}
	return err
}

func newWarmupEndpoint(spec ivnp.DestinationSpec, attempts chan<- recoveryAttempt) *warmupEndpoint {
	return &warmupEndpoint{
		recoveryEndpoint: &recoveryEndpoint{local: spec.Local, attempts: attempts, closed: make(chan struct{})},
		ready:            make(chan struct{}),
	}
}

func assertWarmupReleased(t *testing.T, endpoint *warmupEndpoint) {
	t.Helper()
	select {
	case <-endpoint.closed:
	default:
		t.Error("unused warm endpoint remained open")
	}
	if _, err := endpoint.local.Sign([]byte("released warm identity")); err == nil {
		t.Error("unused warm identity retained signing keys")
	}
}

func TestPrewarmTransfersReadyPrimaryWithoutOpeningIRC(t *testing.T) {
	account := pooledLeaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		attempts := make(chan recoveryAttempt, 1)
		created := make(chan *warmupEndpoint, DestinationPoolSize)
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := newWarmupEndpoint(spec, attempts)
			created <- endpoint
			return endpoint, nil
		})
		caller := Account{ID: account.ID, Identity: Identity{Address: account.Identity.Address, Keys: bytes.Clone(account.Identity.Keys)}}
		if err := manager.Prewarm(t.Context(), caller); err != nil {
			t.Fatal(err)
		}
		caller.ReleaseSensitive()
		primary := <-created
		<-primary.ready
		synctest.Wait()
		select {
		case <-attempts:
			t.Fatal("prewarm opened an IRC stream")
		default:
		}
		if err := manager.Send(t.Context(), account.ID, "#first", "not joined"); !errors.Is(err, ErrNotConnected) {
			t.Fatalf("send after destination warmup = %v, want not connected", err)
		}
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		attempt := receiveRecoveryAttempt(t, attempts, account)
		if attempt.endpoint != primary.recoveryEndpoint {
			t.Fatal("login dialed a different destination instead of the warmed primary")
		}
		endpoints := []*warmupEndpoint{primary}
		for _, identity := range account.Alternates {
			endpoint := <-created
			if endpoint.local.B32() != identity.Address {
				t.Fatalf("alternate identity = %s, want %s", endpoint.local.B32(), identity.Address)
			}
			endpoints = append(endpoints, endpoint)
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range endpoints {
			assertWarmupReleased(t, endpoint)
		}
	})
}

func TestPrewarmTransfersWhileCreationIsInFlight(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		attempts := make(chan recoveryAttempt, 1)
		created := make(chan *warmupEndpoint, 2)
		creationGate := make(chan struct{})
		manager := leaseManager(t, 1, func(ctx context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := newWarmupEndpoint(spec, attempts)
			created <- endpoint
			select {
			case <-creationGate:
				return endpoint, nil
			case <-ctx.Done():
				return endpoint, ctx.Err()
			}
		})
		statuses := make(chan Event, 16)
		manager.onEvent = func(event Event) { statuses <- event }
		warmCtx, cancelWarm := context.WithCancel(t.Context())
		defer cancelWarm()
		if err := manager.Prewarm(warmCtx, account); err != nil {
			t.Fatal(err)
		}
		primary := <-created
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		cancelWarm()
		synctest.Wait()
		select {
		case status := <-statuses:
			if status.AccountID != account.ID || status.State != "connecting" {
				t.Fatalf("login waiting for warmup reported %+v", status)
			}
		default:
			t.Fatal("login waiting for warmup did not publish connecting status")
		}
		select {
		case <-created:
			t.Fatal("login created a duplicate endpoint while warmup was in flight")
		default:
		}
		close(creationGate)
		attempt := receiveRecoveryAttempt(t, attempts, account)
		if attempt.endpoint != primary.recoveryEndpoint {
			t.Fatal("in-flight warmup did not transfer to login")
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		assertWarmupReleased(t, primary)
	})
}

func TestPrewarmReservesTwoIsolatedIdentitiesWithoutBlockingColdAccounts(t *testing.T) {
	first := leaseAccount(t, 1, "alice")
	second := leaseAccount(t, 2, "bob")
	cold := leaseAccount(t, 3, "carol")
	synctest.Test(t, func(t *testing.T) {
		attempts := make(chan recoveryAttempt, 1)
		created := make(chan *warmupEndpoint, 3)
		manager := leaseManager(t, 3, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := newWarmupEndpoint(spec, attempts)
			created <- endpoint
			return endpoint, nil
		})
		for _, account := range []Account{first, second} {
			if err := manager.Prewarm(t.Context(), account); err != nil {
				t.Fatal(err)
			}
		}
		one, two := <-created, <-created
		<-one.ready
		<-two.ready
		if err := manager.Prewarm(t.Context(), cold); !errors.Is(err, errPrewarmCapacity) {
			t.Fatalf("third warmup = %v, want bounded-cache rejection", err)
		}
		stolen := first
		stolen.ID, stolen.Nick = 4, "mallory"
		if _, err := manager.Acquire(t.Context(), stolen); !errors.Is(err, errInvalidAccount) {
			t.Fatalf("cross-account warm identity reuse = %v", err)
		}
		release, err := manager.Acquire(t.Context(), cold)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		attempt := receiveRecoveryAttempt(t, attempts, cold)
		third := <-created
		if attempt.endpoint != third.recoveryEndpoint || attempt.endpoint == one.recoveryEndpoint || attempt.endpoint == two.recoveryEndpoint {
			t.Fatal("cold login used another account's warmed identity")
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		for _, endpoint := range []*warmupEndpoint{one, two, third} {
			assertWarmupReleased(t, endpoint)
		}
	})
}

func TestPrewarmIdentityChangeDiscardsStaleDestination(t *testing.T) {
	old := leaseAccount(t, 1, "alice")
	current := leaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		attempts := make(chan recoveryAttempt, 1)
		created := make(chan *warmupEndpoint, 2)
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := newWarmupEndpoint(spec, attempts)
			created <- endpoint
			return endpoint, nil
		})
		if err := manager.Prewarm(t.Context(), old); err != nil {
			t.Fatal(err)
		}
		stale := <-created
		<-stale.ready
		release, err := manager.Acquire(t.Context(), current)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		attempt := receiveRecoveryAttempt(t, attempts, current)
		fresh := <-created
		if attempt.endpoint != fresh.recoveryEndpoint {
			t.Fatal("changed identity retained a stale warmed endpoint")
		}
		assertWarmupReleased(t, stale)
	})
}

func TestActiveAccountEvictsWarmupsBeforeExceedingDestinationCapacity(t *testing.T) {
	first := leaseAccount(t, 1, "alice")
	second := leaseAccount(t, 2, "bob")
	cold := pooledLeaseAccount(t, 3, "carol")
	synctest.Test(t, func(t *testing.T) {
		attempts := make(chan recoveryAttempt, 1)
		created := make(chan *warmupEndpoint, DestinationPoolSize)
		var warmups []*warmupEndpoint
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			if spec.Local.B32() == cold.Identity.Address {
				for _, warm := range warmups {
					assertWarmupReleased(t, warm)
				}
			}
			endpoint := newWarmupEndpoint(spec, attempts)
			created <- endpoint
			return endpoint, nil
		})
		manager.destinationCapacity = DestinationPoolSize + 1
		for _, account := range []Account{first, second} {
			if err := manager.Prewarm(t.Context(), account); err != nil {
				t.Fatal(err)
			}
			endpoint := <-created
			<-endpoint.ready
			warmups = append(warmups, endpoint)
		}
		release, err := manager.Acquire(t.Context(), cold)
		if err != nil {
			t.Fatalf("warmups denied active account capacity: %v", err)
		}
		defer release()
		receiveRecoveryAttempt(t, attempts, cold)
		if err := manager.Prewarm(t.Context(), first); !errors.Is(err, errPrewarmCapacity) {
			t.Fatalf("warmup exceeded a fully occupied destination budget: %v", err)
		}
	})
}

func TestPrewarmCancellationReleasesUnclaimedDestination(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	for _, phase := range []string{"creation", "readiness", "ready"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				created := make(chan *warmupEndpoint, 1)
				manager := leaseManager(t, 1, func(ctx context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
					endpoint := newWarmupEndpoint(spec, make(chan recoveryAttempt, 1))
					if phase == "readiness" {
						endpoint.readiness = make(chan struct{})
					}
					created <- endpoint
					if phase == "creation" {
						<-ctx.Done()
						return endpoint, ctx.Err()
					}
					return endpoint, nil
				})
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if err := manager.Prewarm(ctx, account); err != nil {
					t.Fatal(err)
				}
				endpoint := <-created
				if phase == "ready" {
					<-endpoint.ready
				}
				synctest.Wait()
				cancel()
				synctest.Wait()
				assertWarmupReleased(t, endpoint)
			})
		})
	}
}

func TestPrewarmCancellationBeforeRouterReadinessCreatesNothing(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		created := make(chan *warmupEndpoint, 1)
		manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := newWarmupEndpoint(spec, make(chan recoveryAttempt, 1))
			created <- endpoint
			return endpoint, nil
		})
		manager.ready = make(chan struct{})
		ctx, cancel := context.WithCancel(t.Context())
		if err := manager.Prewarm(ctx, account); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		cancel()
		synctest.Wait()
		close(manager.ready)
		synctest.Wait()
		select {
		case <-created:
			t.Fatal("cancelled router wait still created a destination")
		default:
		}
		if err := manager.Prewarm(t.Context(), account); err != nil {
			t.Fatalf("cancelled warmup retained its reservation: %v", err)
		}
		endpoint := <-created
		<-endpoint.ready
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		assertWarmupReleased(t, endpoint)
	})
}

func TestFailedPrewarmFallsBackToFreshAccountDestination(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	for _, phase := range []string{"creation", "readiness"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := make(chan recoveryAttempt, 1)
				created := make(chan *warmupEndpoint, 2)
				failure := errors.New("destination unavailable")
				first := true
				manager := leaseManager(t, 1, func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
					endpoint := newWarmupEndpoint(spec, attempts)
					var err error
					if first {
						first = false
						if phase == "creation" {
							err = failure
						} else {
							endpoint.failure = failure
						}
					}
					created <- endpoint
					return endpoint, err
				})
				if err := manager.Prewarm(t.Context(), account); err != nil {
					t.Fatal(err)
				}
				failed := <-created
				<-failed.closed
				synctest.Wait()
				assertWarmupReleased(t, failed)
				release, err := manager.Acquire(t.Context(), account)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
				attempt := receiveRecoveryAttempt(t, attempts, account)
				fresh := <-created
				if attempt.endpoint != fresh.recoveryEndpoint || attempt.endpoint == failed.recoveryEndpoint {
					t.Fatal("login reused a failed warmup instead of creating a healthy destination")
				}
			})
		})
	}
}

func TestRouterRestartDiscardsUnclaimedWarmDestination(t *testing.T) {
	account := leaseAccount(t, 1, "alice")
	synctest.Test(t, func(t *testing.T) {
		first, second := newRecoveryNode(), newRecoveryNode()
		attempts := make(chan recoveryAttempt, 1)
		created := make(chan *warmupEndpoint, 2)
		create := func(_ context.Context, spec ivnp.DestinationSpec) (ivnp.DestinationEndpoint, error) {
			endpoint := newWarmupEndpoint(spec, attempts)
			created <- endpoint
			return endpoint, nil
		}
		oldRuntime, newRuntime := first.runtime(), second.runtime()
		oldRuntime.createDestination, newRuntime.createDestination = create, create
		manager := recoveryManager(t, oldRuntime, func() (*routerRuntime, error) { return newRuntime, nil })
		if err := manager.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := manager.Prewarm(t.Context(), account); err != nil {
			t.Fatal(err)
		}
		old := <-created
		<-old.ready
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		<-second.started
		synctest.Wait()
		assertWarmupReleased(t, old)
		if err := manager.Prewarm(t.Context(), account); err != nil {
			t.Fatal(err)
		}
		fresh := <-created
		<-fresh.ready
		release, err := manager.Acquire(t.Context(), account)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		attempt := receiveRecoveryAttempt(t, attempts, account)
		if attempt.endpoint != fresh.recoveryEndpoint {
			t.Fatal("login reused a destination from the previous router")
		}
		if err := manager.Close(); err != nil {
			t.Fatal(err)
		}
		assertWarmupReleased(t, fresh)
	})
}
