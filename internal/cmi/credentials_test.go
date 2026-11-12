package cmi

import "testing"

func TestCredentialHeader(t *testing.T) {
	for _, h := range []string{
		"Authorization: Bearer x", "authorization: x", "Proxy-Authorization: Basic eDp5", "Cookie: session=1", "Set-Cookie: a=b",
		"X-Api-Key: k", "Api-Key: k", "X-Auth-Token: t", "X-Access-Token: t", "X-Session-Id: s", "X-Csrf-Token: c", "X-Secret: s",
		"X-Password: p", "X-Signature: s", "X-Credentials: c", "X-Custom: Bearer abc", "X-Custom: basic eDp5", "X-Custom: Token abc",
	} {
		if !CredentialHeader(h) {
			t.Errorf("%q passed as an ordinary header", h)
		}
	}
	for _, h := range []string{"Accept-Language: en", "X-Requested-With: idp-cmi", "User-Agent: idp-cmi", "X-Tenant: acme", "X-Custom: tokenless value", "Cache-Control: no-cache"} {
		if CredentialHeader(h) {
			t.Errorf("%q refused as a credential", h)
		}
	}
}
