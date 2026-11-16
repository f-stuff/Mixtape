package main

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

var cleanSpaces = regexp.MustCompile(`\s+`)

func normalize(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return ' '
	}, s)
	return cleanSpaces.ReplaceAllString(strings.TrimSpace(s), " ")
}
func hasPhrase(haystack, needle string) bool {
	haystack = " " + normalize(haystack) + " "
	needle = " " + normalize(needle) + " "
	return len(strings.TrimSpace(needle)) > 0 && strings.Contains(haystack, needle)
}
func tokenCoverage(title, expected string) float64 {
	words := strings.Fields(normalize(expected))
	if len(words) == 0 {
		return 0
	}
	pool := map[string]bool{}
	for _, v := range strings.Fields(normalize(title)) {
		pool[v] = true
	}
	hit := 0
	for _, v := range words {
		if pool[v] {
			hit++
		}
	}
	return float64(hit) / float64(len(words))
}
func scoreCandidate(c Candidate, artist, title string, durationMS int) int {
	score := 0
	expected := normalize(title)
	if expected == "" {
		return 0
	}
	coverage := tokenCoverage(c.Title, title)
	score += int(coverage * 45)
	if hasPhrase(c.Title, title) {
		score += 17
	}
	if coverage < 0.8 {
		score -= 26
	}
	if hasPhrase(c.Title, artist) {
		score += 20
	} else if hasPhrase(c.Channel, artist) {
		score += 17
	}
	// Safeguards against common inaccurate search results.
	for _, p := range []string{"live", "cover", "karaoke", "reaction", "tutorial", "instrumental", "sped up", "slowed", "nightcore", "remix", "8d audio", "lyrics", "fanmade", "fan made"} {
		if hasPhrase(c.Title, p) && !hasPhrase(title, p) {
			score -= 35
		}
	}
	if hasPhrase(c.Title, "official audio") || hasPhrase(c.Title, "topic") {
		score += 8
	}
	if strings.Contains(strings.ToLower(c.Channel), "- topic") {
		score += 12
	}
	if durationMS > 1000 && c.Duration > 0 {
		difference := math.Abs(float64(c.Duration*1000-durationMS)) / 1000
		switch {
		case difference <= 5:
			score += 24
		case difference <= 12:
			score += 17
		case difference <= 22:
			score += 6
		case difference > 45:
			score -= 65
		case difference > 30:
			score -= 40
		default:
			score -= 20
		}
	} else if durationMS > 1000 {
		score -= 10
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	return score
}
func rankCandidates(candidates []Candidate, artist, title string, durationMS int) []Candidate {
	for i := range candidates {
		candidates[i].Score = scoreCandidate(candidates[i], artist, title, durationMS)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Score > candidates[j].Score })
	return candidates
}
