# simhive-forever

The Simhive pool's engine for World of Warcraft: Forever. The engine is
[WoWSims Forever](https://github.com/wowsims/forever) (MIT, by the WoWSims
contributors), unchanged; this repository adds the commands the pool needs
and builds it for every platform the pool runs on.

## What the overlay adds

`overlay/cmd/wowsimcli/cmd/simhive.go` is copied in beside wowsimcli's own
commands at build time and adds `wowsimcli simhive …`. Every command reads
protojson on stdin and writes protojson on stdout:

| Command | In | Out | Who runs it |
|---|---|---|---|
| `request [--iterations n] [--siml path]` | a wowsims link, or `IndividualSimSettings` | the `RaidSimRequest` the site sends | the hub |
| `split --count n` | `RaidSimRequest` | `RaidSimRequestSplitResult`: n slices, each with its own seed | the hub |
| `sim --threads n` | `RaidSimRequest` | `RaidSimResult`; `progress <done> <total>` lines on stderr | a machine |
| `combine` | `RaidSimResultCombinationRequest` | `RaidSimResult` | the hub |
| `names` | nothing | the names, icons and ranks of the spells and items in the engine's database, as JSON | the hub |

The split and combine are the engine's own (`SplitSimRequestForConcurrency`,
`CombineConcurrentSimResults`): the functions the site uses to spread a sim over
its web workers. A request split into slices, each simmed on another machine and
combined, gives the same result as one run of the whole request. Checked: 40,000
iterations as one 8-thread run, and as 4 slices on 2 threads each, both give
799.26 DPS, stdev 29.51.

`request`, `split` and `sim` refuse what the pool does not run: more than
200,000 iterations, fights over 1,200 s, more than 40 targets or players, and
debug or interactive runs. A refusal is one `refused: …` line on stderr and
exit code 1. The hub checks the same limits before it hands a request out.

## Building

```bash
git clone https://github.com/wowsims/forever.git
scripts/build.sh forever bin linux/amd64 windows/amd64
```

This needs `go`, `protoc` and `protoc-gen-go` (`google.golang.org/protobuf`) on
PATH. The repository does not commit its generated protobuf code, so the script
generates it.

## The channel

`.github/workflows/build.yml` builds daily from wowsims/forever's `master`, and
on every push here. Each build is a release named
`forever-<engine sha12>-<overlay sha7>` holding these assets:

- `<tag>-linux-x64.zip`, `<tag>-linux-arm64.zip`, `<tag>-windows-x64.zip` and
  `<tag>-macos-arm64.zip`, each with `wowsimcli`, the WoWSims license and
  `VERSION.txt`;
- `<tag>-source.tar.gz`;
- `SHA256SUMS`.

The hub pins one build, and machines fetch it and verify it against the sums,
the way simc reaches them from `rbardtke/simc-builds`.
