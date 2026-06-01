package assay

// dedupeByHash returns findings with duplicate hashes removed, preserving the
// first occurrence and overall order. Findings with an empty hash are dropped
// (they were never finalized and cannot be persisted or deduped reliably).
func dedupeByHash(findings []Finding) []Finding {
	seen := make(map[string]bool, len(findings))
	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		if f.Hash == "" || seen[f.Hash] {
			continue
		}
		seen[f.Hash] = true
		out = append(out, f)
	}
	return out
}

// suppressPostedNits drops Nit findings whose hash was already posted on a prior
// review of the same PR. Important and PreExisting findings are never
// suppressed. Returns the filtered slice and the number of Nits dropped.
func suppressPostedNits(findings []Finding, posted map[string]bool) ([]Finding, int) {
	if len(posted) == 0 {
		return findings, 0
	}
	out := make([]Finding, 0, len(findings))
	dropped := 0
	for _, f := range findings {
		if f.Severity == SeverityNit && posted[f.Hash] {
			dropped++
			continue
		}
		out = append(out, f)
	}
	return out, dropped
}

// capNits limits the number of Nit findings to limit, preserving order and
// keeping the first limit Nits encountered. Important and PreExisting findings
// are always kept. A limit <= 0 means "no cap". Returns the capped slice and
// the number of Nits dropped.
func capNits(findings []Finding, limit int) ([]Finding, int) {
	if limit <= 0 {
		return findings, 0
	}
	out := make([]Finding, 0, len(findings))
	kept := 0
	dropped := 0
	for _, f := range findings {
		if f.Severity == SeverityNit {
			if kept >= limit {
				dropped++
				continue
			}
			kept++
		}
		out = append(out, f)
	}
	return out, dropped
}
