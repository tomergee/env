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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	env "github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/internal/openshelldriver/third_party/openshell/gen/computev1"
	"github.com/agent-substrate/env/internal/openshelldriver/third_party/openshell/gen/extensionv1"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
)

// Protocol constants from openshell_core::extension_protocol.
const (
	protocolMajor                  = 1
	protocolMinor                  = 0
	computeContractCapability      = "openshell.compute.contract"
	launchAuthenticationCapability = "openshell.compute.launch-authentication"
	implementationName             = "agent-substrate/ate-env"
)

// Config configures the driver.
type Config struct {
	// ATEEnvAPI is the ate-env-api address, host:port.
	ATEEnvAPI string
	// Atespace holds every environment the driver creates (shared workspace
	// mode: OpenShell workspaces are recorded, not mapped to atespaces).
	Atespace string
	// BaseTemplate is the ActorTemplate that carries the OpenShell runtime
	// layer; environments for other images are derived from it by ate-env-api.
	BaseTemplate string
	// DefaultImage is reported to the gateway and used when a sandbox names
	// no image. Empty means "the base template's own image".
	DefaultImage string
	// LayerRoot is where the runtime layer is mounted inside the actor.
	LayerRoot string
	// GatewayEndpoint is the gateway URL the supervisor dials from inside
	// the actor, with its scheme: http://host:port for a plaintext gateway,
	// https://host:port with GatewayTLSCAPEM for a TLS one.
	GatewayEndpoint string
	// GatewayTLSCAPEM, when set, is written into the actor and passed to the
	// supervisor as OPENSHELL_TLS_CA. Empty means a plaintext gateway.
	GatewayTLSCAPEM string
	// WorkloadUID and WorkloadGID are the non-root identity the sandbox and
	// the agent run as when the sandbox spec names none.
	WorkloadUID uint32
	WorkloadGID uint32
	// LogLevel for the two OpenShell binaries.
	LogLevel string
	// Version is reported as the implementation version.
	Version string
	// AllowDriverConfig and ResourceAdmissionEnabled must match the gateway's
	// [openshell.drivers.<name>] table; the gateway refuses a driver whose
	// acknowledgement differs.
	AllowDriverConfig        bool
	ResourceAdmissionEnabled bool
	// ReadyTimeout bounds waiting for a new environment's guest to answer.
	ReadyTimeout time.Duration
	// KeepFailed leaves the environment of a failed launch in place when the
	// gateway deletes the sandbox, so its process logs can be read. For
	// debugging only; the environment must then be deleted by hand.
	KeepFailed bool
	// Logger receives the driver's log lines.
	Logger *slog.Logger
}

type phase string

const (
	phaseCreating phase = "creating"
	phaseReady    phase = "ready"
	phaseStopped  phase = "stopped"
	phaseFailed   phase = "failed"
)

// sandbox is the driver's record of one OpenShell sandbox.
type sandbox struct {
	id, name, namespace, workspace string
	envID                          string
	image                          string
	generation                     string
	identity                       ResolvedWorkloadIdentity
	fence                          *computev1.DriverFenceEvidence
	phase                          phase
	reason, message                string
	created                        time.Time
	processes                      []string // guest process ids: sandbox, supervisor
	spec                           *computev1.DriverSandboxSpec
}

// Driver implements openshell.compute.v1.ComputeDriver on ate-env.
type Driver struct {
	computev1.UnimplementedComputeDriverServer

	cfg    Config
	client *env.Client
	log    *slog.Logger

	mu         sync.Mutex
	sandboxes  map[string]*sandbox
	workspaces map[string]bool
	subs       map[chan *computev1.WatchSandboxesEvent]struct{}
}

