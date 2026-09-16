//go:build unix

// gpt-bench runs a paired local coding pilot: Claude Code through the current
// Gremlord Responses backend versus native Codex, using one metered API route.
// It does not claim to be SWE-bench or to isolate individual translator changes.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/backend/openaibe"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/eval"
	"github.com/gremlord/gremlord/internal/pricing"
	"github.com/gremlord/gremlord/internal/store"
	"github.com/gremlord/gremlord/internal/wire"
)

type measurement struct {
	Session                   string `json:"session"`
	Turn                      int    `json:"turn"`
	Arm                       string `json:"arm"`
	Request                   int    `json:"request"`
	Status                    int    `json:"status"`
	DurationMS                int64  `json:"duration_ms"`
	HeadersMS                 int64  `json:"response_headers_ms"`
	FirstOutputMS             *int64 `json:"first_output_ms,omitempty"`
	InstructionsBytes         int    `json:"instructions_bytes"`
	ToolSchemaBytes           int    `json:"tool_schema_bytes"`
	VisibleInputBytes         int    `json:"visible_input_bytes"`
	InstructionsSHA256        string `json:"instructions_sha256"`
	ToolSchemaSHA256          string `json:"tool_schema_sha256"`
	ExecutionProfile          string `json:"execution_profile,omitempty"`
	Input                     int64  `json:"input_tokens"`
	Cached                    int64  `json:"cached_input_tokens"`
	Output                    int64  `json:"output_tokens"`
	Reasoning                 int64  `json:"reasoning_tokens"`
	EncryptedInput            int    `json:"encrypted_reasoning_input_items"`
	EncryptedOutput           int    `json:"encrypted_reasoning_output_items"`
	Tools                     int    `json:"tool_calls"`
	Effort                    string `json:"effort"`
	ReasoningContext          string `json:"requested_reasoning_context"`
	EffectiveReasoningContext string `json:"effective_reasoning_context"`
	ResponsesLite             bool   `json:"responses_lite"`
	Parallel                  bool   `json:"parallel_tools"`
	ResponseStatus            string `json:"response_status"`
	Error                     string `json:"error,omitempty"`
}

