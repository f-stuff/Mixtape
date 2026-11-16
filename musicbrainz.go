package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var mbBase = "https://musicbrainz.org/ws/2"

type ArtistResult struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Country     string `json:"country,omitempty"`
	Score       int    `json:"score"`
}
type AlbumResult struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Date     string `json:"date"`
	Type     string `json:"type"`
	CoverURL string `json:"coverUrl"`
}
type MBTrack struct {
	Title      string
	Number     string
	Disc       int
	DurationMS int
}

// MusicBrainz asks clients to stay below one request per second. A few
// intermittent gateway/rate-limit failures are normal; retry them rather than
// making someone search for the same artist/album again by hand.
// Retries share the same rate limiter as other metadata requests.
func (a *App) mbGET(ctx context.Context, path string, out any) error {
	a.mbMu.Lock()
	defer a.mbMu.Unlock()
	var lastErr error
	var extraWait time.Duration
	for attempt := 0; attempt < 3; attempt++ {
		wait := time.Until(a.lastMB.Add(1150 * time.Millisecond))
		if extraWait > wait {
			wait = extraWait
		}
		if err := waitForContext(ctx, wait); err != nil {
			return err
		}
		a.lastMB = time.Now()
		req, err := http.NewRequestWithContext(ctx, "GET", mbBase+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "Mixtape/"+appVersion+" (https://github.com/f-stuff/Mixtape)")
		req.Header.Set("Accept", "application/json")
		res, err := a.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lastErr = fmt.Errorf("MusicBrainz connection failed: %w", err)
		} else {
			if res.StatusCode == http.StatusOK {
				decodeErr := json.NewDecoder(io.LimitReader(res.Body, 5*1024*1024)).Decode(out)
				_ = res.Body.Close()
				if decodeErr == nil {
					return nil
				}
				// A truncated JSON response is usually temporary too.
				lastErr = fmt.Errorf("MusicBrainz sent an incomplete response: %w", decodeErr)
			} else {
				if res.StatusCode != http.StatusTooManyRequests && res.StatusCode != 500 &&
					res.StatusCode != 502 && res.StatusCode != 503 && res.StatusCode != 504 {
					_ = res.Body.Close()
					return fmt.Errorf("MusicBrainz returned HTTP %d", res.StatusCode)
				}
				lastErr = fmt.Errorf("MusicBrainz is temporarily unavailable (HTTP %d)", res.StatusCode)
				if seconds, err := strconv.Atoi(strings.TrimSpace(res.Header.Get("Retry-After"))); err == nil && seconds > 0 {
					extraWait = min(time.Duration(seconds)*time.Second, 12*time.Second)
				}
				_ = res.Body.Close()
			}
		}
		if attempt < 2 {
			extraWait = max(extraWait, time.Duration(attempt+1)*1500*time.Millisecond)
		}
	}
	return fmt.Errorf("%w (tried 3 times)", lastErr)
}

func waitForContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *App) findArtists(ctx context.Context, q string) ([]ArtistResult, error) {
	var data struct {
		Artists []struct {
			ID             string `json:"id"`
			Name           string `json:"name"`
			Disambiguation string `json:"disambiguation"`
			Country        string `json:"country"`
			Type           string `json:"type"`
			Score          int    `json:"score"`
		} `json:"artists"`
	}
	path := "/artist?query=" + url.QueryEscape("artist:\""+q+"\"") + "&fmt=json&limit=12"
	if err := a.mbGET(ctx, path, &data); err != nil {
		return nil, err
	}
	result := make([]ArtistResult, 0, len(data.Artists))
	for _, v := range data.Artists {
		description := strings.TrimSpace(v.Disambiguation)
		if description == "" {
			description = v.Type
		}
		result = append(result, ArtistResult{ID: v.ID, Name: v.Name, Description: description, Country: v.Country, Score: v.Score})
	}
	return result, nil
}

