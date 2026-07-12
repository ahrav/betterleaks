package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const planSchemaVersion = 1

type plan struct {
	SchemaVersion                 int               `json:"schema_version"`
	StudyID                       string            `json:"study_id"`
	Phase                         string            `json:"phase"`
	Seed                          int64             `json:"seed"`
	Binary                        string            `json:"binary"`
	BinarySHA256                  string            `json:"binary_sha256,omitempty"`
	WorkingDirectory              string            `json:"working_directory"`
	CommonArgs                    []string          `json:"common_args,omitempty"`
	InheritEnvironment            bool              `json:"inherit_environment"`
	Environment                   map[string]string `json:"environment"`
	Artifacts                     []artifact        `json:"artifacts,omitempty"`
	FixedHorizonBlocks            int               `json:"fixed_horizon_blocks"`
	SampleIntervalMS              int               `json:"sample_interval_ms"`
	TimeoutSeconds                int               `json:"timeout_seconds"`
	RequireScannerMetrics         bool              `json:"require_scanner_metrics"`
	MaxInfrastructureReplacements int               `json:"max_infrastructure_replacement_blocks"`
	PreRunHooks                   []hook            `json:"pre_run_hooks,omitempty"`
	BaselineCandidate             string            `json:"baseline_candidate"`
	Candidates                    []candidate       `json:"candidates"`
	Strata                        []stratum         `json:"strata"`
	Analysis                      analysisConfig    `json:"analysis"`
}

type artifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type candidate struct {
	ID               string            `json:"id"`
	MetricsCandidate string            `json:"metrics_candidate,omitempty"`
	Args             []string          `json:"args,omitempty"`
	Environment      map[string]string `json:"environment,omitempty"`
}

type stratum struct {
	ID                  string   `json:"id"`
	WorkloadClass       string   `json:"workload_class"`
	EvidenceRole        string   `json:"evidence_role"`
	CorpusManifest      string   `json:"corpus_manifest"`
	ManifestSHA256      string   `json:"manifest_sha256,omitempty"`
	ScheduleSHA256      string   `json:"schedule_sha256,omitempty"`
	Root                string   `json:"root"`
	CacheState          string   `json:"cache_state"`
	ScannerArgs         []string `json:"scanner_args"`
	AllowedExitCodes    []int    `json:"allowed_exit_codes,omitempty"`
	PrepareHooks        []hook   `json:"prepare_hooks,omitempty"`
	SkipLiveGuard       bool     `json:"skip_live_guard,omitempty"`
	AllowManifestErrors bool     `json:"allow_manifest_errors,omitempty"`
}

