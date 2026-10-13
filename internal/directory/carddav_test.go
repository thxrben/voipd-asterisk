package directory

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/emersion/go-vcard"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/carddav"
)

func (e *env) davServer() *httptest.Server {
	mux := http.NewServeMux()
	dav := NewCardDAV(e.dir, e.acc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	mux.Handle(DAVPrefix+"/", dav)
	mux.Handle("/.well-known/carddav", dav)
	srv := httptest.NewServer(mux)
	e.t.Cleanup(srv.Close)
	return srv
}

func TestCardDAV(t *testing.T) {
	e := newEnv(t)
	srv := e.davServer()
	ctx := context.Background()
	hc := webdav.HTTPClientWithBasicAuth(http.DefaultClient, "phones", e.pw)

	// Discovery from the URL the user enters.
	c, err := carddav.NewClient(hc, srv.URL+"/dav/")
	if err != nil {
		t.Fatal(err)
	}
	p, err := c.FindCurrentUserPrincipal(ctx)
	if err != nil || p != "/dav/phones/" {
		t.Fatalf("principal = %q %v", p, err)
	}
	home, err := c.FindAddressBookHomeSet(ctx, p)
	if err != nil || home != "/dav/phones/books/" {
		t.Fatalf("home = %q %v", home, err)
	}
	books, err := c.FindAddressBooks(ctx, home)
	if err != nil || len(books) != 2 || books[0].Name != "Kontakte" || books[1].Name != "Telefone" {
		t.Fatalf("books = %+v %v", books, err)
	}

	// All contacts with numbers.
	objs, err := c.QueryAddressBook(ctx, books[0].Path, &carddav.AddressBookQuery{
		DataRequest: carddav.AddressDataRequest{AllProp: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 3 {
		t.Fatalf("contacts = %d", len(objs))
	}
	var anna vcard.Card
	for _, o := range objs {
		if o.Card.PreferredValue(vcard.FieldFormattedName) == "Anna Weber" {
			anna = o.Card
		}
	}
	if anna == nil || anna.Name().FamilyName != "Weber" || anna.Value(vcard.FieldOrganization) != "Zahnarzt" {
		t.Fatalf("anna = %v", anna)
	}
	tels := anna[vcard.FieldTelephone]
	if len(tels) != 2 || tels[0].Value != "+492011234567" || !strings.Contains(strings.ToUpper(strings.Join(tels[1].Params.Types(), ",")), "CELL") {
		for _, x := range tels {
			t.Logf("tel %q params %v", x.Value, x.Params)
		}
		t.Errorf("tels wrong")
	}

	// Query by name (address book query).
	objs, err = c.QueryAddressBook(ctx, books[0].Path, &carddav.AddressBookQuery{
		DataRequest: carddav.AddressDataRequest{AllProp: true},
		PropFilters: []carddav.PropFilter{{Name: vcard.FieldFormattedName,
			TextMatches: []carddav.TextMatch{{Text: "oma", MatchType: carddav.MatchContains}}}}})
	if err != nil || len(objs) != 1 {
		t.Fatalf("query = %d %v", len(objs), err)
	}
	// Reverse lookup by the number as the phone shows it.
	byNum, err := c.QueryAddressBook(ctx, books[0].Path, &carddav.AddressBookQuery{
		DataRequest: carddav.AddressDataRequest{AllProp: true},
		PropFilters: []carddav.PropFilter{{Name: vcard.FieldTelephone,
			TextMatches: []carddav.TextMatch{{Text: "0170 1234567", MatchType: carddav.MatchEquals}}}}})
	if err != nil || len(byNum) != 1 || byNum[0].Card.PreferredValue(vcard.FieldFormattedName) != "Anna Weber" {
		t.Errorf("by number = %v %v", byNum, err)
	}
	// Multiget and single GET.
	got, err := c.MultiGetAddressBook(ctx, books[0].Path, &carddav.AddressBookMultiGet{Paths: []string{objs[0].Path},
		DataRequest: carddav.AddressDataRequest{AllProp: true}})
	if err != nil || len(got) != 1 {
		t.Fatalf("multiget = %v %v", got, err)
	}
	one, err := c.GetAddressObject(ctx, objs[0].Path)
	if err != nil || one.Card.PreferredValue(vcard.FieldFormattedName) != "Oma" {
		t.Fatalf("get = %v %v", one, err)
	}
	// Internal phones.
	ext, err := c.QueryAddressBook(ctx, books[1].Path, &carddav.AddressBookQuery{DataRequest: carddav.AddressDataRequest{AllProp: true}})
	if err != nil || len(ext) != 2 || ext[0].Card.Value(vcard.FieldTelephone) != "10" {
		t.Errorf("extensions = %v %v", ext, err)
	}
	// Read-only.
	if _, err := c.PutAddressObject(ctx, books[0].Path+"x.vcf", anna); err == nil {
		t.Error("PUT accepted")
	}
}

func TestCardDAVAuth(t *testing.T) {
	e := newEnv(t)
	srv := e.davServer()
	req := func(user, pass, path string) int {
		r, _ := http.NewRequest("PROPFIND", srv.URL+path, nil)
		r.Header.Set("Depth", "0")
		if user != "" {
			r.SetBasicAuth(user, pass)
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := req("", "", "/dav/phones/"); code != 401 {
		t.Errorf("no login = %d", code)
	}
	if code := req("phones", "wrong", "/dav/phones/"); code != 401 {
		t.Errorf("wrong password = %d", code)
	}
	e.acc.Create(context.Background(), "other")
	if code := req("phones", e.pw, "/dav/other/books/"); code != 403 {
		t.Errorf("other account's tree = %d", code)
	}
	if code := req("phones", e.pw, "/dav/phones/books/contacts/"); code != 207 {
		t.Errorf("own book = %d", code)
	}
	// Well-known discovery redirects to the principal.
	r, _ := http.NewRequest("GET", srv.URL+"/.well-known/carddav", nil)
	r.SetBasicAuth("phones", e.pw)
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode/100 != 3 || loc != "/dav/phones/" {
		t.Errorf("well-known = %d %q", resp.StatusCode, loc)
	}
}
