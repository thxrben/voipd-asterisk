package notify

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	ttemplate "text/template"
	"time"

	"voipd/internal/events"
	"voipd/internal/modules"
)

// Mail texts are German: voipd's carrier and number formats are German.
type tmpl struct {
	subject *ttemplate.Template
	text    *ttemplate.Template
	html    *template.Template
}

func newTmpl(subject, text, html string) tmpl {
	return tmpl{
		subject: ttemplate.Must(ttemplate.New("s").Parse(subject)),
		text:    ttemplate.Must(ttemplate.New("t").Parse(text)),
		html:    template.Must(template.New("h").Parse(htmlHead + html + htmlFoot)),
	}
}

const htmlHead = `<!doctype html><html><body style="font-family:system-ui,sans-serif;font-size:15px;color:#222">`
const htmlFoot = `{{if .Link}}<p><a href="{{.Link}}">Details in voipd</a></p>{{end}}</body></html>`

var (
	missedTmpl = newTmpl(
		`Verpasster Anruf von {{.Caller}}`,
		`Verpasster Anruf

Von:  {{.Caller}}{{if .CallerNumber}} ({{.CallerNumber}}){{end}}
An:   {{.Callee}}
Zeit: {{.Time}}{{if .Extensions}}
Klingelte bei: {{.Extensions}}{{end}}
{{if .Link}}
Details: {{.Link}}
{{end}}`,
		`<h2 style="margin:0 0 .5em">Verpasster Anruf</h2>
<table cellpadding="3"><tr><td>Von</td><td><b>{{.Caller}}</b>{{if .CallerNumber}} ({{.CallerNumber}}){{end}}</td></tr>
<tr><td>An</td><td>{{.Callee}}</td></tr><tr><td>Zeit</td><td>{{.Time}}</td></tr>
{{if .Extensions}}<tr><td>Klingelte bei</td><td>{{.Extensions}}</td></tr>{{end}}</table>`)

	blockedTmpl = newTmpl(
		`Anruf blockiert: {{.Caller}}`,
		`Ein Anruf wurde blockiert.

Von:   {{.Caller}}{{if .CallerNumber}} ({{.CallerNumber}}){{end}}
An:    {{.Callee}}
Zeit:  {{.Time}}
Grund: {{.Reason}}
{{if .Link}}
Details: {{.Link}}
{{end}}`,
		`<h2 style="margin:0 0 .5em">Anruf blockiert</h2>
<table cellpadding="3"><tr><td>Von</td><td><b>{{.Caller}}</b>{{if .CallerNumber}} ({{.CallerNumber}}){{end}}</td></tr>
<tr><td>An</td><td>{{.Callee}}</td></tr><tr><td>Zeit</td><td>{{.Time}}</td></tr><tr><td>Grund</td><td>{{.Reason}}</td></tr></table>`)

	voicemailTmpl = newTmpl(
		`Neue Sprachnachricht von {{.Caller}}`,
		`Neue Sprachnachricht

Von:   {{.Caller}}{{if .CallerNumber}} ({{.CallerNumber}}){{end}}
An:    {{.Callee}}{{if .Extensions}} (Mailbox {{.Extensions}}){{end}}
Zeit:  {{.Time}}
Länge: {{.Duration}}
{{if .Attached}}
Die Nachricht ist als Audiodatei angehängt.{{end}}{{if .Link}}
Anhören: {{.Link}}
{{end}}`,
		`<h2 style="margin:0 0 .5em">Neue Sprachnachricht</h2>
<table cellpadding="3"><tr><td>Von</td><td><b>{{.Caller}}</b>{{if .CallerNumber}} ({{.CallerNumber}}){{end}}</td></tr>
<tr><td>An</td><td>{{.Callee}}{{if .Extensions}} (Mailbox {{.Extensions}}){{end}}</td></tr>
<tr><td>Zeit</td><td>{{.Time}}</td></tr><tr><td>Länge</td><td>{{.Duration}}</td></tr></table>
{{if .Attached}}<p>Die Nachricht ist als Audiodatei angehängt.</p>{{end}}`)

	trunkDownTmpl = newTmpl(
		`Leitung gestört: {{.Trunk}} {{.Number}}`,
		`Die Leitung {{.Trunk}} ({{.Number}}) ist seit {{.Time}} nicht registriert.
Zustand: {{.Reason}}

voipd versucht die Verbindung automatisch wiederherzustellen; laufende Gespräche werden dabei nicht unterbrochen.
`,
		`<h2 style="margin:0 0 .5em">Leitung gestört</h2>
<p>Die Leitung <b>{{.Trunk}}</b> ({{.Number}}) ist seit {{.Time}} nicht registriert.<br>Zustand: {{.Reason}}</p>
<p>voipd versucht die Verbindung automatisch wiederherzustellen; laufende Gespräche werden dabei nicht unterbrochen.</p>`)

	trunkUpTmpl = newTmpl(
		`Leitung wieder verbunden: {{.Trunk}} {{.Number}}`,
		`Die Leitung {{.Trunk}} ({{.Number}}) ist seit {{.Time}} wieder registriert.
Ausfallzeit: {{.Downtime}}
`,
		`<h2 style="margin:0 0 .5em">Leitung wieder verbunden</h2>
<p>Die Leitung <b>{{.Trunk}}</b> ({{.Number}}) ist seit {{.Time}} wieder registriert.<br>Ausfallzeit: {{.Downtime}}</p>`)

	backupFailedTmpl = newTmpl(
		`voipd: Sicherung fehlgeschlagen`,
		`Die Sicherung von voipd am {{.Time}} ist fehlgeschlagen.
Fehler: {{.Reason}}
{{if .Link}}
Details: {{.Link}}
{{end}}`,
		`<h2 style="margin:0 0 .5em">Sicherung fehlgeschlagen</h2>
<p>Die Sicherung von voipd am {{.Time}} ist fehlgeschlagen.<br>Fehler: {{.Reason}}</p>`)

	bannedTmpl = newTmpl(
		`voipd: SIP-Absender gesperrt: {{.Reason}}`,
		`voipd hat am {{.Time}} einen SIP-Absender gesperrt.

Adresse: {{.Reason}}
Grund:   {{.Detail}}{{if .Extensions}}
Konto:   {{.Extensions}}{{end}}
{{if .Link}}
Entsperren: {{.Link}}
{{end}}`,
		`<h2 style="margin:0 0 .5em">SIP-Absender gesperrt</h2>
<table cellpadding="3"><tr><td>Adresse</td><td><b>{{.Reason}}</b></td></tr><tr><td>Grund</td><td>{{.Detail}}</td></tr>
{{if .Extensions}}<tr><td>Konto</td><td>{{.Extensions}}</td></tr>{{end}}<tr><td>Zeit</td><td>{{.Time}}</td></tr></table>`)

	testTmpl = newTmpl(
		`voipd: Test-E-Mail`,
		`Diese Test-E-Mail zeigt, dass der Mailversand von voipd funktioniert ({{.Time}}).
`,
		`<p>Diese Test-E-Mail zeigt, dass der Mailversand von voipd funktioniert ({{.Time}}).</p>`)
)

