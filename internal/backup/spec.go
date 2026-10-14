// Package backup exports and imports voipd's configuration as a versioned
// YAML document, writes encrypted full backups (database snapshot and
// voicemail) and restores them.
package backup

import "strings"

type kind int

const (
	kStr kind = iota
	kInt
	kBool
	kJSON // a JSON object column, shown as a YAML mapping
	kRef  // a foreign key, shown as the referenced row's natural key
)

// column describes one exported column.
type column struct {
	col  string // database column
	name string // YAML key; defaults to col
	kind kind
	ref  *ref

	// secret columns go to the document's secrets map, never inline.
	secret bool
	// secretWhen makes the column secret only for some rows (settings).
	secretWhen func(Row) bool
	// jsonSecrets are the keys of a kJSON column that are secrets.
	jsonSecrets []string
	// generate fills a secret for a new row when the file has none.
	generate func() (string, string) // value, warning
}

func (c column) key() string {
	if c.name != "" {
		return c.name
	}
	return c.col
}

func (c column) isSecret(r Row) bool {
	return c.secret || (c.secretWhen != nil && c.secretWhen(r))
}

// ref points at the natural key column of a parent table. With scopeCol the
// parent row must also have scopeCol equal to this row's scopeFrom column
// (a card's href is only unique within its list source).
type ref struct {
	table, col string
	scopeCol   string
	scopeFrom  string // database column of this table holding the scope id
}

// table describes one section of the document.
type table struct {
	section string
	table   string
	key     []string // YAML keys of the natural key
	cols    []column
	where   string // limits export and deletion, on alias t
	order   string
	// keep: rows missing from the file stay (settings are merged).
	keep bool
	// insert adds SQL expressions for columns that are not exported but
	// need a value in new rows.
	insert map[string]string
}

func (t *table) column(name string) (column, bool) {
	for _, c := range t.cols {
		if c.key() == name {
			return c, true
		}
	}
	return column{}, false
}

// secretSettings are settings whose values belong to the secrets map.
var secretSettings = map[string]bool{
	"backup.passphrase": true,
	"tls.ca_key":        true,
}

// runtimeSettings are not configuration and are never exported or touched
// by an import.
// The CA (tls.ca_*) is configuration because phones trust it; the server
// certificate issued from it is derived state.
var runtimeSettings = []string{"backup.last_", "tls.server_"}

func settingsWhere() string {
	var parts []string
	for _, p := range runtimeSettings {
		parts = append(parts, "t.key NOT LIKE '"+p+"%'")
	}
	return strings.Join(parts, " AND ")
}

const manualSources = "SELECT id FROM list_sources WHERE kind IN ('manual','file')"

