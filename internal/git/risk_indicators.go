package git

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"deploy-api/internal/domain"
)

var (
	testPathPattern = regexp.MustCompile(`(?i)(_test\.go$|\.(spec|test)\.[cm]?[jt]sx?$|(^|/)(__tests__|test|tests|e2e|fixtures?|testdata)/)`)

	migrationPathPattern = regexp.MustCompile(`(?i)(` +
		`(^|/)(migrations?|migrate)/` +
		`|\.sql$` +
		`|(^|/)schema\.prisma$` +
		`|(^|/)alembic/versions/` +
		`|(^|/)db/(changelog/|schema\.rb$|structure\.sql$)` +
		`|(^|/)\d{10,}-[\w-]+\.[cm]?[jt]s$` +
		`)`)

	contractPathPattern = regexp.MustCompile(`(?i)(` +
		`\.(proto|graphql|gql|avsc)$` +
		`|(^|/)[^/]*(openapi|swagger)[^/]*\.(ya?ml|json)$` +
		`|\.schema\.json$` +
		`|(^|/)(contracts?|events|schemas?|dtos?)/` +
		`|\.(dto|controller|routes?|gateway|consumer|producer|publisher|subscriber)\.[cm]?[jt]s$` +
		`|_handler(_gin)?\.go$` +
		`|(^|/)router[^/]*\.\w+$` +
		`)`)

	configPathPattern = regexp.MustCompile(`(?i)(` +
		`(^|/)\.env(\.[\w.-]+)?$` +
		`|(^|/)configs?/` +
		`|(^|/)[^/]*config[^/]*\.(ya?ml|json|toml|ini)$` +
		`|(^|/)(k8s|kubernetes|kustomize|helm|charts|overlays?|deploy)/` +
		`|(^|/)kustomization\.ya?ml$` +
		`|(^|/)values[^/]*\.ya?ml$` +
		`|(^|/)[^/]*secrets?[^/]*\.(ya?ml|json)$` +
		`)`)

	// Matched on the basename: build and lint tooling, not runtime config.
	buildToolConfigPattern = regexp.MustCompile(`(?i)^\.?(tsconfig|jsconfig|jest|vitest|eslint(rc)?|prettier(rc)?|babel(rc)?|vite|webpack|rollup|tailwind|postcss|nx|turbo|lerna|renovate|commitlint|cspell|nodemon|swcrc|nest-cli|biome|karma|cypress|playwright|stylelint(rc)?|lint-staged(rc)?)([.-][^/]*)?$`)

	sharedLibPathPattern = regexp.MustCompile(`(?i)(^|/)(shared|common|libs?|pkg)/`)
	breakingMsgPattern   = regexp.MustCompile(`(?im)(BREAKING[ -]CHANGE|bc-break|^\w+(\([^)]*\))?!:)`)

	envReadPattern = regexp.MustCompile(`(?:process\.env\.([A-Z][A-Z0-9_]+)` +
		`|process\.env\[['"]([A-Z][A-Z0-9_]+)['"]\]` +
		`|os\.(?:Getenv|LookupEnv)\("([A-Z][A-Z0-9_]+)"\)` +
		`|(?:configService|config)\.(?:get|getOrThrow)(?:<[^>]*>)?\(\s*['"]([A-Z][A-Z0-9_]+)['"])`)
)

// computeRiskIndicators flags a change by the paths it touches (files, already
// scoped to the app's source paths) plus the env vars its added lines read.
func computeRiskIndicators(commits []domain.CommitInfo, files, envVarsAdded []string) domain.RiskIndicators {
	ind := domain.RiskIndicators{}

	for _, c := range commits {
		if breakingMsgPattern.MatchString(c.Message) {
			ind.HasBreakingChange = true
			break
		}
	}

	for _, f := range files {
		if testPathPattern.MatchString(f) {
			continue
		}
		if sharedLibPathPattern.MatchString(f) {
			ind.TouchesSharedLib = true
		}
		if migrationPathPattern.MatchString(f) {
			ind.MigrationFiles = append(ind.MigrationFiles, f)
		}
		if contractPathPattern.MatchString(f) {
			ind.ContractFiles = append(ind.ContractFiles, f)
		}
		if configPathPattern.MatchString(f) && !buildToolConfigPattern.MatchString(path.Base(f)) {
			ind.ConfigFiles = append(ind.ConfigFiles, f)
		}
	}

	ind.EnvVarsAdded = envVarsAdded

	ind.HasMigration = len(ind.MigrationFiles) > 0
	ind.HasContractChange = len(ind.ContractFiles) > 0
	ind.HasConfigChange = len(ind.ConfigFiles) > 0 || len(ind.EnvVarsAdded) > 0
	return ind
}

// envReadScanner collects env vars read on added lines but not on removed
// ones, so moving a read around does not count as a new requirement.
type envReadScanner struct {
	added, removed map[string]bool
}

func newEnvReadScanner() *envReadScanner {
	return &envReadScanner{added: map[string]bool{}, removed: map[string]bool{}}
}

// scan reads unified diff text, one or more files. Only lines inside a hunk
// are content, so a body line like "--- x" is never mistaken for a header.
func (s *envReadScanner) scan(diff string) {
	inHunk, skipFile := false, false

	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
			skipFile = false
			if i := strings.LastIndex(line, " b/"); i >= 0 {
				skipFile = testPathPattern.MatchString(line[i+3:])
			}
			continue
		case strings.HasPrefix(line, "@@"):
			inHunk = true
			continue
		case !inHunk:
			if p, ok := strings.CutPrefix(line, "+++ b/"); ok {
				skipFile = testPathPattern.MatchString(p)
			}
			continue
		case skipFile:
			continue
		}

		var into map[string]bool
		switch {
		case strings.HasPrefix(line, "+"):
			into = s.added
		case strings.HasPrefix(line, "-"):
			into = s.removed
		default:
			continue
		}
		for _, m := range envReadPattern.FindAllStringSubmatch(line, -1) {
			for _, name := range m[1:] {
				if name != "" {
					into[name] = true
				}
			}
		}
	}
}

func (s *envReadScanner) vars() []string {
	var out []string
	for name := range s.added {
		if !s.removed[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func addedEnvVars(diff string) []string {
	s := newEnvReadScanner()
	s.scan(diff)
	return s.vars()
}
