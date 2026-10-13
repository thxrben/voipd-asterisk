package directory

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"testing"

	"github.com/go-ldap/ldap/v3"

	"voipd/internal/numbers"
	"voipd/internal/store"
)

type env struct {
	t    *testing.T
	st   *store.Store
	dir  *Directory
	acc  *Accounts
	addr string
	pw   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.DB.Exec(`INSERT INTO extensions(number, name, sip_username, sip_password) VALUES
		('10', 'Küche', 'kitchen', 'x'), ('11', 'Büro Anna', 'office', 'x')`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []store.Card{
		{Name: "Anna Weber", Org: "Zahnarzt", Numbers: []store.CardNumber{{Pattern: "+492011234567", Label: "work"},
			{Pattern: "+491701234567", Label: "cell"}}},
		{Name: "Oma", Numbers: []store.CardNumber{{Pattern: "+4923145678", Label: "home"}}},
		{Name: "John Smith", Numbers: []store.CardNumber{{Pattern: "+441314960001", Label: ""}}},
		{Name: "Nobody", Numbers: nil}, // no number: not in the directory
	} {
		c.SourceID = store.ManualContactsSource
		if _, err := st.SaveCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	z, _ := numbers.New("DE", "201")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &env{t: t, st: st, dir: New(st, z, log), acc: NewAccounts(st)}
	_, e.pw, err = e.acc.Create(ctx, "phones")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = ln.Addr().String()
	srv := NewLDAP(e.dir, e.acc, z, log)
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); srv.Serve(sctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return e
}

func (e *env) dial() *ldap.Conn {
	e.t.Helper()
	c, err := ldap.DialURL("ldap://" + e.addr)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { c.Close() })
	return c
}

func (e *env) login() *ldap.Conn {
	c := e.dial()
	if err := c.Bind("uid=phones,dc=voipd", e.pw); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func names(t *testing.T, c *ldap.Conn, base, filter string, attrs ...string) []string {
	t.Helper()
	res, err := c.Search(ldap.NewSearchRequest(base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		filter, attrs, nil))
	if err != nil {
		t.Fatalf("search %s: %v", filter, err)
	}
	var out []string
	for _, en := range res.Entries {
		out = append(out, en.GetAttributeValue("cn"))
	}
	sort.Strings(out)
	return out
}

func TestLDAPClientFilters(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	for _, tc := range []struct{ name, filter, want string }{
		{"yealink name", "(|(cn=anna*)(sn=anna*)(telephoneNumber=anna*)(mobile=anna*))", "Anna Weber,Büro Anna"},
		{"yealink family name", "(|(cn=weber*)(sn=weber*))", "Anna Weber"},
		{"snom substring", "(|(cn=*nna*)(telephoneNumber=*nna*))", "Anna Weber,Büro Anna"},
		{"grandstream", "(|(telephoneNumber=Oma*)(cn=Oma*))", "Oma"},
		{"gigaset", "(&(objectClass=person)(|(cn=J*)(sn=J*)))", "John Smith"},
		{"case-insensitive umlaut", "(cn=küche)", "Küche"},
		{"extension number", "(telephoneNumber=10)", "Küche"},
		{"all people", "(objectClass=inetOrgPerson)", "Anna Weber,Büro Anna,John Smith,Küche,Oma"},
		{"present", "(mobile=*)", "Anna Weber"},
		{"not", "(&(objectClass=person)(!(ou=extensions)))", "Anna Weber,John Smith,Oma"},
		// Reverse lookup of an incoming caller as the phone shows it.
		{"national", "(|(telephoneNumber=01701234567)(mobile=01701234567)(homePhone=01701234567))", "Anna Weber"},
		{"international", "(mobile=+491701234567)", "Anna Weber"},
		{"0049", "(mobile=00491701234567)", "Anna Weber"},
		{"local without area code", "(telephoneNumber=1234567)", "Anna Weber"},
		{"foreign", "(telephoneNumber=00441314960001)", "John Smith"},
		{"digits substring", "(telephoneNumber=*0201123*)", "Anna Weber"},
		{"e164 substring", "(mobile=+49170*)", "Anna Weber"},
		{"no match", "(cn=zzz*)", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := strings.Join(names(t, c, "dc=voipd", tc.filter), ","); got != tc.want {
				t.Errorf("%s = %q, want %q", tc.filter, got, tc.want)
			}
		})
	}
}

func TestLDAPEntries(t *testing.T) {
	e := newEnv(t)
	c := e.login()
	res, err := c.Search(ldap.NewSearchRequest("ou=contacts,dc=voipd", ldap.ScopeSingleLevel, ldap.NeverDerefAliases, 0, 0, false,
		"(cn=Anna Weber)", nil, nil))
	if err != nil || len(res.Entries) != 1 {
		t.Fatalf("search: %v %v", err, res)
	}
	en := res.Entries[0]
	for attr, want := range map[string]string{"cn": "Anna Weber", "sn": "Weber", "givenName": "Anna", "o": "Zahnarzt",
		"telephoneNumber": "02011234567", "mobile": "01701234567", "displayName": "Anna Weber"} {
		if got := en.GetAttributeValue(attr); got != want {
			t.Errorf("%s = %q, want %q", attr, got, want)
		}
	}
	if !strings.HasPrefix(en.DN, "uid=c") || !strings.HasSuffix(en.DN, ",ou=contacts,dc=voipd") {
		t.Errorf("dn = %s", en.DN)
	}
	// Foreign numbers are dialable from a German line.
	res, _ = c.Search(ldap.NewSearchRequest("dc=voipd", ldap.ScopeWholeSubtree, 0, 0, 0, false, "(cn=John Smith)", nil, nil))
	if got := res.Entries[0].GetAttributeValue("telephoneNumber"); got != "00441314960001" {
		t.Errorf("foreign number = %q", got)
	}
	// Attribute selection and types only.
	res, _ = c.Search(ldap.NewSearchRequest("dc=voipd", ldap.ScopeWholeSubtree, 0, 0, 0, false, "(cn=Oma)",
		[]string{"cn", "homePhone"}, nil))
	if a := res.Entries[0].Attributes; len(a) != 2 || res.Entries[0].GetAttributeValue("homePhone") != "023145678" {
		t.Errorf("selected attributes = %+v", a)
	}
	res, _ = c.Search(ldap.NewSearchRequest("dc=voipd", ldap.ScopeWholeSubtree, 0, 0, 0, true, "(cn=Oma)", nil, nil))
	if v := res.Entries[0].GetAttributeValues("cn"); len(v) != 0 {
		t.Errorf("types only returned values %v", v)
	}
	// Scopes: one level below the base has the two containers.
	res, _ = c.Search(ldap.NewSearchRequest("dc=voipd", ldap.ScopeSingleLevel, 0, 0, 0, false, "(objectClass=*)", nil, nil))
	if len(res.Entries) != 2 {
		t.Errorf("one level = %d entries", len(res.Entries))
	}
	res, _ = c.Search(ldap.NewSearchRequest("ou=extensions,dc=voipd", ldap.ScopeWholeSubtree, 0, 0, 0, false, "(objectClass=person)", nil, nil))
	if len(res.Entries) != 2 {
		t.Errorf("extensions = %d", len(res.Entries))
	}
	// Size limit.
	_, err = c.Search(ldap.NewSearchRequest("dc=voipd", ldap.ScopeWholeSubtree, 0, 2, 0, false, "(objectClass=person)", nil, nil))
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		t.Errorf("size limit: %v", err)
	}
	// Unknown base.
	_, err = c.Search(ldap.NewSearchRequest("dc=example,dc=com", ldap.ScopeWholeSubtree, 0, 0, 0, false, "(cn=*)", nil, nil))
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
		t.Errorf("unknown base: %v", err)
	}
	// Read-only.
	err = c.Del(ldap.NewDelRequest(en.DN, nil))
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultUnwillingToPerform) {
		t.Errorf("delete: %v", err)
	}
}

