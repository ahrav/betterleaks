package rules

import (
	"github.com/betterleaks/betterleaks/cmd/generate/config/utils"
	"github.com/betterleaks/betterleaks/cmd/generate/secrets"
	"github.com/betterleaks/betterleaks/config"
	"github.com/betterleaks/betterleaks/regexp"
)

const githubTokenCEL = `cel.bind(r,
  http.get("https://api.github.com/user", {
    "Accept": "application/vnd.github+json",
    "Authorization": "token " + secret
  }),
  r.status == 200 && safeGet(r.json, "login", "") != "" ? {
    "result": "valid",
    "username": safeGet(r.json, "login", ""),
    "name": safeGet(r.json, "name", ""),
    "scopes": safeGet(r.headers, "x-oauth-scopes", "")
  } : r.status in [401, 403] ? {
    "result": "invalid",
    "reason": "Unauthorized"
  } : unknown(r)
)`

var githubAllowlist = []*config.Allowlist{
	{
		Paths: []*regexp.Regexp{
			// https://github.com/octokit/auth-token.js/?tab=readme-ov-file#createtokenauthtoken-options
			regexp.MustCompile(`(?:^|/)@octokit/auth-token/README\.md$`),
		},
	},
}

func GitHubPat() *config.Rule {
	// define rule
	r := config.Rule{
		RuleID:      "github-pat",
		Description: "Uncovered a GitHub Personal Access Token, potentially leading to unauthorized repository access and sensitive content exposure.",
		Regex:       regexp.MustCompile(`ghp_[0-9a-zA-Z]{36}`),
		Entropy:     3,
		Keywords:    []string{"ghp_"},
		Allowlists:  githubAllowlist,
		ValidateCEL: githubTokenCEL,
	}

	// validate
	tps := utils.GenerateSampleSecrets("github", "ghp_"+secrets.NewSecret(utils.AlphaNumeric("36")))
	fps := []string{
		"ghp_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
	}
	return utils.Validate(r, tps, fps)
}

func GitHubFineGrainedPat() *config.Rule {
	// define rule
	r := config.Rule{
		RuleID:      "github-fine-grained-pat",
		Description: "Found a GitHub Fine-Grained Personal Access Token, risking unauthorized repository access and code manipulation.",
		Regex:       regexp.MustCompile(`github_pat_\w{82}`),
		Entropy:     3,
		Keywords:    []string{"github_pat_"},
		ValidateCEL: githubTokenCEL,
	}

	// validate
	tps := utils.GenerateSampleSecrets("github", "github_pat_"+secrets.NewSecret(utils.AlphaNumeric("82")))
	fps := []string{
		"github_pat_xxxxxxxxxxxxxxxxxxxxxx_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
	}
	return utils.Validate(r, tps, fps)
}

func GitHubOauth() *config.Rule {
	// define rule
	r := config.Rule{
		RuleID:      "github-oauth",
		Description: "Discovered a GitHub OAuth Access Token, posing a risk of compromised GitHub account integrations and data leaks.",
		Regex:       regexp.MustCompile(`gho_[0-9a-zA-Z]{36}`),
		Entropy:     3,
		Keywords:    []string{"gho_"},
		ValidateCEL: githubTokenCEL,
	}

	// validate
	tps := utils.GenerateSampleSecrets("github", "gho_"+secrets.NewSecret(utils.AlphaNumeric("36")))
	fps := []string{
		"gho_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
	}
	return utils.Validate(r, tps, fps)
}

func GitHubApp() *config.Rule {
	// define rule
	r := config.Rule{
		RuleID:      "github-app-token",
		Description: "Identified a GitHub App Token, which may compromise GitHub application integrations and source code security.",
		Regex:       regexp.MustCompile(`(?:ghu|ghs)_[0-9a-zA-Z]{36}`),
		Entropy:     3,
		Keywords:    []string{"ghu_", "ghs_"},
		Allowlists:  githubAllowlist,
		ValidateCEL: githubTokenCEL,
	}

	// validate
	tps := utils.GenerateSampleSecrets("github", "ghs_"+secrets.NewSecret(utils.AlphaNumeric("36")))
	tps = append(tps, utils.GenerateSampleSecrets("github", "ghu_"+secrets.NewSecret(utils.AlphaNumeric("36")))...)
	fps := []string{
		"ghu_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		"ghs_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
	}
	return utils.Validate(r, tps, fps)
}

func GitHubRefresh() *config.Rule {
	// define rule
	r := config.Rule{
		RuleID:      "github-refresh-token",
		Description: "Detected a GitHub Refresh Token, which could allow prolonged unauthorized access to GitHub services.",
		Regex:       regexp.MustCompile(`ghr_[0-9a-zA-Z]{36}`),
		Entropy:     3,
		Keywords:    []string{"ghr_"},
		ValidateCEL: githubTokenCEL,
	}

	// validate
	tps := utils.GenerateSampleSecrets("github", "ghr_"+secrets.NewSecret(utils.AlphaNumeric("36")))
	fps := []string{
		"ghr_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
	}
	return utils.Validate(r, tps, fps)
}
