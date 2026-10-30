package cmd

// The Simhive pool's commands, built into wowsimcli: a worker sims a slice of a request, the hub splits a request into
// slices and combines the slices' results, with the engine's own split and combine, so a pooled sim is the sim the
// site runs. Every command reads its request as protojson on stdin and writes its answer as protojson on stdout; a
// refusal ("refused: …") or a failure is one line on stderr and exit code 1.

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/wowsims/forever/sim/core"
	"github.com/wowsims/forever/sim/core/proto"
	"github.com/wowsims/forever/sim/core/simsignals"
	"google.golang.org/protobuf/encoding/protojson"
	googleProto "google.golang.org/protobuf/proto"
)

// What one pooled run may ask of a machine. The hub checks the same limits before it hands a request out
// (server/forever.js); the worker checks them again, since a worker runs what reaches it, not what the hub meant.
const (
	poolMaxIterations = 200000
	poolMaxDuration   = 1200 // seconds
	poolMaxTargets    = 40
	poolMaxPlayers    = 40
	poolMaxThreads    = 256
)

var errPoolRefused = errors.New("refused")

func init() {
	simhive := &cobra.Command{Use: "simhive", Short: "the Simhive pool's commands"}
	var threads, count, iterations int
	var siml string
	run := &cobra.Command{
		Use:   "sim",
		Short: "sim a RaidSimRequest on --threads threads; progress lines on stderr",
		RunE: func(*cobra.Command, []string) error {
			request := &proto.RaidSimRequest{}
			if err := poolReadJSON(request); err != nil {
				return err
			}
			if err := poolCheck(request); err != nil {
				return err
			}
			if threads < 1 || threads > poolMaxThreads {
				return fmt.Errorf("%w: threads %d", errPoolRefused, threads)
			}
			return poolWriteJSON(poolSim(request, int32(threads)))
		},
	}
	run.Flags().IntVar(&threads, "threads", 1, "threads to sim on")
	split := &cobra.Command{
		Use:   "split",
		Short: "split a RaidSimRequest into --count requests by iterations, each with its own seed",
		RunE: func(*cobra.Command, []string) error {
			request := &proto.RaidSimRequest{}
			if err := poolReadJSON(request); err != nil {
				return err
			}
			if err := poolCheck(request); err != nil {
				return err
			}
			result := core.SplitSimRequestForConcurrency(request, int32(count))
			if result.ErrorResult != "" {
				return errors.New(result.ErrorResult)
			}
			return poolWriteJSON(result)
		},
	}
	split.Flags().IntVar(&count, "count", 1, "requests to split into")
	combine := &cobra.Command{
		Use:   "combine",
		Short: "combine a RaidSimResultCombinationRequest's results, weighted by their iterations",
		RunE: func(*cobra.Command, []string) error {
			request := &proto.RaidSimResultCombinationRequest{}
			if err := poolReadJSON(request); err != nil {
				return err
			}
			if len(request.Results) == 0 {
				return errors.New("no results to combine")
			}
			for i, r := range request.Results {
				if r.GetError().GetMessage() != "" {
					return fmt.Errorf("result %d failed: %s", i, r.GetError().GetMessage())
				}
			}
			return poolWriteJSON(core.CombineConcurrentSimResults(request.Results, request.Debug))
		},
	}
	request := &cobra.Command{
		Use:   "request",
		Short: "the RaidSimRequest the site sends for a wowsims link or IndividualSimSettings JSON on stdin",
		RunE: func(*cobra.Command, []string) error {
			var req *proto.RaidSimRequest
			var warnings []string
			if siml != "" {
				var err error
				if req, warnings, err = readSimRequest(siml); err != nil {
					return err
				}
			} else {
				settings, err := poolSettings()
				if err != nil {
					return err
				}
				req, warnings = settingsRequest(settings)
			}
			if iterations > 0 {
				req.SimOptions.Iterations = int32(iterations)
			}
			for _, w := range warnings {
				fmt.Fprintln(os.Stderr, "warning:", w)
			}
			if err := poolCheck(req); err != nil {
				return err
			}
			return poolWriteJSON(req)
		},
	}
	request.Flags().IntVar(&iterations, "iterations", 0, "iterations, over what the settings hold")
	request.Flags().StringVar(&siml, "siml", "", "read an x.siml setup from this path instead of stdin")
	simhive.AddCommand(run, split, combine, request)
	simhive.SilenceUsage = true
	rootCmd.AddCommand(simhive)
}

