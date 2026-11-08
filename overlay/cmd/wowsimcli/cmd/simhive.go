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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/wowsims/forever/assets/database"
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
	names := &cobra.Command{
		Use:   "names",
		Short: "the names and icons of the spells and items in the engine's database, and the gear, random suffixes and enchants it knows, as JSON",
		RunE: func(*cobra.Command, []string) error {
			return json.NewEncoder(os.Stdout).Encode(poolNames())
		},
	}
	timeline := &cobra.Command{
		Use:   "timeline",
		Short: "one iteration of a RaidSimRequest as the first player's casts, with its resources and buffs at each",
		RunE: func(*cobra.Command, []string) error {
			request := &proto.RaidSimRequest{}
			if err := poolReadJSON(request); err != nil {
				return err
			}
			request.SimOptions.Iterations = 1
			if err := poolCheck(request); err != nil {
				return err
			}
			out, err := poolTimeline(request)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(out)
		},
	}
	request.Flags().IntVar(&iterations, "iterations", 0, "iterations, over what the settings hold")
	request.Flags().StringVar(&siml, "siml", "", "read an x.siml setup from this path instead of stdin")
	// One line on stderr for a failure, the one root's Execute prints: no usage text, no cobra "Error:" copy.
	for _, c := range []*cobra.Command{run, split, combine, request, names, timeline} {
		c.SilenceUsage, c.SilenceErrors = true, true
		simhive.AddCommand(c)
	}
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

// One cast of the timeline: when, what, whether the rotation pressed it (a proc, a swing or a spell another spell
// casts is not a press), the resources as the cast began, before its cost, and the buffs up then, as [id, stacks].
type poolCast struct {
	T     float64            `json:"t"`
	Spell int32              `json:"spell,omitempty"`
	Item  int32              `json:"item,omitempty"`
	Other string             `json:"other,omitempty"`
	Tag   int32              `json:"tag,omitempty"`
	Press bool               `json:"press"`
	Res   map[string]float64 `json:"res"`
	Buffs [][2]int32         `json:"buffs"`
}

type poolTimelineOut struct {
	Duration float64    `json:"duration"`
	Casts    []poolCast `json:"casts"`
}

// The sim's casts in one iteration — the sim's side of the analyzer's checklist, as the combat log is the
// player's. The engine writes a line per cast to its debug log as the cast begins; the log is pointed at a
// function here (from the first rotation action on, so the prepull is not in it) that reads the player's state
// at that moment instead of formatting text. The " APL: <entry>" the engine adds to a cast the rotation chose
// is what tells a press from a proc.
func poolTimeline(request *proto.RaidSimRequest) (*poolTimelineOut, error) {
	out := &poolTimelineOut{Casts: []poolCast{}}
	var player *core.Character
	hook := func(sim *core.Simulation, _ *core.APLAction) {
		if sim.Log != nil {
			return
		}
		player = sim.Raid.Parties[0].Players[0].GetCharacter()
		prefix := player.LogLabel() + " Casting %s ("
		sim.Log = func(message string, vals ...interface{}) {
			if !strings.HasPrefix(message, prefix) || len(vals) == 0 {
				return
			}
			id, ok := vals[0].(core.ActionID)
			if !ok {
				return
			}
			suffix, _ := vals[len(vals)-1].(string)
			c := poolCast{T: sim.CurrentTime.Seconds(), Spell: id.SpellID, Item: id.ItemID, Tag: id.Tag, Press: strings.HasPrefix(suffix, " APL:"), Res: map[string]float64{}, Buffs: [][2]int32{}}
			if id.OtherID != proto.OtherAction_OtherActionNone {
				c.Other = id.OtherID.String()
			}
			if player.HasManaBar() {
				c.Res["mana"] = player.CurrentMana()
			}
			if player.HasEnergyBar() {
				c.Res["energy"] = player.CurrentEnergy()
				c.Res["combo_points"] = float64(player.ComboPoints())
			}
			if player.HasRageBar() {
				c.Res["rage"] = player.CurrentRage()
			}
			for _, a := range player.GetAuras() {
				if a.IsActive() && (a.ActionID.SpellID != 0 || a.ActionID.ItemID != 0) {
					aid := a.ActionID.SpellID
					if aid == 0 {
						aid = -a.ActionID.ItemID // an item's aura: its item id, negative
					}
					c.Buffs = append(c.Buffs, [2]int32{aid, max(1, a.GetStacks())})
				}
			}
			out.Casts = append(out.Casts, c)
		}
	}
	result := core.RunSimWithAPLHook(request, simsignals.CreateSignals(), hook)
	if msg := result.GetError().GetMessage(); msg != "" {
		return nil, errors.New(msg)
	}
	out.Duration = result.FirstIterationDuration
	return out, nil
}

// What a result's ids are called: a result names its actions and auras by id only, and the site looks them up in
// this same database. Spells carry their rank (one id per rank in Forever).
type poolName struct {
	Name string `json:"name"`
	Icon string `json:"icon,omitempty"`
	Rank int32  `json:"rank,omitempty"`
}

//
// The hub also checks a character's gear against it before a sim goes out: gear is the items a character can wear
// (items also has rows that are only an icon), suffixes the random suffixes and enchants the enchants by effect id.
// The sim panics on an item or a suffix the database lacks and leaves out an enchant it lacks.
func poolNames() map[string]map[int32]poolName {
	db := database.Load()
	spells, items, gear := map[int32]poolName{}, map[int32]poolName{}, map[int32]poolName{}
	suffixes, enchants := map[int32]poolName{}, map[int32]poolName{}
	for _, s := range db.SpellIcons {
		spells[s.Id] = poolName{s.Name, s.Icon, s.Rank}
	}
	for _, i := range db.ItemIcons {
		items[i.Id] = poolName{Name: i.Name, Icon: i.Icon}
	}
	for _, i := range db.Items {
		items[i.Id] = poolName{Name: i.Name, Icon: i.Icon}
		gear[i.Id] = poolName{Name: i.Name}
	}
	for _, s := range db.RandomSuffixes {
		suffixes[s.Id] = poolName{Name: s.Name}
	}
	for _, e := range db.Enchants {
		enchants[e.EffectId] = poolName{Name: e.Name}
	}
	return map[string]map[int32]poolName{"spells": spells, "items": items, "gear": gear, "suffixes": suffixes, "enchants": enchants}
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