type hook struct {
	ID             string            `json:"id"`
	Command        []string          `json:"command"`
	Environment    map[string]string `json:"environment,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}

type analysisConfig struct {
	MinimumWorthwhileEffectPct      float64 `json:"minimum_worthwhile_effect_pct"`
	AlphaFamilywise                 float64 `json:"alpha_familywise"`
	Power                           float64 `json:"power"`
	WinnerClaims                    int     `json:"winner_claims"`
	VarianceInflation               float64 `json:"pilot_variance_inflation"`
	MinimumConfirmationBlocks       int     `json:"minimum_confirmation_blocks"`
	EquivalenceMarginPct            float64 `json:"equivalence_margin_pct"`
	CPUMaterialImprovementPct       float64 `json:"cpu_material_improvement_pct"`
	RSSMaterialImprovementPct       float64 `json:"rss_material_improvement_pct"`
	PageCacheMaterialImprovementPct float64 `json:"page_cache_material_improvement_pct"`
	BootstrapResamples              int     `json:"bootstrap_resamples"`
}

func loadPlan(path string) (plan, string, error) {
	loaded, digest, err := decodePlan(path)
	if err != nil {
		return plan{}, "", err
	}
	if err := loaded.validate(); err != nil {
		return plan{}, "", err
	}
	return loaded, digest, nil
}

func decodePlan(path string) (plan, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return plan{}, "", fmt.Errorf("read plan: %w", err)
	}
	digest := sha256Hex(data)
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var loaded plan
	if err := decoder.Decode(&loaded); err != nil {
		return plan{}, "", fmt.Errorf("decode plan: %w", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return plan{}, "", errors.New("decode plan: trailing JSON value")
		}
		return plan{}, "", fmt.Errorf("decode plan trailing data: %w", err)
	}
	return loaded, digest, nil
}

func scheduleDigestsForDraft(loaded plan) (map[string]string, error) {
	// Supply only the provenance pins needed to validate a draft. Final plans
	// still have to declare their real values before execution.
	if loaded.Phase != "calibration" {
		if loaded.BinarySHA256 == "" {
			loaded.BinarySHA256 = strings.Repeat("0", 64)
		}
		if loaded.Artifacts == nil {
			loaded.Artifacts = []artifact{}
		}
		for index := range loaded.Strata {
			if loaded.Strata[index].ManifestSHA256 == "" {
				loaded.Strata[index].ManifestSHA256 = strings.Repeat("0", 64)
			}
			schedule, err := buildSchedule(
				loaded.Candidates, loaded.FixedHorizonBlocks, loaded.Seed,
				loaded.Strata[index].ID, loaded.Phase,
			)
			if err != nil {
				return nil, err
			}
			loaded.Strata[index].ScheduleSHA256 = scheduleSHA256(schedule)
		}
	}
	if err := loaded.validate(); err != nil {
		return nil, err
	}
	digests := make(map[string]string, len(loaded.Strata))
	for _, stratum := range loaded.Strata {
		schedule, err := buildSchedule(loaded.Candidates, loaded.FixedHorizonBlocks, loaded.Seed, stratum.ID, loaded.Phase)
		if err != nil {
			return nil, err
		}
		digests[stratum.ID] = scheduleSHA256(schedule)
	}
	return digests, nil
}

func (p plan) validate() error {
	if p.SchemaVersion != planSchemaVersion {
		return fmt.Errorf("plan schema_version %d, want %d", p.SchemaVersion, planSchemaVersion)
	}
	if !validID(p.StudyID) {
		return errors.New("plan study_id must contain only letters, digits, '.', '_', or '-'")
	}
	switch p.Phase {
	case "calibration", "pilot", "confirmation":
	default:
		return fmt.Errorf("plan phase %q is invalid", p.Phase)
	}
	if !filepath.IsAbs(p.Binary) {
		return errors.New("plan binary must be absolute")
	}
	if p.BinarySHA256 != "" {
		if err := validateSHA256("binary_sha256", p.BinarySHA256); err != nil {
			return err
		}
	}
	if p.Phase != "calibration" {
		if p.BinarySHA256 == "" {
			return fmt.Errorf("%s plans require binary_sha256", p.Phase)
		}
		if p.Artifacts == nil {
			return fmt.Errorf("%s plans require an explicit artifacts array", p.Phase)
		}
		if p.InheritEnvironment {
			return fmt.Errorf("%s plans must set inherit_environment=false", p.Phase)
		}
	}
	if !filepath.IsAbs(p.WorkingDirectory) {
		return errors.New("plan working_directory must be absolute")
	}
	if p.FixedHorizonBlocks < 1 {
		return errors.New("fixed_horizon_blocks must be positive")
	}
	if p.SampleIntervalMS < 10 || p.SampleIntervalMS > 5000 {
		return errors.New("sample_interval_ms must be between 10 and 5000")
	}
	if p.TimeoutSeconds < 1 {
		return errors.New("timeout_seconds must be positive")
	}
	if p.MaxInfrastructureReplacements < 0 {
		return errors.New("max_infrastructure_replacement_blocks must be nonnegative")
	}
	if err := validateEnvironment("environment", p.Environment); err != nil {
		return err
	}
	if _, exists := p.Environment[metricsFDEnvironment]; exists {
		return fmt.Errorf("plan environment must not set %s", metricsFDEnvironment)
	}
	if option := firstFileScanOption(p.CommonArgs); option != "" {
		return fmt.Errorf("plan common_args must not set candidate option %q", option)
	}
	if len(p.Candidates) < 2 {
		return errors.New("plan requires at least two candidates")
	}
	seenCandidates := make(map[string]struct{}, len(p.Candidates))
	baselineFound := false
	for i, candidate := range p.Candidates {
		if !validID(candidate.ID) {
			return fmt.Errorf("candidates[%d].id is invalid", i)
		}
		if _, exists := seenCandidates[candidate.ID]; exists {
			return fmt.Errorf("duplicate candidate id %q", candidate.ID)
		}
		seenCandidates[candidate.ID] = struct{}{}
		baselineFound = baselineFound || candidate.ID == p.BaselineCandidate
		if err := validateEnvironment("candidate environment", candidate.Environment); err != nil {
			return fmt.Errorf("candidate %q: %w", candidate.ID, err)
		}
		if _, exists := candidate.Environment[metricsFDEnvironment]; exists {
			return fmt.Errorf("candidate %q must not set %s", candidate.ID, metricsFDEnvironment)
		}
		if strings.IndexByte(candidate.MetricsCandidate, 0) >= 0 {
			return fmt.Errorf("candidate %q metrics_candidate contains a NUL byte", candidate.ID)
		}
		if p.RequireScannerMetrics && strings.TrimSpace(candidate.MetricsCandidate) == "" {
			return fmt.Errorf("candidate %q must declare metrics_candidate when scanner metrics are required", candidate.ID)
		}
		if err := validateCandidateFileScanArgs(candidate.Args, p.RequireScannerMetrics); err != nil {
			return fmt.Errorf("candidate %q: %w", candidate.ID, err)
		}
	}
	if !baselineFound {
		return fmt.Errorf("baseline candidate %q is not in candidates", p.BaselineCandidate)
	}
	cycle := scheduleCycleLength(len(p.Candidates))
	if p.Phase != "calibration" && p.FixedHorizonBlocks%cycle != 0 {
		return fmt.Errorf("fixed_horizon_blocks %d must be a multiple of balanced schedule cycle %d", p.FixedHorizonBlocks, cycle)
	}
	if len(p.Strata) == 0 {
		return errors.New("plan requires at least one stratum")
	}
	seenStrata := make(map[string]struct{}, len(p.Strata))
	for i, stratum := range p.Strata {
		if !validID(stratum.ID) || stratum.WorkloadClass == "" {
			return fmt.Errorf("strata[%d] id and workload_class are required", i)
		}
		switch stratum.EvidenceRole {
		case "performance", "correctness":
		default:
			return fmt.Errorf("stratum %q evidence_role %q is invalid", stratum.ID, stratum.EvidenceRole)
		}
		if _, exists := seenStrata[stratum.ID]; exists {
			return fmt.Errorf("duplicate stratum id %q", stratum.ID)
		}
		seenStrata[stratum.ID] = struct{}{}
		if !filepath.IsAbs(stratum.CorpusManifest) || !filepath.IsAbs(stratum.Root) {
			return fmt.Errorf("stratum %q corpus_manifest and root must be absolute", stratum.ID)
		}
		if stratum.ManifestSHA256 != "" {
			if err := validateSHA256(fmt.Sprintf("stratum %q manifest_sha256", stratum.ID), stratum.ManifestSHA256); err != nil {
				return err
			}
		}
		if stratum.ScheduleSHA256 != "" {
			if err := validateSHA256(fmt.Sprintf("stratum %q schedule_sha256", stratum.ID), stratum.ScheduleSHA256); err != nil {
				return err
			}
		}
		if p.Phase != "calibration" {
			if stratum.ManifestSHA256 == "" {
				return fmt.Errorf("%s stratum %q requires manifest_sha256", p.Phase, stratum.ID)
			}
			if stratum.ScheduleSHA256 == "" {
				return fmt.Errorf("%s stratum %q requires schedule_sha256", p.Phase, stratum.ID)
			}
		}
		schedule, err := buildSchedule(p.Candidates, p.FixedHorizonBlocks, p.Seed, stratum.ID, p.Phase)
		if err != nil {
			return fmt.Errorf("stratum %q schedule: %w", stratum.ID, err)
		}
		if stratum.ScheduleSHA256 != "" {
			actual := scheduleSHA256(schedule)
			if actual != stratum.ScheduleSHA256 {
				return fmt.Errorf("stratum %q schedule_sha256 %s, want %s", stratum.ID, stratum.ScheduleSHA256, actual)
			}
		}
		if stratum.CacheState == "" || len(stratum.ScannerArgs) == 0 {
			return fmt.Errorf("stratum %q cache_state and scanner_args are required", stratum.ID)
		}
		if option := firstFileScanOption(stratum.ScannerArgs); option != "" {
			return fmt.Errorf("stratum %q scanner_args must not override candidate option %q", stratum.ID, option)
		}
		allowed := stratum.effectiveAllowedExitCodes()
		if stratum.AllowedExitCodes != nil && len(stratum.AllowedExitCodes) == 0 {
			return fmt.Errorf("stratum %q allowed_exit_codes must not be empty", stratum.ID)
		}
		seenExitCodes := make(map[int]struct{}, len(allowed))
		for _, code := range allowed {
			if code < 0 || code > 255 {
				return fmt.Errorf("stratum %q allowed exit code %d is outside [0,255]", stratum.ID, code)
			}
			if _, exists := seenExitCodes[code]; exists {
				return fmt.Errorf("stratum %q has duplicate allowed exit code %d", stratum.ID, code)
			}
			seenExitCodes[code] = struct{}{}
		}
		if stratum.EvidenceRole == "performance" && (len(allowed) != 1 || allowed[0] != 0) {
			return fmt.Errorf("performance stratum %q must allow only exit code 0", stratum.ID)
		}
		for _, hook := range stratum.PrepareHooks {
			if err := hook.validate(); err != nil {
				return fmt.Errorf("stratum %q: %w", stratum.ID, err)
			}
		}
	}
	for _, hook := range p.PreRunHooks {
		if err := hook.validate(); err != nil {
			return err
		}
	}
	artifactPaths := make(map[string]struct{}, len(p.Artifacts))
	for i, artifact := range p.Artifacts {
		if !filepath.IsAbs(artifact.Path) {
			return fmt.Errorf("artifacts[%d].path must be absolute", i)
		}
		if err := validateSHA256(fmt.Sprintf("artifacts[%d].sha256", i), artifact.SHA256); err != nil {
			return err
		}
		cleaned := filepath.Clean(artifact.Path)
		if _, exists := artifactPaths[cleaned]; exists {
			return fmt.Errorf("duplicate artifact path %q", cleaned)
		}
		artifactPaths[cleaned] = struct{}{}
	}
	if p.Phase != "calibration" {
		for _, hook := range p.allHooks() {
			if _, pinned := artifactPaths[filepath.Clean(hook.Command[0])]; !pinned {
				return fmt.Errorf("%s hook %q executable %q is not pinned in artifacts", p.Phase, hook.ID, hook.Command[0])
			}
		}
		configPaths, err := p.configPaths()
		if err != nil {
			return err
		}
		for _, path := range configPaths {
			if !filepath.IsAbs(path) {
				return fmt.Errorf("config path %q must be absolute", path)
			}
			if _, pinned := artifactPaths[filepath.Clean(path)]; !pinned {
				return fmt.Errorf("%s config %q is not pinned in artifacts", p.Phase, path)
			}
		}
	}
	if err := p.Analysis.validate(); err != nil {
		return err
	}
	if p.Phase == "pilot" && p.FixedHorizonBlocks < 6 {
		return errors.New("pilot plans require at least 6 balanced blocks")
	}
	if p.Phase == "confirmation" {
		if !p.RequireScannerMetrics {
			return errors.New("confirmation plans must require scanner metrics and correctness oracles")
		}
		for _, stratum := range p.Strata {
			if stratum.SkipLiveGuard {
				return fmt.Errorf("confirmation stratum %q must not skip the live guard", stratum.ID)
			}
		}
		if p.FixedHorizonBlocks < p.Analysis.MinimumConfirmationBlocks {
			return fmt.Errorf(
				"confirmation fixed_horizon_blocks %d is below minimum_confirmation_blocks %d",
				p.FixedHorizonBlocks, p.Analysis.MinimumConfirmationBlocks,
			)
		}
	}
	return nil
}

func firstFileScanOption(arguments []string) string {
	for _, argument := range arguments {
		if option := fileScanOptionKey(argument); option != "" {
			return option
		}
	}
	return ""
}

var fileScanValueOptions = map[string]struct{}{
	"--filescan-active-files":     {},
	"--filescan-advice":           {},
	"--filescan-backend":          {},
	"--filescan-detector-workers": {},
	"--filescan-max-inflight-mib": {},
	"--filescan-mmap-window-mib":  {},
	"--filescan-namespace":        {},
	"--filescan-prefetch-kib":     {},
	"--filescan-queue-depth":      {},
	"--filescan-read-mode":        {},
	"--filescan-walkers":          {},
}

var fileScanBooleanOptions = map[string]struct{}{
	"--filescan-inline-detector":    {},
	"--filescan-registered-buffers": {},
	"--filescan-strict":             {},
}

func validateCandidateFileScanArgs(arguments []string, requireBackend bool) error {
	seen := make(map[string]struct{})
	for _, argument := range arguments {
		option := fileScanOptionKey(argument)
		if option == "" {
			return fmt.Errorf("argument %q is not a filesystem-scanner experiment option", argument)
		}
		if _, exists := seen[option]; exists {
			return fmt.Errorf("sets %q more than once", option)
		}
		seen[option] = struct{}{}

		_, value, hasValue := strings.Cut(argument, "=")
		if _, exists := fileScanValueOptions[option]; exists {
			if !hasValue || value == "" {
				return fmt.Errorf("%q must use the canonical --option=value form", option)
			}
			continue
		}
		if _, exists := fileScanBooleanOptions[option]; exists {
			if hasValue && value != "true" && value != "false" {
				return fmt.Errorf("%q boolean value must be true or false", option)
			}
			continue
		}
		return fmt.Errorf("unknown filesystem-scanner experiment option %q", option)
	}
	if requireBackend {
		if _, exists := seen["--filescan-backend"]; !exists {
			return errors.New("scanner metrics require an explicit --filescan-backend option")
		}
	}
	return nil
}

func fileScanOptionKey(argument string) string {
	if !strings.HasPrefix(argument, "--filescan-") {
		return ""
	}
	if separator := strings.IndexByte(argument, '='); separator >= 0 {
		return argument[:separator]
	}
	return argument
}

func (p plan) allHooks() []hook {
	hooks := append([]hook(nil), p.PreRunHooks...)
	for _, stratum := range p.Strata {
		hooks = append(hooks, stratum.PrepareHooks...)
	}
	return hooks
}

func (p plan) configPaths() ([]string, error) {
	argumentSets := [][]string{p.CommonArgs}
	for _, candidate := range p.Candidates {
		argumentSets = append(argumentSets, candidate.Args)
	}
	for _, stratum := range p.Strata {
		argumentSets = append(argumentSets, stratum.ScannerArgs)
	}
	for _, hook := range p.allHooks() {
		argumentSets = append(argumentSets, hook.Command)
	}
	paths := make([]string, 0, 1)
	for _, arguments := range argumentSets {
		observed, err := configPathsFromArgs(arguments)
		if err != nil {
			return nil, err
		}
		paths = append(paths, observed...)
	}
	environments := append([]map[string]string{p.Environment}, candidateEnvironments(p.Candidates)...)
	for _, hook := range p.allHooks() {
		environments = append(environments, hook.Environment)
	}
	for _, environment := range environments {
		for _, key := range []string{"BETTERLEAKS_CONFIG", "GITLEAKS_CONFIG"} {
			if path := strings.TrimSpace(environment[key]); path != "" {
				paths = append(paths, path)
			}
		}
	}
	return paths, nil
}

func candidateEnvironments(candidates []candidate) []map[string]string {
	environments := make([]map[string]string, 0, len(candidates))
	for _, candidate := range candidates {
		environments = append(environments, candidate.Environment)
	}
	return environments
}

func configPathsFromArgs(arguments []string) ([]string, error) {
	paths := make([]string, 0, 1)
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		switch {
		case argument == "--config" || argument == "-c":
			if index+1 == len(arguments) || strings.TrimSpace(arguments[index+1]) == "" {
				return nil, fmt.Errorf("config flag %q requires a path", argument)
			}
			index++
			paths = append(paths, arguments[index])
		case strings.HasPrefix(argument, "--config="):
			path := strings.TrimPrefix(argument, "--config=")
			if strings.TrimSpace(path) == "" {
				return nil, errors.New("--config requires a path")
			}
			paths = append(paths, path)
		case strings.HasPrefix(argument, "-c="):
			path := strings.TrimPrefix(argument, "-c=")
			if strings.TrimSpace(path) == "" {
				return nil, errors.New("-c requires a path")
			}
			paths = append(paths, path)
		}
	}
	return paths, nil
}

func (s stratum) effectiveAllowedExitCodes() []int {
	if s.AllowedExitCodes == nil {
		return []int{0}
	}
	return append([]int(nil), s.AllowedExitCodes...)
}

func (h hook) validate() error {
	if !validID(h.ID) || len(h.Command) == 0 || !filepath.IsAbs(h.Command[0]) {
		return errors.New("hook id and absolute command are required")
	}
	if h.TimeoutSeconds < 1 {
		return fmt.Errorf("hook %q timeout_seconds must be positive", h.ID)
	}
	return validateEnvironment("hook environment", h.Environment)
}

func (a analysisConfig) validate() error {
	if a.MinimumWorthwhileEffectPct <= 0 || a.MinimumWorthwhileEffectPct >= 100 {
		return errors.New("analysis minimum_worthwhile_effect_pct must be in (0,100)")
	}
	if a.AlphaFamilywise <= 0 || a.AlphaFamilywise >= 1 || a.Power <= 0 || a.Power >= 1 {
		return errors.New("analysis alpha_familywise and power must be in (0,1)")
	}
	if a.WinnerClaims < 1 || a.VarianceInflation < 1 || a.MinimumConfirmationBlocks < 2 {
		return errors.New("analysis winner_claims, variance inflation, and minimum blocks are invalid")
	}
	for name, value := range map[string]float64{
		"equivalence_margin_pct":              a.EquivalenceMarginPct,
		"cpu_material_improvement_pct":        a.CPUMaterialImprovementPct,
		"rss_material_improvement_pct":        a.RSSMaterialImprovementPct,
		"page_cache_material_improvement_pct": a.PageCacheMaterialImprovementPct,
	} {
		if value <= 0 || value >= 100 {
			return fmt.Errorf("analysis %s must be in (0,100)", name)
		}
	}
	if a.BootstrapResamples < 1000 {
		return errors.New("analysis bootstrap_resamples must be at least 1000")
	}
	return nil
}

func validateEnvironment(name string, environment map[string]string) error {
	for key, value := range environment {
		if key == "" || strings.ContainsRune(key, '=') || strings.IndexByte(key, 0) >= 0 {
			return fmt.Errorf("%s contains invalid key %q", name, key)
		}
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("%s value for %q contains a NUL byte", name, key)
		}
	}
	return nil
}

func validateSHA256(name, digest string) error {
	decoded, err := hex.DecodeString(digest)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("%s must be 64 hexadecimal characters", name)
	}
	if digest != strings.ToLower(digest) {
		return fmt.Errorf("%s must use lowercase hexadecimal", name)
	}
	return nil
}

func validID(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}
