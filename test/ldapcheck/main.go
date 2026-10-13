// Command ldapcheck runs an LDAP search like a desk phone and prints one
// line per entry: "cn<TAB>telephoneNumber,mobile,homePhone". For e2e tests.
//
//	go run ./test/ldapcheck -addr 127.0.0.1:389 -user phones -pass … -filter '(cn=Mom*)'
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/go-ldap/ldap/v3"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:389", "server")
	user := flag.String("user", "", "bind name (empty: anonymous)")
	pass := flag.String("pass", "", "password")
	base := flag.String("base", "dc=voipd", "search base")
	filter := flag.String("filter", "(objectClass=person)", "search filter")
	flag.Parse()

	c, err := ldap.DialURL("ldap://" + *addr)
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	defer c.Close()
	if *user != "" {
		if err := c.Bind(*user, *pass); err != nil {
			fmt.Println("error:", err)
			os.Exit(1)
		}
	}
	res, err := c.Search(ldap.NewSearchRequest(*base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 10, false,
		*filter, []string{"cn", "telephoneNumber", "mobile", "homePhone"}, nil))
	if err != nil {
		fmt.Println("error:", err)
		os.Exit(1)
	}
	for _, e := range res.Entries {
		var nums []string
		for _, a := range []string{"telephoneNumber", "mobile", "homePhone"} {
			nums = append(nums, e.GetAttributeValues(a)...)
		}
		fmt.Printf("%s\t%s\n", e.GetAttributeValue("cn"), strings.Join(nums, ","))
	}
}
