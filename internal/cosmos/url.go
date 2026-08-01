package cosmos

import "net/url"

func RedactedURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "invalid-url"
	}
	redacted := url.URL{Scheme: u.Scheme, Host: u.Host}
	return redacted.String()
}
