package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
	evalpkg "github.com/gremlord/gremlord/internal/eval"
)

var evalCmd = &cobra.Command{
	Use:   "eval",
	Short: "Run paired model evaluations through Claude Code",
}

var (
	evalBaseline      string
	evalMUT           string
	evalJudge         string
	evalAttempts      int
	evalTasks         []string
	evalTimeout       time.Duration
	evalSeed          uint64
	evalOutput        string
	evalResume        bool
	evalJSON          bool
	evalPython        string
	evalDockerBin     string
	evalKeepContainer bool
)

var evalRunCmd = &cobra.Command{
	Use:   "run <manifest.yaml>",
	Short: "Run a manifest-driven paired evaluation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		manifest, err := evalpkg.LoadManifest(args[0])
		if err != nil {
			return err
		}
		if err := evalpkg.FilterTasks(manifest, evalTasks); err != nil {
			return err
		}
		cfg, dataDir, err := loadConfig()
		if err != nil {
			return err
		}
		for what, alias := range map[string]string{"baseline": evalBaseline, "mut": evalMUT} {
			if err := checkEvalModel(cfg, what, alias); err != nil {
				return err
			}
		}
		if evalJudge != "none" {
			if err := checkEvalModel(cfg, "judge", evalJudge); err != nil {
				return err
			}
		}
		if evalOutput == "" {
			evalOutput = filepath.Join(dataDir, "evals", manifest.Name)
		}
		baseURL, token, stop, err := ensureRouter(cmd.Context(), cfg, dataDir)
		if err != nil {
			return err
		}
		defer stop()

		var swebench evalpkg.SWEBenchEnv
		if manifest.IsDataset() {
			swebench, err = evalpkg.MaterializeSWEBenchBridge(evalOutput, evalPython)
			if err != nil {
				return err
			}
		}
		runner := &evalpkg.Runner{Options: evalpkg.Options{
			Baseline: evalBaseline, MUT: evalMUT, Judge: evalJudge,
			Attempts: evalAttempts, Tasks: evalTasks, Timeout: evalTimeout,
			Seed: evalSeed, OutputDir: evalOutput, Resume: evalResume, JSON: evalJSON,
			BaseURL: baseURL, Token: token, Profile: cfg.DefaultProfile, DataDir: dataDir,
			SWEBench: swebench, Docker: evalpkg.DockerOptions{
				DockerBin: evalDockerBin, KeepContainers: evalKeepContainer,
			},
		}}
		if !evalJSON {
			fmt.Printf("Evaluation %s — artifacts: %s\n", manifest.Name, evalOutput)
			runner.OnCandidate = func(r evalpkg.CandidateResult) {
				verdict := "skipped"
				if r.Verifier.Ran {
					verdict = fmt.Sprintf("%v", r.Verifier.Passed)
				}
				fmt.Printf("%-8s %-20s %-16s verifier=%-7s $%.4f %dms\n",
					r.Label, r.Model, r.Status, verdict, r.Usage.CostUSD, r.DurationMS)
			}
		}
		summary, err := runner.Run(cmd.Context(), manifest)
		if err != nil {
			return err
		}
		if evalJSON {
			return json.NewEncoder(os.Stdout).Encode(summary)
		}
		printEvalSummary(summary)
		return nil
	},
}

var evalReportJSON bool

var evalReportCmd = &cobra.Command{
	Use:   "report <output-dir>",
	Short: "Report a completed or partial evaluation",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		s, err := evalpkg.LoadSummary(filepath.Join(args[0], "summary.json"))
		if err != nil {
			return err
		}
		if evalReportJSON {
			return json.NewEncoder(os.Stdout).Encode(s)
		}
		printEvalSummary(s)
		return nil
	},
}

