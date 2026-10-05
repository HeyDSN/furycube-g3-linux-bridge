# furycube-g3-linux-bridge

Use the official **Furycube G3 web driver on Linux**, with no Windows and no vendor app.

> **Only for the Furycube G3, and only on Linux.**
> This is not a general Furycube driver. It is hard-wired to the G3's USB ids, device id,
> report layout and button table. Other Furycube mice (G1, G7 Pro, G9 Pro, ...) use different
> ones and will not work. It also relies on Linux's `hidraw` interface and POSIX ACLs, so it
> does not build on macOS or Windows (Windows already has the vendor's own helper).

The Furycube G3 gaming mouse has no Linux driver. Its settings (DPI, button remapping, polling
rate, lighting) are configured in a web page, [bk-gmice.com](https://www.bk-gmice.com), but that
page only works if a Windows helper program is running on your machine and listening on
`ws://localhost:10086`. `furycube-g3-linux-bridge` is a drop-in replacement for that helper. Run it, open
the web driver in your browser, change settings, close it again. The settings are stored in the
mouse itself and survive without the bridge running.

```
browser (bk-gmice.com)  <-- WebSocket -->  furycube-g3-linux-bridge  <-- /dev/hidraw -->  USB dongle  -->  mouse
                                 ws://localhost:10086
```

It is a single static Go binary with no dependencies.

> Unofficial. Not affiliated with or endorsed by Furycube or its OEM. The bridge sends
> configuration reports to your mouse's firmware; it only forwards the report types the web
> driver uses for settings (see [Safety](#safety)), but you use it at your own risk.

> **Written entirely by AI.** All code, tests and documentation in this repository were written
> by Claude (Anthropic's AI model, via Claude Code), working from the owner's instructions and
> hardware. The owner tested it on a single G3 but has not audited every line. It runs briefly
> as root and sends data to your mouse's firmware, so read the source (it is small) before
> running it, and expect rough edges.

## Status

Developed and tested against one G3 on Fedora (Linux 7.x), connected through its 2.4 GHz
dongle.

| Feature                                  | Status                                              |
| ---------------------------------------- | --------------------------------------------------- |
| Mouse + battery level shown in web UI    | Works                                               |
| DPI stages and active stage              | Works (verified on hardware)                        |
| Button remapping (e.g. back/forward to volume) | Works (verified on hardware)                  |
| Polling rate, lighting, sleep time, macros | Forwarded the same way, not individually verified |
| Wired mode (`1d57:2266`)                 | USB id taken from the vendor app, untested          |
| Bluetooth mode                           | Not supported (no hidraw device to talk to)         |
| Other Furycube / Inphic mice             | Not supported, only the G3 is wired up              |

If you test another mode or setting, an issue or PR with the result is welcome.

## Requirements

- Linux with `hidraw` (any current distribution). Builds for amd64 and arm64; only tested on amd64
- `sudo`
- A Chromium-based browser. The web driver is an `https` page that connects to `ws://localhost`;
  Chromium allows that, Firefox may block it.
- Go 1.22 or newer, only to build

## Build

```sh
git clone https://github.com/HeyDSN/furycube-g3-linux-bridge.git
cd furycube-g3-linux-bridge
CGO_ENABLED=0 go build -ldflags "-s -w" -o furycube-g3-linux-bridge .
```

Run the tests with `go test ./...`. They need no hardware.

## Use

1. Plug in the G3's 2.4 GHz dongle and wake the mouse by moving it.
2. Start the bridge:

   ```sh
   sudo ./furycube-g3-linux-bridge
   ```

3. Open <https://www.bk-gmice.com> and pick the G3. Change DPI, buttons, and so on.
4. Press `Ctrl-C` in the terminal when you are done.

You will see log lines like `mouse attached on /dev/hidraw3` and `sent report 0x4 (56 bytes)`
as it works. Add `-v` to log every frame exchanged with the web page.

### Why sudo, and what it does

`/dev/hidraw*` nodes are root-only. Running under `sudo` splits the program in two:

- A short-lived **root parent** that only gives your user read/write access to the mouse's
  hidraw node, using a temporary POSIX ACL, and restores the previous permissions when the
  program exits.
- A **child process running as your own user** that does everything else, including listening on
  port 10086. The network-facing part never runs as root.

Nothing is installed, and no permission outlives the run. If you start it from a root shell
instead of through `sudo`, pass `-user NAME` to say whom to serve.

### Flags

| Flag                    | Meaning                                                              |
| ----------------------- | -------------------------------------------------------------------- |
| `-v`                    | Log every frame to and from the web page                             |
| `-allow-origin A,B`     | Extra browser Origins allowed to connect (default: bk-gmice.com and localhost) |
| `-user NAME`            | When started as root, the user to run the bridge as                  |

## Safety

- The bridge listens on loopback only (`127.0.0.1` and `::1`, port 10086).
- It rejects WebSocket connections from any browser Origin other than the web driver's own site
  (or localhost), so a random website cannot send commands to your mouse through it.
- It forwards only the configuration reports the G3 page uses: DPI, lighting/sleep, polling
  rate, buttons, macros and one control report (`0x04 0x05 0x06 0x08 0x09 0x0C`). Calibration and
  firmware-related reports are refused.
- Reports are cut or zero-padded to the exact size the device's report descriptor declares.

## Limitations

- **No read-back.** The mouse cannot be asked for its current settings through this bridge. For
  the G3 the web driver does not ask either; it keeps its own state in the browser's
  `localStorage`. After the mouse is reset (or used on another machine), the page can show
  values that differ from what the mouse holds until you apply each setting again.
- The web driver and its setting layouts are the vendor's; this project does not modify them.
- Only one program should talk to the mouse's config interface at a time. Do not run the
  bridge together with the vendor's helper.

## Troubleshooting

| Symptom                                           | Fix                                                              |
| ------------------------------------------------- | ---------------------------------------------------------------- |
| Web page says the driver is not installed         | The bridge is not running, or the browser blocks `ws://localhost` (try Chromium) |
| `listen ...: address already in use`              | Another helper is on port 10086, stop it                         |
| `cannot open /dev/hidraw3: permission denied`     | Start with `sudo`                                                |
| Bridge runs but the mouse never appears           | Wake the mouse by moving it; check the dongle is the `1d57:fa60` device in `lsusb` |
| A setting seems ignored                           | Run with `-v` and check a `sent report 0x..` line follows the click |

## How it works

The vendor's web page sends hex-encoded frames over a WebSocket, and its Windows helper turns
them into HID feature reports. This project was built by reading the web driver's JavaScript
and capturing the reports the vendor's Windows app sends, then reproducing both sides. The wire
formats are written down in [docs/PROTOCOL.md](docs/PROTOCOL.md).

## Project layout

| File           | Contents                                                              |
| -------------- | --------------------------------------------------------------------- |
| `main.go`      | WebSocket server, web driver protocol, hotplug handling              |
| `hid.go`       | Device discovery, HID report descriptor parsing, `HIDIOCSFEATURE`    |
| `ws.go`        | Minimal RFC 6455 WebSocket server (standard library only)            |
| `broker.go`    | Root parent for `sudo`: runs the bridge unprivileged                  |
| `acl.go`       | Temporary per-user ACL on the hidraw node, and its restore           |
| `*_test.go`    | Tests; no hardware needed                                            |

## License

MIT, see [LICENSE](LICENSE).
