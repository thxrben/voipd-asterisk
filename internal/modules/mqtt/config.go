package mqtt

import (
	"encoding/json"
	"net/url"
	"strings"

	"voipd/internal/admin"
)

type Config struct {
	URL      string `json:"url"` // mqtt://host:1883, mqtts://host:8883, ws(s)://host/mqtt
	Username string `json:"username"`
	Password string `json:"password"`
	ClientID string `json:"client_id"`
	Base     string `json:"base_topic"`
	Insecure bool   `json:"insecure"` // skip TLS certificate checks (self-signed brokers)

	Commands        bool   `json:"commands"`  // accept commands (blocking, DND, hang up)
	Discovery       bool   `json:"discovery"` // Home Assistant MQTT discovery
	DiscoveryPrefix string `json:"discovery_prefix"`

	url *url.URL
}

func parseConfig(raw json.RawMessage) (Config, error) {
	c := Config{ClientID: "voipd", Base: "voipd", Commands: true, Discovery: true, DiscoveryPrefix: "homeassistant"}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, admin.Errors{"config": "invalid: " + err.Error()}
	}
	errs := admin.Errors{}
	c.URL = strings.TrimSpace(c.URL)
	if !strings.Contains(c.URL, "://") && c.URL != "" {
		c.URL = "mqtt://" + c.URL
	}
	u, err := url.Parse(c.URL)
	switch {
	case err != nil || u.Host == "":
		errs["url"] = "enter the broker, e.g. mqtt://homeassistant.local:1883"
	case u.Scheme != "mqtt" && u.Scheme != "mqtts" && u.Scheme != "tcp" && u.Scheme != "ssl" && u.Scheme != "tls" &&
		u.Scheme != "ws" && u.Scheme != "wss":
		errs["url"] = "use mqtt://, mqtts://, ws:// or wss://"
	default:
		if u.Port() == "" && (u.Scheme == "mqtt" || u.Scheme == "tcp") {
			u.Host += ":1883"
		} else if u.Port() == "" && (u.Scheme == "mqtts" || u.Scheme == "ssl" || u.Scheme == "tls") {
			u.Host += ":8883"
		}
		c.url = u
	}
	c.ClientID = strings.TrimSpace(c.ClientID)
	if c.ClientID == "" || len(c.ClientID) > 64 {
		errs["client_id"] = "1–64 characters"
	}
	if msg := topicErr(c.Base); msg != "" {
		errs["base_topic"] = msg
	}
	if msg := topicErr(c.DiscoveryPrefix); c.Discovery && msg != "" {
		errs["discovery_prefix"] = msg
	}
	if len(errs) > 0 {
		return c, errs
	}
	return c, nil
}

func topicErr(t string) string {
	switch {
	case t == "":
		return "required"
	case strings.ContainsAny(t, "+#") || strings.HasPrefix(t, "/") || strings.HasSuffix(t, "/") || strings.Contains(t, "//"):
		return "a topic without wildcards and without leading or trailing /"
	}
	return ""
}
