package cmd

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/gremlord/gremlord/internal/config"
)

var (
	routeClassifier string
	routeDefault    string
	routeDeep       string
	routeStandard   string
	routeLight      string
	routeTasks      []string
)

var routingCmd = &cobra.Command{
	Use:   "routing",
	Short: "Dynamic tier routing (an LLM assigns each task to a model tier)",
}

var routingSetCmd = &cobra.Command{
	Use:   "set <alias>",
	Short: "Create or update a dynamic routing alias",
	Long: `A routing alias classifies each new user turn with a cheap model and
dispatches it to a tier. Use it like any model: /model auto, or
profiles: {model: auto}.`,
	Example: `  gremlord routing set auto --classifier haiku \
      --deep opus --standard sonnet --light qwen \
      --task security_review=fable --task critical_review=opus`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}
		existing := cfg.Routing[args[0]]
		classifier := routeClassifier
		if classifier == "" {
			classifier = existing.Classifier
		}
		if classifier == "" {
			return fmt.Errorf("--classifier is required (a cheap model alias)")
		}
		tiers := map[string]string{"deep": routeDeep, "standard": routeStandard, "light": routeLight}
		for tier, model := range existing.Tiers {
			if tiers[tier] == "" {
				tiers[tier] = model
			}
		}
		defaultTier := routeDefault
		if defaultTier == "" {
			defaultTier = existing.Default
		}
		snippet := "classifier: " + classifier + "\n"
		if defaultTier != "" {
			snippet += "default: " + defaultTier + "\n"
		}
		snippet += "tiers:\n"
		any := false
		for _, tier := range []string{"deep", "standard", "light"} {
			if tiers[tier] != "" {
				snippet += fmt.Sprintf("  %s: %s\n", tier, tiers[tier])
				any = true
			}
		}
		if !any {
			return fmt.Errorf("at least one of --deep / --standard / --light is required")
		}
		tasks, err := mergeTaskFlags(existing.Tasks, routeTasks)
		if err != nil {
			return err
		}
		if len(tasks) > 0 {
			labels := make([]string, 0, len(tasks))
			for l := range tasks {
				labels = append(labels, l)
			}
			sort.Strings(labels)
			snippet += "tasks:\n"
			for _, l := range labels {
				snippet += fmt.Sprintf("  %s: %s\n", l, tasks[l])
			}
		}
		return editConfig(func(doc *config.Doc) error {
			return doc.SetSubtree("routing", args[0], snippet)
		}, "routing "+args[0])
	},
}

// mergeTaskFlags parses repeatable --task label=model flags and merges them
// onto the alias's existing task mappings, so task-only updates preserve the
// rest of the routing rule.
func mergeTaskFlags(existing map[string]string, flags []string) (map[string]string, error) {
	merged := map[string]string{}
	for label, model := range existing {
		merged[label] = model
	}
	for _, f := range flags {
		label, model, ok := strings.Cut(f, "=")
		label = strings.TrimSpace(label)
		model = strings.TrimSpace(model)
		if !ok || label == "" {
			return nil, fmt.Errorf("--task must be label=model, got %q", f)
		}
		if !config.IsTaskLabel(label) {
			return nil, fmt.Errorf("--task: unknown label %q (want one of: %s)", label, strings.Join(config.TaskLabels, ", "))
		}
		if model == "" {
			delete(merged, label)
			continue
		}
		merged[label] = model
	}
	return merged, nil
}

var routingListCmd = &cobra.Command{
	Use:   "list",
	Short: "List routing aliases",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, _, err := loadConfig()
		if err != nil {
			return err
		}
		if len(cfg.Routing) == 0 {
			fmt.Println("no routing aliases — create one with `gremlord routing set`")
			return nil
		}
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ALIAS\tCLASSIFIER\tDEEP\tSTANDARD\tLIGHT\tDEFAULT\tTASKS")
		names := make([]string, 0, len(cfg.Routing))
		for n := range cfg.Routing {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			r := cfg.Routing[n]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n, r.Classifier,
				orDefault(r.Tiers["deep"], "—"), orDefault(r.Tiers["standard"], "—"),
				orDefault(r.Tiers["light"], "—"), orDefault(r.Default, "standard"),
				formatTasks(r.Tasks))
		}
		return tw.Flush()
	},
}

// formatTasks renders a routing rule's task mappings as a compact
// comma-separated "label=model" list for `routing list`, sorted for
// deterministic output. "—" when no tasks are configured.
func formatTasks(tasks map[string]string) string {
	if len(tasks) == 0 {
		return "—"
	}
	labels := make([]string, 0, len(tasks))
	for l := range tasks {
		labels = append(labels, l)
	}
	sort.Strings(labels)
	parts := make([]string, 0, len(labels))
	for _, l := range labels {
		parts = append(parts, l+"="+tasks[l])
	}
	return strings.Join(parts, ",")
}

var routingRemoveCmd = &cobra.Command{
	Use:   "remove <alias>",
	Short: "Remove a routing alias",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return editConfig(func(doc *config.Doc) error {
			return doc.Delete("routing", args[0])
		}, "removed routing "+args[0])
	},
}

func init() {
	routingSetCmd.Flags().StringVar(&routeClassifier, "classifier", "", "cheap model alias that assesses complexity")
	routingSetCmd.Flags().StringVar(&routeDefault, "default", "", "tier when classification fails (default: standard)")
	routingSetCmd.Flags().StringVar(&routeDeep, "deep", "", "model alias for planning/architecture/hard reasoning")
	routingSetCmd.Flags().StringVar(&routeStandard, "standard", "", "model alias for ordinary coding/tool work")
	routingSetCmd.Flags().StringVar(&routeLight, "light", "", "model alias for mechanical edits/verification")
	routingSetCmd.Flags().StringArrayVar(&routeTasks, "task", nil,
		"task→model override, repeatable: label=model; use label= to remove (e.g. --task security_review=fable); "+
			"labels: "+strings.Join(config.TaskLabels, ", "))
	routingCmd.AddCommand(routingSetCmd, routingListCmd, routingRemoveCmd)
	rootCmd.AddCommand(routingCmd)
}
