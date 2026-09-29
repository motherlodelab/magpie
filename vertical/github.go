package vertical

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

func init() {
	register(Extractor{
		Info: Info{
			Name:  "github_repo",
			Label: "GitHub",
			Desc:  "Repo, issue, PR, or release via the api.github.com REST API (no key needed; MAGPIE_GITHUB_TOKEN raises the 60→5000 req/h limit).",
			Patterns: []string{
				"https://github.com/{owner}/{repo}",
				"https://github.com/{owner}/{repo}/issues/{n}",
				"https://github.com/{owner}/{repo}/pull/{n}",
				"https://github.com/{owner}/{repo}/releases[/tag/{tag}]",
			},
		},
		Match:   matchGithub,
		Extract: extractGithub,
	})
}

// matchGithub accepts github.com/{owner}/{repo} with optional
// issues|pull|releases tails. Gists, raw, and codeload hosts are out.
func matchGithub(u *url.URL) bool {
	if !hostIs(u, "github.com", "www.github.com") {
		return false
	}
	segs := pathSegs(u.Path)
	if len(segs) < 2 {
		return false
	}
	if len(segs) == 2 {
		return true
	}
	switch segs[2] {
	case "issues", "pull", "releases":
		return true
	}
	return false
}

func extractGithub(ctx context.Context, f Fetcher, u *url.URL) (map[string]any, error) {
	segs := pathSegs(u.Path)
	owner, repo := segs[0], segs[1]
	base := "https://api.github.com/repos/" + owner + "/" + repo
	canon := "https://github.com/" + owner + "/" + repo
	if len(segs) == 2 {
		m, err := fetchJSON(ctx, f, base, githubAuthHeaders()...)
		if err != nil {
			return nil, err
		}
		repoM := m
		_ = repoM
		return map[string]any{
			"kind":        "repo",
			"full_name":   str(m, "full_name"),
			"description": str(m, "description"),
			"stars":       num(m, "stargazers_count"),
			"forks":       num(m, "forks_count"),
			"language":    str(m, "language"),
			"license":     strings.ToLower(str(child(m, "license"), "name")),
			"open_issues": num(m, "open_issues_count"),
			"url":         canon,
		}, nil
	}
	switch segs[2] {
	case "issues", "pull":
		if len(segs) < 4 || segs[3] == "" {
			return nil, fmt.Errorf("vertical: github %s URL missing number", segs[2])
		}
		n := segs[3]
		api := base + "/issues/" + n
		if segs[2] == "pull" {
			api = base + "/pulls/" + n
		}
		m, err := fetchJSON(ctx, f, api, githubAuthHeaders()...)
		if err != nil {
			return nil, err
		}
		kind := "issue"
		if segs[2] == "pull" {
			kind = "pr"
		}
		return map[string]any{
			"kind":     kind,
			"title":    str(m, "title"),
			"author":   str(child(m, "user"), "login"),
			"state":    str(m, "state"),
			"comments": num(m, "comments"),
			"body":     str(m, "body"),
			"url":      canon + "/" + segs[2] + "/" + n,
		}, nil
	case "releases":
		if len(segs) >= 5 && segs[3] == "tag" {
			tag := segs[4]
			m, err := fetchJSON(ctx, f, base+"/releases/tags/"+tag, githubAuthHeaders()...)
			if err != nil {
				return nil, err
			}
			return releaseMap(m, canon+"/releases/tag/"+tag), nil
		}
		body, err := fetchBytes(ctx, f, base+"/releases", githubAuthHeaders()...)
		if err != nil {
			return nil, err
		}
		m, err := firstJSONArray(body)
		if err != nil {
			return nil, fmt.Errorf("vertical: GET %s/releases: %w", base, err)
		}
		return releaseMap(m, canon+"/releases"), nil
	}
	return nil, fmt.Errorf("vertical: github: unhandled path %s: %w", u.Path, ErrNoMatch)
}

func releaseMap(m map[string]any, url string) map[string]any {
	title := str(m, "name")
	if title == "" {
		title = str(m, "tag_name")
	}
	return map[string]any{
		"kind":  "release",
		"title": title,
		"tag":   str(m, "tag_name"),
		"body":  str(m, "body"),
		"url":   url,
	}
}

// firstJSONArray unmarshals a top-level JSON array and returns its first
// object element.