func (a *App) findAlbums(ctx context.Context, artistID string) ([]AlbumResult, error) {
	result := []AlbumResult{}
	seen := map[string]bool{}
	for offset := 0; offset < 300; offset += 100 {
		var data struct {
			Count  int `json:"release-group-count"`
			Groups []struct {
				ID        string   `json:"id"`
				Title     string   `json:"title"`
				Date      string   `json:"first-release-date"`
				Primary   string   `json:"primary-type"`
				Secondary []string `json:"secondary-types"`
			} `json:"release-groups"`
		}
		p := fmt.Sprintf("/release-group?artist=%s&type=album&fmt=json&limit=100&offset=%d", url.QueryEscape(artistID), offset)
		if err := a.mbGET(ctx, p, &data); err != nil {
			return nil, err
		}
		for _, g := range data.Groups {
			if g.Primary != "Album" || seen[g.ID] {
				continue
			}
			// Keep the discography focused on regular albums. Exclude special releases.
			excluded := false
			for _, s := range g.Secondary {
				switch strings.ToLower(s) {
				case "live", "compilation", "remix", "soundtrack", "dj-mix", "audiobook", "interview", "demo", "spokenword", "field recording", "audio drama":
					excluded = true
				}
			}
			if excluded {
				continue
			}
			seen[g.ID] = true
			result = append(result, AlbumResult{ID: g.ID, Title: g.Title, Date: g.Date, Type: "Album", CoverURL: "https://coverartarchive.org/release-group/" + g.ID + "/front-250"})
		}
		if len(data.Groups) == 0 || offset+100 >= data.Count {
			break
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Date == result[j].Date {
			return strings.ToLower(result[i].Title) < strings.ToLower(result[j].Title)
		}
		if result[i].Date == "" {
			return false
		}
		if result[j].Date == "" {
			return true
		}
		return result[i].Date < result[j].Date
	})
	return result, nil
}

func (a *App) getAlbumTracks(ctx context.Context, groupID string, groupTitle string, groupDate string) ([]MBTrack, error) {
	var data struct {
		Releases []struct {
			ID             string `json:"id"`
			Title          string `json:"title"`
			Date           string `json:"date"`
			Country        string `json:"country"`
			Status         string `json:"status"`
			Disambiguation string `json:"disambiguation"`
		} `json:"releases"`
	}
	path := "/release?release-group=" + url.QueryEscape(groupID) + "&status=official&fmt=json&limit=100"
	if err := a.mbGET(ctx, path, &data); err != nil {
		return nil, err
	}
	if len(data.Releases) == 0 { // Some unusual/older releases have no status categorisation.
		path = "/release?release-group=" + url.QueryEscape(groupID) + "&fmt=json&limit=100"
		if err := a.mbGET(ctx, path, &data); err != nil {
			return nil, err
		}
	}
	if len(data.Releases) == 0 {
		return nil, errors.New("No album editions found in MusicBrainz")
	}
	type ranked struct {
		id    string
		score int
	}
	candidates := []ranked{}
	for _, v := range data.Releases {
		score := 0
		if strings.EqualFold(v.Title, groupTitle) {
			score += 25
		}
		if v.Country == "XW" || v.Country == "US" || v.Country == "GB" {
			score += 12
		}
		if v.Status == "Official" {
			score += 15
		}
		bad := strings.ToLower(v.Title + " " + v.Disambiguation)
		for _, term := range []string{"deluxe", "expanded", "anniversary", "bonus track", "special edition", "karaoke", "remix", "vinyl", "live at"} {
			if strings.Contains(bad, term) {
				score -= 15
			}
		}
		if len(v.Date) >= 4 {
			score += 3
			if len(groupDate) >= 4 {
				groupYear, err1 := strconv.Atoi(groupDate[:4])
				releaseYear, err2 := strconv.Atoi(v.Date[:4])
				if err1 == nil && err2 == nil {
					gap := releaseYear - groupYear
					if gap >= 0 && gap <= 1 {
						score += 22
					}
					if gap > 6 {
						score -= 9
					}
				}
			}
		}
		candidates = append(candidates, ranked{v.ID, score})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	var last error
	for i, c := range candidates {
		if i >= 4 {
			break
		}
		var release struct {
			Media []struct {
				Position int `json:"position"`
				Tracks   []struct {
					Number    string `json:"number"`
					Position  int    `json:"position"`
					Title     string `json:"title"`
					Length    int    `json:"length"`
					Recording struct {
						Title  string `json:"title"`
						Length int    `json:"length"`
					} `json:"recording"`
				} `json:"tracks"`
			} `json:"media"`
		}
		err := a.mbGET(ctx, "/release/"+c.id+"?inc=recordings&fmt=json", &release)
		if err != nil {
			last = err
			continue
		}
		songs := []MBTrack{}
		discCount := len(release.Media)
		for discIndex, media := range release.Media {
			if len(media.Tracks) == 0 {
				continue
			}
			for _, t := range media.Tracks {
				title := strings.TrimSpace(t.Title)
				if title == "" {
					title = t.Recording.Title
				}
				if title == "" {
					continue
				}
				num := t.Number
				if num == "" {
					num = fmt.Sprintf("%02d", t.Position)
				}
				if discCount > 1 {
					num = fmt.Sprintf("%d-%02d", discIndex+1, t.Position)
				}
				duration := t.Length
				if duration == 0 {
					duration = t.Recording.Length
				}
				songs = append(songs, MBTrack{Title: title, Number: num, Disc: discIndex + 1, DurationMS: duration})
			}
		}
		if len(songs) > 0 && len(songs) < 250 {
			return songs, nil
		}
	}
	if last != nil {
		return nil, last
	}
	return nil, errors.New("No usable tracklist found for this album")
}
