# Protocol notes

How the Furycube G3 web driver, the helper, and the mouse talk to each other. Everything here
was derived from the web driver's JavaScript and from reports captured from the vendor's
Windows app; none of it is from vendor documentation, so treat unverified parts accordingly.

The mouse is a Xenta/Inphic OEM design. Its 2.4 GHz dongle is USB `1d57:fa60` ("G3 2.4G") and
exposes four HID interfaces: keyboard, mouse, a vendor interface, and a second keyboard. The
vendor interface (the one whose report descriptor declares feature report `0x04`) carries all
configuration.

## Layer 1: web page to bridge (WebSocket, port 10086)

Text frames containing a hex string (two hex digits per byte).

Bridge to page:

| Frame (bytes)                                  | Meaning                                              |
| ---------------------------------------------- | ---------------------------------------------------- |
| `03 05`                                        | Helper version 3.5, sent on connect. Must match what the page expects. |
| `<conn> 01 <dev> 41 <state> <battery>`         | Mouse present, with battery state and level          |
| `<conn> 00 <dev>`                              | Mouse unplugged                                      |
| `<conn> <dev> 50 <status> 00`                  | Acknowledgement of a write, `status` 0 = ok, 1 = failed |

- `<conn>`: connection method, `0x80` wired, `0x81` 2.4 GHz dongle, `0x84` Bluetooth.
- `<dev>`: device id, `0x66` (102) is the G3.
- The presence frame is the dongle's own heartbeat (below) prefixed with `<conn> 01`.

Page to bridge:

| Frame                                  | Meaning                                                  |
| -------------------------------------- | -------------------------------------------------------- |
| `<conn> <dev> <report id> <payload...>` | A HID feature report to write. The page builds buffers a few bytes longer than the real report. |
| `F0 <conn> <dev> <cmd> 00`             | Helper commands (`0B` version, `0A` bulk read). Not used for the G3, ignored. |

The G3 is on the web driver's "no read-back" list: it never asks the helper to read settings,
and it keeps its own copy in `localStorage`.

## Layer 2: bridge to dongle (hidraw)

Reports are written as **HID feature reports** (`HIDIOCSFEATURE`) on the vendor interface. The
size of each report, including the report ID byte, comes from the interface's report
descriptor:

| Report ID | Size | Used for                  |
| --------- | ---- | ------------------------- |
| `0x04`    | 56   | DPI configuration         |
| `0x05`    | 15   | Lighting / sleep          |
| `0x06`    | 9    | Polling rate              |
| `0x08`    | 59   | Button map                |
| `0x09`    | 64   | Macros                    |
| `0x0C`    | 10   | Control report            |

Only the DPI (`0x04`) and button (`0x08`) layouts below have been decoded and verified. The
others are forwarded as the web driver builds them.

### DPI report (`0x04`, 56 bytes)

| Bytes     | Meaning                                                               |
| --------- | --------------------------------------------------------------------- |
| 0         | `0x04`                                                                |
| 1         | `0x38`                                                                |
| 2         | Profile, `1`                                                          |
| 3         | Ripple control in bit 0 (sensitivity in the high nibble)              |
| 4         | Angle snapping in the high nibble, motion sync in the low nibble      |
| 5         | Bitmask of enabled stages (`0x3F` = six stages)                       |
| 6, 7      | Double-range flags, `0`                                               |
| 8..15     | Low byte of each stage's DPI                                          |
| 16..23    | High byte of each stage's DPI (DPI is a 16-bit little-endian value split across the two arrays, e.g. 800 = `0x0320` gives `0x20` and `0x03`) |
| 24        | Active stage, 1-based                                                 |
| 25..48    | Eight RGB indicator colours, 3 bytes each                             |
| 49        | Indication type, `1`                                                  |
| 50, 51    | 16-bit big-endian sum of bytes 3 through 49                           |
| 52..55    | `0`                                                                   |

### Button report (`0x08`, 59 bytes)

| Bytes   | Meaning                                                                 |
| ------- | ----------------------------------------------------------------------- |
| 0..2    | `0x08 0x3B 0x01`                                                        |
| 3..56   | 18 slots of `[function, a, b]`                                          |
| 57, 58  | 16-bit big-endian sum of bytes 3 through 56                             |

Slot order for the G3: left, right, middle, back, forward, four unused, DPI button, six
unused, wheel down, wheel up. Unused slots hold `[1, 0, 0]` (off). The report always carries
all 18 slots, so changing one button resends the whole table.

Function codes seen in the web driver's tables:

| Code | Function        | Code | Function            |
| ---- | --------------- | ---- | ------------------- |
| 1    | Off             | 22   | Previous track      |
| 2    | Left click      | 23   | Next track          |
| 3    | Right click     | 24   | Play / pause        |
| 4    | Middle click    | 26   | Mute                |
| 5    | Back            | 27   | Volume up           |
| 6    | Forward         | 28   | Volume down         |
| 7    | Double click    | 32   | Browser forward     |
| 13   | DPI cycle       | 33   | Browser back        |
| 14   | DPI up          | 17   | Shortcut (`a` = modifier mask, `b` = HID usage) |
| 15   | DPI down        | 18   | Macro               |

Volume up/down (27/28) on the back and forward slots is verified on hardware.

### Dongle heartbeat

Roughly every two seconds the vendor interface sends input report `0x03`:

```
03 66 41 <state> <battery %>
```

`0x66` is the device id, `0x41` the report kind, `<battery %>` is a plain percentage.

## How the layouts were captured

The vendor's Windows app was run under Wine with HID debug logging to see which report IDs it
sends. Wine truncates this device's feature reports to the report ID alone, so the payloads
were read from the app's memory while it was sending, then cross-checked against the web
driver's JavaScript encoders (the byte layouts and checksums above match both).
