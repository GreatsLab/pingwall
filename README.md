# pingwall

Like having `ping` open in eight terminals at once, except it's one screen, it keeps score, and it writes down when things went down.

```
pingwall  6 targets  every 1s  up 14m22s  sort: as given
q quit  p pause  r reset  s sort

  TARGET        MODE         LAST     AVG     MIN     MAX  JITTER   LOSS  MOS  HISTORY
● gateway       icmp       0.61ms  0.74ms  0.48ms  3.10ms  0.21ms   0.0%  4.4  ▁▁▁▂▁▁▁▁▃▁▁▁▁▁▁▂▁▁▁▁▁▁▁
● isp-hop       icmp       9.82ms  14.1ms  8.90ms   212ms  6.40ms   1.2%  4.3  ▁▂▁▁▁▁█▅▁▁▁▂▁▁×▁▁▁▁▁▁▂▁
● cloudflare    icmp       41.2ms  44.9ms  38.0ms   260ms  7.10ms   1.4%  4.3  ▂▁▂▁▁▁█▆▂▁▁▁▂▁×▁▂▁▁▁▂▁▁
● google-dns    icmp       47.0ms  49.3ms  44.1ms   231ms  6.80ms   1.3%  4.3  ▁▁▂▁▁▁▇█▁▁▂▁▁▁×▁▁▂▁▁▁▁▁
● quad9         icmp        168ms   171ms   160ms   402ms  9.30ms   1.6%  4.1  ▁▁▁▁▂▁▆█▁▁▁▁▁▂×▁▁▁▁▂▁▁▁
● github-https  tcp/443     201ms   204ms   190ms   480ms  12.0ms   0.0%  4.0  ▁▂▁▁▁▁▅█▂▁▁▁▁▁▁▁▂▁▁▁▁▁▁

events
21:04:11  isp-hop DOWN (timeout)
21:04:19  isp-hop UP after 10s
```

(Illustrative output from [`examples/isp-watch.txt`](examples/isp-watch.txt). In a real terminal the bars and numbers are coloured green / yellow / red by latency and loss.)

## Why

In hosting support, "your server is slow" or "the internet is slow" usually comes down to one question: *where* is it slow? Your own gateway, the ISP's first hop, or something past the ISP? `ping` answers that one host at a time. `mtr` is great for one path but you can't compare paths side by side.

Put the gateway, the ISP's first hop, and a few big anycast resolvers on one screen, and the answer jumps out:

- gateway clean, ISP hop spiky, everything after it spiky: **ISP problem**. The CSV log is your evidence when you open a ticket with them.
- gateway spiky too: **it's your LAN / Wi-Fi / router CPU**
- only one destination bad: **that destination or its route**, not your line

## Features

- **ICMP and TCP probes.** `tcp://host:443` measures TCP connect time, for networks that drop ICMP. A refused connection (RST) still counts as a reply, because the host answered.
- **No root needed.** On Linux it uses the unprivileged "ping socket" when `net.ipv4.ping_group_range` allows it (most distros now do), and on macOS it always works. Otherwise it falls back to a raw socket (root/`CAP_NET_RAW`).
- **Jitter**: mean difference between consecutive RTTs.
- **MOS score**: the simplified E-model VoIP tools use. 4.3+ is fine for calls, under 3.6 people will complain.
- **Outage events**: N losses in a row (`-down`, default 3) logs `DOWN`, the first reply after logs `UP after Xs`.
- **CSV log** of every sample (`-log file.csv`), so you can graph it later or attach it to a ticket.
- **Report mode**: `-c 20` sends 20 probes per target and prints a table. Handy in scripts and cron.
- **DNS failures count as loss.** If a name doesn't resolve, it keeps retrying every interval instead of giving up.
- **Zero dependencies.** Standard library only: ICMP packets are built by hand, and the terminal is handled with `stty`. One static binary.

## Install

Download a binary from [Releases](https://github.com/GreatsLab/pingwall/releases) (Linux amd64/arm64/arm, macOS Intel/Apple Silicon), or:

```
go install github.com/GreatsLab/pingwall@latest
```

On a Linux box where unprivileged ping is off:

```
sudo setcap cap_net_raw+ep ./pingwall      # or: sudo sysctl -w net.ipv4.ping_group_range="0 2147483647"
```

## Usage

```
pingwall gw=192.168.1.1 cf=1.1.1.1 google=8.8.8.8
pingwall -f examples/isp-watch.txt -log isp-$(date +%F).csv
pingwall -i 200ms -down 5 10.0.0.1 10.0.0.2
pingwall -c 50 1.1.1.1 tcp://github.com:443      # report and exit
```

| flag | default | |
|---|---|---|
| `-i` | `1s` | interval per target |
| `-t` | `1s` | timeout per probe |
| `-f` | | targets file, one per line, `name host` or `name=host` |
| `-c` | `0` | probes per target, then print a report and exit |
| `-w` | `300` | samples kept per target for the graph |
| `-log` | | append samples to CSV |
| `-down` | `3` | losses in a row before a target is DOWN |
| `-no-color` | | also respects `NO_COLOR` |

Keys: `q` quit, `p`/space pause, `r` reset stats, `s` cycle sort (as given / loss / latency / name).

Probes are staggered across the interval, so 10 targets at 1s don't send 10 packets in the same millisecond.

## Bugs I hit while building it

- `pad()` cut off strings that were *exactly* the column width (`blackhol…`).
- A target whose DNS failed at startup was shown once and never retried. When I fixed that, the first version segfaulted: `NewProber` returned a nil `*icmpProber` wrapped in a non-nil `Prober` interface, so `if prober == nil` was false. Classic Go.

## Limitations

- IPv6 ICMP isn't done yet (use `tcp://[2001:db8::1]:443` meanwhile)
- The live view needs a Unix terminal (`stty`). On Windows only `-c` report mode with `tcp://` targets works.

## License

MIT