// New connects to ate-env-api and returns a driver.
func New(cfg Config) (*Driver, error) {
	if cfg.ATEEnvAPI == "" || cfg.Atespace == "" || cfg.BaseTemplate == "" || cfg.GatewayEndpoint == "" {
		return nil, errors.New("openshelldriver: ATEEnvAPI, Atespace, BaseTemplate and GatewayEndpoint are required")
	}
	if cfg.LayerRoot == "" {
		cfg.LayerRoot = "/opt/openshell"
	}
	if !strings.HasPrefix(cfg.GatewayEndpoint, "http://") && !strings.HasPrefix(cfg.GatewayEndpoint, "https://") {
		return nil, fmt.Errorf("openshelldriver: GatewayEndpoint %q must be a URL with an http or https scheme; the supervisor speaks TLS to anything else", cfg.GatewayEndpoint)
	}
	if strings.HasPrefix(cfg.GatewayEndpoint, "https://") && cfg.GatewayTLSCAPEM == "" {
		return nil, errors.New("openshelldriver: an https GatewayEndpoint needs GatewayTLSCAPEM")
	}
	if cfg.WorkloadUID == 0 || cfg.WorkloadGID == 0 {
		cfg.WorkloadUID, cfg.WorkloadGID = 65534, 65534
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 3 * time.Minute
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	client, err := env.NewClient(env.ClientOptions{Endpoint: cfg.ATEEnvAPI})
	if err != nil {
		return nil, err
	}
	return &Driver{
		cfg:        cfg,
		client:     client,
		log:        cfg.Logger,
		sandboxes:  map[string]*sandbox{},
		workspaces: map[string]bool{},
		subs:       map[chan *computev1.WatchSandboxesEvent]struct{}{},
	}, nil
}

// Close releases the ate-env connection.
func (d *Driver) Close() error { return d.client.Close() }

// Register adds the driver to a gRPC server.
func (d *Driver) Register(s *grpc.Server) { computev1.RegisterComputeDriverServer(s, d) }

// --- capabilities and negotiation -------------------------------------------

// admissionConfig mirrors openshell_core::resource_admission::DriverAdmissionConfig;
// the gateway parses the JSON back and compares it structurally.
type admissionConfig struct {
	AllowDriverConfig bool `json:"allow_driver_config"`
	ResourceAdmission struct {
		Enabled        bool              `json:"enabled"`
		RequiredLabels map[string]string `json:"required_labels"`
	} `json:"resource_admission"`
}

func (d *Driver) admissionAcknowledgement() string {
	var a admissionConfig
	a.AllowDriverConfig = d.cfg.AllowDriverConfig
	a.ResourceAdmission.Enabled = d.cfg.ResourceAdmissionEnabled
	a.ResourceAdmission.RequiredLabels = map[string]string{
		"openshell.ai/sandbox-attachable":           "true",
		"openshell.ai/sandbox-attachable-workspace": "${workspace}",
	}
	b, _ := json.Marshal(a)
	return "v1:" + string(b)
}

func (d *Driver) peerMetadata() *extensionv1.PeerMetadata {
	return &extensionv1.PeerMetadata{
		ProtocolVersion:       &extensionv1.ProtocolVersion{Major: protocolMajor, Minor: protocolMinor},
		ImplementationName:    implementationName,
		ImplementationVersion: d.cfg.Version,
		SupportedCapabilities: []string{computeContractCapability, launchAuthenticationCapability},
		// Requiring launch authentication makes the gateway fail at startup
		// when it has no signing keys, instead of at the first create.
		RequiredCapabilities: []string{computeContractCapability, launchAuthenticationCapability},
	}
}

func checkGatewayMetadata(m *extensionv1.PeerMetadata) error {
	if m == nil || m.GetProtocolVersion() == nil {
		return status.Error(codes.FailedPrecondition, "gateway did not provide extension protocol metadata")
	}
	if m.GetProtocolVersion().GetMajor() != protocolMajor {
		return status.Errorf(codes.FailedPrecondition, "gateway speaks compute protocol %d.%d; this driver speaks %d.%d",
			m.GetProtocolVersion().GetMajor(), m.GetProtocolVersion().GetMinor(), protocolMajor, protocolMinor)
	}
	for _, want := range []string{computeContractCapability, launchAuthenticationCapability} {
		found := false
		for _, have := range m.GetSupportedCapabilities() {
			if have == want {
				found = true
			}
		}
		if !found {
			return status.Errorf(codes.FailedPrecondition, "gateway lacks capability %q", want)
		}
	}
	return nil
}

// GetCapabilities implements ComputeDriver.
func (d *Driver) GetCapabilities(_ context.Context, req *computev1.GetCapabilitiesRequest) (*computev1.GetCapabilitiesResponse, error) {
	if err := checkGatewayMetadata(req.GetGateway()); err != nil {
		return nil, err
	}
	return &computev1.GetCapabilitiesResponse{
		DriverName:    "substrate",
		DriverVersion: d.cfg.Version,
		DefaultImage:  d.cfg.DefaultImage,
		// Actors outlive the gateway process; nothing to stop on its shutdown.
		GatewayManagesLifecycle:       false,
		SupportsSandboxAuthentication: false,
		DriverReportsRuntimeReadiness: false,
		ResourceCapabilities: &computev1.ResourceCapabilities{
			Cpu:    &computev1.CpuResourceCapabilities{LimitSupported: false},
			Memory: &computev1.MemoryResourceCapabilities{LimitSupported: false},
			Gpu:    &computev1.GpuResourceCapabilities{},
		},
		RootfsTarStagingDir:     "",
		RootfsTarMaxBytes:       0,
		Extension:               d.peerMetadata(),
		ResourceAdmissionPolicy: d.admissionAcknowledgement(),
	}, nil
}

// --- workspaces --------------------------------------------------------------

// EnsureWorkspace implements ComputeDriver. Workspaces share one atespace.
func (d *Driver) EnsureWorkspace(_ context.Context, req *computev1.EnsureWorkspaceRequest) (*computev1.EnsureWorkspaceResponse, error) {
	if req.GetWorkspace() == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace is required")
	}
	d.mu.Lock()
	d.workspaces[req.GetWorkspace()] = true
	d.mu.Unlock()
	return &computev1.EnsureWorkspaceResponse{}, nil
}

