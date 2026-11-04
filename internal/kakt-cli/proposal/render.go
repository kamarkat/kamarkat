package proposal

import (
	"fmt"
	"strings"
)

// Markdown renders the verdict for the pull request the proposal opened.
//
// Written for the person deciding, not for the platform: it opens with the
// answer, and the detail is what was observed rather than which check ran. A
// verdict nobody reads is the same as no verdict, and a wall of check names is
// how that happens.
func (v Verdict) Markdown() string {
	var b strings.Builder

	switch v.Outcome {
	case Pass:
		b.WriteString("## Pre-flight: this cluster can take this version\n\n")
	case Fail:
		b.WriteString("## Pre-flight: **do not merge**\n\n")
	default:
		b.WriteString("## Pre-flight: **unverified**\n\n")
	}

	from := v.From
	if from == "" {
		from = "unknown"
	}
	fmt.Fprintf(&b, "`%s` — %s → **%s**\n\n", v.Cluster, from, v.Candidate)

	if v.Outcome == Unverified {
		// ADR-067: "A proposal that cannot be verified against its target is
		// raised as unverified and says so." Saying so means saying what it
		// means, or the word is read as a softer pass.
		b.WriteString("Some checks could not run, so this is not a judgement that " +
			"the version is unsafe — it is the absence of one. Merging is the " +
			"tenant's decision either way (ADR-065); what is missing is the " +
			"evidence it would have been made on.\n\n")
	}

	b.WriteString("| | check | observed |\n|---|---|---|\n")
	for _, r := range v.Results {
		mark := map[Outcome]string{Pass: "✓", Fail: "✗", Unverified: "?"}[r.Outcome]
		fmt.Fprintf(&b, "| %s | %s | %s |\n", mark, r.Name, r.Detail)
	}

	b.WriteString("\nProduced inside this box, against this cluster. The platform " +
		"holds no access to it and receives only this verdict (ADR-067).\n")
	return b.String()
}

// Summary is the one line a check run or a log shows.
func (v Verdict) Summary() string {
	var pass, fail, unver int
	for _, r := range v.Results {
		switch r.Outcome {
		case Pass:
			pass++
		case Fail:
			fail++
		default:
			unver++
		}
	}
	return fmt.Sprintf("%s: %d passed, %d failed, %d unverified",
		strings.ToUpper(string(v.Outcome)), pass, fail, unver)
}
