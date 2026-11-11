# hospitium-agent

A small Go agent for Raspberry Pi that keeps Google TVs at the configuration Hospitium asks for, over ADB. Licensed Apache-2.0.

**Its only job is managing TVs.** It doesn't serve the guest interface, handle reservations or talk to Home Assistant. TVs work normally without it.

It runs in one of two modes:

- **Standalone:** desired state comes from a JSON file you edit (`--local`). No account, no control plane, no network beyond the TVs. See [`examples/desired.json`](examples/desired.json).
- **Managed:** desired state and one-time operations (such as clearing streaming sign-ins between guests) come from a control plane that speaks [`hospitium-protocol`](https://github.com/spectrum-labs-tech/hospitium-protocol).

## How it works

Every 30–60 seconds, with jitter, the agent:

1. **Pulls** desired state from the control plane over HTTPS (`GET /v1/agent/desired-state`). Nothing connects *in* to the property.
2. **Reconciles** each assigned TV over ADB:
   - **Observe:** reads installed package versions, the HOME activity and settings.
   - **Diff:** compares those against the desired state.
   - **Apply** only what differs: install or upgrade the launcher (after verifying its SHA-256), activate it, and apply allowlisted settings.
   - **Verify** the result.

   A compliant TV only receives read-only commands.
3. **Executes one-time operations** such as clearing streaming-app data at checkout, rebooting, or running diagnostics. Each operation has a durable execution record, so a finished operation is never repeated because a report was lost or the agent restarted. Expired operations are refused.
4. **Reports** device health, versions, conditions and operation results.

When the control plane is unreachable, the agent keeps reconciling from its cached desired state, but it never runs one-time operations from the cache. Failing TVs back off exponentially (15s up to 10 minutes) without blocking the other TVs.

The agent has no arbitrary shell feature. Its `tv.Device` interface is a closed list of actions.

## Install on Raspberry Pi OS Lite

```bash
sudo apt-get install -y adb
sudo useradd --system --home /var/lib/hospitium-agent --shell /usr/sbin/nologin hospitium
# One static ARMv6 binary runs on every Pi from the original Model B to the 3B (and later):
GOOS=linux GOARCH=arm GOARM=6 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o hospitium-agent ./cmd/hospitium-agent
sudo install -m 0755 hospitium-agent /usr/local/bin/
sudo install -m 0644 deploy/hospitium-agent.service /etc/systemd/system/
sudo systemctl daemon-reload && sudo systemctl start hospitium-agent   # creates the state dir
sudo systemctl stop hospitium-agent
```

## Standalone mode

```bash
sudo install -m 0644 examples/desired.json /var/lib/hospitium-agent/desired.json   # then edit it
sudo -u hospitium HOSPITIUM_AGENT_STATE_DIR=/var/lib/hospitium-agent   hospitium-agent run --local /var/lib/hospitium-agent/desired.json
```

To make systemd use it, add `--local /var/lib/hospitium-agent/desired.json` to `ExecStart` in the unit. Observed state is written to `status.json` in the state directory after every pass. Bump a device's `generation` when you change its spec, so `status.json` shows when the TV has caught up. One-time operations need a control plane.

## Managed mode: enroll and provision

1. In the control plane, create a controller for the property. The response includes a **single-use enrollment token**, valid for 24 hours.
2. On the Pi:
   ```bash
   sudo -u hospitium HOSPITIUM_AGENT_STATE_DIR=/var/lib/hospitium-agent \
     hospitium-agent enroll --server https://hospitium.example.com --token hse_...
   sudo systemctl enable --now hospitium-agent
   ```
3. On each TV, enable developer options and network debugging. See [docs/google-tv.md](docs/google-tv.md). If the option exists, turn on *Disable adb authorization timeout*.
4. In the control plane, register the TV with its ADB address (`10.0.50.21:5555`) and assign it to this controller.
5. The first time the agent connects, the TV asks **"Allow debugging?"**. Tick *Always allow* and accept with the remote. Until then the device reports `unauthorized` with that instruction.
6. Set the desired state (launcher version, activation strategy, cleanup apps) in the control plane. The agent converges within one poll interval.

**Network requirement:** TVs and the Pi must sit on a management or IoT network segment, and TCP 5555 to the TVs must be reachable only from the Pi, never from guest Wi-Fi. See [docs/google-tv.md](docs/google-tv.md) §5.

### Recovery

| Situation | Fix |
|---|---|
| Stock home screen needed back | Set `activation: none`. The agent re-enables the stock launcher. Manually: `adb shell pm enable com.google.android.apps.tv.launcherx` |
| Bad launcher release | Set the desired version back. The agent downgrades with `install -r -d`, falling back to uninstall and reinstall |
| Pi replaced | Enroll the new Pi and reassign the TVs, then accept the ADB prompt on each TV (new key) |
| TV lost ADB access after an OTA update | Device shows `unreachable` or `unauthorized`. Re-enable network debugging and accept the prompt |

## Commands

```
hospitium-agent enroll --server URL --token TOKEN [--state-dir DIR]
hospitium-agent run [--local FILE] [--adb PATH] [--interval 45s] [--once] [--simulate] [--debug]
hospitium-agent version
```

`--simulate` manages in-memory simulated TVs, which persist to `simulated-tvs.json` in the state dir, so you can develop and demo without hardware. Simulated behavior is modeled on documented Android behavior and **is not evidence of real-device behavior**.

## State directory

| File | Purpose |
|---|---|
| `credentials.json` | control-plane URL and agent token (mode 0600; managed mode) |
| `status.json` | observed state of every TV (standalone mode) |
| `operations.json` | durable operation log used to deduplicate operations |
| `desired-state.json` | last desired state, used when the control plane is offline |
| `artifacts/` | SHA-256-verified APK cache |
| `.android/adbkey` | the agent's ADB identity. **Keep it**: losing it means every TV prompts again |

## Development

```bash
task check    # fmt, vet, lint (incl. context checks), tests, tidy, SPDX headers
task build:pi # static ARMv6 binary for every Raspberry Pi
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

The tests cover:

- idempotent reconciliation
- upgrade and rollback
- install failure with backoff
- TV reboot
- ADB unauthorized
- cleanup verification
- operation deduplication across restart
- expired and unknown operations
- retry on a higher attempt
- offline reconcile from cache
- artifact integrity and scoping of the agent token
- standalone mode

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