type proxy struct {
	url, token string
	route      config.Resolved
	key        string
	backend    *openaibe.Backend
	client     *http.Client
	store      *store.Store
	prices     *pricing.Table
	log        *os.File
	mu         sync.Mutex
	arms       map[string]string
	counts     map[string]int
	turns      map[string]int
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	manifestPath := flag.String("manifest", "", "local eval manifest (required)")
	out := flag.String("out", "", "new artifact directory (required; no resume)")
	alias := flag.String("model", "gpt-5.6-sol", "configured Responses alias")
	attempts := flag.Int("attempts", 2, "paired repetitions per task")
	timeout := flag.Duration("timeout", 6*time.Minute, "time limit per candidate")
	seed := flag.Uint64("seed", 20260915, "paired launch-order seed")
	sequencePath := flag.String("sequence", "", "optional JSON array of user turns and external verifiers (one manifest task)")
	baseline := flag.String("baseline", "codex", "baseline arm: codex, gremlord, or gpt-efficient")
	mut := flag.String("mut", "gremlord", "comparison arm: codex, gremlord, or gpt-efficient")
	flag.Parse()
	if err := validateArms(*baseline, *mut); err != nil {
		return err
	}
	if *manifestPath == "" || *out == "" {
		return errors.New("-manifest and -out are required")
	}
	absOut, err := filepath.Abs(*out)
	if err != nil {
		return err
	}
	if _, err := os.Stat(absOut); !os.IsNotExist(err) {
		return errors.New("output directory must not already exist; use a fresh run")
	}
	manifest, err := eval.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	if manifest.IsDataset() {
		return errors.New("this pilot driver supports local manifests only")
	}
	sequence, err := loadSequence(*sequencePath, len(manifest.Tasks))
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dataDir := filepath.Join(home, ".gremlord")
	raw, err := os.ReadFile(filepath.Join(dataDir, "config.yaml"))
	if err != nil {
		return err
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return err
	}
	route, err := cfg.Resolve(*alias)
	if err != nil {
		return err
	}
	if route.Provider.Type != config.ProviderOpenAI || route.APIFlavor() != config.APIResponses {
		return errors.New("benchmark requires an OpenAI Responses alias")
	}
	if route.Provider.Key() == "" {
		return errors.New("configured provider has no key")
	}
	route.Model.Reasoning, route.Model.ReasoningEffort = "effort", "high"
	route.Model.ExecutionProfile = "" // only the selected arm may enable it
	route.Model.EffectiveContext = 600000
	claude, err := exec.LookPath("claude")
	if err != nil {
		return err
	}
	codex, err := exec.LookPath("codex")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absOut, 0700); err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(absOut, config.DBName))
	if err != nil {
		return err
	}
	defer db.Close()
	log, err := os.OpenFile(filepath.Join(absOut, "requests.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return err
	}
	p := &proxy{route: route, key: route.Provider.Key(), token: hex.EncodeToString(tokenBytes), backend: openaibe.New(), client: &http.Client{Transport: backend.NewTransport()}, store: db, prices: pricing.Load(dataDir, cfg), log: log, arms: map[string]string{}, counts: map[string]int{}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.url = "http://" + ln.Addr().String()
	server := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(ln)
	defer server.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	manifestBytes, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(absOut, "manifest.yaml"), manifestBytes, 0600); err != nil {
		return err
	}
	price, priced := p.prices.Get(route.Model.ID)
	meta := map[string]any{"model": route.Model.ID, "effort": "high", "context_budget": 600000, "max_output_tokens_per_request": 32768, "max_requests_per_candidate": 64, "seed": *seed, "attempts": *attempts, "timeout": timeout.String(), "claude_version": version(claude), "codex_version": version(codex), "manifest_sha256": fmt.Sprintf("%x", sha256.Sum256(manifestBytes)), "price_per_million": price, "priced": priced, "price_source": "local Gremlord pricing table; estimates, not invoices", "git_head": commandOutput("git", "rev-parse", "HEAD"), "git_diff_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(commandOutput("git", "diff", "HEAD")))), "scope": "local synthetic coding pilot; native harnesses, no web/MCP/subagents; not a compaction or parity proof"}
	meta["user_turns_per_candidate"] = max(1, len(sequence))
	meta["baseline"], meta["mut"] = *baseline, *mut
	meta["execution_profile_sha256"] = fmt.Sprintf("%x", sha256.Sum256([]byte(openaibe.GPTEfficientPrompt)))
	if err := os.WriteFile(filepath.Join(absOut, "execution-profile.txt"), []byte(openaibe.GPTEfficientPrompt), 0600); err != nil {
		return err
	}
	if exe, err := os.Executable(); err == nil {
		if data, err := os.ReadFile(exe); err == nil {
			meta["executable_sha256"] = fmt.Sprintf("%x", sha256.Sum256(data))
		}
	}
	for _, name := range []string{"main.go", "arms.go", "sequence.go", "fixtures.py", "complex.py"} {
		source, err := os.ReadFile(filepath.Join("scripts", "gpt-bench", name))
		if err != nil {
			return err
		}
		meta[name+"_sha256"] = fmt.Sprintf("%x", sha256.Sum256(source))
		if err := os.WriteFile(filepath.Join(absOut, "source-"+name), source, 0600); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(absOut, "environment.json"), meta); err != nil {
		return err
	}
	if len(sequence) > 0 {
		if err := writeJSON(filepath.Join(absOut, "sequence.json"), sequence); err != nil {
			return err
		}
	}
	runner := &eval.Runner{Options: eval.Options{Baseline: *baseline, MUT: *mut, Judge: "none", Attempts: *attempts, Timeout: *timeout, Seed: *seed, OutputDir: absOut, BaseURL: p.url, Token: p.token, Profile: "gpt-bench", ClaudeBin: claude, DataDir: absOut}, Exec: &executor{proxy: p, claude: claude, codex: codex, sequence: sequence}}
	runner.OnCandidate = func(c eval.CandidateResult) {
		fmt.Printf("%s %s pass=%t time=%.1fs requests=%d input=%d cached=%d output=%d estimated_usd=%.4f\n", c.Model, c.Status, c.Verifier.Passed, float64(c.AgentMS)/1000, c.Usage.Requests, c.Usage.InputTokens, c.Usage.CacheReadTokens, c.Usage.OutputTokens, c.Usage.CostUSD)
	}
	summary, err := runner.Run(ctx, manifest)
	if err != nil {
		return err
	}
	fmt.Printf("Completed %d pairs. Artifacts: %s\n", len(summary.Pairs), absOut)
	return nil
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+p.token && r.Header.Get("X-Api-Key") != p.token {
		http.Error(w, "unauthorized", 401)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/upstream/") {
		p.upstream(w, r)
		return
	}
	if r.URL.Path != "/v1/messages" && r.URL.Path != "/v1/messages/count_tokens" {
		http.NotFound(w, r)
		return
	}
	sid := wire.Session(r.Header)
	p.mu.Lock()
	arm := p.arms[sid]
	p.mu.Unlock()
	if !claudeArm(arm) {
		http.Error(w, "unknown session", 400)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	if err != nil {
		http.Error(w, "invalid body", 400)
		return
	}
	env, err := anthropic.ParseEnvelope(raw)
	if err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	route := p.route
	route.Model.ExecutionProfile = ""
	if arm == "gpt-efficient" {
		route.Model.ExecutionProfile = "gpt-efficient-v1"
	}
	route.Provider.BaseURL = p.url + "/upstream/" + sid + "/v1"
	route.Provider.APIKey, route.Provider.APIKeyEnv = p.token, ""
	call := &backend.Call{Raw: raw, Envelope: env, Route: route, Header: r.Header}
	if strings.HasSuffix(r.URL.Path, "count_tokens") {
		p.backend.CountTokens(r.Context(), call, w)
	} else {
		p.backend.Messages(r.Context(), call, w)
	}
}

func (p *proxy) upstream(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "v1" || parts[3] != "responses" || r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	sid := parts[1]
	p.mu.Lock()
	arm := p.arms[sid]
	turn := p.turns[sid]
	p.counts[sid]++
	n := p.counts[sid]
	p.mu.Unlock()
	if arm == "" {
		http.Error(w, "unknown session", 400)
		return
	}
	m := measurement{Session: sid, Turn: turn, Arm: arm, Request: n, Status: 502}
	m.ResponsesLite = r.Header.Get("X-Openai-Internal-Codex-Responses-Lite") == "true"
	start := time.Now()
	recorded := false
	record := func() {
		if recorded {
			return
		}
		recorded = true
		m.DurationMS = time.Since(start).Milliseconds()
		p.record(m)
	}
	defer record()
	if n > 64 {
		m.Status = 429
		m.Error = "benchmark request limit"
		http.Error(w, m.Error, m.Status)
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(&body); err != nil {
		m.Status = 400
		m.Error = "invalid request body"
		http.Error(w, m.Error, m.Status)
		return
	}
	var model string
	json.Unmarshal(body["model"], &model)
	var reasoning struct {
		Effort  string `json:"effort"`
		Context string `json:"context"`
	}
	json.Unmarshal(body["reasoning"], &reasoning)
	m.Effort = reasoning.Effort
	m.ReasoningContext = reasoning.Context
	json.Unmarshal(body["parallel_tool_calls"], &m.Parallel)
	measureInput(body, &m)
	var instructions string
	json.Unmarshal(body["instructions"], &instructions)
	if arm == "gpt-efficient" {
		m.ExecutionProfile = "gpt-efficient-v1"
	}
	if claudeArm(arm) && strings.Contains(instructions, openaibe.GPTEfficientPrompt) != (arm == "gpt-efficient") {
		m.Status, m.Error = 400, "execution profile differs from benchmark arm"
		http.Error(w, m.Error, m.Status)
		return
	}
	if model != p.route.Model.ID || m.Effort != "high" {
		m.Status = 400
		m.Error = "model or effort differs from benchmark contract"
		http.Error(w, m.Error, 400)
		return
	}
	var input []struct {
		Type      string `json:"type"`
		Encrypted string `json:"encrypted_content"`
	}
	json.Unmarshal(body["input"], &input)
	for _, it := range input {
		if it.Type == "reasoning" && it.Encrypted != "" {
			m.EncryptedInput++
		}
	}
	// A shared per-response output limit bounds spend without silently changing
	// the model or reasoning effort of either arm.
	body["max_output_tokens"] = json.RawMessage("32768")
	data, err := json.Marshal(body)
	if err != nil {
		m.Error = err.Error()
		http.Error(w, "marshal failure", 500)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "POST", strings.TrimSuffix(p.route.Provider.BaseURL, "/")+"/responses", bytes.NewReader(data))
	if err != nil {
		m.Error = err.Error()
		http.Error(w, "request failure", 500)
		return
	}
	copyProtocolHeaders(req.Header, r.Header)
	req.Header.Del("X-Api-Key")
	req.Header.Del("Content-Length")
	req.Header.Del("Content-Encoding")
	req.Header.Del("Accept-Encoding") // let Go negotiate/decompress for the meter
	req.Header.Set("Authorization", "Bearer "+p.key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		m.Error = err.Error()
		http.Error(w, "upstream transport failure", 502)
		return
	}
	defer resp.Body.Close()
	m.HeadersMS = time.Since(start).Milliseconds()
	m.Status = resp.StatusCode
	copyProtocolHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	w.WriteHeader(resp.StatusCode)
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			m.Error = err.Error()
		}
		if resp.StatusCode >= 400 {
			var failure struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			json.Unmarshal(data, &failure)
			m.Error = fmt.Sprintf("upstream HTTP %d: %s %s", resp.StatusCode, failure.Error.Type, failure.Error.Code)
		} else {
			measureResponse(data, &m)
		}
		record()
		w.Write(data)
		return
	}
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 65536), 16<<20)
	for scan.Scan() {
		line := scan.Bytes()
		if bytes.HasPrefix(line, []byte("data: ")) {
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
				Delta    json.RawMessage `json:"delta"`
			}
			if json.Unmarshal(line[6:], &event) == nil {
				if m.FirstOutputMS == nil && strings.HasSuffix(event.Type, ".delta") && len(event.Delta) > 0 && string(event.Delta) != `""` && string(event.Delta) != "null" {
					ms := time.Since(start).Milliseconds()
					m.FirstOutputMS = &ms
				}
				if event.Type == "response.completed" || event.Type == "response.incomplete" || event.Type == "response.failed" {
					measureResponse(event.Response, &m)
					record()
				}
			}
		}
		if _, err := w.Write(append(append([]byte(nil), line...), '\n')); err != nil {
			m.Error = err.Error()
			return
		}
		if len(line) == 0 {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
	if err := scan.Err(); err != nil {
		m.Error = err.Error()
	}
	if m.ResponseStatus == "" && m.Error == "" {
		m.Error = "stream ended without final response"
	}
}