// DeleteWorkspace implements ComputeDriver.
func (d *Driver) DeleteWorkspace(_ context.Context, req *computev1.DeleteWorkspaceRequest) (*computev1.DeleteWorkspaceResponse, error) {
	d.mu.Lock()
	delete(d.workspaces, req.GetWorkspace())
	d.mu.Unlock()
	return &computev1.DeleteWorkspaceResponse{}, nil
}

// --- validation and naming ----------------------------------------------------

var digestPinned = regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)

func (d *Driver) imageFor(spec *computev1.DriverSandboxSpec) (string, error) {
	image := spec.GetTemplate().GetImage()
	if image == "" {
		image = d.cfg.DefaultImage
	}
	if image == "" {
		return "", nil // the base template's own image
	}
	if !digestPinned.MatchString(image) {
		return "", status.Errorf(codes.InvalidArgument, "image %q must be pinned by digest (repo@sha256:...): ate-env derives a template per image and does not resolve tags", image)
	}
	return image, nil
}

// envIDFor derives the environment id from the gateway's stable sandbox id:
// a DNS label, deterministic so a restarted driver finds its environments.
func envIDFor(sandboxID string) string {
	sum := sha256.Sum256([]byte(sandboxID))
	return "os-" + hex.EncodeToString(sum[:])[:20]
}

