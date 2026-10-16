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

// Package openshelldriver is an NVIDIA OpenShell compute driver that runs
// OpenShell sandboxes as ate-env environments on Agent Substrate.
//
// This file mirrors the launch contract between an OpenShell compute driver
// and the two trusted binaries it starts, openshell-supervisor and
// openshell-sandbox, as defined in OpenShell v0.1.4:
//
//   - crates/openshell-core/src/jwt.rs: SandboxLaunchAuthentication and
//     SupervisorAuthBundle (what the gateway hands the driver, and what the
//     supervisor reads from --auth-bundle-file);
//   - crates/openshell-sandbox-backend/src/boundary_protocol.rs:
//     SandboxRuntimeDescriptor (the supervisor's --backend-descriptor-file),
//     BoundaryConfig (openshell-sandbox --bootstrap), BoundaryListener,
//     SandboxTransport, the TLS configs and generate_sandbox_tls_material;
//   - crates/openshell-isolation-interface/src/contract.rs:
//     ResolvedWorkloadIdentity, OuterFenceGuarantees;
//   - crates/openshell-core/src/sandbox_env.rs: MainProcessConfig.
//
// Field names and encodings follow serde: structs are JSON objects with
// snake_case keys and deny unknown fields, internally tagged enums use
// "kind" with kebab-case variants, the session id is a hyphenated UUID, the
// evidence digest is 64 lowercase hex characters, and the outer-fence
// guarantee set is a sorted list of snake_case names.
package openshelldriver

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"
)

// BackendName is the isolation backend the supervisor must select; the
// driver passes it in OPENSHELL_ADMITTED_ISOLATION_BACKEND
// (openshell_sandbox_backend::BACKEND_NAME).
const BackendName = "openshell-sandbox"

// LaunchAuthentication is openshell_core::jwt::SandboxLaunchAuthentication,
// the JSON the gateway puts in DriverSandboxSpec.launch_authentication. The
// supervisor bundle is kept opaque and written back verbatim as the
// supervisor's --auth-bundle-file; only the fields the driver itself needs
// are decoded from it.
type LaunchAuthentication struct {
	Supervisor       json.RawMessage          `json:"supervisor"`
	GatewayID        string                   `json:"gateway_id"`
	VerificationKeys []SessionVerificationKey `json:"verification_keys"`
}

// SessionVerificationKey is openshell_core::jwt::SessionVerificationKey. The
// PEM is a byte vector in Rust, which serde encodes as a JSON array of
// numbers; Go's []byte would encode it as base64, so it is decoded by hand.
type SessionVerificationKey struct {
	KeyID        string
	PublicKeyPEM []byte
}

