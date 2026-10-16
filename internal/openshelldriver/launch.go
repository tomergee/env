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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	env "github.com/agent-substrate/env/clients/go"
	"github.com/agent-substrate/env/internal/openshelldriver/third_party/openshell/gen/computev1"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
)

// Paths inside the actor. The layout follows OpenShell's Docker driver: the
// sandbox side under /.openshell/channel/sandbox, the supervisor's private
// inputs under /.openshell/supervisor, the workspace at /sandbox.
const (
	channelDir            = "/.openshell/channel/sandbox"
	bootstrapPath         = channelDir + "/bootstrap.json"
	controlSocketPath     = channelDir + "/control.sock"
	serverCertPath        = channelDir + "/server.crt"
	serverKeyPath         = channelDir + "/server.key"
	supervisorDir         = "/.openshell/supervisor"
	runtimeDescriptorPath = supervisorDir + "/runtime-descriptor.json"
	authBundlePath        = supervisorDir + "/auth.json"
	gatewayCAPath         = supervisorDir + "/gateway-ca.pem"
	runDir                = "/run/openshell"
	// The sandbox publishes the supervisor's interception CA to workload
	// processes from here; OpenShell's Docker driver mounts it as a tmpfs owned
	// by the workload identity (openshell_sandbox_backend::SUPERVISOR_CA_RUNTIME_DIR).
	supervisorCARoot = "/run/openshell-supervisor-ca"
	supervisorCADir  = supervisorCARoot + "/material"
	healthSocketPath = runDir + "/health.sock"
	sshSocketPath    = runDir + "/ssh.sock"
	proxyTLSDir      = runDir + "/proxy-tls"
	workspaceDir     = "/sandbox"
)

type launchResult struct {
	processes []string
	fence     OuterFenceGuarantees
	claims    map[string]string
}

type runResult struct {
	exitCode       int
	stdout, stderr string
}

// run starts argv in the environment through the guest, with the layer's
// busybox so nothing depends on the rootfs image, and waits for it.
func (d *Driver) run(ctx context.Context, e *env.Env, argv []string, environ map[string]string, timeout time.Duration) (*runResult, error) {
	req := &ateenvv1alpha.StartProcessRequest{Command: argv, Env: environ}
	if timeout > 0 {
		req.Timeout = durationpb.New(timeout)
	}
	proc, err := e.StartProcess(ctx, req)
	if err != nil {
		return nil, err
	}
	stream, err := proc.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{Follow: true})
	if err != nil {
		return nil, err
	}
	var out, errb bytes.Buffer
	var exit *ateenvv1alpha.Process
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		switch o := msg.GetOutput().(type) {
		case *ateenvv1alpha.ProcessOutput_Stdout:
			out.Write(o.Stdout)
		case *ateenvv1alpha.ProcessOutput_Stderr:
			errb.Write(o.Stderr)
		case *ateenvv1alpha.ProcessOutput_Exit:
			exit = o.Exit
		}
	}
	if exit == nil {
		return nil, errors.New("output stream ended before the process exited")
	}
	return &runResult{exitCode: int(exit.GetExitCode()), stdout: out.String(), stderr: errb.String()}, nil
}

// sh runs a shell command line with the layer's busybox and fails on a
// non-zero exit.
func (d *Driver) sh(ctx context.Context, e *env.Env, script string) error {
	res, err := d.run(ctx, e, []string{d.cfg.LayerRoot + "/bin/busybox", "sh", "-ec", script}, nil, 60*time.Second)
	if err != nil {
		return err
	}
	if res.exitCode != 0 {
		return fmt.Errorf("exit %d: %s%s", res.exitCode, res.stdout, res.stderr)
	}
	return nil
}

// start launches a long-lived process and returns its guest process id. After
// a short grace period it confirms the process is still running, so a binary
// that exits at once fails the launch with its output.
func (d *Driver) start(ctx context.Context, e *env.Env, argv []string, environ map[string]string) (string, error) {
	proc, err := e.StartProcess(ctx, &ateenvv1alpha.StartProcessRequest{Command: argv, Env: environ})
	if err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return proc.ID(), ctx.Err()
	case <-time.After(3 * time.Second):
	}
	info, err := e.FindProcess(ctx, proc.ID())
	if err != nil {
		return proc.ID(), fmt.Errorf("inspect %s: %w", argv[0], err)
	}
	if info.GetState() == ateenvv1alpha.ProcessState_PROCESS_STATE_EXITED {
		var tail bytes.Buffer
		if stream, err := proc.Output(ctx, &ateenvv1alpha.StreamProcessOutputRequest{}); err == nil {
			for {
				msg, err := stream.Recv()
				if err != nil {
					break
				}
				if o, ok := msg.GetOutput().(*ateenvv1alpha.ProcessOutput_Stderr); ok {
					tail.Write(o.Stderr)
				}
				if o, ok := msg.GetOutput().(*ateenvv1alpha.ProcessOutput_Stdout); ok {
					tail.Write(o.Stdout)
				}
			}
		}
		return proc.ID(), fmt.Errorf("%s exited %d at start: %s", argv[0], info.GetExitCode(), lastLines(tail.String(), 12, 1500))
	}
	return proc.ID(), nil
}