func TestLDAPAuthentication(t *testing.T) {
	e := newEnv(t)
	// Root DSE without login.
	c := e.dial()
	res, err := c.Search(ldap.NewSearchRequest("", ldap.ScopeBaseObject, 0, 0, 0, false, "(objectClass=*)", []string{"namingContexts"}, nil))
	if err != nil || res.Entries[0].GetAttributeValue("namingContexts") != "dc=voipd" {
		t.Fatalf("root dse: %v %+v", err, res)
	}
	// No anonymous searches.
	_, err = c.Search(ldap.NewSearchRequest("dc=voipd", ldap.ScopeWholeSubtree, 0, 0, 0, false, "(cn=*)", nil, nil))
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultInsufficientAccessRights) {
		t.Errorf("anonymous search: %v", err)
	}
	// Plain user names and cn= work too.
	for _, user := range []string{"phones", "cn=phones,ou=accounts,dc=voipd"} {
		if err := e.dial().Bind(user, e.pw); err != nil {
			t.Errorf("bind as %s: %v", user, err)
		}
	}
	// Wrong passwords, then lockout.
	for i := 0; i < maxFailures; i++ {
		if err := e.dial().Bind("phones", "wrong"); !ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			t.Fatalf("wrong password %d: %v", i, err)
		}
	}
	err = e.dial().Bind("phones", e.pw)
	if !ldap.IsErrorWithCode(err, ldap.LDAPResultUnwillingToPerform) {
		t.Errorf("locked out: %v", err)
	}
	// A regenerated password replaces the old one, also in the cache.
	e.acc.mu.Lock()
	e.acc.failures = map[string]*failure{}
	e.acc.mu.Unlock()
	acc, _ := e.st.DirectoryAccountByName(context.Background(), "phones")
	pw2, _ := e.acc.Regenerate(context.Background(), acc.ID)
	if err := e.dial().Bind("phones", e.pw); err == nil {
		t.Error("old password still works")
	}
	if err := e.dial().Bind("phones", pw2); err != nil {
		t.Errorf("new password: %v", err)
	}
	var verr interface{ Error() string }
	if _, _, err := e.acc.Create(context.Background(), "Bad Name"); !errors.As(err, &verr) {
		t.Error("invalid username accepted")
	}
}
