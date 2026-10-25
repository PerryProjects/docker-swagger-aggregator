package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/perryprojects/docker-swagger-aggregator/internal/store"
)

func open(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.OpenMemory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrationsApplyAndSurviveReopen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "data") // does not exist yet: Open creates it
	s, err := store.Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CreateUser(ctx, "Alice", "hash", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(ctx, dir) // second start: migrations are already applied, data is still there
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if n, _ := s.CountUsers(ctx); n != 1 {
		t.Fatalf("user must survive a restart, got %d users", n)
	}
}

func TestUsernamesAreCaseInsensitiveAndUnique(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := open(t)
	u, err := s.CreateUser(ctx, "Alice", "h", store.RoleUser)
	if err != nil || u.Username != "alice" {
		t.Fatalf("got %+v, %v", u, err)
	}
	if _, err = s.CreateUser(ctx, "ALICE", "h", store.RoleUser); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate must be refused, got %v", err)
	}
	if got, lookupErr := s.UserByName(ctx, "aLiCe"); lookupErr != nil || got.ID != u.ID {
		t.Fatalf("lookup is case-insensitive: %+v, %v", got, lookupErr)
	}
	if _, err = s.UserByName(ctx, "nobody"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSessionEpochChangesWithCredentialsAndStatus(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := open(t)
	u, _ := s.CreateUser(ctx, "bob", "h", store.RoleUser)
	epoch := func() int { got, _ := s.UserByID(ctx, u.ID); return got.SessionEpoch }
	before := epoch()
	if err := s.SetPassword(ctx, u.ID, "h2"); err != nil || epoch() != before+1 {
		t.Fatalf("password change must sign out everywhere: %v %d", err, epoch())
	}
	if err := s.SetDisabled(ctx, u.ID, true); err != nil || epoch() != before+2 {
		t.Fatalf("disabling must sign out: %v %d", err, epoch())
	}
}

func TestTheLastActiveAdminIsProtected(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := open(t)
	a, _ := s.CreateUser(ctx, "admin", "h", store.RoleAdmin)
	if err := s.SetRole(ctx, a.ID, store.RoleUser); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("demote: %v", err)
	}
	if err := s.SetDisabled(ctx, a.ID, true); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("disable: %v", err)
	}
	if err := s.DeleteUser(ctx, a.ID); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("delete: %v", err)
	}
	b, _ := s.CreateUser(ctx, "second", "h", store.RoleAdmin)
	if err := s.DeleteUser(ctx, a.ID); err != nil { // another admin exists now
		t.Fatalf("delete with a second admin: %v", err)
	}
	if err := s.SetDisabled(ctx, b.ID, true); !errors.Is(err, store.ErrLastAdmin) {
		t.Fatalf("the remaining admin is protected: %v", err)
	}
	if err := s.DeleteUser(ctx, 9999); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}