func (d *Driver) identityFor(spec *computev1.DriverSandboxSpec, image string) (ResolvedWorkloadIdentity, error) {
	id := ResolvedWorkloadIdentity{UID: d.cfg.WorkloadUID, GID: d.cfg.WorkloadGID, SupplementaryGIDs: []uint32{}, Source: "template"}
	if u := spec.GetWorkloadIdentity().GetUser(); u != "" {
		n, err := strconv.ParseUint(u, 10, 32)
		if err != nil {
			return id, status.Errorf(codes.InvalidArgument, "workload identity user %q: only numeric ids are supported", u)
		}
		id.UID, id.Source = uint32(n), "policy"
	}
	if g := spec.GetWorkloadIdentity().GetGroup(); g != "" {
		n, err := strconv.ParseUint(g, 10, 32)
		if err != nil {
			return id, status.Errorf(codes.InvalidArgument, "workload identity group %q: only numeric ids are supported", g)
		}
		id.GID, id.Source = uint32(n), "policy"
	}
	digestSource := image
	if digestSource == "" {
		digestSource = "template:" + d.cfg.BaseTemplate
	}
	sum := sha256.Sum256([]byte(digestSource))
	id.ResourceDigest = hex.EncodeToString(sum[:])
	if err := id.Validate(); err != nil {
		return id, status.Error(codes.InvalidArgument, err.Error())
	}
	return id, nil
}

// ValidateSandboxCreate implements ComputeDriver.
func (d *Driver) ValidateSandboxCreate(_ context.Context, req *computev1.ValidateSandboxCreateRequest) (*computev1.ValidateSandboxCreateResponse, error) {
	sb := req.GetSandbox()
	if sb.GetId() == "" || sb.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox id and name are required")
	}
	image, err := d.imageFor(sb.GetSpec())
	if err != nil {
		return nil, err
	}
	if _, err := d.identityFor(sb.GetSpec(), image); err != nil {
		return nil, err
	}
	if sb.GetSpec().GetTemplate().GetDriverConfig() != nil && len(sb.GetSpec().GetTemplate().GetDriverConfig().GetFields()) > 0 && !d.cfg.AllowDriverConfig {
		return nil, status.Error(codes.InvalidArgument, "driver config is not enabled for this driver")
	}
	return &computev1.ValidateSandboxCreateResponse{}, nil
}

// --- lifecycle ----------------------------------------------------------------

// CreateSandbox implements ComputeDriver: one ate-env environment per sandbox,
// with the OpenShell binaries started inside it from the runtime layer.
func (d *Driver) CreateSandbox(ctx context.Context, req *computev1.CreateSandboxRequest) (*computev1.CreateSandboxResponse, error) {
	sb := req.GetSandbox()
	if _, err := d.ValidateSandboxCreate(ctx, &computev1.ValidateSandboxCreateRequest{Sandbox: sb}); err != nil {
		return nil, err
	}
	spec := sb.GetSpec()
	auth, bundle, err := DecodeLaunchAuthentication(spec.GetLaunchAuthentication())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	image, _ := d.imageFor(spec)
	identity, err := d.identityFor(spec, image)
	if err != nil {
		return nil, err
	}
	rec := &sandbox{
		id: sb.GetId(), name: sb.GetName(), namespace: sb.GetNamespace(), workspace: sb.GetWorkspace(),
		envID: envIDFor(sb.GetId()), image: image, generation: bundle.RuntimeGeneration,
		identity: identity, phase: phaseCreating, reason: "Starting", message: "creating environment",
		created: time.Now(), spec: spec,
	}
	d.mu.Lock()
	if existing, ok := d.sandboxes[rec.id]; ok && existing.phase != phaseFailed {
		d.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "sandbox %s exists as environment %s", rec.id, existing.envID)
	}
	d.sandboxes[rec.id] = rec
	d.mu.Unlock()
	d.notify(rec)

	log := d.log.With("sandbox", rec.id, "name", rec.name, "environment", rec.envID)
	fail := func(reason string, err error) (*computev1.CreateSandboxResponse, error) {
		log.Error("create failed", "reason", reason, "err", err)
		msg := lastLines(err.Error(), 12, 1500)
		d.setPhase(rec, phaseFailed, reason, msg)
		return nil, status.Errorf(codes.Internal, "%s: %s", reason, msg)
	}

	e, err := d.createEnvironment(ctx, rec)
	if err != nil {
		return fail("EnvironmentCreateFailed", err)
	}
	log.Info("environment created", "image", image)
	if err := d.waitGuest(ctx, e); err != nil {
		return fail("GuestNotReady", err)
	}
	launch, err := d.launch(ctx, e, rec, spec, auth, bundle)
	if err != nil {
		return fail("LaunchFailed", err)
	}
	rec.processes = launch.processes
	rec.fence = &computev1.DriverFenceEvidence{
		Generation:     rec.generation,
		EvidenceDigest: launch.fence.EvidenceDigest,
		ResourceClaims: launch.claims,
	}
	d.setPhase(rec, phaseReady, "BackendReady", "openshell-sandbox and openshell-supervisor started")
	log.Info("sandbox launched", "processes", rec.processes)
	return &computev1.CreateSandboxResponse{RuntimeIdentity: rec.envID}, nil
}

