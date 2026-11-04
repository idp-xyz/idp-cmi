// Package guard is the CMI's write-time pollution check (ADR-0001 D12): deterministic detection of
// instruction-injection patterns in content that is about to become memory or knowledge. The rules
// are the same as the HOST's input guard (HOST ADR-0001 D27), copied rather than shared (D8).
// Findings never block a write; they flag the record so the resolver quarantines it.
package guard

import (
	"regexp"
	"sort"
)

var injectionRules = []struct {
	name string
	re   *regexp.Regexp
}{
	{"instruction_override", regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\b[^.\n]{0,40}\b(previous|prior|above|earlier|all|your)\b[^.\n]{0,20}\b(instructions?|prompts?|rules|guidelines)\b`)},
	{"instruction_override", regexp.MustCompile(`(?i)\b(new|updated|real|actual)\s+(system\s+)?(instructions?|prompt)\s*:`)},
	{"role_impersonation", regexp.MustCompile(`(?im)^\s*(system|assistant|developer)\s*:\s`)},
	{"role_impersonation", regexp.MustCompile(`(?i)(<\|im_start\|>|<\|system\|>|<\|assistant\|>|\[INST\]|<<SYS>>|###\s*(system|instruction)s?\b|<system>|</system>)`)},
	{"persona_switch", regexp.MustCompile(`(?i)\byou are now\b|\bact as (the|a|an)\s+(system|administrator|developer)\b|\bpretend (that )?you (are|have)\b`)},
	{"embedded_tool_call", regexp.MustCompile(`"(tool_calls|function_call|toolCalls)"\s*:|"function"\s*:\s*\{\s*"name"\s*:`)},
	{"secret_exfiltration", regexp.MustCompile(`(?i)\b(reveal|print|show|repeat|output)\b[^.\n]{0,30}\b(system prompt|your instructions|hidden prompt|api key|secret)\b`)},
	{"invisible_text", regexp.MustCompile(`[\x{200B}\x{200C}\x{200D}\x{2060}\x{FEFF}\x{202A}-\x{202E}\x{2066}-\x{2069}]`)},
}

// Scan returns the flags for text: "injection:<rule>" per matching rule, sorted, each once.
func Scan(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range injectionRules {
		if seen[r.name] || !r.re.MatchString(text) {
			continue
		}
		seen[r.name] = true
		out = append(out, "injection:"+r.name)
	}
	sort.Strings(out)
	return out
}

// Union merges flag lists (sorted, unique).
func Union(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, f := range l {
			if !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	sort.Strings(out)
	return out
}
