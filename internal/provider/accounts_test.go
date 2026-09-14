package provider

import (
	"errors"
	"testing"
	"time"
)

type accountTestStore struct {
	accounts []Credentials
}

func (s *accountTestStore) Get() Credentials {
	if len(s.accounts) == 0 {
		return Credentials{}
	}
	return s.accounts[0]
}

func (s *accountTestStore) Put(c Credentials) error { return s.PutAccount(c.Identity(), c) }
func (s *accountTestStore) Accounts() []Credentials { return append([]Credentials(nil), s.accounts...) }
func (s *accountTestStore) PutAccount(identity string, c Credentials) error {
	for i := range s.accounts {
		if s.accounts[i].Identity() == identity {
			s.accounts[i] = c
			return nil
		}
	}
	return errors.New("not found")
}

type accountCooldownError time.Duration

func (e accountCooldownError) Error() string           { return "failed" }
func (e accountCooldownError) Cooldown() time.Duration { return time.Duration(e) }

func TestAccountPoolPreservesOrderAndCooldown(t *testing.T) {
	now := time.Unix(1_000, 0)
	pool := AccountPool{}
	pool.SetClock(func() time.Time { return now })
	store := &accountTestStore{accounts: []Credentials{{AccountID: "one"}, {AccountID: "two"}}}
	accounts := pool.Available(store)
	if len(accounts) != 2 || accounts[0].AccountID != "one" || accounts[1].AccountID != "two" {
		t.Fatalf("accounts = %+v", accounts)
	}
	pool.MarkFailed(accounts[0], accountCooldownError(2*time.Minute))
	accounts = pool.Available(store)
	if len(accounts) != 1 || accounts[0].AccountID != "two" {
		t.Fatalf("healthy accounts = %+v", accounts)
	}
	now = now.Add(2*time.Minute + time.Second)
	accounts = pool.Available(store)
	if len(accounts) != 2 || accounts[0].AccountID != "one" {
		t.Fatalf("accounts after cooldown = %+v", accounts)
	}
}