func (d *Driver) createEnvironment(ctx context.Context, rec *sandbox) (*env.Env, error) {
	req := &ateenvv1alpha.CreateEnvironmentRequest{
		Id:       rec.envID,
		Atespace: d.cfg.Atespace,
		Template: &ateenvv1alpha.Template{Name: d.cfg.BaseTemplate, Atespace: d.cfg.Atespace},
	}
	if rec.image != "" {
		req.RootfsImage = rec.image
	}
	e, err := d.client.Create(ctx, req)
	if err != nil {
		if status.Code(errors.Unwrap(err)) == codes.AlreadyExists || status.Code(err) == codes.AlreadyExists {
			d.log.Warn("environment exists, reusing", "environment", rec.envID)
			return d.client.Env(d.cfg.Atespace, rec.envID), nil
		}
		return nil, err
	}
	return e, nil
}

// waitGuest retries a trivial command until the guest answers. ate-env-api
// already waits for a waking actor; this covers the first boot.
func (d *Driver) waitGuest(ctx context.Context, e *env.Env) error {
	deadline := time.Now().Add(d.cfg.ReadyTimeout)
	var last error
	for time.Now().Before(deadline) {
		out, err := d.run(ctx, e, []string{d.cfg.LayerRoot + "/bin/busybox", "true"}, nil, 30*time.Second)
		if err == nil && out.exitCode == 0 {
			return nil
		}
		if err != nil {
			last = err
		} else {
			last = fmt.Errorf("busybox true exited %d: %s", out.exitCode, out.stderr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("guest did not answer within %s: %v", d.cfg.ReadyTimeout, last)
}

// GetSandbox implements ComputeDriver.
func (d *Driver) GetSandbox(ctx context.Context, req *computev1.GetSandboxRequest) (*computev1.GetSandboxResponse, error) {
	rec := d.find(req.GetSandboxId(), req.GetName())
	if rec == nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %q/%q not found", req.GetSandboxId(), req.GetName())
	}
	return &computev1.GetSandboxResponse{Sandbox: d.observe(ctx, rec)}, nil
}

// ListSandboxes implements ComputeDriver.
func (d *Driver) ListSandboxes(ctx context.Context, _ *computev1.ListSandboxesRequest) (*computev1.ListSandboxesResponse, error) {
	resp := &computev1.ListSandboxesResponse{}
	for _, rec := range d.snapshot() {
		resp.Sandboxes = append(resp.Sandboxes, d.observe(ctx, rec))
	}
	return resp, nil
}

// StopSandbox implements ComputeDriver: suspend the environment.
func (d *Driver) StopSandbox(ctx context.Context, req *computev1.StopSandboxRequest) (*computev1.StopSandboxResponse, error) {
	rec := d.find(req.GetSandboxId(), req.GetName())
	if rec == nil {
		return &computev1.StopSandboxResponse{}, nil
	}
	d.mu.Lock()
	stopped := rec.phase == phaseStopped
	d.mu.Unlock()
	if stopped {
		return &computev1.StopSandboxResponse{}, nil
	}
	// A stopped sandbox has no running processes, as with OpenShell's own
	// drivers: the gateway rotates the supervisor's credentials on the next
	// start, so the running supervisor could not continue anyway. End both
	// processes (the guest signals their process groups, which takes the
	// workload with them), then let Substrate snapshot the idle actor.
	e := d.client.Env(d.cfg.Atespace, rec.envID)
	d.killProcesses(ctx, e, rec)
	if err := d.client.Suspend(ctx, d.cfg.Atespace, rec.envID); err != nil && !errors.Is(err, env.ErrNotFound) {
		if status.Code(err) != codes.FailedPrecondition { // already suspended
			return nil, status.Errorf(codes.Internal, "suspend %s: %v", rec.envID, err)
		}
	}
	d.setPhase(rec, phaseStopped, "ContainerStopped", "environment suspended on request")
	return &computev1.StopSandboxResponse{}, nil
}

// StartSandbox brings a stopped sandbox back: Substrate resumes the actor on
// the first guest call, then the sandbox and supervisor start again from the
// launch authentication minted for this start.
func (d *Driver) StartSandbox(ctx context.Context, req *computev1.StartSandboxRequest) (*computev1.StartSandboxResponse, error) {
	rec := d.find(req.GetSandboxId(), req.GetName())
	if rec == nil {
		return nil, status.Errorf(codes.NotFound, "sandbox %q/%q not found", req.GetSandboxId(), req.GetName())
	}
	if want := req.GetExpectedRuntimeIdentity(); want != "" && want != rec.envID {
		return nil, status.Errorf(codes.FailedPrecondition, "sandbox %s is environment %s, not %s", rec.id, rec.envID, want)
	}
	auth, bundle, err := DecodeLaunchAuthentication(req.GetLaunchAuthentication())
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition, "start %s: %v", rec.id, err)
	}
	log := d.log.With("sandbox", rec.id, "name", rec.name, "environment", rec.envID)
	if gen := req.GetGenerationId(); gen != "" && gen != bundle.RuntimeGeneration {
		log.Warn("start generation differs from the supervisor bundle", "generation_id", gen, "bundle", bundle.RuntimeGeneration)
	}
	d.setPhase(rec, phaseCreating, "Starting", "environment resuming")
	fail := func(reason string, err error) (*computev1.StartSandboxResponse, error) {
		log.Error("start failed", "reason", reason, "err", err)
		msg := lastLines(err.Error(), 12, 1500)
		d.setPhase(rec, phaseFailed, reason, msg)
		return nil, status.Errorf(codes.Internal, "%s: %s", reason, msg)
	}
	e := d.client.Env(d.cfg.Atespace, rec.envID)
	if err := d.waitGuest(ctx, e); err != nil {
		return fail("GuestNotReady", err)
	}
	d.killProcesses(ctx, e, rec) // leftovers from a start that failed half-way
	d.mu.Lock()
	rec.generation = bundle.RuntimeGeneration
	spec := rec.spec
	d.mu.Unlock()
	launch, err := d.launch(ctx, e, rec, spec, auth, bundle)
	if err != nil {
		return fail("LaunchFailed", err)
	}
	d.mu.Lock()
	rec.processes = launch.processes
	rec.fence = &computev1.DriverFenceEvidence{
		Generation:     rec.generation,
		EvidenceDigest: launch.fence.EvidenceDigest,
		ResourceClaims: launch.claims,
	}
	d.mu.Unlock()
	d.setPhase(rec, phaseReady, "BackendReady", "openshell-sandbox and openshell-supervisor started")
	log.Info("sandbox relaunched", "processes", rec.processes)
	return &computev1.StartSandboxResponse{RuntimeIdentity: rec.envID}, nil
}

