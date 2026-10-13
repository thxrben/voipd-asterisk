package directory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"

	"voipd/internal/numbers"
)

// DAVPrefix is where the CardDAV server is mounted.
const DAVPrefix = "/dav"

// Address books of every directory account.
var books = []struct{ id, name, desc, kind string }{
	{"contacts", "Kontakte", "Kontakte aus voipd", "contact"},
	{"extensions", "Telefone", "Interne Telefone (Nebenstellen)", "extension"},
}

type ctxUser struct{}

// CardDAV serves the directory read-only over CardDAV (RFC 6352), behind
// HTTP Basic auth with directory accounts:
//
//	/dav/<user>/                         principal
//	/dav/<user>/books/                   address book home
//	/dav/<user>/books/contacts/          contacts
//	/dav/<user>/books/extensions/        internal phones
type CardDAV struct {
	dir *Directory
	acc *Accounts
	log *slog.Logger
	h   *carddav.Handler
	// ClientIP returns the client address (behind trusted proxies).
	ClientIP func(*http.Request) string
}

func NewCardDAV(dir *Directory, acc *Accounts, log *slog.Logger) *CardDAV {
	c := &CardDAV{dir: dir, acc: acc, log: log}
	c.h = &carddav.Handler{Backend: davBackend{c}, Prefix: DAVPrefix}
	c.ClientIP = func(r *http.Request) string {
		if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
			return h
		}
		return r.RemoteAddr
	}
	return c
}

