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
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/agent-substrate/env/internal/openshelldriver/third_party/openshell/gen/computev1"
	ateenvv1alpha "github.com/agent-substrate/env/proto/ateenv/v1alpha"
)

// gatewayPhase mirrors derive_phase in OpenShell's gateway
// (crates/openshell-server/src/compute/mod.rs) closely enough to check that
// the driver's conditions land in the intended phase.
func gatewayPhase(conds []*computev1.DriverCondition, deleting bool) string {
	if deleting {
		return "Deleting"
	}
	ready := false
	for _, c := range conds {
		if c.GetType() == "Ready" && c.GetStatus() == "True" {
			ready = true
		}
	}
	if !ready {
		for _, c := range conds {
			if c.GetType() == "Suspended" && c.GetStatus() == "True" {
				return "Stopped"
			}
		}
	}
	for _, c := range conds {
		if c.GetType() != "Ready" {
			continue
		}
		if c.GetStatus() == "True" {
			return "Ready"
		}
		switch c.GetReason() {
		case "ReconcilerError", "DependenciesNotReady", "SupervisorNotConnected", "Starting",
			"ContainerStarting", "ContainerCreated", "HealthCheckStarting", "InspectFailed":
			return "Provisioning"
		}
		return "Error"
	}
	return "Provisioning"
}

func TestConditionsForSpeakTheGatewayVocabulary(t *testing.T) {
	answered := func(st ateenvv1alpha.EnvironmentStatus) liveState {
		return liveState{answered: true, status: st}
	}
	cases := []struct {
		name            string
		phase           phase
		reason          string
		live            liveState
		wantPhase       string
		wantReason      string
		wantSuspended   string // "" when no Suspended condition is expected
		wantSuspendedRs string
	}{
		{"creating", phaseCreating, "Starting", liveState{}, "Provisioning", "Starting", "", ""},
		{"failed launch", phaseFailed, "LaunchFailed", liveState{}, "Error", "LaunchFailed", "", ""},
		{"ready running", phaseReady, "BackendReady", answered(ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RUNNING), "Ready", "BackendReady", "", ""},
		{"ready but idle-suspended", phaseReady, "BackendReady", answered(ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED), "Ready", "BackendReady", "", ""},
		{"ready, api unreachable", phaseReady, "BackendReady", liveState{}, "Ready", "BackendReady", "", ""},
		{"ready, environment gone", phaseReady, "BackendReady", liveState{missing: true}, "Error", "EnvironmentMissing", "", ""},
		{"ready, actor crashed", phaseReady, "BackendReady", answered(ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_CRASHED), "Error", "Failed", "", ""},
		{"ready, resuming", phaseReady, "BackendReady", answered(ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_RESUMING), "Ready", "Starting", "", ""},
		{"ready, deleting", phaseReady, "BackendReady", answered(ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_DELETING), "Deleting", "BackendReady", "", ""},
		{"stopped, suspended", phaseStopped, "ContainerStopped", answered(ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDED), "Stopped", "ContainerStopped", "True", "EnvironmentSuspended"},
		// derive_phase alone reads this as Error; during a stop the gateway's
		// own flow consults driver_snapshot_confirms_stopping/_stopped first,
		// which see PodTerminating and ContainerStopped and keep Stopping or
		// commit Stopped.
		{"stopped, still suspending", phaseStopped, "ContainerStopped", answered(ateenvv1alpha.EnvironmentStatus_ENVIRONMENT_STATUS_SUSPENDING), "Error", "ContainerStopped", "False", "PodTerminating"},
		{"stopped, api unreachable", phaseStopped, "ContainerStopped", liveState{}, "Stopped", "ContainerStopped", "True", "EnvironmentSuspended"},
		{"stopped, environment gone", phaseStopped, "ContainerStopped", liveState{missing: true}, "Error", "EnvironmentMissing", "", ""},
	}
	now := timestamppb.Now()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			conds, deleting := conditionsFor(tc.phase, tc.reason, "msg", tc.live, now)
			if conds[0].GetType() != "Ready" {
				t.Fatalf("first condition = %v, want Ready", conds[0])
			}
			if got := conds[0].GetReason(); got != tc.wantReason {
				t.Errorf("Ready reason = %q, want %q", got, tc.wantReason)
			}
			var suspended *computev1.DriverCondition
			for _, c := range conds[1:] {
				if c.GetType() == "Suspended" {
					suspended = c
				}
			}
			switch {
			case tc.wantSuspended == "" && suspended != nil:
				t.Errorf("unexpected Suspended condition %v", suspended)
			case tc.wantSuspended != "" && suspended == nil:
				t.Errorf("missing Suspended=%s condition", tc.wantSuspended)
			case suspended != nil && (suspended.GetStatus() != tc.wantSuspended || suspended.GetReason() != tc.wantSuspendedRs):
				t.Errorf("Suspended = %s/%s, want %s/%s", suspended.GetStatus(), suspended.GetReason(), tc.wantSuspended, tc.wantSuspendedRs)
			}
			if got := gatewayPhase(conds, deleting); got != tc.wantPhase {
				t.Errorf("gateway would derive %s, want %s (conditions %v)", got, tc.wantPhase, conds)
			}
		})
	}
}