// tables is the document layout, in dependency order: parents first.
var tables = []*table{
	{section: "settings", table: "settings", key: []string{"key"}, where: settingsWhere(), order: "t.key", keep: true,
		cols: []column{
			{col: "key"},
			{col: "value", secretWhen: func(r Row) bool { k, _ := r["key"].(string); return secretSettings[k] },
				generate: func() (string, string) { return "", "" }},
		}},
	{section: "trunks", table: "trunks", key: []string{"name"}, order: "t.name",
		cols: []column{
			{col: "name"}, {col: "enabled", kind: kBool}, {col: "profile"}, {col: "registrar"},
			{col: "outbound_proxy"}, {col: "transport"}, {col: "address_family"},
			{col: "auth_username"}, {col: "auth_password", secret: true},
			{col: "srtp", kind: kBool}, {col: "dial_format"}, {col: "codecs"},
			{col: "reg_expiration", kind: kInt},
		}},
	{section: "numbers", table: "trunk_numbers", key: []string{"number"}, order: "t.number",
		cols: []column{
			{col: "number"},
			{col: "trunk_id", name: "trunk", kind: kRef, ref: &ref{table: "trunks", col: "name"}},
			{col: "sip_user"}, {col: "register", kind: kBool}, {col: "label"},
		}},
	{section: "extensions", table: "extensions", key: []string{"number"}, order: "t.number",
		cols: []column{
			{col: "number"}, {col: "name"}, {col: "enabled", kind: kBool},
			{col: "sip_username"},
			{col: "sip_password", secret: true, generate: func() (string, string) {
				return randomToken(20), "gets a new SIP password (not in the file); set it on the phone"
			}},
			{col: "transport"}, {col: "srtp", kind: kBool}, {col: "codecs"},
			{col: "max_contacts", kind: kInt},
			{col: "outbound_number_id", name: "outbound_number", kind: kRef, ref: &ref{table: "trunk_numbers", col: "number"}},
			{col: "clir", kind: kBool}, {col: "dnd", kind: kBool},
			{col: "voicemail_pin", secret: true},
			{col: "owner_subject", name: "owner"},
		}},
	{section: "ring_groups", table: "ring_groups", key: []string{"name"}, order: "t.name",
		cols: []column{
			{col: "name"}, {col: "ring_timeout", kind: kInt}, {col: "fallback"},
			{col: "fallback_extension_id", name: "fallback_extension", kind: kRef, ref: &ref{table: "extensions", col: "number"}},
		}},
	{section: "ring_group_members", table: "ring_group_members", key: []string{"ring_group", "extension"},
		order: "(SELECT name FROM ring_groups WHERE id = t.ring_group_id), (SELECT number FROM extensions WHERE id = t.extension_id)",
		cols: []column{
			{col: "ring_group_id", name: "ring_group", kind: kRef, ref: &ref{table: "ring_groups", col: "name"}},
			{col: "extension_id", name: "extension", kind: kRef, ref: &ref{table: "extensions", col: "number"}},
		}},
	{section: "routes", table: "did_routes", key: []string{"number"},
		order: "(SELECT number FROM trunk_numbers WHERE id = t.trunk_number_id)",
		cols: []column{
			{col: "trunk_number_id", name: "number", kind: kRef, ref: &ref{table: "trunk_numbers", col: "number"}},
			{col: "ring_group_id", name: "ring_group", kind: kRef, ref: &ref{table: "ring_groups", col: "name"}},
		}},
	{section: "list_sources", table: "list_sources", key: []string{"name"}, order: "t.name",
		cols: []column{
			{col: "name"}, {col: "kind"}, {col: "target"}, {col: "url"}, {col: "username"},
			{col: "password", secret: true}, {col: "interval_min", kind: kInt},
			{col: "enabled", kind: kBool}, {col: "min_votes", kind: kInt},
		}},
	{section: "cards", table: "cards", key: []string{"source", "href"}, where: "t.source_id IN (" + manualSources + ")",
		order: "(SELECT name FROM list_sources WHERE id = t.source_id), t.href",
		cols: []column{
			{col: "source_id", name: "source", kind: kRef, ref: &ref{table: "list_sources", col: "name"}},
			{col: "href"}, {col: "name"}, {col: "org"},
		}},
	{section: "list_entries", table: "list_entries", key: []string{"source", "card", "pattern"},
		where: "t.source_id IN (" + manualSources + ")",
		order: "(SELECT name FROM list_sources WHERE id = t.source_id), (SELECT href FROM cards WHERE id = t.card_id), t.pattern",
		cols: []column{
			{col: "source_id", name: "source", kind: kRef, ref: &ref{table: "list_sources", col: "name"}},
			{col: "card_id", name: "card", kind: kRef, ref: &ref{table: "cards", col: "href", scopeCol: "source_id", scopeFrom: "source_id"}},
			{col: "pattern"}, {col: "label"},
		}},
	{section: "modules", table: "module_settings", key: []string{"module"}, order: "t.module",
		cols: []column{
			{col: "module"}, {col: "enabled", kind: kBool},
			{col: "config", kind: kJSON, jsonSecrets: []string{"password"}},
		}},
	{section: "webhooks", table: "webhook_endpoints", key: []string{"name"}, order: "t.name",
		cols: []column{
			{col: "name"}, {col: "url"}, {col: "events"}, {col: "enabled", kind: kBool},
			{col: "secret", secret: true, generate: func() (string, string) {
				return randomToken(32), "gets a new signing secret (not in the file); update the receiver"
			}},
		}},
	{section: "directory_accounts", table: "directory_accounts", key: []string{"username"}, order: "t.username",
		insert: map[string]string{"created_at": "CAST(unixepoch('subsec') * 1000 AS INTEGER)"},
		cols: []column{
			{col: "username"},
			{col: "password_hash", secret: true, generate: func() (string, string) {
				return "!" + randomToken(16), "has no password in the file; set a new one on the Directory page"
			}},
		}},
}

func tableBySection(name string) *table {
	for _, t := range tables {
		if t.section == name {
			return t
		}
	}
	return nil
}
