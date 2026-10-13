package directory

import (
	"context"
	"crypto/sha256"
	"errors"
	"regexp"
	"sync"
	"time"

	"voipd/internal/admin"
	"voipd/internal/auth"
	"voipd/internal/store"
)

var (
	// ErrInvalidLogin: unknown user or wrong password.
	ErrInvalidLogin = errors.New("invalid directory login")
	// ErrLocked: too many failed logins from this address.
	ErrLocked = errors.New("too many failed logins, try again later")
)

const (
	maxFailures  = 5
	failureReset = time.Minute
)

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,31}$`)

// Accounts checks directory logins. Phones log in for every lookup, and
// argon2id is deliberately slow, so successful logins are cached.
type Accounts struct {
	st  *store.Store
	Now func() time.Time

	mu       sync.Mutex
	verified map[[32]byte]store.DirectoryAccount // sha256(user, password)
	failures map[string]*failure                 // by client IP
}

type failure struct {
	n     int
	first time.Time
}

func NewAccounts(st *store.Store) *Accounts {
	return &Accounts{st: st, Now: time.Now, verified: map[[32]byte]store.DirectoryAccount{}, failures: map[string]*failure{}}
}

func cacheKey(user, password string) [32]byte {
	return sha256.Sum256([]byte(user + "\x00" + password))
}

// Verify checks a login from ip.
func (a *Accounts) Verify(ctx context.Context, user, password, ip string) (store.DirectoryAccount, error) {
	now := a.Now()
	key := cacheKey(user, password)
	a.mu.Lock()
	if f := a.failures[ip]; f != nil && now.Sub(f.first) > failureReset {
		delete(a.failures, ip)
	}
	if f := a.failures[ip]; f != nil && f.n >= maxFailures {
		a.mu.Unlock()
		return store.DirectoryAccount{}, ErrLocked
	}
	acc, ok := a.verified[key]
	a.mu.Unlock()
	if ok {
		return acc, nil
	}
	acc, err := a.st.DirectoryAccountByName(ctx, user)
	valid := false
	if err == nil {
		valid, err = auth.VerifyPassword(acc.PasswordHash, password)
	} else if errors.Is(err, store.ErrNotFound) {
		err = nil
	}
	if err != nil {
		return store.DirectoryAccount{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !valid {
		f := a.failures[ip]
		if f == nil {
			f = &failure{first: now}
			a.failures[ip] = f
		}
		f.n++
		return store.DirectoryAccount{}, ErrInvalidLogin
	}
	delete(a.failures, ip)
	a.verified[key] = acc
	go a.st.TouchDirectoryAccount(context.WithoutCancel(ctx), acc.ID, now)
	return acc, nil
}

// Create adds an account with a generated password, which is returned.
func (a *Accounts) Create(ctx context.Context, username string) (store.DirectoryAccount, string, error) {
	if !usernameRe.MatchString(username) {
		return store.DirectoryAccount{}, "", admin.Errors{"username": "1–32 characters: lowercase letters, digits, . _ -"}
	}
	pw := admin.GeneratePassword(20)
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return store.DirectoryAccount{}, "", err
	}
	id, err := a.st.CreateDirectoryAccount(ctx, username, hash, a.Now())
	if errors.Is(err, store.ErrConflict) {
		return store.DirectoryAccount{}, "", admin.Errors{"username": "already exists"}
	}
	if err != nil {
		return store.DirectoryAccount{}, "", err
	}
	acc, err := a.st.DirectoryAccount(ctx, id)
	return acc, pw, err
}

// Regenerate sets a new generated password.
func (a *Accounts) Regenerate(ctx context.Context, id int64) (string, error) {
	pw := admin.GeneratePassword(20)
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return "", err
	}
	if err := a.st.SetDirectoryPassword(ctx, id, hash); err != nil {
		return "", err
	}
	a.forget(id)
	return pw, nil
}

func (a *Accounts) Delete(ctx context.Context, id int64) error {
	if err := a.st.DeleteDirectoryAccount(ctx, id); err != nil {
		return err
	}
	a.forget(id)
	return nil
}

func (a *Accounts) forget(id int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, acc := range a.verified {
		if acc.ID == id {
			delete(a.verified, k)
		}
	}
}
