// Package carddavtest is an in-memory CardDAV server for tests: one
// address book behind basic auth, with counters to check that clients
// fetch only what changed.
package carddavtest

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"
)

type Server struct {
	User, Password string
	// Home is the address book home ("/dav/u/"); the book is Home+"contacts/".
	Home string

	mu    sync.Mutex
	cards map[string]vcard.Card // path → card
	etags map[string]string

	// Fetched counts vCards delivered (GET or multiget).
	Fetched atomic.Int64
	// Fail makes every request fail with this status (0 = off).
	Fail atomic.Int32
}

func New(user, password, home string) *Server {
	if !strings.HasSuffix(home, "/") {
		home += "/"
	}
	return &Server{User: user, Password: password, Home: home, cards: map[string]vcard.Card{}, etags: map[string]string{}}
}

// Book is the address book path.
func (s *Server) Book() string { return s.Home + "contacts/" }

// Put adds or replaces a contact with the given UID and phone numbers.
func (s *Server) Put(uid, name string, tels ...string) {
	c := vcard.Card{}
	c.SetValue(vcard.FieldVersion, "3.0")
	c.SetValue(vcard.FieldUID, uid)
	c.SetValue(vcard.FieldFormattedName, name)
	for _, t := range tels {
		c.Add(vcard.FieldTelephone, &vcard.Field{Value: t, Params: vcard.Params{vcard.ParamType: {"voice"}}})
	}
	p := s.Book() + uid + ".vcf"
	sum := sha1.Sum([]byte(fmt.Sprint(name, tels)))
	s.mu.Lock()
	s.cards[p] = c
	s.etags[p] = hex.EncodeToString(sum[:8])
	s.mu.Unlock()
}

func (s *Server) Delete(uid string) {
	s.mu.Lock()
	delete(s.cards, s.Book()+uid+".vcf")
	s.mu.Unlock()
}

func (s *Server) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.cards)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if code := s.Fail.Load(); code != 0 {
		http.Error(w, "failing on purpose", int(code))
		return
	}
	if u, p, ok := r.BasicAuth(); !ok || u != s.User || p != s.Password {
		w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	(&carddav.Handler{Backend: (*backend)(s)}).ServeHTTP(w, r)
}

type backend Server

func (b *backend) CurrentUserPrincipal(context.Context) (string, error) { return b.Home, nil }
func (b *backend) AddressBookHomeSetPath(context.Context) (string, error) {
	return b.Home, nil
}

func (b *backend) book() carddav.AddressBook {
	return carddav.AddressBook{Path: (*Server)(b).Book(), Name: "Contacts"}
}

func (b *backend) ListAddressBooks(context.Context) ([]carddav.AddressBook, error) {
	return []carddav.AddressBook{b.book()}, nil
}

func (b *backend) GetAddressBook(_ context.Context, p string) (*carddav.AddressBook, error) {
	ab := b.book()
	if strings.TrimSuffix(p, "/") != strings.TrimSuffix(ab.Path, "/") {
		return nil, webdav.NewHTTPError(http.StatusNotFound, nil)
	}
	return &ab, nil
}

func (b *backend) CreateAddressBook(context.Context, *carddav.AddressBook) error {
	return webdav.NewHTTPError(http.StatusForbidden, nil)
}
func (b *backend) DeleteAddressBook(context.Context, string) error {
	return webdav.NewHTTPError(http.StatusForbidden, nil)
}

func (b *backend) object(p string, withCard bool) (*carddav.AddressObject, bool) {
	c, ok := b.cards[p]
	if !ok {
		return nil, false
	}
	o := &carddav.AddressObject{Path: p, ETag: b.etags[p]}
	if withCard {
		o.Card = c
	}
	return o, true
}

func (b *backend) GetAddressObject(_ context.Context, p string, _ *carddav.AddressDataRequest) (*carddav.AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.object(path.Clean(p), true)
	if !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, nil)
	}
	b.Fetched.Add(1)
	return o, nil
}

func (b *backend) ListAddressObjects(_ context.Context, p string, _ *carddav.AddressDataRequest) ([]carddav.AddressObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.cards))
	for k := range b.cards {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]carddav.AddressObject, 0, len(keys))
	for _, k := range keys {
		o, _ := b.object(k, true) // the handler needs the card for address-data
		out = append(out, *o)
	}
	return out, nil
}

func (b *backend) QueryAddressObjects(ctx context.Context, p string, _ *carddav.AddressBookQuery) ([]carddav.AddressObject, error) {
	return b.ListAddressObjects(ctx, p, nil)
}

func (b *backend) PutAddressObject(context.Context, string, vcard.Card, *carddav.PutAddressObjectOptions) (*carddav.AddressObject, error) {
	return nil, webdav.NewHTTPError(http.StatusForbidden, nil)
}

func (b *backend) DeleteAddressObject(context.Context, string) error {
	return webdav.NewHTTPError(http.StatusForbidden, nil)
}