// killProcesses ends the guest processes recorded for the sandbox, supervisor
// first. Processes that already exited are not an error.
func (d *Driver) killProcesses(ctx context.Context, e *env.Env, rec *sandbox) {
	d.mu.Lock()
	ids := append([]string(nil), rec.processes...)
	rec.processes = nil
	d.mu.Unlock()
	for i := len(ids) - 1; i >= 0; i-- {
		if _, err := e.Process(ids[i]).Kill(ctx); err != nil && !errors.Is(err, env.ErrNotFound) {
			d.log.Warn("kill guest process", "environment", rec.envID, "process", ids[i], "err", err)
		}
	}
}

func (d *Driver) DeleteSandbox(ctx context.Context, req *computev1.DeleteSandboxRequest) (*computev1.DeleteSandboxResponse, error) {
	rec := d.find(req.GetSandboxId(), req.GetName())
	if rec == nil {
		return &computev1.DeleteSandboxResponse{Deleted: false}, nil
	}
	d.mu.Lock()
	keep := d.cfg.KeepFailed && rec.phase == phaseFailed
	d.mu.Unlock()
	if keep {
		d.log.Warn("keeping the environment of a failed launch for inspection", "environment", rec.envID)
	} else if err := d.client.Delete(ctx, d.cfg.Atespace, rec.envID); err != nil && !errors.Is(err, env.ErrNotFound) {
		return nil, status.Errorf(codes.Internal, "delete %s: %v", rec.envID, err)
	}
	d.mu.Lock()
	delete(d.sandboxes, rec.id)
	d.mu.Unlock()
	d.broadcast(&computev1.WatchSandboxesEvent{Payload: &computev1.WatchSandboxesEvent_Deleted{
		Deleted: &computev1.WatchSandboxesDeletedEvent{SandboxId: rec.id},
	}})
	return &computev1.DeleteSandboxResponse{Deleted: true}, nil
}

