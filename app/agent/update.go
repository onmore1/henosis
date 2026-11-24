package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Repo is the project's home page; new versions and problem reports go
// through it.
const Repo = "https://github.com/onmore1/henosis"

// ReleasesURL is where the app asks for the newest version. Empty turns the
// check off (the tests do that).
var ReleasesURL = "https://api.github.com/repos/onmore1/henosis/releases/latest"

// UpdateCheckDelay is how long after start the first check waits.
var UpdateCheckDelay = 5 * time.Second

// UpdateInfo is a newer release than the one running.
type UpdateInfo struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// watchUpdates asks for the newest release shortly after start and then a
// few times a day. GitHub allows 60 such requests an hour per address.
func (a *Agent) watchUpdates(ctx context.Context) {
	url, running := a.releasesURL, a.version
	if url == "" {
		return
	}
	wait := a.updateDelay
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = 6 * time.Hour
		info, err := latestRelease(ctx, url, running)
		if err != nil || info == nil || !newerVersion(info.Version, running) {
			continue
		}
		if old := a.update.Swap(info); old == nil || old.Version != info.Version {
			a.logf("info", "Hénōsis %s is out (this is %s): %s", info.Version, running, info.URL)
		}
	}
}

func latestRelease(ctx context.Context, url, running string) (*UpdateInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Henosis/"+running)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		URL    string `json:"html_url"`
		Draft  bool   `json:"draft"`
		Prerel bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.Draft || rel.Prerel || rel.Tag == "" {
		return nil, nil
	}
	// Only ever send people to this project's own release pages.
	if !strings.HasPrefix(rel.URL, Repo+"/releases/") {
		rel.URL = Repo + "/releases/latest"
	}
	return &UpdateInfo{Version: strings.TrimPrefix(rel.Tag, "v"), URL: rel.URL}, nil
}

// newerVersion reports whether a is a later x.y.z than b. Anything that is
// not a plain version counts as not newer.
func newerVersion(a, b string) bool {
	pa, okA := parseVersion(a)
	pb, okB := parseVersion(b)
	if !okA || !okB {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// OpenLink opens a web page in the default browser. Tests replace it.
func (a *Agent) OpenLink(url string) error {
	if a.OpenURL != nil {
		return a.OpenURL(url)
	}
	return OpenBrowser(url)
}
