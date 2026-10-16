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

// Command openshell-driver-substrate is an NVIDIA OpenShell extension compute
// driver that runs sandboxes as ate-env environments on Agent Substrate. The
// gateway dials it over a Unix socket ([openshell.drivers.<name>] socket_path).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/agent-substrate/env/internal/openshelldriver"
)

var version = "dev"

func main() {
	var (
		socket    = flag.String("socket", envOr("OPENSHELL_COMPUTE_DRIVER_SOCKET", "/run/openshell/substrate.sock"), "Unix socket the gateway dials")
		ateEnvAPI = flag.String("ate-env-api", envOr("ATE_ENV_API_TARGET", "ate-env-api.ate-env.svc.cluster.local:7777"), "ate-env-api address")
		atespace  = flag.String("atespace", envOr("ATE_ENV_ATESPACE", "ate-env"), "atespace for the environments")
		template  = flag.String("template", envOr("OPENSHELL_BASE_TEMPLATE", "openshell-microvm"), "ActorTemplate with the OpenShell runtime layer")
		image     = flag.String("default-image", os.Getenv("OPENSHELL_DEFAULT_IMAGE"), "digest-pinned default sandbox image (empty: the template's image)")
		layer     = flag.String("layer-root", envOr("OPENSHELL_LAYER_ROOT", "/opt/openshell"), "mount path of the runtime layer inside the actor")
		gateway   = flag.String("gateway-endpoint", envOr("OPENSHELL_SUPERVISOR_ENDPOINT", ""), "gateway URL the supervisor dials from inside the actor, e.g. http://openshell-gateway.openshell.svc.cluster.local:8080")
		caFile    = flag.String("gateway-ca-file", os.Getenv("OPENSHELL_GATEWAY_CA_FILE"), "PEM CA for a TLS gateway (empty: plaintext)")
		uid       = flag.Uint("workload-uid", 65534, "default non-root uid for the sandbox and agent")
		gid       = flag.Uint("workload-gid", 65534, "default non-root gid for the sandbox and agent")
		logLevel  = flag.String("log-level", envOr("OPENSHELL_LOG_LEVEL", "info"), "log level for the OpenShell binaries")
		allowCfg  = flag.Bool("allow-driver-config", false, "must equal allow_driver_config in the gateway's driver table")
		admission = flag.Bool("resource-admission", true, "must equal resource_admission.enabled in the gateway's driver table")
		ready     = flag.Duration("ready-timeout", 3*time.Minute, "how long to wait for a new environment's guest")
		keep      = flag.Bool("keep-failed", false, "debugging: keep the environment of a failed launch when the gateway deletes the sandbox")
	)
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	var caPEM string
	if *caFile != "" {
		b, err := os.ReadFile(*caFile)
		if err != nil {
			log.Error("read gateway CA", "err", err)
			os.Exit(2)
		}
		caPEM = string(b)
	}
	drv, err := openshelldriver.New(openshelldriver.Config{
		ATEEnvAPI: *ateEnvAPI, Atespace: *atespace, BaseTemplate: *template, DefaultImage: *image,
		LayerRoot: *layer, GatewayEndpoint: *gateway, GatewayTLSCAPEM: caPEM,
		WorkloadUID: uint32(*uid), WorkloadGID: uint32(*gid), LogLevel: *logLevel, Version: version,
		AllowDriverConfig: *allowCfg, ResourceAdmissionEnabled: *admission, ReadyTimeout: *ready, KeepFailed: *keep, Logger: log,
	})
	if err != nil {
		log.Error("configure driver", "err", err)
		os.Exit(2)
	}
	defer drv.Close()

	_ = os.Remove(*socket)
	lis, err := net.Listen("unix", *socket)
	if err != nil {
		log.Error("listen", "socket", *socket, "err", err)
		os.Exit(2)
	}
	if err := os.Chmod(*socket, 0o660); err != nil {
		log.Error("chmod socket", "err", err)
		os.Exit(2)
	}
	srv := grpc.NewServer()
	drv.Register(srv)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		srv.GracefulStop()
	}()
	log.Info("openshell-driver-substrate serving", "socket", *socket, "ate_env_api", *ateEnvAPI, "template", *template, "gateway_endpoint", *gateway, "version", version)
	if err := srv.Serve(lis); err != nil {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "stopped")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
