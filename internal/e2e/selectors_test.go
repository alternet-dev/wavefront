package e2e

import (
	"bytes"
	"io"
	"net/http"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/config"
)

func TestNonVersionSelectorsChooseResolution(t *testing.T) {
	cases := []struct {
		name, yamlField, header, value string
	}{
		{"profile", "profile", "X-Wavefront-Profile", "mobile"},
		{"tenant", "tenant", "X-Wavefront-Tenant", "partner-a"},
		{"cohort", "cohort", "X-Wavefront-Cohort", "canary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := Spawn(t, SpawnOpts{
				Resolution: "version: 1\noverrides:\n  - contract_version: \"2024-11\"\n    " + tc.yamlField + ": " + tc.value + "\n    transform:\n      response:\n        - default: { field: text, value: " + tc.value + " }\n",
				ConfigOverride: func(cfg *config.Config) {
					cfg.ProfileHeader = "X-Wavefront-Profile"
					cfg.TenantHeader = "X-Wavefront-Tenant"
					cfg.CohortHeader = "X-Wavefront-Cohort"
				},
			})

			req, err := http.NewRequest(http.MethodPost, h.Proxy.URL+"/v3/echo", bytes.NewReader(PingBytes(t, h.Bundle, "in", 1)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/protobuf")
			req.Header.Set("X-Api-Contract-Version", "2024-11")
			req.Header.Set(tc.header, tc.value)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			out, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			md, err := h.Bundle.Message("acme.v1.Pong")
			if err != nil {
				t.Fatal(err)
			}
			message := dynamicpb.NewMessage(md)
			if err := proto.Unmarshal(out, message); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if got := message.Get(md.Fields().ByName(protoreflect.Name("text"))).String(); got != tc.value {
				t.Errorf("response text = %q, want %q", got, tc.value)
			}
		})
	}
}