// UnmarshalJSON accepts the serde form: {"key_id": "...", "public_key_pem": [byte, ...]}.
func (k *SessionVerificationKey) UnmarshalJSON(data []byte) error {
	var raw struct {
		KeyID        string `json:"key_id"`
		PublicKeyPEM []int  `json:"public_key_pem"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	k.KeyID = raw.KeyID
	k.PublicKeyPEM = make([]byte, len(raw.PublicKeyPEM))
	for i, b := range raw.PublicKeyPEM {
		if b < 0 || b > 255 {
			return fmt.Errorf("verification key %q: byte %d out of range", raw.KeyID, b)
		}
		k.PublicKeyPEM[i] = byte(b)
	}
	return nil
}

// supervisorBundleFields are the parts of SupervisorAuthBundle the driver
// reads: the identifiers that must agree between the supervisor and the
// sandbox side of the launch.
type supervisorBundleFields struct {
	SessionID         string `json:"session_id"`
	RuntimeGeneration string `json:"runtime_generation"`
	SessionRotation   uint64 `json:"session_rotation"`
	AuthEpoch         uint64 `json:"auth_epoch"`
}

var (
	sessionIDPattern  = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	generationPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// DecodeLaunchAuthentication parses and validates the gateway's launch
// authentication. The checks match SandboxLaunchAuthentication::validate:
// a non-empty gateway id, at least one verification key with a unique id and
// a non-empty PEM, and a bundle with a valid session id, generation, rotation
// and epoch.
func DecodeLaunchAuthentication(encoded []byte) (*LaunchAuthentication, *supervisorBundleFields, error) {
	if len(encoded) == 0 {
		return nil, nil, fmt.Errorf("launch authentication is required")
	}
	var auth LaunchAuthentication
	if err := json.Unmarshal(encoded, &auth); err != nil {
		return nil, nil, fmt.Errorf("decode launch authentication: %w", err)
	}
	if auth.GatewayID == "" {
		return nil, nil, fmt.Errorf("launch authentication has no gateway id")
	}
	if len(auth.VerificationKeys) == 0 {
		return nil, nil, fmt.Errorf("launch authentication has no verification keys")
	}
	seen := map[string]bool{}
	for _, k := range auth.VerificationKeys {
		if k.KeyID == "" || len(k.PublicKeyPEM) == 0 {
			return nil, nil, fmt.Errorf("launch authentication has an invalid verification key")
		}
		if seen[k.KeyID] {
			return nil, nil, fmt.Errorf("launch authentication repeats key id %q", k.KeyID)
		}
		seen[k.KeyID] = true
	}
	var b supervisorBundleFields
	if err := json.Unmarshal(auth.Supervisor, &b); err != nil {
		return nil, nil, fmt.Errorf("decode supervisor auth bundle: %w", err)
	}
	if !sessionIDPattern.MatchString(b.SessionID) {
		return nil, nil, fmt.Errorf("supervisor auth bundle session id %q is not a hyphenated UUID", b.SessionID)
	}
	if !generationPattern.MatchString(b.RuntimeGeneration) {
		return nil, nil, fmt.Errorf("supervisor auth bundle generation %q is not valid", b.RuntimeGeneration)
	}
	if b.SessionRotation == 0 || b.AuthEpoch == 0 {
		return nil, nil, fmt.Errorf("supervisor auth bundle rotation and epoch must be positive")
	}
	return &auth, &b, nil
}

// ResolvedWorkloadIdentity is
// openshell_isolation_interface::contract::ResolvedWorkloadIdentity: the
// non-root uid and gid the sandbox and every workload child run as.
type ResolvedWorkloadIdentity struct {
	UID               uint32   `json:"uid"`
	GID               uint32   `json:"gid"`
	SupplementaryGIDs []uint32 `json:"supplementary_gids"`
	Source            string   `json:"source"`
	ResourceDigest    string   `json:"resource_digest"`
}

// Validate applies ResolvedWorkloadIdentity::new's rules.
func (w ResolvedWorkloadIdentity) Validate() error {
	if w.UID == 0 || w.GID == 0 {
		return fmt.Errorf("workload identity must not contain UID or GID zero")
	}
	for _, g := range w.SupplementaryGIDs {
		if g == 0 {
			return fmt.Errorf("workload identity must not contain UID or GID zero")
		}
	}
	if w.Source == "" || w.ResourceDigest == "" {
		return fmt.Errorf("workload identity source and resource digest are required")
	}
	return nil
}

// GatewayVerificationKey is the sandbox-side form of a verification key
// (boundary_protocol::GatewayVerificationKey): the PEM as a string.
type GatewayVerificationKey struct {
	KeyID        string `json:"key_id"`
	PublicKeyPEM string `json:"public_key_pem"`
}

// SandboxTLSServerConfig is boundary_protocol::SandboxTlsServerConfig.
type SandboxTLSServerConfig struct {
	CertificateChainPath string `json:"certificate_chain_path"`
	PrivateKeyPath       string `json:"private_key_path"`
}

// SandboxTLSClientConfig is boundary_protocol::SandboxTlsClientConfig.
type SandboxTLSClientConfig struct {
	ServerName     string `json:"server_name"`
	TrustAnchorPEM string `json:"trust_anchor_pem"`
}

// UnixListener is BoundaryListener::Unix, the only listener kind this driver
// provisions: the supervisor and the sandbox share the actor's filesystem.
type UnixListener struct {
	Kind       string                 `json:"kind"`
	SocketPath string                 `json:"socket_path"`
	TLS        SandboxTLSServerConfig `json:"tls"`
}

// UnixTransport is SandboxTransport::Unix.
type UnixTransport struct {
	Kind       string `json:"kind"`
	SocketPath string `json:"socket_path"`
}

// OuterFenceGuarantee names are OuterFenceGuarantee variants in snake_case.
const (
	GuaranteeDefaultDenyEgress         = "default_deny_egress"
	GuaranteeNoUnmanagedEgressPath     = "no_unmanaged_egress_path"
	GuaranteeRevocationVerified        = "revocation_verified"
	GuaranteeControllerLossFailsClosed = "controller_loss_fails_closed"
)

// OuterFenceGuarantees is contract::OuterFenceGuarantees. The supervisor's
// validate() requires all four guarantees and the admitted generation; the
// digest binds the driver's native evidence to that generation.
type OuterFenceGuarantees struct {
	Generation     string   `json:"generation"`
	Established    []string `json:"established"`
	EvidenceDigest string   `json:"evidence_digest"`
}

// NewOuterFenceGuarantees is OuterFenceGuarantees::from_enforcement_evidence:
// sha256(be64(len(generation)) || generation || evidence), hex encoded, with
// the guarantee set sorted as a BTreeSet would be.
func NewOuterFenceGuarantees(generation string, established []string, evidence []byte) (OuterFenceGuarantees, error) {
	if generation == "" || len(evidence) == 0 {
		return OuterFenceGuarantees{}, fmt.Errorf("outer fence generation and native evidence are required")
	}
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(generation)))
	h.Write(n[:])
	h.Write([]byte(generation))
	h.Write(evidence)
	set := map[string]bool{}
	for _, g := range established {
		set[g] = true
	}
	sorted := make([]string, 0, len(set))
	for g := range set {
		sorted = append(sorted, g)
	}
	sort.Strings(sorted)
	return OuterFenceGuarantees{
		Generation:     generation,
		Established:    sorted,
		EvidenceDigest: hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// BoundaryConfig is boundary_protocol::BoundaryConfig, the one-use bootstrap
// file openshell-sandbox reads.
type BoundaryConfig struct {
	BoundaryID         string                   `json:"boundary_id"`
	Generation         string                   `json:"generation"`
	SessionID          string                   `json:"session_id"`
	SessionRotation    uint64                   `json:"session_rotation"`
	AuthEpoch          uint64                   `json:"auth_epoch"`
	GatewayID          string                   `json:"gateway_id"`
	VerificationKeys   []GatewayVerificationKey `json:"verification_keys"`
	Listener           UnixListener             `json:"listener"`
	ResourceClaims     map[string]string        `json:"resource_claims"`
	ResourceClaimFiles map[string]string        `json:"resource_claim_files"`
	WorkloadIdentity   ResolvedWorkloadIdentity `json:"workload_identity"`
	OuterFence         OuterFenceGuarantees     `json:"outer_fence"`
	ChildEnv           map[string]string        `json:"child_env"`
}

// SandboxRuntimeDescriptor is boundary_protocol::SandboxRuntimeDescriptor,
// the supervisor's --backend-descriptor-file.
type SandboxRuntimeDescriptor struct {
	BoundaryID       string                   `json:"boundary_id"`
	Generation       string                   `json:"generation"`
	SessionID        string                   `json:"session_id"`
	WorkloadIdentity ResolvedWorkloadIdentity `json:"workload_identity"`
	Transport        UnixTransport            `json:"transport"`
	TLS              SandboxTLSClientConfig   `json:"tls"`
	HostGatewayIP    *string                  `json:"host_gateway_ip,omitempty"`
	ResourceClaims   map[string]string        `json:"resource_claims"`
	OuterFence       OuterFenceGuarantees     `json:"outer_fence"`
}

// MainProcessConfig is openshell_core::sandbox_env::MainProcessConfig,
// passed to the supervisor as JSON in OPENSHELL_MAIN_PROCESS_SPEC. An empty
// command means "no command supplied": the sandbox picks a login shell from
// the image.
type MainProcessConfig struct {
	Version                    uint32   `json:"version"`
	Command                    []string `json:"command"`
	TTY                        bool     `json:"tty"`
	AwaitMainProcessAttachment bool     `json:"await_main_process_attachment"`
}

// NewMainProcessConfig follows MainProcessConfig::from_driver_spec.
func NewMainProcessConfig(command []string, tty, awaitAttachment bool) MainProcessConfig {
	if len(command) == 0 {
		return MainProcessConfig{Version: 1, Command: []string{}, TTY: true}
	}
	return MainProcessConfig{Version: 1, Command: command, TTY: tty, AwaitMainProcessAttachment: awaitAttachment}
}

// SandboxTLSMaterial is boundary_protocol::SandboxTlsMaterial: a per-session
// CA, discarded after signing, and the sandbox's server certificate.
type SandboxTLSMaterial struct {
	ServerName          string
	TrustAnchorPEM      string
	CertificateChainPEM string
	PrivateKeyPEM       string
}

// GenerateSandboxTLSMaterial is generate_sandbox_tls_material: Ed25519 keys,
// validity 1975-01-01 to 4096-01-01, a CA named "OpenShell sandbox session
// CA" with keyCertSign and cRLSign, and a leaf named "OpenShell sandbox
// runtime" for server auth with the DNS name sandbox.<session>.openshell.internal.
func GenerateSandboxTLSMaterial(sessionID string) (*SandboxTLSMaterial, error) {
	serverName := fmt.Sprintf("sandbox.%s.openshell.internal", sessionID)
	notBefore := time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC)
	notAfter := time.Date(4096, 1, 1, 0, 0, 0, 0, time.UTC)

	caPub, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate sandbox CA key: %w", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "OpenShell sandbox session CA"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caPub, caPriv)
	if err != nil {
		return nil, fmt.Errorf("generate sandbox CA certificate: %w", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, fmt.Errorf("parse sandbox CA certificate: %w", err)
	}

	leafPub, leafPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate sandbox TLS server key: %w", err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "OpenShell sandbox runtime"},
		DNSNames:     []string{serverName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caCert, leafPub, caPriv)
	if err != nil {
		return nil, fmt.Errorf("sign sandbox TLS server certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(leafPriv)
	if err != nil {
		return nil, fmt.Errorf("encode sandbox TLS server key: %w", err)
	}
	return &SandboxTLSMaterial{
		ServerName:          serverName,
		TrustAnchorPEM:      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})),
		CertificateChainPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})),
		PrivateKeyPEM:       string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
	}, nil
}

func randomSerial() *big.Int {
	limit := new(big.Int).Lsh(big.NewInt(1), 127)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		panic(err) // crypto/rand failing is not recoverable here
	}
	return n
}
