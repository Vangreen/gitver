package githubapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

// fetchStarredRepos returns a slice of starred repositories.
func fetchStarredRepos(page int, githubToken string) ([]GitHubRepo, error) {
	url := "https://api.github.com/user/starred?per_page=100&page=" + strconv.Itoa(page)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+githubToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var repos []GitHubRepo
	if err := json.NewDecoder(resp.Body).Decode(&repos); err != nil {
		return nil, err
	}
	return repos, nil
}
