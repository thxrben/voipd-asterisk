package directory

import (
	"strings"
)

// BaseDN is the root of voipd's directory tree.
const BaseDN = "dc=voipd"

const (
	contactsOU   = "ou=contacts," + BaseDN
	extensionsOU = "ou=extensions," + BaseDN
)

// phoneAttrs hold numbers and are matched as numbers.
var phoneAttrs = map[string]string{
	TypeWork: "telephoneNumber", TypeOther: "telephoneNumber", TypeHome: "homePhone", TypeCell: "mobile",
}

func isPhoneAttr(lower string) bool {
	switch lower {
	case "telephonenumber", "homephone", "mobile", "ipphone", "othertelephone", "homephonenumber", "mobiletelephonenumber":
		return true
	}
	return false
}

type attr struct {
	name   string
	values []string
	nums   []Number // for phone attributes, parallel to values
}

// node is an LDAP entry.
type node struct {
	dn    string
	attrs []attr
}

func (n *node) add(name string, values ...string) {
	var vs []string
	for _, v := range values {
		if v != "" {
			vs = append(vs, v)
		}
	}
	if len(vs) == 0 {
		return
	}
	for i := range n.attrs {
		if n.attrs[i].name == name {
			n.attrs[i].values = append(n.attrs[i].values, vs...)
			return
		}
	}
	n.attrs = append(n.attrs, attr{name: name, values: vs})
}

func (n *node) addNumber(name string, num Number) {
	for i := range n.attrs {
		if n.attrs[i].name == name {
			n.attrs[i].values = append(n.attrs[i].values, num.Dial)
			n.attrs[i].nums = append(n.attrs[i].nums, num)
			return
		}
	}
	n.attrs = append(n.attrs, attr{name: name, values: []string{num.Dial}, nums: []Number{num}})
}

// get returns an attribute (case-insensitive), objectClass included.
func (n *node) get(name string) *attr {
	for i := range n.attrs {
		if strings.EqualFold(n.attrs[i].name, name) {
			return &n.attrs[i]
		}
	}
	return nil
}

func personNode(e Entry) *node {
	ou, parent := "contacts", contactsOU
	if e.Kind == "extension" {
		ou, parent = "extensions", extensionsOU
	}
	n := &node{dn: "uid=" + e.ID + "," + parent}
	n.add("objectClass", "top", "person", "organizationalPerson", "inetOrgPerson")
	n.add("uid", e.ID)
	n.add("cn", e.Name)
	n.add("displayName", e.Name)
	n.add("sn", e.Family)
	n.add("givenName", e.Given)
	n.add("o", e.Org)
	n.add("ou", ou)
	for _, num := range e.Numbers {
		n.addNumber(phoneAttrs[num.Type], num)
	}
	return n
}

func rootDSE() *node {
	n := &node{dn: ""}
	n.add("objectClass", "top")
	n.add("namingContexts", BaseDN)
	n.add("supportedLDAPVersion", "3")
	n.add("vendorName", "voipd")
	return n
}

func containerNodes() (base, contacts, extensions *node) {
	base = &node{dn: BaseDN}
	base.add("objectClass", "top", "domain", "dcObject")
	base.add("dc", "voipd")
	contacts = &node{dn: contactsOU}
	contacts.add("objectClass", "top", "organizationalUnit")
	contacts.add("ou", "contacts")
	extensions = &node{dn: extensionsOU}
	extensions.add("objectClass", "top", "organizationalUnit")
	extensions.add("ou", "extensions")
	return
}

// normDN lowercases a DN and removes spaces around separators.
func normDN(dn string) string {
	parts := strings.Split(strings.ToLower(dn), ",")
	for i, p := range parts {
		kv := strings.SplitN(p, "=", 2)
		for j := range kv {
			kv[j] = strings.TrimSpace(kv[j])
		}
		parts[i] = strings.Join(kv, "=")
	}
	return strings.Join(parts, ",")
}

// parent returns the DN one level up ("" for a top entry).
func parent(dn string) string {
	if i := strings.IndexByte(dn, ','); i >= 0 {
		return dn[i+1:]
	}
	return ""
}