func (d *Driver) writeFile(ctx context.Context, e *env.Env, path string, data []byte, mode fs.FileMode) error {
	if err := e.WriteFile(ctx, path, bytes.NewReader(data), mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// launch stages the launch material in the environment and starts the two
// OpenShell binaries from the runtime layer.
func (d *Driver) launch(ctx context.Context, e *env.Env, rec *sandbox, spec *computev1.DriverSandboxSpec, auth *LaunchAuthentication, bundle *supervisorBundleFields) (*launchResult, error) {
	tls, err := GenerateSandboxTLSMaterial(bundle.SessionID)
	if err != nil {
		return nil, err
	}
	claims := map[string]string{
		"substrate.atespace":    d.cfg.Atespace,
		"substrate.environment": rec.envID,
		"substrate.template":    d.cfg.BaseTemplate,
	}
	// The outer fence is the actor: its only egress is the atenet egress
	// gateway, which forwards only what the actor's EgressPolicy allows and
	// nothing when the policy or the gateway is absent (fail closed).
	evidence, _ := json.Marshal(map[string]any{
		"platform":    "agent-substrate",
		"atespace":    d.cfg.Atespace,
		"environment": rec.envID,
		"egress":      "atenet-egress with per-actor EgressPolicy; no policy means no egress",
		"generation":  rec.generation,
	})
	fence, err := NewOuterFenceGuarantees(rec.generation, []string{
		GuaranteeDefaultDenyEgress, GuaranteeNoUnmanagedEgressPath,
		GuaranteeRevocationVerified, GuaranteeControllerLossFailsClosed,
	}, evidence)
	if err != nil {
		return nil, err
	}
	keys := make([]GatewayVerificationKey, 0, len(auth.VerificationKeys))
	for _, k := range auth.VerificationKeys {
		keys = append(keys, GatewayVerificationKey{KeyID: k.KeyID, PublicKeyPEM: string(k.PublicKeyPEM)})
	}
	childEnv := map[string]string{}
	for k, v := range spec.GetTemplate().GetEnvironment() {
		childEnv[k] = v
	}
	for k, v := range spec.GetEnvironment() {
		childEnv[k] = v
	}
	boundary := BoundaryConfig{
		BoundaryID: rec.id, Generation: rec.generation, SessionID: bundle.SessionID,
		SessionRotation: bundle.SessionRotation, AuthEpoch: bundle.AuthEpoch, GatewayID: auth.GatewayID,
		VerificationKeys: keys,
		Listener:         UnixListener{Kind: "unix", SocketPath: controlSocketPath, TLS: SandboxTLSServerConfig{CertificateChainPath: serverCertPath, PrivateKeyPath: serverKeyPath}},
		ResourceClaims:   claims, ResourceClaimFiles: map[string]string{},
		WorkloadIdentity: rec.identity, OuterFence: fence, ChildEnv: childEnv,
	}
	descriptor := SandboxRuntimeDescriptor{
		BoundaryID: rec.id, Generation: rec.generation, SessionID: bundle.SessionID,
		WorkloadIdentity: rec.identity,
		Transport:        UnixTransport{Kind: "unix", SocketPath: controlSocketPath},
		TLS:              SandboxTLSClientConfig{ServerName: tls.ServerName, TrustAnchorPEM: tls.TrustAnchorPEM},
		ResourceClaims:   claims, OuterFence: fence,
	}
	boundaryJSON, err := json.Marshal(boundary)
	if err != nil {
		return nil, err
	}
	descriptorJSON, err := json.Marshal(descriptor)
	if err != nil {
		return nil, err
	}
	mainSpec, err := json.Marshal(NewMainProcessConfig(spec.GetCommand(), spec.GetTty(), spec.GetAwaitMainProcessAttachment()))
	if err != nil {
		return nil, err
	}

	uid, gid := rec.identity.UID, rec.identity.GID
	// Directories first, owned so the sandbox (uid) can write its socket and
	// the supervisor keeps its inputs to itself.
	// The sandbox's DNS relay binds 127.0.0.53:53 after it has dropped every
	// capability, so the actor's network namespace must allow unprivileged
	// binds below 1024; OpenShell's Docker and Kubernetes drivers set the same
	// sysctl on the workload. The actor runs as root and /proc/sys is
	// writable, so the guest can set it directly.
	// Name resolution: OpenShell's drivers point the workload at the sandbox's
	// DNS relay (127.0.0.53) and nothing else, while the supervisor takes its
	// trusted upstream from the first literal nameserver in /etc/resolv.conf.
	// Both read the same file here (the actor has no CAP_SYS_ADMIN for a
	// private mount namespace), so the file lists the actor's resolver first
	// and the relay second: the supervisor uses the first; the workload's
	// first query is refused at once by the broker and glibc falls through
	// to the relay. The search list goes: with ndots:5 every lookup would
	// first reach the relay as cluster-suffixed names and be staged for
	// policy review.
	if err := d.sh(ctx, e, fmt.Sprintf(`B=%s/bin/busybox
echo 0 > /proc/sys/net/ipv4/ip_unprivileged_port_start
if ! $B grep -q '^nameserver 127\.0\.0\.53' /etc/resolv.conf; then
  up=$($B awk '/^nameserver/ { print $2; exit }' /etc/resolv.conf)
  [ -n "$up" ] || { echo "no nameserver in /etc/resolv.conf" >&2; exit 1; }
  $B printf 'nameserver %%s\nnameserver 127.0.0.53\noptions timeout:2 attempts:2\n' "$up" > /etc/resolv.conf
fi
$B mkdir -p %s %s %s %s %s %s
$B rm -f %s %s %s
$B chmod 0700 %s
$B chmod 0755 %s %s
$B chown %d:%d %s %s %s`, d.cfg.LayerRoot, channelDir, supervisorDir, runDir, proxyTLSDir, workspaceDir, supervisorCADir,
		controlSocketPath, healthSocketPath, sshSocketPath,
		supervisorDir, supervisorCARoot, supervisorCADir, uid, gid, channelDir, workspaceDir, supervisorCADir)); err != nil {
		return nil, fmt.Errorf("prepare the actor: %w", err)
	}
	files := []struct {
		path string
		data []byte
		mode fs.FileMode
	}{
		{bootstrapPath, boundaryJSON, 0o640},
		{serverCertPath, []byte(tls.CertificateChainPEM), 0o644},
		{serverKeyPath, []byte(tls.PrivateKeyPEM), 0o600},
		{runtimeDescriptorPath, descriptorJSON, 0o600},
		{authBundlePath, auth.Supervisor, 0o600},
	}
	if d.cfg.GatewayTLSCAPEM != "" {
		files = append(files, struct {
			path string
			data []byte
			mode fs.FileMode
		}{gatewayCAPath, []byte(d.cfg.GatewayTLSCAPEM), 0o644})
	}
	for _, f := range files {
		if err := d.writeFile(ctx, e, f.path, f.data, f.mode); err != nil {
			return nil, err
		}
	}
	if err := d.sh(ctx, e, fmt.Sprintf(`%s/bin/busybox chown %d:%d %s %s %s`, d.cfg.LayerRoot, uid, gid, bootstrapPath, serverCertPath, serverKeyPath)); err != nil {
		return nil, fmt.Errorf("hand the boundary files to uid %d: %w", uid, err)
	}

	common := map[string]string{
		"OPENSHELL_LOG_LEVEL":         d.cfg.LogLevel,
		"OPENSHELL_TELEMETRY_ENABLED": "false",
	}
	// openshell-sandbox starts as root through its capability-free launcher:
	// it clears the bounding set, switches to uid:gid with no capabilities,
	// sets no_new_privs and then runs the boundary. The actor's template must
	// grant CAP_SETPCAP, CAP_SETUID, CAP_SETGID and CAP_CHOWN for this.
	sandboxPID, err := d.start(ctx, e, []string{
		d.cfg.LayerRoot + "/bin/openshell-sandbox", "launch-capability-free",
		fmt.Sprint(uid), fmt.Sprint(gid), bootstrapPath, workspaceDir,
	}, common)
	if err != nil {
		return nil, fmt.Errorf("start openshell-sandbox: %w", err)
	}
	supEnv := map[string]string{
		"OPENSHELL_ADMITTED_ISOLATION_BACKEND":   BackendName,
		"OPENSHELL_MAIN_PROCESS_SPEC":            string(mainSpec),
		"OPENSHELL_ENDPOINT":                     d.cfg.GatewayEndpoint,
		"OPENSHELL_SANDBOX_ID":                   rec.id,
		"OPENSHELL_SANDBOX":                      rec.name,
		"OPENSHELL_SSH_SOCKET_PATH":              sshSocketPath,
		"OPENSHELL_PROXY_TLS_DIR":                proxyTLSDir,
		"OPENSHELL_NETWORK_RUNTIME_CAPABILITIES": "",
	}
	for k, v := range common {
		supEnv[k] = v
	}
	if d.cfg.GatewayTLSCAPEM != "" {
		supEnv["OPENSHELL_TLS_CA"] = gatewayCAPath
	}
	supervisorPID, err := d.start(ctx, e, []string{
		d.cfg.LayerRoot + "/bin/supervisor",
		"--backend-descriptor-file", runtimeDescriptorPath,
		"--auth-bundle-file", authBundlePath,
		"--workdir", workspaceDir,
		"--health-socket-path=" + healthSocketPath,
	}, supEnv)
	if err != nil {
		return nil, fmt.Errorf("start openshell-supervisor: %w", err)
	}
	return &launchResult{processes: []string{sandboxPID, supervisorPID}, fence: fence, claims: claims}, nil
}

// lastLines keeps the last n lines of s, at most limit bytes, with control
// characters other than newline replaced, so the text fits a gRPC status
// message and a log line.
func lastLines(s string, n, limit int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > limit {
		out = "..." + out[len(out)-limit:]
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f) {
			return r
		}
		return '?'
	}, out)
}
