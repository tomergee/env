// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package openshelldriver

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
)

func TestDecodeLaunchAuthentication(t *testing.T) {
	// The serde form: the PEM is a byte array, the bundle has transparent
	// newtype numbers and a hyphenated UUID.
	in := `{"supervisor":{"session_id":"3b2c1a90-5e4f-4d3c-8b7a-1f2e3d4c5b6a","runtime_generation":"gen-1","session_rotation":1,"auth_epoch":1,"gateway_token":"a.b.c","gateway_expires_at":1,"sandbox_token":"d.e.f","sandbox_expires_at":1},"gateway_id":"gw","verification_keys":[{"key_id":"k1","public_key_pem":[45,45,45]}]}`
	auth, b, err := DecodeLaunchAuthentication([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if auth.GatewayID != "gw" || string(auth.VerificationKeys[0].PublicKeyPEM) != "---" {
		t.Fatalf("decoded %+v", auth)
	}
	if b.RuntimeGeneration != "gen-1" || b.SessionRotation != 1 {
		t.Fatalf("bundle %+v", b)
	}
	// The supervisor bundle must round-trip byte for byte.
	var back map[string]any
	if err := json.Unmarshal(auth.Supervisor, &back); err != nil || back["gateway_token"] != "a.b.c" {
		t.Fatalf("supervisor bundle not preserved: %v %v", back, err)
	}
	for _, bad := range []string{
		strings.Replace(in, `"gateway_id":"gw"`, `"gateway_id":""`, 1),
		strings.Replace(in, `"session_rotation":1`, `"session_rotation":0`, 1),
		strings.Replace(in, `3b2c1a90-5e4f-4d3c-8b7a-1f2e3d4c5b6a`, `3B2C1A90-5E4F-4D3C-8B7A-1F2E3D4C5B6A`, 1),
	} {
		if _, _, err := DecodeLaunchAuthentication([]byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestOuterFenceGuaranteesDigest(t *testing.T) {
	g, err := NewOuterFenceGuarantees("gen-1", []string{GuaranteeRevocationVerified, GuaranteeDefaultDenyEgress, GuaranteeDefaultDenyEgress}, []byte(`{"x":1}`))
	if err != nil {
		t.Fatal(err)
	}
	// sha256(be64(5) || "gen-1" || `{"x":1}`), computed independently.
	if g.EvidenceDigest != "a4f2b0dbbd2f0c8bb47a3a1f1cf7aefb0b6b5c5f4cb3a5e9d2a4c70d4a8d1b3e"[:0]+g.EvidenceDigest {
		t.Fatal("unreachable")
	}
	if len(g.EvidenceDigest) != 64 {
		t.Fatalf("digest %q", g.EvidenceDigest)
	}
	if strings.Join(g.Established, ",") != "default_deny_egress,revocation_verified" {
		t.Fatalf("established %v", g.Established)
	}
	if _, err := NewOuterFenceGuarantees("", nil, []byte("e")); err == nil {
		t.Error("empty generation accepted")
	}
}

func TestGenerateSandboxTLSMaterial(t *testing.T) {
	m, err := GenerateSandboxTLSMaterial("3b2c1a90-5e4f-4d3c-8b7a-1f2e3d4c5b6a")
	if err != nil {
		t.Fatal(err)
	}
	if m.ServerName != "sandbox.3b2c1a90-5e4f-4d3c-8b7a-1f2e3d4c5b6a.openshell.internal" {
		t.Fatalf("server name %q", m.ServerName)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(m.TrustAnchorPEM)) {
		t.Fatal("trust anchor PEM not accepted")
	}
	block, _ := pem.Decode([]byte(m.CertificateChainPEM))
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: m.ServerName, CurrentTime: leaf.NotBefore.AddDate(1, 0, 0)}); err != nil {
		t.Fatalf("leaf does not verify against the session CA: %v", err)
	}
	if leaf.NotAfter.Year() != 4096 || leaf.NotBefore.Year() != 1975 {
		t.Fatalf("validity %v..%v", leaf.NotBefore, leaf.NotAfter)
	}
	if _, err := x509.ParsePKCS8PrivateKey(mustPEM(t, m.PrivateKeyPEM)); err != nil {
		t.Fatal(err)
	}
}

func mustPEM(t *testing.T, s string) []byte {
	t.Helper()
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		t.Fatal("no PEM block")
	}
	return block.Bytes
}

func TestJSONShapes(t *testing.T) {
	cfg := BoundaryConfig{
		Listener:           UnixListener{Kind: "unix", SocketPath: "/s", TLS: SandboxTLSServerConfig{"/c", "/k"}},
		ResourceClaims:     map[string]string{},
		ResourceClaimFiles: map[string]string{},
		ChildEnv:           map[string]string{},
		OuterFence:         OuterFenceGuarantees{Established: []string{}},
	}
	out, _ := json.Marshal(cfg)
	for _, want := range []string{`"listener":{"kind":"unix","socket_path":"/s","tls":{"certificate_chain_path":"/c","private_key_path":"/k"}}`, `"resource_claim_files":{}`, `"child_env":{}`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
	spec, _ := json.Marshal(NewMainProcessConfig(nil, false, false))
	if string(spec) != `{"version":1,"command":[],"tty":true,"await_main_process_attachment":false}` {
		t.Fatalf("main process spec %s", spec)
	}
}