func measureResponse(data []byte, m *measurement) {
	var response struct {
		Status    string `json:"status"`
		Reasoning struct {
			Context string `json:"context"`
		} `json:"reasoning"`
		Error json.RawMessage `json:"error"`
		Usage struct {
			Input        int64 `json:"input_tokens"`
			Output       int64 `json:"output_tokens"`
			InputDetails struct {
				Cached int64 `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputDetails struct {
				Reasoning int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		Output []struct {
			Type      string `json:"type"`
			Encrypted string `json:"encrypted_content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &response); err != nil {
		m.Error = "invalid upstream JSON"
		return
	}
	m.ResponseStatus = response.Status
	m.EffectiveReasoningContext = response.Reasoning.Context
	m.Input, m.Output, m.Cached, m.Reasoning = response.Usage.Input, response.Usage.Output, response.Usage.InputDetails.Cached, response.Usage.OutputDetails.Reasoning
	for _, it := range response.Output {
		if it.Type == "reasoning" && it.Encrypted != "" {
			m.EncryptedOutput++
		}
		if it.Type == "function_call" || it.Type == "custom_tool_call" {
			m.Tools++
		}
	}
	if response.Status != "completed" {
		m.Error = "response " + response.Status
	}
}

func (p *proxy) record(m measurement) {
	cost, priced := p.prices.Cost(p.route.Model.ID, m.Input-m.Cached, m.Output, m.Cached, 0)
	errType := ""
	if m.Error != "" {
		errType = "benchmark_upstream_error"
	}
	err := p.store.RecordUsage(store.UsageEvent{TS: time.Now(), SessionID: m.Session, Profile: "gpt-bench", Provider: p.route.ProviderName, Model: p.route.Model.ID, Alias: m.Arm, InputTokens: m.Input - m.Cached, OutputTokens: m.Output, CacheReadTokens: m.Cached, CostUSD: cost, Priced: priced, Status: m.Status, ErrType: errType, DurationMS: m.DurationMS, CtxBudget: 600000})
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		fmt.Fprintln(os.Stderr, "meter database:", err)
	}
	if err := json.NewEncoder(p.log).Encode(m); err != nil {
		fmt.Fprintln(os.Stderr, "meter log:", err)
	}
}

type executor struct {
	proxy         *proxy
	claude, codex string
	sequence      []sequenceTurn
}

func (e *executor) runTurn(ctx context.Context, dir string, env, argv []string, stdin io.Reader, stdout, stderr io.Writer, state *turnState) error {
	if len(argv) == 0 || argv[0] != e.claude {
		return runCommand(ctx, dir, env, argv, stdin, stdout, stderr)
	}
	values := map[string]string{}
	for _, entry := range env {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			values[k] = v
		}
	}
	arm, sid := values["ANTHROPIC_MODEL"], values["GREMLORD_SESSION_ID"]
	e.proxy.mu.Lock()
	e.proxy.arms[sid] = arm
	turn := 1
	if state != nil {
		turn = state.turn
	}
	if e.proxy.turns == nil {
		e.proxy.turns = map[string]int{}
	}
	e.proxy.turns[sid] = turn
	e.proxy.mu.Unlock()
	// Keep credentials only in the parent proxy. Both harnesses receive fresh
	// homes and the same short-lived, loopback-only benchmark token.
	clean := []string{"PATH=" + os.Getenv("PATH"), "LANG=en_US.UTF-8", "TMPDIR=" + os.TempDir(), "HOME=" + values["HOME"], "NO_COLOR=1"}
	prompt := argv[len(argv)-1]
	fmt.Printf("Starting %s user turn %d: %s\n", arm, turn, dir)
	artifactDir := filepath.Dir(dir)
	if state != nil {
		artifactDir = filepath.Join(artifactDir, fmt.Sprintf("turn-%02d", turn))
	}
	if claudeArm(arm) {
		for _, k := range []string{"ANTHROPIC_BASE_URL", "ANTHROPIC_CUSTOM_HEADERS", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_HAIKU_MODEL", "CLAUDE_CONFIG_DIR"} {
			clean = append(clean, k+"="+values[k])
		}
		clean = append(clean, "ANTHROPIC_API_KEY="+e.proxy.token, "ENABLE_TOOL_SEARCH=false", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
		args := append([]string(nil), argv[:len(argv)-1]...)
		for i := range args {
			if args[i] == "Task,WebSearch,WebFetch" {
				args[i] = "Task,Agent,WebSearch,WebFetch"
			}
		}
		args = append(args, "--safe-mode", "--disable-slash-commands", "--effort", "high")
		if state == nil {
			args = append(args, "--no-session-persistence")
		} else if state.turn == 1 {
			args = append(args, "--session-id", state.claudeSession)
		} else {
			args = append(args, "--resume", state.claudeSession)
		}
		args = append(args, prompt)
		var raw bytes.Buffer
		err := runCommand(ctx, dir, clean, args, stdin, io.MultiWriter(stdout, &raw), stderr)
		var result struct {
			IsError bool `json:"is_error"`
		}
		if err == nil && json.Unmarshal(raw.Bytes(), &result) == nil && result.IsError {
			return errors.New("Claude Code reported is_error; inspect stdout")
		}
		return err
	}
	if arm != "codex" {
		return fmt.Errorf("unknown harness %q", arm)
	}
	codexHome := filepath.Join(values["HOME"], ".codex")
	if err := os.MkdirAll(codexHome, 0700); err != nil {
		return err
	}
	clean = append(clean, "CODEX_HOME="+codexHome, "GREMLORD_BENCH_TOKEN="+e.proxy.token)
	finalPath := filepath.Join(artifactDir, "native-final.txt")
	args := []string{e.codex, "exec"}
	if state != nil && state.turn > 1 {
		if state.nativeThread == "" {
			return errors.New("missing native thread for resume")
		}
		args = append(args, "resume")
	}
	args = append(args, "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "-m", e.proxy.route.Model.ID, "-o", finalPath)
	if state == nil {
		args = append(args, "--ephemeral")
	}
	if state == nil || state.turn == 1 {
		args = append(args, "-C", dir)
	}
	overrides := []string{`model_provider="bench"`, `model_providers.bench.name="OpenAI"`, `model_providers.bench.base_url="` + e.proxy.url + "/upstream/" + sid + `/v1"`, `model_providers.bench.env_key="GREMLORD_BENCH_TOKEN"`, `model_providers.bench.wire_api="responses"`, `model_providers.bench.requires_openai_auth=false`, `model_providers.bench.request_max_retries=1`, `model_providers.bench.stream_max_retries=1`, `model_reasoning_effort="high"`, `model_context_window=600000`, `model_auto_compact_token_limit=540000`, `web_search="disabled"`, `agents.enabled=false`, `features.multi_agent=false`, `features.multi_agent_v2=false`, `features.responses_websockets=false`, `features.responses_websockets_v2=false`, `features.request_compression=false`, `features.skills=false`, `features.apps=false`, `features.plugins=false`, `shell_environment_policy.inherit="all"`}
	for _, c := range overrides {
		args = append(args, "-c", c)
	}
	if state != nil && state.turn > 1 {
		args = append(args, state.nativeThread)
	}
	args = append(args, "-")
	var raw bytes.Buffer
	events, err := os.OpenFile(filepath.Join(artifactDir, "native-events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer events.Close()
	err = runCommand(ctx, dir, clean, args, strings.NewReader(prompt), io.MultiWriter(&raw, events), stderr)
	if state != nil {
		for _, line := range bytes.Split(raw.Bytes(), []byte{'\n'}) {
			var event struct {
				Type   string `json:"type"`
				Thread string `json:"thread_id"`
			}
			if json.Unmarshal(line, &event) == nil && event.Type == "thread.started" {
				if state.nativeThread != "" && state.nativeThread != event.Thread {
					return errors.New("native resume changed thread identity")
				}
				state.nativeThread = event.Thread
			}
		}
	}
	final, _ := os.ReadFile(finalPath)
	json.NewEncoder(stdout).Encode(map[string]string{"result": string(final)})
	if err == nil && !bytes.Contains(raw.Bytes(), []byte(`"type":"turn.completed"`)) {
		return errors.New("native Codex exited without turn.completed; inspect native-events.jsonl")
	}
	return err
}

func runCommand(ctx context.Context, dir string, env, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdin, cmd.Stdout, cmd.Stderr = dir, env, stdin, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd.Run()
}

func version(path string) string { return commandOutput(path, "--version") }

// Preserve native API semantics (Responses Lite, turn-state, cache affinity,
// request IDs, etc.) in both directions. Only hop-by-hop fields are removed.
func copyProtocolHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	for _, line := range src.Values("Connection") {
		for _, k := range strings.Split(line, ",") {
			dst.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		dst.Del(k)
	}
}
func commandOutput(name string, args ...string) string {
	b, _ := exec.Command(name, args...).Output()
	return strings.TrimSpace(string(b))
}
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0600)
}