// fields are the values the templates use.
type fields struct {
	Caller, CallerNumber, Callee, Time, Extensions, Reason string
	Trunk, Number, Downtime                                string
	Link, Duration, Detail                                 string
	Attached                                               bool
}

var weekdays = [...]string{"So", "Mo", "Di", "Mi", "Do", "Fr", "Sa"}

// render fills a template from an event; extra may adjust the fields.
func render(h *modules.Host, t tmpl, ev events.Event, extra ...func(*fields)) Message {
	str := func(k string) string { s, _ := ev.Data[k].(string); return s }
	display := func(e164 string) string {
		if e164 == "" {
			return ""
		}
		if h.Numbers != nil && strings.HasPrefix(e164, "+") {
			return h.Numbers.Display(h.Numbers.FromE164(e164))
		}
		return e164
	}
	at := ev.At.Local()
	f := fields{
		Callee:     display(str("callee")),
		Time:       fmt.Sprintf("%s, %s Uhr", weekdays[at.Weekday()], at.Format("02.01.2006 15:04")),
		Extensions: strings.Join(stringList(ev.Data["extensions"]), ", "),
		Reason:     firstOf(str("reason"), str("detail"), str("state")),
		Trunk:      str("trunk"),
		Number:     display(str("number")),
	}
	switch caller := str("caller"); {
	case caller == "" || caller == "anonymous":
		f.Caller = "Unbekannt"
	case str("caller_name") != "":
		f.Caller, f.CallerNumber = str("caller_name"), display(caller)
	default:
		f.Caller = display(caller)
	}
	if s, ok := ev.Data["down_seconds"].(float64); ok {
		f.Downtime = (time.Duration(s) * time.Second).String()
	}
	if ev.CallID != "" && h.PublicURL != "" {
		f.Link = strings.TrimRight(h.PublicURL, "/") + "/calls/" + ev.CallID
	}
	if ext := str("extension"); ext != "" {
		f.Extensions = ext
	}
	if s, ok := ev.Data["duration_seconds"].(float64); ok {
		d := int(s + 0.5)
		f.Duration = fmt.Sprintf("%d:%02d min", d/60, d%60)
	}
	for _, x := range extra {
		x(&f)
	}
	var subj, text, html bytes.Buffer
	t.subject.Execute(&subj, f)
	t.text.Execute(&text, f)
	t.html.Execute(&html, f)
	return Message{Subject: subj.String(), Text: text.String(), HTML: html.String()}
}

func firstOf(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}