// WatchSandboxes implements ComputeDriver: a snapshot, then every change the
// driver makes, plus a periodic refresh of what ate-env reports.
func (d *Driver) WatchSandboxes(_ *computev1.WatchSandboxesRequest, stream grpc.ServerStreamingServer[computev1.WatchSandboxesEvent]) error {
	ctx := stream.Context()
	ch := make(chan *computev1.WatchSandboxesEvent, 64)
	d.mu.Lock()
	d.subs[ch] = struct{}{}
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.subs, ch)
		d.mu.Unlock()
	}()
	send := func(rec *sandbox) error {
		return stream.Send(&computev1.WatchSandboxesEvent{Payload: &computev1.WatchSandboxesEvent_Sandbox{
			Sandbox: &computev1.WatchSandboxesSandboxEvent{Sandbox: d.observe(ctx, rec)},
		}})
	}
	for _, rec := range d.snapshot() {
		if err := send(rec); err != nil {
			return err
		}
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case ev := <-ch:
			if err := stream.Send(ev); err != nil {
				return err
			}
		case <-ticker.C:
			for _, rec := range d.snapshot() {
				if err := send(rec); err != nil {
					return err
				}
			}
		}
	}
}

// --- records and observation ----------------------------------------------------

func (d *Driver) find(id, name string) *sandbox {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id != "" {
		if rec, ok := d.sandboxes[id]; ok {
			return rec
		}
	}
	if name != "" {
		for _, rec := range d.sandboxes {
			if rec.name == name {
				return rec
			}
		}
	}
	return nil
}

func (d *Driver) snapshot() []*sandbox {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*sandbox, 0, len(d.sandboxes))
	for _, rec := range d.sandboxes {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].created.Before(out[j].created) })
	return out
}

func (d *Driver) setPhase(rec *sandbox, p phase, reason, message string) {
	d.mu.Lock()
	rec.phase, rec.reason, rec.message = p, reason, message
	d.mu.Unlock()
	d.notify(rec)
}

