package repository

import (
	"html/template"
	"time"
)

type Release struct {
	RepoName         string
	TagName          string
	ReleaseName      string
	PublishedAt      time.Time
	PublishedTimeAgo string
	URL              string
	AvatarURL        string
	Changelog        template.HTML
}

type PaginatedReleases struct {
	Releases    []Release
	CurrentPage int
	TotalPages  int
	PrevPage    int
	NextPage    int
}
