package callflow

import "voipd/internal/store"

// Code is a feature code dialed on a phone.
type Code struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Codes lists the phone codes voipd understands (shown in the web UI).
var Codes = []Code{
	{Code: "*98", Name: "Voicemail",
		Description: "Listen to the messages of your own mailbox and record your personal greeting. " +
			"Main menu: 1 listen, 0 record greeting. While listening: 1 repeat, 2 or # next message, 7 delete. " +
			"After recording a greeting: 1 keep, 2 discard."},
}

// phoneCode runs a feature code dialed by an extension. It reports false
// if dialed is not a code.
func (c *call) phoneCode(from store.Extension, dialed string) bool {
	if len(dialed) < 2 || dialed[0] != '*' {
		return false
	}
	switch dialed {
	case "*98":
		c.mailboxMenu(from)
	default:
		c.unknownCode(from, dialed)
	}
	return true
}