func (d *Driver) notify(rec *sandbox) {
	d.broadcast(&computev1.WatchSandboxesEvent{Payload: &computev1.WatchSandboxesEvent_Sandbox{
		Sandbox: &computev1.WatchSandboxesSandboxEvent{Sandbox: d.observe(context.Background(), rec)},
	}})
}

func (d *Driver) broadcast(ev *computev1.WatchSandboxesEvent) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for ch := range d.subs {
		select {
		case ch <- ev:
		default: // a slow watcher gets the next periodic refresh instead
		}
	}
}

// observe builds the gateway's view of a sandbox from the record and from
// what ate-env currently reports for its environment.
func (d *Driver) observe(ctx context.Context, rec *sandbox) *computev1.DriverSandbox {
	d.mu.Lock()
	p, reason, message := rec.phase, rec.reason, rec.message
	d.mu.Unlock()
	ready, deleting, suspending := "False", false, false
	switch p {
	case phaseReady:
		ready = "True"
	case phaseFailed, phaseCreating, phaseStopped:
	}
	if p == phaseReady || p == phaseStopped {
		getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		info, err := d.client.Get(getCtx, d.cfg.Atespace, rec.envID)
		cancel()
		switch {
		case errors.Is(err, env.ErrNotFound):
			ready, reason, message = "False", "EnvironmentMissing", "environment no longer exists in ate-env"
		case err != nil:
			// keep the recorded state; the API is unreachable right now
		default:
			switch info.GetStatus() {
			case ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING:
				if p == phaseReady {
					reason, message = "BackendReady", "environment running"
				}
			case ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED:
				if p == phaseReady {
					reason, message = "BackendReady", "environment suspended; Substrate resumes it on the next request"
				}
			case ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDING:
				if p == phaseReady {
					reason, message = "BackendReady", "environment suspending; Substrate resumes it on the next request"
				}
				suspending = true
			case ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RESUMING:
				reason, message = "Starting", "environment resuming"
			case ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_CRASHED:
				ready, reason, message = "False", "Failed", "actor crashed"
			case ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_DELETING:
				deleting = true
			}
		}
	}
	conditions := []*computev1.DriverCondition{{
		Type: "Ready", Status: ready, Reason: reason, Message: message, TransitionTime: timestamppb.Now(),
	}}
	// The gateway derives Stopped from a Suspended=True condition (or a Ready
	// reason of ContainerStopped) and treats other Ready=False reasons as a
	// failure, so a requested stop is reported in those words.
	if p == phaseStopped {
		if suspending {
			conditions = append(conditions, &computev1.DriverCondition{
				Type: "Suspended", Status: "False", Reason: "PodTerminating",
				Message: "environment is suspending", TransitionTime: timestamppb.Now(),
			})
		} else {
			conditions[0].Reason, conditions[0].Message = "ContainerStopped", "environment suspended on request"
			conditions = append(conditions, &computev1.DriverCondition{
				Type: "Suspended", Status: "True", Reason: "EnvironmentSuspended",
				Message: "environment suspended on request", TransitionTime: timestamppb.Now(),
			})
		}
	}
	return &computev1.DriverSandbox{
		Id:        rec.id,
		Name:      rec.name,
		Namespace: rec.namespace,
		Workspace: rec.workspace,
		Status: &computev1.DriverSandboxStatus{
			Name:       rec.envID,
			InstanceId: rec.envID,
			Conditions: conditions,
			Deleting:   deleting,
			ResolvedIdentity: &computev1.ResolvedWorkloadIdentity{
				Uid: rec.identity.UID, Gid: rec.identity.GID, SupplementaryGids: rec.identity.SupplementaryGIDs,
				Source: rec.identity.Source, ResourceDigest: rec.identity.ResourceDigest,
			},
			FenceEvidence: rec.fence,
		},
	}
}

// String renders a short description for logs.
func (rec *sandbox) String() string {
	return strings.Join([]string{rec.id, rec.name, rec.envID, string(rec.phase)}, " ")
}
