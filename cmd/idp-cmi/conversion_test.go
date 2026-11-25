package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"idp-cmi/adapters/blob"
	"idp-cmi/adapters/store/postgres"
	"idp-cmi/internal/config"
	"idp-cmi/internal/observability"
)

func TestStartConversionRefusesAnUnsafeSetup(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "key")
	short := filepath.Join(dir, "short")
	secret := filepath.Join(dir, "secret")
	_ = os.WriteFile(good, []byte(strings.Repeat("k", 40)), 0o600)
	_ = os.WriteFile(short, []byte("short"), 0o600)
	_ = os.WriteFile(secret, []byte("s"), 0o600)
	conf := func() *config.Conversion {
		return &config.Conversion{Enabled: true, GatewayURL: "https://gw/mcp", TokenURL: "https://idp/token", ClientID: "c", ClientSecretFile: secret,
			WorkloadTenant: "system", WorkloadSubject: "svc", EndpointURL: "http://cmi:8553", ToolPrefix: "ingest__", Concurrency: 1, SandboxConcurrency: 8, MaxAttempts: 3, RunSeconds: 60}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := observability.NewMetrics()
	withStore := func() *postgres.Store { return &postgres.Store{Originals: &blob.Mem{}} }
	cases := map[string]struct {
		store  *postgres.Store
		listen string
		key    string
		mod    func(*config.Conversion)
		want   string
	}{
		"no original store":                 {&postgres.Store{}, "127.0.0.1:0", good, nil, "originals must be configured"},
		"no listener":                       {withStore(), "", good, nil, "IDP_CMI_INGEST_LISTEN is required"},
		"the API's own address":             {withStore(), "127.0.0.1:8550", good, nil, "must not be the API's address"},
		"no key":                            {withStore(), "127.0.0.1:0", "", nil, "IDP_CMI_INGEST_TICKET_KEY_FILE is required"},
		"a short key":                       {withStore(), "127.0.0.1:0", short, nil, "at least 32 bytes"},
		"a secret file that cannot be read": {withStore(), "127.0.0.1:0", good, func(c *config.Conversion) { c.ClientSecretFile = filepath.Join(dir, "none") }, "clientSecretFile"},
		"a gateway with a query":            {withStore(), "127.0.0.1:0", good, func(c *config.Conversion) { c.GatewayURL = "https://gw/mcp?x=1" }, "without credentials, query or fragment"},
	}
	for name, tc := range cases {
		t.Setenv("IDP_CMI_INGEST_LISTEN", tc.listen)
		t.Setenv("IDP_CMI_INGEST_TICKET_KEY_FILE", tc.key)
		c := conf()
		if tc.mod != nil {
			tc.mod(c)
		}
		stop, err := startConversion(context.Background(), log, tc.store, m, c, "it")
		if err == nil {
			stop()
			t.Errorf("%s was accepted", name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