func (c *CardDAV) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="voipd directory", charset="UTF-8"`)
		http.Error(w, "login with a directory account", http.StatusUnauthorized)
		return
	}
	if _, err := c.acc.Verify(r.Context(), user, pass, c.ClientIP(r)); err != nil {
		if errors.Is(err, ErrLocked) {
			http.Error(w, err.Error(), http.StatusTooManyRequests)
			return
		}
		c.log.Info("carddav: login failed", "client", c.ClientIP(r), "user", user)
		w.Header().Set("WWW-Authenticate", `Basic realm="voipd directory", charset="UTF-8"`)
		http.Error(w, "invalid login", http.StatusUnauthorized)
		return
	}
	// Each account sees only its own tree.
	if p := strings.TrimPrefix(r.URL.Path, DAVPrefix+"/"); r.URL.Path != "/.well-known/carddav" && p != "" && p != r.URL.Path {
		if first := strings.SplitN(p, "/", 2)[0]; first != user {
			http.Error(w, "not your directory", http.StatusForbidden)
			return
		}
	}
	if r.URL.Path == DAVPrefix || r.URL.Path == DAVPrefix+"/" {
		if r.Method == http.MethodGet {
			http.Redirect(w, r, principal(user), http.StatusFound)
			return
		}
	}
	c.h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxUser{}, user)))
}

func principal(user string) string { return DAVPrefix + "/" + user + "/" }
func home(user string) string      { return principal(user) + "books/" }

type davBackend struct{ c *CardDAV }

var errReadOnly = webdav.NewHTTPError(http.StatusForbidden, errors.New("the directory is read-only"))

func userOf(ctx context.Context) string { u, _ := ctx.Value(ctxUser{}).(string); return u }

func (b davBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return principal(userOf(ctx)), nil
}

func (b davBackend) AddressBookHomeSetPath(ctx context.Context) (string, error) {
	return home(userOf(ctx)), nil
}

func (b davBackend) ListAddressBooks(ctx context.Context) ([]carddav.AddressBook, error) {
	var out []carddav.AddressBook
	for _, bk := range books {
		out = append(out, carddav.AddressBook{Path: home(userOf(ctx)) + bk.id + "/", Name: bk.name,
			Description: bk.desc, MaxResourceSize: 64 << 10})
	}
	return out, nil
}

// book resolves an address book path ("…/books/<id>/") to its kind.
func (b davBackend) book(ctx context.Context, p string) (string, string, bool) {
	p = strings.TrimSuffix(path.Clean(p), "/") + "/"
	for _, bk := range books {
		if p == home(userOf(ctx))+bk.id+"/" {
			return bk.id, bk.kind, true
		}
	}
	return "", "", false
}

func (b davBackend) GetAddressBook(ctx context.Context, p string) (*carddav.AddressBook, error) {
	if _, _, ok := b.book(ctx, p); !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, errors.New("no such address book"))
	}
	all, _ := b.ListAddressBooks(ctx)
	for i := range all {
		if strings.TrimSuffix(all[i].Path, "/") == strings.TrimSuffix(path.Clean(p), "/") {
			return &all[i], nil
		}
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, errors.New("no such address book"))
}

func (b davBackend) CreateAddressBook(context.Context, *carddav.AddressBook) error {
	return errReadOnly
}
func (b davBackend) DeleteAddressBook(context.Context, string) error { return errReadOnly }
func (b davBackend) PutAddressObject(context.Context, string, vcard.Card, *carddav.PutAddressObjectOptions) (*carddav.AddressObject, error) {
	return nil, errReadOnly
}
func (b davBackend) DeleteAddressObject(context.Context, string) error { return errReadOnly }

func (b davBackend) objects(ctx context.Context, bookPath string) ([]carddav.AddressObject, error) {
	id, kind, ok := b.book(ctx, bookPath)
	if !ok {
		return nil, webdav.NewHTTPError(http.StatusNotFound, errors.New("no such address book"))
	}
	entries, err := b.c.dir.Entries(ctx)
	if err != nil {
		return nil, err
	}
	var out []carddav.AddressObject
	for _, e := range entries {
		if e.Kind != kind {
			continue
		}
		out = append(out, addressObject(home(userOf(ctx))+id+"/"+e.ID+".vcf", e))
	}
	return out, nil
}

func (b davBackend) ListAddressObjects(ctx context.Context, p string, _ *carddav.AddressDataRequest) ([]carddav.AddressObject, error) {
	return b.objects(ctx, p)
}

func (b davBackend) QueryAddressObjects(ctx context.Context, p string, q *carddav.AddressBookQuery) ([]carddav.AddressObject, error) {
	all, err := b.objects(ctx, p)
	if err != nil {
		return nil, err
	}
	if q == nil || len(q.PropFilters) == 0 { // no filter: everything (up to the limit)
		if q != nil && q.Limit > 0 && len(all) > q.Limit {
			all = all[:q.Limit]
		}
		return all, nil
	}
	var out []carddav.AddressObject
	for _, o := range all {
		if b.matchQuery(q, o.Card) {
			out = append(out, o)
			if q.Limit > 0 && len(out) >= q.Limit {
				break
			}
		}
	}
	return out, nil
}

// matchQuery evaluates an addressbook-query filter like go-webdav's
// carddav.Filter, but case-insensitively (the RFC's default collation),
// against every value of a property, and TEL by digits.
func (b davBackend) matchQuery(q *carddav.AddressBookQuery, card vcard.Card) bool {
	all := q.FilterTest == carddav.FilterAllOf
	for _, pf := range q.PropFilters {
		ok := b.matchProp(pf, card)
		if ok && !all {
			return true
		}
		if !ok && all {
			return false
		}
	}
	return all
}

func (b davBackend) matchProp(pf carddav.PropFilter, card vcard.Card) bool {
	fields := card[strings.ToUpper(pf.Name)]
	if len(fields) == 0 {
		return pf.IsNotDefined
	}
	if pf.IsNotDefined {
		return false
	}
	if len(pf.TextMatches) == 0 {
		return true
	}
	all := pf.Test == carddav.FilterAllOf
	for _, tm := range pf.TextMatches {
		ok := false
		tel := strings.EqualFold(pf.Name, vcard.FieldTelephone)
		for _, f := range fields {
			values := []string{f.Value}
			if tel && strings.HasPrefix(f.Value, "+") { // also as dialed: 0170…, 0044…
				if d, err := b.c.dir.z.Dial(b.c.dir.z.FromE164(f.Value), numbers.FormatNational); err == nil {
					values = append(values, d)
				}
			}
			for _, v := range values {
				if matchText(tm, v, tel) {
					ok = true
				}
			}
			if ok {
				break
			}
		}
		if ok && !all {
			return true
		}
		if !ok && all {
			return false
		}
	}
	return all
}

func matchText(tm carddav.TextMatch, value string, tel bool) bool {
	v, t := strings.ToLower(value), strings.ToLower(tm.Text)
	if tel && numeric(tm.Text) {
		v, t = digitsOnly(value), digitsOnly(tm.Text)
	}
	var ok bool
	switch tm.MatchType {
	case carddav.MatchEquals:
		ok = v == t
	case carddav.MatchStartsWith:
		ok = strings.HasPrefix(v, t)
	case carddav.MatchEndsWith:
		ok = strings.HasSuffix(v, t)
	default:
		ok = strings.Contains(v, t)
	}
	return ok != tm.NegateCondition
}

func (b davBackend) GetAddressObject(ctx context.Context, p string, _ *carddav.AddressDataRequest) (*carddav.AddressObject, error) {
	all, err := b.objects(ctx, path.Dir(p))
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Path == path.Clean(p) {
			return &all[i], nil
		}
	}
	return nil, webdav.NewHTTPError(http.StatusNotFound, errors.New("no such contact"))
}

var telTypes = map[string]string{TypeWork: "WORK,VOICE", TypeHome: "HOME,VOICE", TypeCell: "CELL", TypeOther: "VOICE"}

// Card renders an entry as a vCard 3.0.
func Card(e Entry) vcard.Card {
	c := vcard.Card{}
	c.SetValue(vcard.FieldVersion, "3.0")
	c.SetValue(vcard.FieldUID, "voipd-"+e.ID)
	c.SetValue(vcard.FieldFormattedName, e.Name)
	c.SetName(&vcard.Name{FamilyName: e.Family, GivenName: e.Given})
	if e.Org != "" {
		c.SetValue(vcard.FieldOrganization, e.Org)
	}
	for _, n := range e.Numbers {
		tel := n.Dial
		if n.E164 != "" {
			tel = n.E164 // softphones format it themselves
		}
		c.Add(vcard.FieldTelephone, &vcard.Field{Value: tel, Params: vcard.Params{vcard.ParamType: strings.Split(telTypes[n.Type], ",")}})
	}
	return c
}

func addressObject(p string, e Entry) carddav.AddressObject {
	card := Card(e)
	var buf bytes.Buffer
	vcard.NewEncoder(&buf).Encode(card)
	sum := sha256.Sum256(buf.Bytes())
	return carddav.AddressObject{Path: p, Card: card, ContentLength: int64(buf.Len()),
		ETag: hex.EncodeToString(sum[:8]), ModTime: time.Time{}}
}
