package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/alternet-dev/wavefront/internal/bundlegen"
)

// TestVersionSkew is the deliberate "old contract survives a moved backend"
// regression gate. Each scenario under testdata/skew/ pins an old contract
// bundle, points the proxy at a backend serving the drifted internal shape,
// and asserts the transform shim in resolution.yaml translates the drift
// away — a client speaking the old contract gets back the old contract's
// response (modulo JSON formatting), and the backend (if asserted) sees the
// transformed request.
//
// The fixture shape is shared with drift-detection work: old/openapi.json
// and new/openapi.json double as the inputs the drift drafter compares;
// resolution.yaml is the golden shim a drafter must match.
//
//	testdata/skew/<scenario>/
//	  old/openapi.json                     required — source for the old layer
//	  new/openapi.json                     optional — drifted contract (#47 input)
//	  resolution.yaml                      optional — golden transform shim
//	  backend-shape.json                   required — stub backend's response body
//	  request-old.json                     required — client request (old contract)
//	  expected-response-old.json           required — client's expected response
//	  expected-backend-request.json        optional — assert what the backend saw
func TestVersionSkew(t *testing.T) {
	const root = "testdata/skew"
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read %s: %v", root, err)
	}
	scenarios := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			scenarios = append(scenarios, e.Name())
		}
	}
	if len(scenarios) == 0 {
		t.Fatalf("no skew scenarios under %s", root)
	}
	for _, name := range scenarios {
		t.Run(name, func(t *testing.T) {
			runSkewScenario(t, filepath.Join(root, name))
		})
	}
}

func runSkewScenario(t *testing.T, dir string) {
	t.Helper()

	bundleDir := t.TempDir()
	oldOpenAPI := filepath.Join(dir, "old", "openapi.json")
	if err := bundlegen.Add(oldOpenAPI, bundleDir); err != nil {
		t.Fatalf("bundlegen.Add(%s): %v", oldOpenAPI, err)
	}
	if res, err := os.ReadFile(filepath.Join(dir, "resolution.yaml")); err == nil {
		if err := os.WriteFile(filepath.Join(bundleDir, "resolution.yaml"), res, 0o600); err != nil {
			t.Fatalf("write resolution.yaml: %v", err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatalf("read resolution.yaml: %v", err)
	}

	backendShape := mustReadJSONFixture(t, filepath.Join(dir, "backend-shape.json"))
	requestPayload := mustReadJSONFixture(t, filepath.Join(dir, "request-old.json"))
	expectedResponse := mustReadJSONFixture(t, filepath.Join(dir, "expected-response-old.json"))

	var expectedBackendReq []byte
	if b, err := os.ReadFile(filepath.Join(dir, "expected-backend-request.json")); err == nil {
		expectedBackendReq = b
	} else if !os.IsNotExist(err) {
		t.Fatalf("read expected-backend-request.json: %v", err)
	}

	h := Spawn(t, SpawnOpts{
		BundleDir: bundleDir,
		BackendHandler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(backendShape)
		},
	})

	version := singleLayerVersion(t, bundleDir)
	contract, ok := h.Bundle.Contract(version)
	if !ok {
		t.Fatalf("contract %q not in bundle", version)
	}

	reqMD, err := h.Bundle.Message(contract.RequestMessage())
	if err != nil {
		t.Fatalf("resolve %s: %v", contract.RequestMessage(), err)
	}
	reqMsg := dynamicpb.NewMessage(reqMD)
	if err := protojson.Unmarshal(requestPayload, reqMsg); err != nil {
		t.Fatalf("unmarshal request-old.json into %s: %v", contract.RequestMessage(), err)
	}
	reqBytes, err := proto.Marshal(reqMsg)
	if err != nil {
		t.Fatalf("marshal request proto: %v", err)
	}

	req, _ := http.NewRequest(contract.Method(), h.Proxy.URL+contract.Route(), bytes.NewReader(reqBytes))
	req.Header.Set("Content-Type", "application/protobuf")
	req.Header.Set("X-Api-Contract-Version", version)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}

	if expectedBackendReq != nil {
		jsonEqualMaps(t, "backend request", h.Backend.Last().Body, expectedBackendReq)
	}

	rawResp, _ := io.ReadAll(resp.Body)
	respMD, err := h.Bundle.Message(contract.ResponseMessage())
	if err != nil {
		t.Fatalf("resolve %s: %v", contract.ResponseMessage(), err)
	}
	respMsg := dynamicpb.NewMessage(respMD)
	if err := proto.Unmarshal(rawResp, respMsg); err != nil {
		t.Fatalf("unmarshal response proto: %v", err)
	}
	gotJSON, err := protojson.Marshal(respMsg)
	if err != nil {
		t.Fatalf("marshal response json: %v", err)
	}
	jsonEqualMaps(t, "client response", gotJSON, expectedResponse)
}

func mustReadJSONFixture(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func singleLayerVersion(t *testing.T, bundleDir string) string {
	t.Helper()
	entries, err := os.ReadDir(bundleDir)
	if err != nil {
		t.Fatalf("read bundle dir: %v", err)
	}
	layers := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			layers = append(layers, e.Name())
		}
	}
	if len(layers) != 1 {
		t.Fatalf("expected exactly one layer in %s, got %d (%v)", bundleDir, len(layers), layers)
	}
	return layers[0]
}

func jsonEqualMaps(t *testing.T, label string, got, want []byte) {
	t.Helper()
	var gotMap, wantMap map[string]any
	if err := json.Unmarshal(got, &gotMap); err != nil {
		t.Fatalf("%s: decode got: %v (%s)", label, err, got)
	}
	if err := json.Unmarshal(want, &wantMap); err != nil {
		t.Fatalf("%s: decode want: %v (%s)", label, err, want)
	}
	if !reflect.DeepEqual(gotMap, wantMap) {
		t.Errorf("%s mismatch:\n got  = %v\n want = %v", label, gotMap, wantMap)
	}
}