// The pool's limits on a request, and no logs: a debug log is megabytes per iteration and is never what a pooled
// run is for.
func poolCheck(r *proto.RaidSimRequest) error {
	o, e := r.GetSimOptions(), r.GetEncounter()
	switch {
	case o == nil:
		return fmt.Errorf("%w: no simOptions", errPoolRefused)
	case o.Iterations < 1 || o.Iterations > poolMaxIterations:
		return fmt.Errorf("%w: iterations %d (1–%d)", errPoolRefused, o.Iterations, poolMaxIterations)
	case o.Debug || o.DebugFirstIteration || o.Interactive:
		return fmt.Errorf("%w: debug and interactive runs are not pooled", errPoolRefused)
	case e == nil:
		return fmt.Errorf("%w: no encounter", errPoolRefused)
	case e.Duration <= 0 || e.Duration+e.DurationVariation > poolMaxDuration:
		return fmt.Errorf("%w: duration %g±%g s (up to %d s)", errPoolRefused, e.Duration, e.DurationVariation, poolMaxDuration)
	case len(e.Targets) > poolMaxTargets:
		return fmt.Errorf("%w: %d targets (up to %d)", errPoolRefused, len(e.Targets), poolMaxTargets)
	}
	players := 0
	for _, p := range r.GetRaid().GetParties() {
		players += len(p.GetPlayers())
	}
	if players == 0 || players > poolMaxPlayers {
		return fmt.Errorf("%w: %d players (1–%d)", errPoolRefused, players, poolMaxPlayers)
	}
	return nil
}

// The request simmed in threads slices, each with its own seed from the engine's split, combined the way the site
// combines its workers'. Progress goes to stderr as "progress <done> <total>" lines, at most one per finished percent.
func poolSim(request *proto.RaidSimRequest, threads int32) *proto.RaidSimResult {
	split := core.SplitSimRequestForConcurrency(request, threads)
	if split.ErrorResult != "" {
		return &proto.RaidSimResult{Error: &proto.ErrorOutcome{Message: split.ErrorResult}}
	}
	total := request.SimOptions.Iterations
	results := make([]*proto.RaidSimResult, len(split.Requests))
	done := make([]int32, len(split.Requests))
	var mu sync.Mutex
	last := int32(-1)
	var wg sync.WaitGroup
	for i, slice := range split.Requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			progress := make(chan *proto.ProgressMetrics, 16)
			finished := make(chan struct{})
			go func() {
				results[i] = core.RunSim(slice, progress, simsignals.CreateSignals())
				close(finished)
			}()
			// RunSim closes progress on its way out, before its result is returned: the result is read after both.
			defer func() { <-finished }()
			for m := range progress {
				if m.FinalRaidResult != nil {
					continue
				}
				mu.Lock()
				done[i] = m.CompletedIterations
				sum := int32(0)
				for _, d := range done {
					sum += d
				}
				if pct := sum * 100 / total; pct > last {
					last = pct
					fmt.Fprintf(os.Stderr, "progress %d %d\n", sum, total)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	for _, r := range results {
		if r == nil {
			return &proto.RaidSimResult{Error: &proto.ErrorOutcome{Message: "a slice ended without a result"}}
		}
		if r.GetError().GetMessage() != "" {
			return r
		}
	}
	return core.CombineConcurrentSimResults(results, false)
}

// Settings from stdin: a wowsims link (…/forever/<spec>/#<base64 zlib proto>) or IndividualSimSettings as protojson.
func poolSettings() (*proto.IndividualSimSettings, error) {
	data, err := io.ReadAll(bufio.NewReader(os.Stdin))
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(data))
	settings := &proto.IndividualSimSettings{}
	if strings.HasPrefix(text, "{") {
		return settings, protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal([]byte(text), settings)
	}
	_, hash, ok := strings.Cut(text, "#")
	if !ok || hash == "" || strings.Contains(text, "/raid/") {
		return nil, errInvalidLink
	}
	raw, err := base64.StdEncoding.DecodeString(hash)
	if err != nil {
		return nil, fmt.Errorf("cannot decode the link: %w", err)
	}
	z, err := zlib.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("cannot decode the link: %w", err)
	}
	defer z.Close()
	// A link is a few kilobytes; a bomb is cut off here.
	body, err := io.ReadAll(io.LimitReader(z, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("cannot decode the link: %w", err)
	}
	return settings, googleProto.Unmarshal(body, settings)
}

func poolReadJSON(m googleProto.Message) error {
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 256<<20))
	if err != nil {
		return err
	}
	return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(data, m)
}

func poolWriteJSON(m googleProto.Message) error {
	out, err := protojson.MarshalOptions{EmitUnpopulated: true}.Marshal(m)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(out)
	return err
}
