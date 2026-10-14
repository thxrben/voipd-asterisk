package backup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"voipd/internal/events"
	"voipd/internal/store"
)

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func exec(t *testing.T, st *store.Store, q string, args ...any) {
	t.Helper()
	if _, err := st.DB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

// seed fills a database with one of everything.
func seed(t *testing.T, st *store.Store) {
	exec(t, st, `INSERT INTO settings(key, value) VALUES('anonymous_block','true'), ('backup.passphrase','correct horse battery'), ('backup.last_at','123')`)
	exec(t, st, `INSERT INTO trunks(id, name, profile, registrar, auth_username, auth_password, srtp, transport)
		VALUES(1, 'telekom', 'telekom', 'tel.t-online.de', 'user@t-online.de', 'trunkpw', 1, 'tls')`)
	exec(t, st, `INSERT INTO trunk_numbers(id, trunk_id, number, label) VALUES(1, 1, '+492011234567', 'Haupt'), (2, 1, '+492017654321', '')`)
	exec(t, st, `INSERT INTO extensions(id, number, name, sip_username, sip_password, outbound_number_id, voicemail_pin, owner_subject)
		VALUES(1, '10', 'Küche', 'kueche', 'sip10pw', 2, '4711', 'oidc:alice'), (2, '11', 'Büro', 'buero', 'sip11pw', NULL, '', '')`)
	exec(t, st, `INSERT INTO ring_groups(id, name, ring_timeout, fallback, fallback_extension_id) VALUES(1, 'Alle', 25, 'voicemail', 1)`)
	exec(t, st, `INSERT INTO ring_group_members(ring_group_id, extension_id) VALUES(1, 1), (1, 2)`)
	exec(t, st, `INSERT INTO did_routes(trunk_number_id, ring_group_id) VALUES(1, 1)`)
	exec(t, st, `INSERT INTO list_sources(id, name, kind, target, url, username, password)
		VALUES(10, 'Familie', 'carddav', 'contacts', 'https://dav.example/', 'me', 'davpw'),
		      (11, 'PhoneBlock', 'phoneblock', 'block', '', '', 'pbkey')`)
	exec(t, st, `INSERT INTO list_entries(source_id, pattern, label) VALUES(1, '+4930999', 'Werbung'), (11, '+4940111', 'synced')`)
	exec(t, st, `INSERT INTO cards(id, source_id, href, name, org) VALUES(1, 3, 'manual-1', 'Oma', ''), (2, 10, 'remote.vcf', 'Synced', '')`)
	exec(t, st, `INSERT INTO list_entries(source_id, card_id, pattern, label) VALUES(3, 1, '+492015555', 'home'), (10, 2, '+492016666', 'cell')`)
	exec(t, st, `INSERT INTO module_settings(module, enabled, config) VALUES('mqtt', 1, '{"broker":"tcp://ha:1883","password":"mqttpw","topic":"voipd"}')`)
	exec(t, st, `INSERT INTO webhook_endpoints(name, url, secret, events) VALUES('ha', 'https://ha/hook', 'whsecret', 'call.')`)
	exec(t, st, `INSERT INTO directory_accounts(username, password_hash, created_at) VALUES('yealink', '$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA', 1)`)
	exec(t, st, `INSERT INTO calls(id, direction, status, caller, callee, started_at) VALUES('c1', 'inbound', 'answered', '+4930', '+492011234567', 1)`)
	exec(t, st, `INSERT INTO voicemails(id, extension_id, received_at) VALUES('vm1', 1, 1)`)
}

func yamlOf(t *testing.T, st *store.Store, secrets bool) []byte {
	t.Helper()
	d, err := Export(context.Background(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Marshal(secrets)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withoutHeader drops the lines that differ between two exports.
func withoutHeader(b []byte) string {
	var keep []string
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(l, "exported_at:") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

func importDoc(t *testing.T, st *store.Store, data []byte, pass string) Diff {
	t.Helper()
	ctx := context.Background()
	d, err := ReadDoc(data, pass)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(ctx, st.DB, d)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Apply(ctx, st.DB, d, plan.Token)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestExportShape(t *testing.T) {
	st := openStore(t, ":memory:")
	seed(t, st)
	out := string(yamlOf(t, st, false))
	for _, want := range []string{
		"trunks:\n  - name: telekom", "trunk: telekom", "outbound_number: \"+492017654321\"",
		"fallback_extension: \"10\"", "ring_group: Alle", "pattern: \"+4930999\"", "card: manual-1",
		"broker: tcp://ha:1883", "username: yealink", "anonymous_block",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{"trunkpw", "sip10pw", "4711", "davpw", "pbkey", "mqttpw", "whsecret", "argon2id",
		"correct horse", "backup.last_at", "+4940111", "remote.vcf", "+492016666"} {
		if strings.Contains(out, leak) {
			t.Errorf("plain export contains %q:\n%s", leak, out)
		}
	}
	withSecrets := string(yamlOf(t, st, true))
	for _, want := range []string{"trunks/telekom/auth_password: trunkpw", "extensions/10/sip_password: sip10pw",
		"modules/mqtt/config.password: mqttpw", "settings/backup.passphrase/value: correct horse battery"} {
		if !strings.Contains(withSecrets, want) {
			t.Errorf("secrets lack %q:\n%s", want, withSecrets)
		}
	}
}

func TestRoundTripIntoEmptyDatabase(t *testing.T) {
	src := openStore(t, ":memory:")
	seed(t, src)
	full := yamlOf(t, src, true)

	dst := openStore(t, ":memory:")
	res := importDoc(t, dst, full, "")
	if len(res.Warnings) != 0 {
		t.Errorf("warnings: %v", res.Warnings)
	}
	if got, want := withoutHeader(yamlOf(t, dst, true)), withoutHeader(full); got != want {
		t.Fatalf("round trip differs:\n--- got\n%s\n--- want\n%s", got, want)
	}
	// Re-importing changes nothing.
	d, _ := ReadDoc(full, "")
	plan, err := Plan(context.Background(), dst.DB, d)
	if err != nil || !plan.Empty() {
		t.Fatalf("second plan not empty: %+v %v", plan, err)
	}
	// Synced data and runtime settings are not imported.
	var n int
	dst.DB.QueryRow(`SELECT count(*) FROM settings WHERE key = 'backup.last_at'`).Scan(&n)
	if n != 0 {
		t.Error("runtime setting imported")
	}
}

func TestPlainImportKeepsSecretsAndHistory(t *testing.T) {
	st := openStore(t, ":memory:")
	seed(t, st)
	plain := yamlOf(t, st, false)
	// Edit: rename an extension, drop the second one, add a third.
	edited := strings.Replace(string(plain), "name: Küche", "name: Kochen", 1)
	edited = strings.Replace(edited, "  - number: \"11\"\n    name: Büro", "  - number: \"12\"\n    name: Flur", 1)
	edited = strings.Replace(edited, "sip_username: buero", "sip_username: flur", 1)
	edited = strings.Replace(edited, "extension: \"11\"", "extension: \"12\"", 1)

	d, err := ReadDoc([]byte(edited), "")
	if err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(context.Background(), st.DB, d)
	if err != nil {
		t.Fatal(err)
	}
	var ext SectionDiff
	for _, s := range plan.Sections {
		if s.Section == "extensions" {
			ext = s
		}
	}
	if len(ext.Added) != 1 || ext.Added[0] != "12" || len(ext.Removed) != 1 || ext.Removed[0] != "11" ||
		len(ext.Changed) != 1 || ext.Changed[0].Fields[0] != "name" {
		t.Fatalf("extensions diff: %+v", ext)
	}
	if len(plan.Warnings) != 1 || !strings.Contains(plan.Warnings[0], "Extension 12 gets a new SIP password") {
		t.Fatalf("warnings: %v", plan.Warnings)
	}
	if _, err := Apply(context.Background(), st.DB, d, plan.Token); err != nil {
		t.Fatal(err)
	}
	var pw, name, pin string
	st.DB.QueryRow(`SELECT sip_password, name, voicemail_pin FROM extensions WHERE number = '10'`).Scan(&pw, &name, &pin)
	if pw != "sip10pw" || name != "Kochen" || pin != "4711" {
		t.Errorf("extension 10: %q %q %q", pw, name, pin)
	}
	st.DB.QueryRow(`SELECT sip_password FROM extensions WHERE number = '12'`).Scan(&pw)
	if len(pw) != 20 {
		t.Errorf("new extension password %q", pw)
	}
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM voicemails WHERE extension_id = 1`).Scan(&n)
	if n != 1 {
		t.Error("voicemail of the kept extension lost")
	}
	st.DB.QueryRow(`SELECT count(*) FROM calls`).Scan(&n)
	if n != 1 {
		t.Error("history touched")
	}
	var cfg string
	st.DB.QueryRow(`SELECT config FROM module_settings WHERE module = 'mqtt'`).Scan(&cfg)
	if !strings.Contains(cfg, `"password":"mqttpw"`) {
		t.Errorf("module secret lost: %s", cfg)
	}
	// Synced lists keep their entries; the trunk keeps its password.
	st.DB.QueryRow(`SELECT count(*) FROM list_entries WHERE source_id IN (10, 11)`).Scan(&n)
	if n != 2 {
		t.Errorf("synced entries: %d", n)
	}
	st.DB.QueryRow(`SELECT auth_password FROM trunks`).Scan(&pw)
	if pw != "trunkpw" {
		t.Errorf("trunk password %q", pw)
	}
	// The members follow the new extension.
	st.DB.QueryRow(`SELECT count(*) FROM ring_group_members m JOIN extensions e ON e.id = m.extension_id WHERE e.number = '12'`).Scan(&n)
	if n != 1 {
		t.Error("ring group member not re-linked")
	}
}

func TestApplyStaleToken(t *testing.T) {
	st := openStore(t, ":memory:")
	seed(t, st)
	d, _ := ReadDoc(yamlOf(t, st, false), "")
	plan, _ := Plan(context.Background(), st.DB, d)
	exec(t, st, `UPDATE extensions SET name = 'changed' WHERE number = '10'`)
	if _, err := Apply(context.Background(), st.DB, d, plan.Token); !errors.Is(err, ErrStale) {
		t.Fatalf("want ErrStale, got %v", err)
	}
}

func TestPartialDocumentLeavesOtherSections(t *testing.T) {
	st := openStore(t, ":memory:")
	seed(t, st)
	res := importDoc(t, st, []byte("version: 1\nextensions:\n  - number: \"10\"\n    name: Nur\n"), "")
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM trunks`).Scan(&n)
	if n != 1 {
		t.Error("absent section was cleared")
	}
	st.DB.QueryRow(`SELECT count(*) FROM extensions`).Scan(&n)
	if n != 1 || len(res.Sections) != 1 {
		t.Errorf("extensions %d, diff %+v", n, res.Sections)
	}
}

func TestParseErrors(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"version: 2\n", "version 2"},
		{"version: 1\nfoo: []\n", `unknown section "foo"`},
		{"version: 1\ntrunks:\n  - name: x\n    colour: red\n", `unknown field "colour"`},
		{"version: 1\ntrunks:\n  - name: x\n    auth_password: y\n", "secrets section"},
		{"version: 1\ntrunks:\n  - name: x\n    enabled: yes please\n", "true or false"},
		{"version: 1\ntrunks:\n  - name: x\n  - name: x\n", "appears twice"},
		{"version: 1\nextensions:\n  - name: x\n", `"number" is missing`},
		{"just text", "not a voipd configuration"},
	} {
		if _, err := Parse([]byte(tc.in)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: got %v, want %q", tc.in, err, tc.want)
		}
	}
	// A dangling reference fails the import as a whole.
	st := openStore(t, ":memory:")
	seed(t, st)
	d, err := Parse([]byte("version: 1\nnumbers:\n  - number: \"+49\"\n    trunk: nope\n"))
	if err != nil {
		t.Fatal(err)
	}
	plan, _ := Plan(context.Background(), st.DB, d)
	if _, err := Apply(context.Background(), st.DB, d, plan.Token); err == nil || !strings.Contains(err.Error(), `trunk "nope" does not exist`) {
		t.Fatalf("got %v", err)
	}
	var n int
	st.DB.QueryRow(`SELECT count(*) FROM trunk_numbers`).Scan(&n)
	if n != 2 {
		t.Error("failed import was not rolled back")
	}
}

func TestEncryptedDocument(t *testing.T) {
	st := openStore(t, ":memory:")
	seed(t, st)
	var buf bytes.Buffer
	w, err := Encrypt(&buf, "a long passphrase")
	if err != nil {
		t.Fatal(err)
	}
	w.Write(yamlOf(t, st, true))
	w.Close()
	if !IsEncrypted(buf.Bytes()) {
		t.Fatal("no age header")
	}
	if _, err := ReadDoc(buf.Bytes(), "wrong one"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	d, err := ReadDoc(buf.Bytes(), "a long passphrase")
	if err != nil || d.Secrets["trunks/telekom/auth_password"] != "trunkpw" {
		t.Fatalf("decrypt: %v %v", err, d)
	}
}

func TestFullBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "data", "voipd.db")
	vmDir := filepath.Join(dir, "vm")
	os.MkdirAll(filepath.Join(vmDir, "vm", "10"), 0o2775)
	os.WriteFile(filepath.Join(vmDir, "vm", "10", "vm1.wav"), []byte("RIFF-message"), 0o640)
	os.MkdirAll(filepath.Join(vmDir, "greet"), 0o2775)
	os.WriteFile(filepath.Join(vmDir, "greet", "10.wav"), []byte("RIFF-greeting"), 0o640)
	os.WriteFile(filepath.Join(vmDir, "greet", "10.upload"), []byte("partial"), 0o640)

	st, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, st)
	var archive bytes.Buffer
	m, err := Create(ctx, st.DB, vmDir, filepath.Dir(dbPath), "backup passphrase", &archive)
	if err != nil {
		t.Fatal(err)
	}
	if m.Calls != 1 || m.Voicemails != 1 || m.Files != 2 || m.Schema != store.LatestSchema() {
		t.Fatalf("manifest %+v", m)
	}

	// Things change after the backup.
	exec(t, st, `DELETE FROM extensions WHERE number = '11'`)
	st.Close()
	os.WriteFile(filepath.Join(vmDir, "vm", "10", "later.wav"), []byte("new"), 0o640)

	if _, err := Stage(ctx, bytes.NewReader(archive.Bytes()), "nope", filepath.Dir(dbPath), vmDir, store.LatestSchema()); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
	if _, err := Stage(ctx, bytes.NewReader(archive.Bytes()), "backup passphrase", filepath.Dir(dbPath), vmDir, m.Schema-1); err == nil {
		t.Fatal("newer schema accepted")
	}
	if Pending(filepath.Dir(dbPath)) {
		t.Fatal("failed stage left a pending restore")
	}
	if _, err := Stage(ctx, bytes.NewReader(archive.Bytes()), "backup passphrase", filepath.Dir(dbPath), vmDir, store.LatestSchema()); err != nil {
		t.Fatal(err)
	}
	ok, err := ApplyPending(dbPath, vmDir)
	if err != nil || !ok {
		t.Fatalf("apply pending: %v %v", ok, err)
	}
	st2 := openStore(t, dbPath)
	var n int
	st2.DB.QueryRow(`SELECT count(*) FROM extensions`).Scan(&n)
	if n != 2 {
		t.Errorf("restored extensions: %d", n)
	}
	if b, _ := os.ReadFile(filepath.Join(vmDir, "vm", "10", "vm1.wav")); string(b) != "RIFF-message" {
		t.Errorf("message: %q", b)
	}
	if _, err := os.Stat(filepath.Join(vmDir, "vm", "10", "later.wav")); err == nil {
		t.Error("file newer than the backup still there")
	}
	if _, err := os.Stat(filepath.Join(vmDir, "greet", "10.upload")); err == nil {
		t.Error("temp upload was backed up")
	}
	if _, err := os.Stat(filepath.Join(vmDir, ".pre-restore", "vm", "10", "later.wav")); err != nil {
		t.Error("replaced voicemail not kept")
	}
	if _, err := os.Stat(filepath.Join(dir, "data", "pre-restore", "voipd.db")); err != nil {
		t.Error("replaced database not kept")
	}
	if ok, _ := ApplyPending(dbPath, vmDir); ok {
		t.Error("applied twice")
	}
}

func TestServiceScheduleAndPrune(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := openStore(t, filepath.Join(dir, "voipd.db"))
	bus := events.NewBus(slog.New(slog.NewTextHandler(io.Discard, nil)))
	evs, unsub := bus.Subscribe("backup.", 8)
	defer unsub()
	s := NewService(st, bus, filepath.Join(dir, "backups"), dir, filepath.Join(dir, "vm"), slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := s.SaveSettings(ctx, Settings{Enabled: true, Time: "03:30", Keep: 2}, nil); err == nil {
		t.Fatal("enabled without passphrase")
	}
	short := "short"
	if _, err := s.SaveSettings(ctx, Settings{Time: "03:30", Keep: 2}, &short); err == nil {
		t.Fatal("short passphrase accepted")
	}
	if _, err := s.RunNow(ctx); !errors.Is(err, ErrNoPassphrase) {
		t.Fatalf("run without passphrase: %v", err)
	}
	if ev := <-evs; ev.Type != EventFailed {
		t.Fatalf("event %v", ev)
	}
	pass := "a good long passphrase"
	set, err := s.SaveSettings(ctx, Settings{Enabled: true, Time: "03:30", Keep: 2}, &pass)
	if err != nil || !set.PassphraseSet || !set.Enabled {
		t.Fatalf("save: %+v %v", set, err)
	}
	clock := time.Date(2026, 10, 14, 3, 30, 0, 0, time.Local)
	for i := range 3 {
		s.now = func() time.Time { return clock.AddDate(0, 0, i) }
		if _, err := s.RunNow(ctx); err != nil {
			t.Fatal(err)
		}
		if ev := <-evs; ev.Type != EventOK {
			t.Fatalf("event %v", ev)
		}
	}
	files, _ := s.Files()
	if len(files) != 2 || files[0].Name != "voipd-backup-20261016-033000.tar.gz.age" {
		t.Fatalf("files %+v", files)
	}
	if _, err := s.Path("../voipd.db"); err == nil {
		t.Error("path traversal")
	}
	if st := s.Status(ctx); st.LastFile != files[0].Name || st.LastError != "" {
		t.Errorf("status %+v", st)
	}
	if n := nextRun(time.Date(2026, 10, 14, 3, 30, 0, 0, time.Local), "03:30"); n.Day() != 15 {
		t.Errorf("next run %v", n)
	}
	if n := nextRun(time.Date(2026, 10, 14, 1, 0, 0, 0, time.Local), "03:30"); n.Day() != 14 || n.Hour() != 3 {
		t.Errorf("next run %v", n)
	}
}
