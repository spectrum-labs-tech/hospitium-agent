// SPDX-License-Identifier: Apache-2.0

package agent

import (
	"time"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"

	"github.com/spectrum-labs-tech/hospitium-agent/internal/tv"
)

// The agent reports only what it saw happen on a TV, never what a model or
// platform is expected to do. An observation from the simulator is labeled
// simulated so it can never pass as hardware evidence.

func evidence(dev tv.Device) string {
	if dev.Driver() == v1.DriverSimulator {
		return v1.EvidenceSimulated
	}
	return v1.EvidenceVerified
}

func observe(dev tv.Device, capability, subject, state, detail string, at time.Time) v1.CapabilityObservation {
	return v1.CapabilityObservation{
		Capability: capability, Subject: subject, State: state,
		Evidence: evidence(dev), Detail: detail, ObservedAt: at,
	}
}

func facts(dev tv.Device, info tv.Info, home string) *v1.DeviceFacts {
	return &v1.DeviceFacts{
		Driver: dev.Driver(), Manufacturer: info.Manufacturer, Model: info.Model,
		OSVersion: info.OSVersion, SDKLevel: info.SDKLevel, BuildFingerprint: info.Fingerprint,
		HomeActivity: home,
	}
}