// checkEvalModel fails fast on an unconfigured alias, so a whole evaluation
// doesn't burn time and money before the router rejects every request. A
// routing alias like "auto" is a valid selection.
func checkEvalModel(cfg *config.Config, what, alias string) error {
	if alias == "" {
		return fmt.Errorf("--%s model alias is required", what)
	}
	if _, ok := cfg.Models[alias]; ok {
		if cfg.IsCLIAlias(alias) {
			return fmt.Errorf("--%s %q is a cli alias — eval pins the whole candidate to one model, and cli aliases are explicit-subagent-only", what, alias)
		}
		return nil
	}
	if _, ok := cfg.Routing[alias]; ok {
		return nil
	}
	known := make([]string, 0, len(cfg.Models)+len(cfg.Routing))
	for a := range cfg.Models {
		known = append(known, a)
	}
	for a := range cfg.Routing {
		known = append(known, a)
	}
	sort.Strings(known)
	return fmt.Errorf("--%s references unknown model alias %q (configured: %s)",
		what, alias, strings.Join(known, ", "))
}

func printEvalSummary(s *evalpkg.Summary) {
	fmt.Printf("%s: baseline=%s mut=%s judge=%s pairs=%d\n", s.Name, s.Baseline, s.MUT, s.Judge, len(s.Pairs))
	fmt.Printf("wins: baseline %d · mut %d · ties %d · judge errors %d · infra pairs %d\n",
		s.BaselineWins, s.MUTWins, s.Ties, s.JudgeErrors, s.InfraPairs)
	fmt.Printf("verifier passes: baseline %d · mut %d — run failures: baseline %d · mut %d · infra failures %d\n",
		s.BaselineVerifierPasses, s.MUTVerifierPasses, s.BaselineFailures, s.MUTFailures, s.InfraFailures)
	fmt.Printf("cost: baseline $%.4f · mut $%.4f\n", s.BaselineCostUSD, s.MUTCostUSD)
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tATTEMPT\tWINNER\tBASELINE\tMUT\tBASELINE $\tMUT $")
	for _, p := range s.Pairs {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%.4f\t%.4f\n", p.TaskID, p.Attempt, p.Winner,
			candidateCell(p.Baseline), candidateCell(p.MUT), p.Baseline.Usage.CostUSD, p.MUT.Usage.CostUSD)
	}
	tw.Flush()
}

func candidateCell(c evalpkg.CandidateResult) string {
	if !c.Verifier.Ran {
		return c.Status + "/verifier-skipped"
	}
	if c.Verifier.Passed {
		return c.Status + "/pass"
	}
	return c.Status + "/fail"
}

func init() {
	evalRunCmd.Flags().StringVar(&evalBaseline, "baseline", "opus", "baseline model alias")
	evalRunCmd.Flags().StringVar(&evalMUT, "mut", "auto", "model under test alias, including auto")
	evalRunCmd.Flags().StringVar(&evalJudge, "judge", "none", "judge model alias or none")
	evalRunCmd.Flags().IntVar(&evalAttempts, "attempts", 1, "attempts per task and candidate")
	evalRunCmd.Flags().StringSliceVar(&evalTasks, "task", nil, "task id filter (repeat or comma-separate)")
	evalRunCmd.Flags().DurationVar(&evalTimeout, "timeout", 30*time.Minute, "candidate and judge timeout")
	evalRunCmd.Flags().Uint64Var(&evalSeed, "seed", 1, "deterministic launch and blinding seed")
	evalRunCmd.Flags().StringVarP(&evalOutput, "output", "o", "", "artifact output directory")
	evalRunCmd.Flags().BoolVar(&evalResume, "resume", false, "resume completed pairs in the output directory")
	evalRunCmd.Flags().BoolVar(&evalJSON, "json", false, "machine-readable final summary")
	evalRunCmd.Flags().StringVar(&evalPython, "python", "python3", "Python interpreter for the SWE-bench bridge")
	evalRunCmd.Flags().StringVar(&evalDockerBin, "docker-bin", "docker", "Docker CLI for SWE-bench candidates")
	evalRunCmd.Flags().BoolVar(&evalKeepContainer, "keep-containers", false, "keep SWE-bench candidate containers for debugging")
	evalReportCmd.Flags().BoolVar(&evalReportJSON, "json", false, "machine-readable summary")
	evalCmd.AddCommand(evalRunCmd, evalReportCmd)
}
