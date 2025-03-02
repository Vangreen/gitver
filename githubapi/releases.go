package githubapi

import (
	"encoding/json"
	"fmt"
	"gitvergo/repository"
	"html/template"
	"io"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
)

// LoadReleases fetches your starred repos and then for each fetches up to 3 latest releases.
func LoadReleases(githubToken string) ([]repository.Release, error) {
	page := 0
	var starred []GitHubRepo
	fetchedStarredRepos, err := fetchStarredRepos(page, githubToken)
	if err != nil {
		return nil, err
	}
	for {
		if len(fetchedStarredRepos) > 0 {
			fmt.Println(page)
			starred = append(starred, fetchedStarredRepos...)
			page++
			fetchedStarredRepos, err = fetchStarredRepos(page, githubToken)
			if err != nil {
				return nil, err
			}
		} else {
			fmt.Println("break")
			break

		}
	}

	const maxConcurrency = 5
	sem := make(chan struct{}, maxConcurrency)

	var (
		wg          sync.WaitGroup
		releaseChan = make(chan repository.Release)
	)

	// Launch a goroutine for each starred repository
	for _, repo := range starred {
		wg.Add(1)
		go func(repo GitHubRepo) {
			defer wg.Done()

			// Acquire semaphore to limit concurrency
			sem <- struct{}{}
			defer func() { <-sem }()

			rels, err := fetchReleases(repo.Owner.Login, repo.Name, githubToken)
			if err != nil {
				log.Printf("Error fetching releases for %s/%s: %v", repo.Owner.Login, repo.Name, err)
				return
			}
			// Limit to max 3 releases per repository
			if len(rels) > 3 {
				rels = rels[:3]
			}
			for _, r := range rels {
				releaseChan <- repository.Release{
					RepoName:         repo.Name,
					TagName:          r.TagName,
					ReleaseName:      r.Name,
					PublishedAt:      r.PublishedAt,
					PublishedTimeAgo: timeAgo(r.PublishedAt),
					URL:              r.HTMLURL,
					AvatarURL:        repo.Owner.AvatarURL,
					Changelog:        template.HTML(r.Body),
				}
			}
		}(repo)
	}

	// Close the channel once all goroutines finish
	go func() {
		wg.Wait()
		close(releaseChan)
	}()

	var allReleases []repository.Release
	for rel := range releaseChan {
		allReleases = append(allReleases, rel)
	}

	// Sort all releases in chronological order (newest first).
	sort.Slice(allReleases, func(i, j int) bool {
		return allReleases[i].PublishedAt.After(allReleases[j].PublishedAt)
	})

	return allReleases, nil
}

// fetchReleases returns a slice of releases for a given repository.
func fetchReleases(owner, repo string, githubToken string) ([]GitHubRelease, error) {
	fmt.Println("fetch/" + owner + "/" + repo)
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases", owner, repo)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/vnd.github.v3+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read the entire body
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// If status code is not 200, try to unmarshal the error message.
	if resp.StatusCode != 200 {
		var errMsg struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &errMsg)
		return nil, fmt.Errorf("GitHub API error: %s", errMsg.Message)
	}

	var releases []GitHubRelease
	if err := json.Unmarshal(data, &releases); err != nil {
		// If unmarshaling into an array fails, try to unmarshal as an error object.
		var errObj struct {
			Message string `json:"message"`
		}
		if err2 := json.Unmarshal(data, &errObj); err2 == nil && errObj.Message != "" {
			return nil, fmt.Errorf("GitHub API error: %s", errObj.Message)
		}
		return nil, err
	}

	return releases, nil
}

func timeAgo(t time.Time) string {
	duration := time.Since(t)
	seconds := int(duration.Seconds())

	switch {
	case seconds < 60:
		return fmt.Sprintf("%d seconds ago", seconds)
	case seconds < 3600:
		return fmt.Sprintf("%d minutes ago", seconds/60)
	case seconds < 86400:
		return fmt.Sprintf("%d hours ago", seconds/3600)
	case seconds < 604800:
		return fmt.Sprintf("%d days ago", seconds/86400)
	default:
		return fmt.Sprintf("%d weeks ago", seconds/604800)
	}
}
