# Google TV and Raspberry Pi: compatibility, limitations and open questions

Nothing in this document has been verified on real hardware yet. Each item is labeled with its source:

- **[documented]**: Android or Google platform behavior described in official documentation
- **[community]**: widely reported by integrators or forums; plausible but unverified
- **[sim]**: what the simulator models; not evidence of real behavior
- **[measured]**: measured during this work (and where)

Hardware testing replaces these labels with results for each TV model in the compatibility matrix at the end. Contributions of results for other models are welcome.

## 1. ADB access on Google TV

| Topic | What we expect | Source | Test |
|---|---|---|---|
| Enabling | Settings → System → About → tap *Android TV OS build* 7×, then Developer options → *USB debugging* / *Network debugging* (TCP 5555) | documented / community | Record the exact menu path per model |
| Wireless pairing | Android 11+ "Wireless debugging" with pairing codes (`adb pair`) exists on some Google TV builds; the agent currently uses `adb connect` (legacy TCP) | documented | Check which models need `adb pair`; add a pairing flow if required |
| Authorization prompt | The first connection shows "Allow debugging?" on the TV; tick *Always allow from this computer* and accept with the remote. The agent reports `ADBUnauthorized` with this instruction | documented | Confirm the prompt is usable with the stock remote |
| Persistence across reboots | Accepted keys are stored on the TV (`/data/misc/adb/adb_keys`) and survive reboots. The agent's key must persist too (`$HOME/.android/adbkey` of the service user; the systemd unit sets `HOME` to the state dir) | documented | Reboot the TV 5×, power-cycle it at the wall, then confirm the agent reconnects without a prompt |
| **Authorization timeout** | Android 11+ **revokes ADB authorizations unused for 7 days** by default. The agent polls every 30–60s, which counts as use, but a Pi outage of over 7 days would need the prompt accepted again. Developer options has *Disable adb authorization timeout* | documented | Enable that toggle during provisioning where available |
| Network debugging survives reboot | Some builds turn network debugging off after a reboot or OTA | community | Reboot and OTA observation; this is the single biggest operational risk |
| OTA updates | May reset developer options or re-enable a disabled stock launcher | community | Watch for it; the agent re-reconciles automatically and reports `unreachable` or `unauthorized` if access is lost |

## 2. Launcher replacement (no firmware changes)

Strategies are implemented as `activation` in the desired state, so each TV can use what works on its model:

| Strategy | Mechanism | Expected behavior | Source | Recovery |
|---|---|---|---|---|
| `home_activity` | `cmd package set-home-activity <component>` | The supported mechanism on stock Android. On Google TV builds the setting is often ignored or reverted. The agent verifies by resolving HOME afterwards and reports `LauncherActive=False` if it didn't stick | documented / community | Nothing to undo |
| `disable_stock` | `pm disable-user --user 0 com.google.android.apps.tv.launcherx` (some builds also have `com.google.android.tungsten.setupwraith`) | The Hospitium launcher becomes the only HOME handler. Commonly used by hospitality and kiosk deployments. **Side effects reported:** Google TV home rows, ambient mode, some Assistant/voice entry points and the stock "Apps" shortcut can stop working | community | `pm enable <pkg>`, which the agent does automatically when activation is set back to `none`. Worst case: factory reset (no firmware change, but the TV must be set up again) |
| `none` | Launcher installed, stock home kept | Hospitium opens as an app (e.g. from a home-screen row or input shortcut). Lowest risk, weakest experience | — | — |

**Native streaming and DRM are unaffected** by any of these: streaming apps are untouched, and Widevine lives in the platform, not the launcher. The remote keeps working: HOME goes to whichever launcher is active, and BACK and D-pad are standard.

Hardware testing must also check: the HOME button behavior after `disable_stock`, the voice button, the input/HDMI switcher, and whether OTA re-enables the stock launcher.

## 3. Streaming session cleanup

`pm clear <pkg>` deletes the app's private data (`/data/data/<pkg>`: databases, shared preferences, app-stored tokens) and its external app-specific storage, and force-stops it. **[documented]**

| App | Expectation | Caveat |
|---|---|---|
| Netflix (`com.netflix.ninja`) | Signs out (credentials are app-private) | **[community]** Widely relied on by rentals; verify |
| YouTube (`com.google.android.youtube.tv`) | **Not reliable.** YouTube on Google TV can use the **device's Google account** from AccountManager, which `pm clear` does not remove. If a guest adds their Google account to the TV, clearing YouTube data won't remove it | **[documented]** AccountManager accounts survive `pm clear`. Mitigation to research: provisioning policy that blocks adding accounts, or a guarded `remove_account` operation |
| Disney+ / Hulu / Prime Video / Max / Peacock | Expected to sign out (own authentication) | **[community]**; verify each |

Rules regardless of results:

- **Local clearing does not revoke server-side tokens.** The device stays in the guest's "signed-in devices" list until the provider expires it. Guest-facing copy should suggest "Sign out of all devices" after a stay with sensitive accounts, and the checkout screen can show this.
- **Verification today** (no root): the clear command succeeded, the app process is not running, and external storage is empty. That is evidence, not proof. A planned addition is **screenshot-based verification**: launch the app after clearing, `screencap`, and confirm a sign-in screen. Because this is fixed and read-only, it stays within the closed operation set.
- Behavior may vary by app version and OS. The compatibility matrix records app version and OS for each result.

## 4. Raspberry Pi management appliance

| Item | Value | Source |
|---|---|---|
| Agent binary (linux/armv6, stripped) | **6.6 MB**, static, no cgo | **[measured]** cross-compiled on the dev machine, 2026-11-11 |
| Targets | Pi 1 / Zero (ARMv6, `GOARM=6`), Pi 2 (ARMv7), Pi 3B (ARMv7/ARM64). One ARMv6 build runs on all of them | documented |
| OS | Raspberry Pi OS Lite (32-bit for Pi 1/2/Zero) + Debian `adb` package | documented |
| Local state | `/var/lib/hospitium-agent`: `credentials.json`, `operations.json`, `desired-state.json`, `artifacts/`, `.android/adbkey` | — |
| RAM / CPU | **Not measured yet.** Expect idle RSS of 10–20 MB and brief CPU spikes during `adb install`. To be measured on a Pi 1 and a Pi 3B | — |
| SD-card wear | The agent writes small JSON files on change, plus one desired-state cache write per poll. Planned: skip the write when unchanged | — |

## 5. Network placement and ADB isolation (required)

ADB over TCP accepts connections from **anyone on the TV's network segment**. Only the authorization prompt stands in the way, and a guest holding the remote can accept it. So:

1. Put TVs and the Pi on a **management/IoT VLAN** that is not the guest Wi-Fi. The TVs still need internet access for streaming.
2. Allow TCP **5555 to the TVs only from the Pi** (firewall rule on the router or VLAN).
3. Enable **client isolation** on the guest SSID.
4. If a TV must share the guest network, accept the risk explicitly per property. In that case the agent's ADB key should be the *only* authorized key, which the diagnostics operation can audit via the diagnostics operation.

## 6. Compatibility matrix (results welcome)

| Model | OS / build | Network debugging persists reboot | Auth persists reboot | `home_activity` works | `disable_stock` side effects | Netflix cleared | YouTube cleared | Disney+ cleared | Notes |
|---|---|---|---|---|---|---|---|---|---|
| _model_ | | | | | | | | | |
| _model_ | | | | | | | | | |

| Pi model | Agent RSS idle | Peak during install | Install time (10 MB APK) | Notes |
|---|---|---|---|---|
| Pi 1 B | | | | |
| Pi 3 B | | | | |
