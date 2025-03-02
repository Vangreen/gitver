package githubapi

import (
	"time"
)

type GitHubRepo struct {
	Name  string `json:"name"`
	Owner struct {
		AvatarURL string `json:"avatar_url"`
		Login     string `json:"login"`
	} `json:"owner"`
}

type GitHubRelease struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	PublishedAt time.Time `json:"published_at"`
	Body        string    `json:"body"`
}
