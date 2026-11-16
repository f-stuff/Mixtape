package main

import (
	"strings"
	"unicode/utf8"
)

// jobFailure stores a short, readable message for the queue and a longer
// technical explanation for the Copy error action. It never loses a useful
// yt-dlp diagnostic just because the queue only has room for a short label.
func jobFailure(err error) (summary, detail string) {
	if err == nil {
		return "", ""
	}
	detail = trimErrorDetail(err.Error(), 4000)
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "no space left on device"), strings.Contains(lower, "disk full"), strings.Contains(lower, "not enough space"):
		return "The music drive is full. Free up some space and try again.", detail
	case strings.Contains(lower, "permission denied"), strings.Contains(lower, "access is denied"), strings.Contains(lower, "access denied"):
		return "Can't write to the music folder. Check its permissions.", detail
	case strings.Contains(lower, "http error 429"), strings.Contains(lower, "too many requests"):
		return "YouTube is limiting requests. Try again later.", detail
	case strings.Contains(lower, "http error 403"), strings.Contains(lower, "forbidden"):
		return "YouTube refused this download. Try later or choose another recording.", detail
	case strings.Contains(lower, "sign in"), strings.Contains(lower, "login required"), strings.Contains(lower, "private video"):
		return "This recording requires sign-in or isn't publicly available.", detail
	case strings.Contains(lower, "video unavailable"), strings.Contains(lower, "video is unavailable"), strings.Contains(lower, "not available in your country"):
		return "This recording isn't available. Try a different link or match.", detail
	case strings.Contains(lower, "timed out"), strings.Contains(lower, "timeout"), strings.Contains(lower, "connection reset"), strings.Contains(lower, "network is unreachable"), strings.Contains(lower, "no such host"):
		return "Network problem. Check your connection and retry.", detail
	case strings.Contains(lower, "mp3 tagging failed"), strings.Contains(lower, "tagging did not produce"):
		return "Couldn't finish the MP3 tags. Check the audio tools and retry.", detail
	case strings.Contains(lower, "download ended without a completed mp3"):
		return "No finished MP3 was produced. Open the error details and retry.", detail
	case strings.Contains(lower, "downloaded file could not be found"):
		return "The downloaded file is missing. Check the output folder.", detail
	default:
		// Unrecognised failures should still be meaningful, rather than just
		// displaying "Download failed" with no explanation.
		s := strings.TrimSpace(strings.ReplaceAll(detail, "\r", ""))
		if idx := strings.LastIndex(s, "ERROR:"); idx >= 0 {
			s = strings.TrimSpace(s[idx+len("ERROR:"):])
		}
		if idx := strings.IndexByte(s, '\n'); idx >= 0 {
			s = s[:idx]
		}
		if s == "" {
			s = "Download failed. See technical details."
		}
		if utf8.RuneCountInString(s) > 200 {
			s = string([]rune(s)[:200]) + "…"
		}
		return s, detail
	}
}

func trimErrorDetail(s string, limit int) string {
	s = strings.TrimSpace(s)
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	// The last lines usually carry the actionable cause (not the progress log).
	r := []rune(s)
	if limit == 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(limit-1):])
}
