package proxy

import (
	"kiro-go/config"
	"strings"
)

// Kiro/CodeWhisperer data-plane region handling.
//
// An account has two independent regions:
//   - the IdC/auth region (account.Region) where the OIDC token is minted and
//     refreshed (e.g. us-east-1), and
//   - the data-plane region where the account's Kiro profile actually lives and
//     where usage/profile/chat calls must go (e.g. eu-central-1).
//
// These frequently differ: an org's IAM Identity Center can be in us-east-1
// while AWS provisions the Q Developer/Kiro profile in eu-central-1. The profile
// ARN (arn:aws:codewhisperer:<region>:<acct>:profile/<id>) is authoritative for
// the data-plane region. Routing data-plane calls to the auth region instead
// yields an empty profile list and 403s.

// dataPlaneRegion returns the region for an account's Kiro data-plane calls.
// Priority: profile ARN region → account region → us-east-1.
func dataPlaneRegion(account *config.Account) string {
	if account != nil {
		if r := regionFromProfileArn(account.ProfileArn); r != "" {
			return r
		}
		if r := strings.TrimSpace(account.Region); r != "" {
			return r
		}
	}
	return "us-east-1"
}

// regionFromProfileArn extracts <region> from
// arn:aws:codewhisperer:<region>:<account>:profile/<id>.
func regionFromProfileArn(arn string) string {
	arn = strings.TrimSpace(arn)
	if arn == "" {
		return ""
	}
	parts := strings.Split(arn, ":")
	if len(parts) >= 4 && parts[0] == "arn" {
		return strings.TrimSpace(parts[3])
	}
	return ""
}

// restBaseURL returns the Kiro REST/control-plane base URL for a region.
// us-east-1 keeps the historical codewhisperer.* host (the only region where it
// resolves and the path proven for existing accounts); other regions use the
// regional q.* host, which is the only one that resolves there.
func restBaseURL(region string) string {
	region = strings.TrimSpace(region)
	if region == "" || region == "us-east-1" {
		return "https://codewhisperer.us-east-1.amazonaws.com"
	}
	return "https://q." + region + ".amazonaws.com"
}

// candidateProfileRegions lists regions to probe when discovering an account's
// profile (profile ARN not yet known). The account's own region is tried first,
// then the common Kiro/Q Developer regions.
func candidateProfileRegions(account *config.Account) []string {
	seen := map[string]bool{}
	var out []string
	add := func(r string) {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			return
		}
		seen[r] = true
		out = append(out, r)
	}
	if account != nil {
		add(regionFromProfileArn(account.ProfileArn))
		add(account.Region)
	}
	for _, r := range []string{"us-east-1", "eu-central-1", "ap-southeast-1", "eu-west-1", "us-west-2"} {
		add(r)
	}
	return out
}
